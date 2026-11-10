package domaindb

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func newTestFsStorageDriver(t *testing.T) (*FsStorageDriver, string) {
	t.Helper()

	dir := t.TempDir()
	drv, err := NewFsStorageDriver(dir)
	if err != nil {
		t.Fatalf("NewFsStorageDriver failed: %v", err)
	}
	return drv, dir
}

func readDatabase(t *testing.T, drv *FsStorageDriver, name string) string {
	t.Helper()

	r, err := drv.ReadDatabase(name)
	if err != nil {
		t.Fatalf("ReadDatabase(%q) failed: %v", name, err)
	}
	defer func() {
		_ = r.Close()
	}()

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	return string(data)
}

// A missing directory is reported as fs.ErrNotExist, and a file instead of a
// directory as syscall.ENOTDIR, with well-formed error messages.
func TestNewFsStorageDriver_InvalidPaths(t *testing.T) {
	dir := t.TempDir()

	if _, err := NewFsStorageDriver(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("NewFsStorageDriver for a missing directory returned %v, want fs.ErrNotExist", err)
	}

	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	_, err := NewFsStorageDriver(file)
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("NewFsStorageDriver for a file returned %v, want ENOTDIR", err)
	}
	if err != nil && strings.Contains(err.Error(), "%!") {
		t.Errorf("malformed error message: %q", err.Error())
	}
}

// Missing databases and checkpoints are reported as fs.ErrNotExist.
func TestFsStorageDriver_MissingFilesAreNotExist(t *testing.T) {
	drv, _ := newTestFsStorageDriver(t)

	if _, err := drv.ReadDatabase("missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadDatabase for a missing database returned %v, want fs.ErrNotExist", err)
	}
	if _, err := drv.ReadCheckpoints(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadCheckpoints without checkpoints returned %v, want fs.ErrNotExist", err)
	}
}

// Overwriting a database or the checkpoints with shorter content leaves no
// trailing bytes from the previous content.
func TestFsStorageDriver_OverwriteWithShorterContent(t *testing.T) {
	drv, dir := newTestFsStorageDriver(t)

	for _, content := range []string{"a-much-longer-list.com\nexample.com\n", "short.com\n"} {
		if err := drv.WriteDatabase(testDb, io.NopCloser(strings.NewReader(content))); err != nil {
			t.Fatalf("WriteDatabase failed: %v", err)
		}
	}
	if got := readDatabase(t, drv, testDb); got != "short.com\n" {
		t.Errorf("database contents = %q, want %q", got, "short.com\n")
	}

	long := &AllCheckpoints{Checkpoints: map[string]Checkpoint{"some_very_long_database_name": {LastUpdatedUnix: 1234567890}}}
	short := &AllCheckpoints{Checkpoints: map[string]Checkpoint{"a": {LastUpdatedUnix: 1}}}
	for _, chk := range []*AllCheckpoints{long, short} {
		if err := drv.WriteCheckpoints(chk); err != nil {
			t.Fatalf("WriteCheckpoints failed: %v", err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, checkpointsFilename))
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !json.Valid(raw) {
		t.Errorf("checkpoints file is not valid JSON: %q", string(raw))
	}
	got, err := drv.ReadCheckpoints()
	if err != nil {
		t.Fatalf("ReadCheckpoints failed: %v", err)
	}
	if len(got.Checkpoints) != 1 || got.Checkpoints["a"].LastUpdatedUnix != 1 {
		t.Errorf("unexpected checkpoints: %+v", got.Checkpoints)
	}
}

type faultyReader struct{}

func (f *faultyReader) Read(_ []byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// A failed write leaves the previous database intact and no temp files behind.
func TestFsStorageDriver_FailedWriteKeepsPreviousDatabase(t *testing.T) {
	drv, dir := newTestFsStorageDriver(t)

	if err := drv.WriteDatabase(testDb, io.NopCloser(strings.NewReader("example.com\n"))); err != nil {
		t.Fatalf("WriteDatabase failed: %v", err)
	}
	if err := drv.WriteDatabase(testDb, io.NopCloser(&faultyReader{})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("WriteDatabase with a failing reader returned %v, want io.ErrUnexpectedEOF", err)
	}

	if got := readDatabase(t, drv, testDb); got != "example.com\n" {
		t.Errorf("previous database was not kept: %q", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected only the database file in the data directory, found %d entries", len(entries))
	}
}

// Old-format checkpoint files without a checkpoints map are read as empty
// checkpoints rather than a nil map.
func TestFsStorageDriver_ReadCheckpoints_MissingMap(t *testing.T) {
	drv, dir := newTestFsStorageDriver(t)

	if err := os.WriteFile(filepath.Join(dir, checkpointsFilename), []byte("{}"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	chk, err := drv.ReadCheckpoints()
	if err != nil {
		t.Fatalf("ReadCheckpoints failed: %v", err)
	}
	if chk.Checkpoints == nil {
		t.Errorf("ReadCheckpoints returned a nil checkpoints map")
	}
}

// Concurrent writes of the same database each use their own temp file, so the
// result is one complete write and no temp files are left behind.
func TestFsStorageDriver_WriteDatabase_ConcurrentWritesDoNotInterleave(t *testing.T) {
	drv, dir := newTestFsStorageDriver(t)

	const writers = 8
	const size = 1 << 20

	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			content := bytes.Repeat([]byte{byte('a' + i)}, size)
			if err := drv.WriteDatabase(testDb, io.NopCloser(bytes.NewReader(content))); err != nil {
				t.Errorf("WriteDatabase failed: %v", err)
			}
		})
	}
	wg.Wait()

	got := readDatabase(t, drv, testDb)
	if len(got) != size || got != strings.Repeat(got[:1], size) {
		t.Errorf("database file is a mix of multiple writes (length %d)", len(got))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected only the database file in the data directory, found %d entries", len(entries))
	}
}

// Database names cannot collide with the checkpoints file or temp files, and
// invalid names are rejected.
func TestFsStorageDriver_DatabaseNames(t *testing.T) {
	drv, _ := newTestFsStorageDriver(t)

	cps := &AllCheckpoints{Checkpoints: map[string]Checkpoint{"a": {LastUpdatedUnix: 1}}}
	if err := drv.WriteCheckpoints(cps); err != nil {
		t.Fatalf("WriteCheckpoints failed: %v", err)
	}

	for _, name := range []string{checkpointsFilename, ".", "..", tmpFilePrefix + "123", "../escape", "a/b"} {
		if err := drv.WriteDatabase(name, io.NopCloser(strings.NewReader("example.com\n"))); err != nil {
			t.Errorf("WriteDatabase(%q) failed: %v", name, err)
			continue
		}
		if got := readDatabase(t, drv, name); got != "example.com\n" {
			t.Errorf("database %q contents = %q", name, got)
		}
	}

	if _, err := drv.ReadCheckpoints(); err != nil {
		t.Errorf("checkpoints are unreadable after writing databases: %v", err)
	}

	if err := drv.WriteDatabase("", io.NopCloser(strings.NewReader("example.com\n"))); !errors.Is(err, ErrInvalidDbName) {
		t.Errorf("WriteDatabase with an empty name returned %v, want ErrInvalidDbName", err)
	}
	if _, err := drv.ReadDatabase(strings.Repeat("a", DbNameMaxSize+1)); !errors.Is(err, ErrDbNameTooLong) {
		t.Errorf("ReadDatabase with a long name returned %v, want ErrDbNameTooLong", err)
	}
}
