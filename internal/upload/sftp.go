// Copyright 2020 The Moov Authors
// Use of this source code is governed by an Apache License
// license that can be found in the LICENSE file.

package upload

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/moov-io/achgateway/internal/service"
	"github.com/moov-io/achgateway/internal/sshx"
	"github.com/moov-io/base/log"

	"github.com/go-kit/kit/metrics/prometheus"
	"github.com/pkg/sftp"
	stdprometheus "github.com/prometheus/client_golang/prometheus"
	"golang.org/x/crypto/ssh"
)

var (
	sftpAgentUp = prometheus.NewGaugeFrom(stdprometheus.GaugeOpts{
		Name: "sftp_agent_up",
		Help: "Status of SFTP agent connection",
	}, []string{"hostname"})

	sftpConnectionRetries = prometheus.NewCounterFrom(stdprometheus.CounterOpts{
		Name: "sftp_connection_retries",
		Help: "Counter of SFTP connection retry attempts",
	}, []string{"hostname"})
)

type SFTPTransferAgent struct {
	conn   *ssh.Client
	client *sftp.Client
	cfg    service.UploadAgent
	logger log.Logger
	mu     sync.Mutex // protects all read/write methods
}

func newSFTPTransferAgent(logger log.Logger, cfg *service.UploadAgent) (*SFTPTransferAgent, error) {
	if cfg == nil || cfg.SFTP == nil {
		return nil, errors.New("nil SFTP config")
	}

	agent := &SFTPTransferAgent{cfg: *cfg, logger: logger}

	if err := rejectOutboundIPRange(cfg.SplitAllowedIPs(), cfg.SFTP.Hostname); err != nil {
		return nil, fmt.Errorf("sftp: %s is not whitelisted: %v", cfg.SFTP.Hostname, err)
	}

	_, err := agent.connection()

	agent.record(err) // track up metric for remote server

	return agent, err
}

func (agent *SFTPTransferAgent) ID() string {
	return agent.cfg.ID
}

// connection returns an sftp.Client which is connected to the remote server.
// This function will attempt to establish a new connection if none exists already.
//
// connection must be called within a mutex lock.
func (agent *SFTPTransferAgent) connection() (*sftp.Client, error) {
	if agent == nil {
		return nil, errors.New("nil agent / config")
	}

	if agent.client != nil {
		// Verify the connection works and if not drop through and reconnect
		if _, err := agent.client.Getwd(); err == nil {
			return agent.client, nil
		} else {
			// Our connection is having issues, so retry connecting
			agent.client.Close()
		}
	}

	conn, stdin, stdout, err := sftpConnect(agent.logger, agent.cfg)
	if err != nil {
		return nil, fmt.Errorf("upload: %v", err)
	}
	agent.conn = conn

	// Setup our SFTP client
	var opts = []sftp.ClientOption{
		sftp.MaxConcurrentRequestsPerFile(agent.cfg.SFTP.MaxConnections()),
		sftp.MaxPacket(agent.cfg.SFTP.PacketSize()),
	}
	// client, err := sftp.NewClient(conn, opts...)
	client, err := sftp.NewClientPipe(stdout, stdin, opts...)
	if err != nil {
		go conn.Close()
		return nil, fmt.Errorf("upload: sftp connect: %v", err)
	}
	agent.client = client

	return agent.client, nil
}

var (
	hostKeyCallbackOnce sync.Once
	hostKeyCallback     = func(logger log.Logger) {
		logger.Warn().Logf("sftp: WARNING!!! Insecure default of skipping SFTP host key validation. Please set sftp_configs.host_public_key")
	}
)

func sftpConnect(logger log.Logger, cfg service.UploadAgent) (*ssh.Client, io.WriteCloser, io.Reader, error) {
	if cfg.SFTP == nil {
		return nil, nil, nil, errors.New("nil config or sftp config")
	}

	sshConf := ssh.Config{}
	sshConf.SetDefaults()
	sshConf.KeyExchanges = append(
		sshConf.KeyExchanges,
		"diffie-hellman-group-exchange-sha256",
	)
	conf := &ssh.ClientConfig{
		Config:  sshConf,
		User:    cfg.SFTP.Username,
		Timeout: cfg.SFTP.Timeout(),
	}
	conf.SetDefaults()

	if cfg.SFTP.HostPublicKey != "" {
		pubKey, err := sshx.ReadPubKey([]byte(cfg.SFTP.HostPublicKey))
		if err != nil {
			return nil, nil, nil, fmt.Errorf("problem parsing ssh public key: %v", err)
		}
		conf.HostKeyCallback = ssh.FixedHostKey(pubKey)
	} else {
		hostKeyCallbackOnce.Do(func() {
			hostKeyCallback(logger)
		})
		//nolint:gosec
		conf.HostKeyCallback = ssh.InsecureIgnoreHostKey() // insecure default
	}
	switch {
	case cfg.SFTP.Password != "":
		conf.Auth = append(conf.Auth, ssh.Password(cfg.SFTP.Password))
	case cfg.SFTP.ClientPrivateKey != "":
		signer, err := readSigner(cfg.SFTP.ClientPrivateKey)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("sftpConnect: failed to read client private key: %v", err)
		}
		conf.Auth = append(conf.Auth, ssh.PublicKeys(signer))
	default:
		return nil, nil, nil, errors.New("sftpConnect: no auth method provided")
	}

	// Connect to the remote server
	var client *ssh.Client
	var err error
	for i := 0; i < 3; i++ {
		if client == nil {
			if i > 0 {
				sftpConnectionRetries.With("hostname", cfg.SFTP.Hostname).Add(1)
			}
			client, err = ssh.Dial("tcp", cfg.SFTP.Hostname, conf) // retry connection
			time.Sleep(250 * time.Millisecond)
		}
	}
	if client == nil && err != nil {
		return nil, nil, nil, fmt.Errorf("sftpConnect: %v", err)
	}

	session, err := client.NewSession()
	if err != nil {
		go client.Close()
		return nil, nil, nil, err
	}
	if err = session.RequestSubsystem("sftp"); err != nil {
		go client.Close()
		return nil, nil, nil, err
	}
	pw, err := session.StdinPipe()
	if err != nil {
		go client.Close()
		return nil, nil, nil, err
	}
	pr, err := session.StdoutPipe()
	if err != nil {
		go client.Close()
		return nil, nil, nil, err
	}

	return client, pw, pr, nil
}

func readSigner(raw string) (ssh.Signer, error) {
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if len(decoded) > 0 && err == nil {
		return ssh.ParsePrivateKey(decoded)
	}
	return ssh.ParsePrivateKey([]byte(raw))
}

func (agent *SFTPTransferAgent) Ping() error {
	if agent == nil {
		return errors.New("nil SFTPTransferAgent")
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()

	conn, err := agent.connection()
	agent.record(err)
	if err != nil {
		return err
	}

	_, err = conn.ReadDir(".")
	agent.record(err)
	if err != nil {
		return fmt.Errorf("sftp: ping %v", err)
	}
	return nil
}

func (agent *SFTPTransferAgent) record(err error) {
	if agent == nil {
		return
	}
	if err != nil {
		sftpAgentUp.With("hostname", agent.cfg.SFTP.Hostname).Set(0)
	} else {
		sftpAgentUp.With("hostname", agent.cfg.SFTP.Hostname).Set(1)
	}
}

func (agent *SFTPTransferAgent) Close() error {
	if agent == nil {
		return nil
	}
	if agent.client != nil {
		agent.client.Close()
	}
	if agent.conn != nil {
		agent.conn.Close()
	}
	return nil
}

func (agent *SFTPTransferAgent) InboundPath() string {
	return agent.cfg.Paths.Inbound
}

func (agent *SFTPTransferAgent) OutboundPath() string {
	return agent.cfg.Paths.Outbound
}

func (agent *SFTPTransferAgent) ReconciliationPath() string {
	return agent.cfg.Paths.Reconciliation
}

func (agent *SFTPTransferAgent) ReturnPath() string {
	return agent.cfg.Paths.Return
}

func (agent *SFTPTransferAgent) Hostname() string {
	if agent == nil || agent.cfg.SFTP == nil {
		return ""
	}
	return agent.cfg.SFTP.Hostname
}

func (agent *SFTPTransferAgent) Delete(path string) error {
	agent.mu.Lock()
	defer agent.mu.Unlock()

	conn, err := agent.connection()
	if err != nil {
		return err
	}

	info, err := conn.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("sftp: delete stat: %v", err)
	}
	if info != nil {
		if err := conn.Remove(path); err != nil {
			return fmt.Errorf("sftp: delete: %v", err)
		}
	}
	return nil // not found
}

// uploadFile saves the content of File at the given filename in the OutboundPath directory
//
// The File's contents will always be closed
func (agent *SFTPTransferAgent) UploadFile(f File) error {
	defer f.Close()

	agent.mu.Lock()
	defer agent.mu.Unlock()

	conn, err := agent.connection()
	if err != nil {
		return err
	}

	// Create OutboundPath if it doesn't exist and we're told to create it
	if agent.cfg.SFTP != nil && !agent.cfg.SFTP.SkipDirectoryCreation {
		info, err := conn.Stat(agent.cfg.Paths.Outbound)
		if info == nil || (err != nil && os.IsNotExist(err)) {
			if err := conn.Mkdir(agent.cfg.Paths.Outbound); err != nil {
				return fmt.Errorf("sftp: problem creating parent dir %s: %v", agent.cfg.Paths.Outbound, err)
			}
		}
	}

	// Take the base of f.Filename and our (out of band) OutboundPath to avoid accepting a write like '../../../../etc/passwd'.
	pathToWrite := filepath.Join(agent.cfg.Paths.Outbound, filepath.Base(f.Filename))

	fd, err := conn.OpenFile(pathToWrite, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("sftp: problem creating %s: %v", pathToWrite, err)
	}
	n, err := io.Copy(fd, f.Contents)
	if err != nil {
		fd.Close()
		return fmt.Errorf("sftp: problem copying (n=%d) %s: %v", n, f.Filename, err)
	}
	if err := fd.Sync(); err != nil {
		// Skip sync if the remote server doesn't support it
		if !strings.Contains(err.Error(), "SSH_FX_OP_UNSUPPORTED") {
			return fmt.Errorf("sftp: problem with sync on %s: %v", f.Filename, err)
		}
	}
	if err := fd.Chmod(0600); err != nil {
		return fmt.Errorf("sftp: problem with chmod on %s: %v", f.Filename, err)
	}
	if err := fd.Close(); err != nil {
		return fmt.Errorf("sftp: problem closing %s: %v", f.Filename, err)
	}
	return nil
}

func (agent *SFTPTransferAgent) GetInboundFiles() ([]File, error) {
	return agent.readFiles(agent.cfg.Paths.Inbound)
}

func (agent *SFTPTransferAgent) GetReconciliationFiles() ([]File, error) {
	return agent.readFiles(agent.cfg.Paths.Reconciliation)
}

func (agent *SFTPTransferAgent) GetReturnFiles() ([]File, error) {
	return agent.readFiles(agent.cfg.Paths.Return)
}

func (agent *SFTPTransferAgent) readFiles(dir string) ([]File, error) {
	agent.mu.Lock()
	defer agent.mu.Unlock()

	conn, err := agent.connection()
	if err != nil {
		return nil, err
	}

	infos, err := conn.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("sftp: readdir %s: %v", dir, err)
	}

	var files []File
	for i := range infos {
		fd, err := conn.Open(filepath.Join(dir, infos[i].Name()))
		if err != nil {
			return nil, fmt.Errorf("sftp: open %s: %v", infos[i].Name(), err)
		}

		// skip this file descriptor if it's a directory - we only reading one level deep
		info, err := fd.Stat()
		if err != nil {
			fd.Close()
			return nil, fmt.Errorf("sftp: stat %s: %v", infos[i].Name(), err)
		}
		if info.IsDir() {
			fd.Close()
			continue
		}

		// download the remote file to our local directory
		var buf bytes.Buffer
		if n, err := io.Copy(&buf, fd); err != nil {
			fd.Close()
			if err != nil && !strings.Contains(err.Error(), sftp.ErrInternalInconsistency.Error()) {
				return nil, fmt.Errorf("sftp: read (n=%d) %s: %v", n, infos[i].Name(), err)
			}
			return nil, fmt.Errorf("sftp: read (n=%d) on %s: %v", n, infos[i].Name(), err)
		} else {
			fd.Close()
		}
		files = append(files, File{
			Filename: infos[i].Name(),
			Contents: io.NopCloser(&buf),
		})
	}
	return files, nil
}
