// Licensed to The Moov Authors under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. The Moov Authors licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package odfi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/moov-io/ach"
	"github.com/moov-io/achgateway/internal/audittrail"
	"github.com/stretchr/testify/require"
)

func TestProcessor(t *testing.T) {
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "invalid.ach"), []byte("invalid-ach-file"), 0600)
	require.NoError(t, err)

	proc := &MockProcessor{}
	processors := SetupProcessors(proc)
	auditSaver := &AuditSaver{
		storage:  &audittrail.MockStorage{},
		hostname: "ftp.foo.com",
	}

	// By reading a file without ACH FileHeaders we still want to try and process
	// Batches inside of it if any are found, so reading this kind of file shouldn't
	// return an error from reading the file.
	err = processDir(dir, auditSaver, processors)
	require.NoError(t, err)

	require.NotNil(t, proc.HandledFile)
	require.NotNil(t, proc.HandledFile.ACHFile)
	require.Equal(t, "7ffdca32898fc89e5e680d0a01e9e1c2a1cd2717", proc.HandledFile.ACHFile.ID)

	// Real world file
	path := filepath.Join("..", "..", "..", "testdata", "HMBRAD_ACHEXPORT_1001_08_19_2022_09_10")
	err = processFile(path, auditSaver, processors)
	require.ErrorContains(t, err, "record:FileHeader *ach.FieldError FileCreationDate  is a mandatory field")
}

func TestProcessor_populateHashes(t *testing.T) {
	file, err := ach.ReadFile(filepath.Join("testdata", "forward.ach"))
	require.ErrorContains(t, err, ach.ErrFileHeader.Error())

	populateHashes(file)
	require.Equal(t, "", file.Batches[0].ID())

	entries := file.Batches[0].GetEntries()
	require.Equal(t, "389723d3a8293a802169b5db27f288d32e96b9c6", entries[0].ID)
}
