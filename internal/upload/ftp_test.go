// Copyright 2020 The Moov Authors
// Use of this source code is governed by an Apache License
// license that can be found in the LICENSE file.

package upload

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mhttptest "github.com/moov-io/achgateway/internal/httptest"
	"github.com/moov-io/achgateway/internal/service"
	"github.com/moov-io/achgateway/internal/util"
	"github.com/moov-io/base"
	"github.com/moov-io/base/log"

	"github.com/jlaffaye/ftp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goftp.io/server"
	"goftp.io/server/driver/file"
)

var (
	portSource = rand.NewSource(time.Now().Unix())

	rootFTPPath = filepath.Join("..", "..", "testdata", "ftp-server")
)

func port() int {
	return int(30000 + (portSource.Int63() % 9999))
}

func createTestFTPServer(t *testing.T) (*server.Server, error) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping due to -short")
	}

	// Create the outbound directory, this seems especially flakey in remote CI
	if err := os.MkdirAll(filepath.Join(rootFTPPath, "outbound"), 0777); err != nil {
		t.Fatal(err)
	}

	opts := &server.ServerOpts{
		Auth: &server.SimpleAuth{
			Name:     "moov",
			Password: "password",
		},
		Factory: &file.DriverFactory{
			RootPath: rootFTPPath,
			Perm:     server.NewSimplePerm("test", "test"),
		},
		Hostname: "localhost",
		Port:     port(),
		Logger:   &server.DiscardLogger{},
	}
	svc := server.NewServer(opts)
	if svc == nil {
		return nil, errors.New("nil FTP server")
	}
	if err := util.Timeout(func() error { return svc.ListenAndServe() }, 50*time.Millisecond); err != nil {
		if err == util.ErrTimeout {
			return svc, nil
		}
		return nil, err
	}
	return svc, nil
}

func TestFTPConfig__String(t *testing.T) {
	cfg := &service.FTP{
		Hostname: "host",
		Username: "user",
		Password: "pass",
	}
	if !strings.Contains(cfg.String(), "Password=p**s") {
		t.Error(cfg.String())
	}
}

func createTestFTPConnection(t *testing.T, svc *server.Server) (*ftp.ServerConn, error) {
	t.Helper()
	conn, err := ftp.DialTimeout(fmt.Sprintf("localhost:%d", svc.Port), 10*time.Second)
	require.NoError(t, err)
	if err := conn.Login("moov", "password"); err != nil {
		t.Fatal(err)
	}
	return conn, nil
}

func TestFTP(t *testing.T) {
	svc, err := createTestFTPServer(t)
	require.NoError(t, err)
	defer svc.Shutdown()

	conn, err := createTestFTPConnection(t, svc)
	require.NoError(t, err)
	defer conn.Quit()

	dir, err := conn.CurrentDir()
	require.NoError(t, err)
	if dir == "" {
		t.Error("empty current dir?!")
	}

	// Change directory
	if err := conn.ChangeDir("scratch"); err != nil {
		t.Error(err)
	}

	// Read a file we know should exist
	resp, err := conn.RetrFrom("existing-file", 0) // offset of 0
	if err != nil {
		t.Error(err)
	}
	bs, _ := io.ReadAll(resp)
	bs = bytes.TrimSpace(bs)
	if !bytes.Equal(bs, []byte("Hello, World!")) {
		t.Errorf("got %q", string(bs))
	}
}

func createTestFTPAgent(t *testing.T) (*server.Server, *FTPTransferAgent) {
	svc, err := createTestFTPServer(t)
	if err != nil {
		return nil, nil
	}

	auth, ok := svc.Auth.(*server.SimpleAuth)
	if !ok {
		t.Errorf("unknown svc.Auth: %T", svc.Auth)
	}
	cfg := &service.UploadAgent{ // these need to match paths at testdata/ftp-srever/
		FTP: &service.FTP{
			Hostname: fmt.Sprintf("%s:%d", svc.Hostname, svc.Port),
			Username: auth.Name,
			Password: auth.Password,
		},
		Paths: service.UploadPaths{
			Inbound:        "inbound",
			Outbound:       "outbound",
			Reconciliation: "reconciliation",
			Return:         "returned",
		},
	}
	agent, err := newFTPTransferAgent(log.NewNopLogger(), cfg)
	if err != nil {
		svc.Shutdown()
		t.Fatalf("problem creating Agent: %v", err)
		return nil, nil
	}
	require.NotNil(t, agent)
	return svc, agent
}

func TestFTPAgent(t *testing.T) {
	svc, agent := createTestFTPAgent(t)
	defer agent.Close()
	defer svc.Shutdown()

	assert.Equal(t, "inbound", agent.InboundPath())
	assert.Equal(t, "outbound", agent.OutboundPath())
	assert.Equal(t, "reconciliation", agent.ReconciliationPath())
	assert.Equal(t, "returned", agent.ReturnPath())
	assert.Contains(t, agent.Hostname(), "localhost:")
}

func TestFTPAgent_Hostname(t *testing.T) {
	tests := []struct {
		desc             string
		agent            Agent
		expectedHostname string
	}{
		{"no FTP config", &FTPTransferAgent{cfg: service.UploadAgent{}}, ""},
		{"returns expected hostname", &FTPTransferAgent{
			cfg: service.UploadAgent{
				FTP: &service.FTP{
					Hostname: "ftp.mybank.com:4302",
				},
			},
		}, "ftp.mybank.com:4302"},
		{"empty hostname", &FTPTransferAgent{
			cfg: service.UploadAgent{
				FTP: &service.FTP{
					Hostname: "",
				},
			},
		}, ""},
	}

	for _, test := range tests {
		assert.Equal(t, test.expectedHostname, test.agent.Hostname(), "Test: "+test.desc)
	}
}

func TestFTP__tlsDialOption(t *testing.T) {
	if testing.Short() {
		return // skip network calls
	}

	cafile, err := mhttptest.GrabConnectionCertificates(t, "google.com:443")
	require.NoError(t, err)
	defer os.Remove(cafile)

	opt, err := tlsDialOption(cafile)
	require.NoError(t, err)
	if opt == nil {
		t.Fatal("nil tls DialOption")
	}
}

func TestFTP__getInboundFiles(t *testing.T) {
	svc, agent := createTestFTPAgent(t)
	defer agent.Close()
	defer svc.Shutdown()

	files, err := agent.GetInboundFiles()
	require.NoError(t, err)
	if len(files) != 3 {
		t.Errorf("got %d files", len(files))
	}
	for i := range files {
		if files[i].Filename == "iat-credit.ach" {
			bs, _ := io.ReadAll(files[i].Contents)
			bs = bytes.TrimSpace(bs)
			if !strings.HasPrefix(string(bs), "101 121042882 2313801041812180000A094101Bank                   My Bank Name                   ") {
				t.Errorf("got %v", string(bs))
			}
		}
	}

	// make sure we perform the same call and get the same result
	files, err = agent.GetInboundFiles()
	require.NoError(t, err)
	if len(files) != 3 {
		t.Errorf("got %d files", len(files))
	}
	for i := range files {
		if files[0].Filename == "iat-credit.ach" {
			continue
		}
		if files[0].Filename == "cor-c01.ach" {
			continue
		}
		if files[0].Filename == "prenote-ppd-debit.ach" {
			continue
		}
		t.Errorf("files[%d]=%s", i, files[i])
	}
}

func TestFTP__getReconciliationFiles(t *testing.T) {
	svc, agent := createTestFTPAgent(t)
	defer agent.Close()
	defer svc.Shutdown()

	files, err := agent.GetReconciliationFiles()
	require.NoError(t, err)
	if len(files) != 1 {
		t.Errorf("got %d files", len(files))
	}
	for i := range files {
		if files[i].Filename == "ppd-debit.ach" {
			bs, _ := io.ReadAll(files[i].Contents)
			bs = bytes.TrimSpace(bs)
			if !strings.HasPrefix(string(bs), "5225companyname                         origid    PPDCHECKPAYMT000002080730   1076401250000001") {
				t.Errorf("got %v", string(bs))
			}
		}
	}

	// make sure we perform the same call and get the same result
	files, err = agent.GetReconciliationFiles()
	require.NoError(t, err)
	if len(files) != 1 {
		t.Errorf("got %d files", len(files))
	}
	for i := range files {
		if files[0].Filename == "ppd-debit.ach" {
			continue
		}
		t.Errorf("files[%d]=%s", i, files[i])
	}
}

func TestFTP__getReturnFiles(t *testing.T) {
	svc, agent := createTestFTPAgent(t)
	defer agent.Close()
	defer svc.Shutdown()

	files, err := agent.GetReturnFiles()
	require.NoError(t, err)
	if len(files) != 1 {
		t.Errorf("got %d files", len(files))
	}
	if files[0].Filename != "return-WEB.ach" {
		t.Errorf("files[0]=%s", files[0])
	}
	bs, _ := io.ReadAll(files[0].Contents)
	bs = bytes.TrimSpace(bs)
	if !strings.HasPrefix(string(bs), "101 091400606 6910001341810170306A094101FIRST BANK & TRUST     ASF APPLICATION SUPERVI        ") {
		t.Errorf("got %v", string(bs))
	}

	// make sure we perform the same call and get the same result
	files, err = agent.GetReturnFiles()
	require.NoError(t, err)
	if len(files) != 1 {
		t.Errorf("got %d files", len(files))
	}
	if files[0].Filename != "return-WEB.ach" {
		t.Errorf("files[0]=%s", files[0])
	}
}

func TestFTP__uploadFile(t *testing.T) {
	svc, agent := createTestFTPAgent(t)
	defer agent.Close()
	defer svc.Shutdown()

	content := base.ID()
	f := File{
		Filename: base.ID(),
		Contents: io.NopCloser(strings.NewReader(content)), // random file contents
	}

	// Create outbound directory
	parent := filepath.Join(rootFTPPath, agent.OutboundPath())
	if err := os.MkdirAll(parent, 0777); err != nil {
		t.Fatal(err)
	}

	if err := agent.UploadFile(f); err != nil {
		t.Fatal(err)
	}

	// manually read file contents
	agent.conn.ChangeDir(agent.OutboundPath())
	resp, _ := agent.conn.Retr(f.Filename)
	if resp == nil {
		t.Fatal("nil File response")
	}
	r, _ := agent.readResponse(resp)
	if r == nil {
		t.Fatal("failed to read file")
	}
	bs, _ := io.ReadAll(r)
	if !bytes.Equal(bs, []byte(content)) {
		t.Errorf("got %q", string(bs))
	}

	// delete the file
	if err := agent.Delete(f.Filename); err != nil {
		t.Fatal(err)
	}

	// get an error with no FTP configs
	agent.cfg.FTP = nil
	if err := agent.UploadFile(f); err == nil {
		t.Error("expected error")
	}
}

func TestFTP__Issue494(t *testing.T) {
	// Issue 494 talks about how readFiles fails when directories exist inside of
	// the return/inbound directories. Let's make a directory inside and verify
	// downloads happen.
	svc, agent := createTestFTPAgent(t)
	defer agent.Close()
	defer svc.Shutdown()

	// Create extra directory
	path := filepath.Join(rootFTPPath, agent.ReturnPath(), "issue494")
	if err := os.MkdirAll(path, 0777); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	// Read without an error
	files, err := agent.GetReturnFiles()
	if err != nil {
		t.Error(err)
	}
	if len(files) != 1 {
		t.Errorf("got %d files: %v", len(files), files)
	}
}

func TestFTP__DeleteMissing(t *testing.T) {
	svc, agent := createTestFTPAgent(t)
	defer agent.Close()
	defer svc.Shutdown()

	err := agent.Delete("/missing.txt")
	require.NoError(t, err)
}
