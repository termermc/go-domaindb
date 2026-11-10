package domaindb

// Shared helpers for tests.

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testDb = "test"

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// memStorage is an in-memory StorageDriver.
type memStorage struct {
	mu          sync.Mutex
	dbs         map[string][]byte
	checkpoints *AllCheckpoints
	failWrites  bool
}

func newMemStorage() *memStorage {
	return &memStorage{dbs: make(map[string][]byte)}
}

func (m *memStorage) WriteDatabase(name string, input io.ReadCloser) error {
	defer func() {
		_ = input.Close()
	}()

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failWrites {
		return errors.New("disk full")
	}

	data, err := io.ReadAll(input)
	if err != nil {
		return err
	}
	m.dbs[name] = data
	return nil
}

func (m *memStorage) ReadDatabase(name string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, has := m.dbs[name]
	if !has {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *memStorage) WriteCheckpoints(checkpoints *AllCheckpoints) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	cpy := &AllCheckpoints{Checkpoints: make(map[string]Checkpoint, len(checkpoints.Checkpoints))}
	maps.Copy(cpy.Checkpoints, checkpoints.Checkpoints)
	m.checkpoints = cpy
	return nil
}

func (m *memStorage) ReadCheckpoints() (*AllCheckpoints, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.checkpoints == nil {
		return nil, fs.ErrNotExist
	}
	return m.checkpoints, nil
}

func (m *memStorage) cached(name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, has := m.dbs[name]
	return string(data), has
}

// stringSource returns a data source whose Get function returns the current value of body.
func stringSource(body *atomic.Value) *DataSource {
	return &DataSource{
		RefreshInterval: time.Hour,
		Get: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(body.Load().(string))), nil
		},
	}
}

// staticSource returns a data source whose Get function always returns body.
func staticSource(body string) *DataSource {
	var val atomic.Value
	val.Store(body)
	return stringSource(&val)
}

// urlSource returns a data source that downloads from the specified URLs.
func urlSource(urls ...string) *DataSource {
	src := &DataSource{RefreshInterval: time.Hour}
	for _, str := range urls {
		parsed, err := url.Parse(str)
		if err != nil {
			panic(err)
		}
		src.Urls = append(src.Urls, parsed)
	}
	return src
}

// flakySource returns a data source that fails the specified number of times before returning body.
func flakySource(failures int32, body string) (*DataSource, *atomic.Int32) {
	calls := &atomic.Int32{}
	return &DataSource{
		RefreshInterval: time.Hour,
		Get: func() (io.ReadCloser, error) {
			if calls.Add(1) <= failures {
				return nil, errors.New("source is down")
			}
			return io.NopCloser(strings.NewReader(body)), nil
		},
	}, calls
}

// newTestDomainDb creates a DomainDb instance without loading any databases or starting any background work.
// Missing storage driver and logger options are filled in.
func newTestDomainDb(t *testing.T, options Options) *DomainDb {
	t.Helper()

	if options.StorageDriver == nil {
		options.StorageDriver = newMemStorage()
	}
	if options.Logger == nil {
		options.Logger = testLogger()
	}

	s, err := newDomainDb(options)
	if err != nil {
		t.Fatalf("newDomainDb failed: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
	})

	return s
}

// newSingleTestDomainDb creates a DomainDb instance with a single database named testDb.
func newSingleTestDomainDb(t *testing.T, src *DataSource) *DomainDb {
	t.Helper()

	return newTestDomainDb(t, Options{
		Sources: map[string]*DataSource{testDb: src},
	})
}

// mustHave checks whether the domain is in the database named testDb.
func mustHave(t *testing.T, s *DomainDb, domain string, want bool) {
	t.Helper()

	got, err := s.DoesDbHaveDomain(testDb, domain)
	if err != nil {
		t.Fatalf("DoesDbHaveDomain(%s) failed: %v", domain, err)
	}
	if got != want {
		t.Errorf("DoesDbHaveDomain(%s) = %v, want %v", domain, got, want)
	}
}

// hasDomain returns whether the database named testDb is loaded and contains the domain.
func hasDomain(s *DomainDb, domain string) bool {
	has, err := s.DoesDbHaveDomain(testDb, domain)
	return err == nil && has
}

// waitFor polls cond until it returns true, failing the test after 5 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// failAfterReader returns data, then the specified error.
type failAfterReader struct {
	data []byte
	err  error
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// goroutineStackContains returns whether the stack of any goroutine contains marker.
func goroutineStackContains(marker string) bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Contains(string(buf[:n]), marker)
		}
		buf = make([]byte, len(buf)*2)
	}
}
