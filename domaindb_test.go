package domaindb

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A read error part-way through a list fails the load, rather than a truncated list
// being accepted as complete.
func TestParseDomainList_ReturnsReadError(t *testing.T) {
	s := newSingleTestDomainDb(t, staticSource(""))

	reader := &failAfterReader{
		data: []byte("example.com\n"),
		err:  io.ErrUnexpectedEOF,
	}

	_, err := s.parseDomainList(testDb, reader)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("parseDomainList returned %v for a reader that failed with %v", err, io.ErrUnexpectedEOF)
	}
}

// Malformed lines are skipped without stopping the parse, so valid entries after
// them are still loaded, and the cache contains the whole list.
func TestParseDomainList_ReadsPastMalformedLines(t *testing.T) {
	var sb strings.Builder
	for i := range 20 {
		_, _ = fmt.Fprintf(&sb, "domain%d.com\n", i)
	}
	for range 10 {
		sb.WriteString("not_a_domain!\n")
	}
	sb.WriteString("last.example\n")

	storage := newMemStorage()
	s := newTestDomainDb(t, Options{
		StorageDriver: storage,
		Sources:       map[string]*DataSource{testDb: staticSource(sb.String())},
	})
	if err := s.DownloadAndLoadDatabase(testDb); err != nil {
		t.Fatalf("DownloadAndLoadDatabase failed: %v", err)
	}

	mustHave(t, s, "last.example", true)
	if cached, _ := storage.cached(testDb); cached != sb.String() {
		t.Errorf("cached database is not the complete list (%d of %d bytes)", len(cached), sb.Len())
	}
}

// A list that is mostly garbage is rejected and does not replace existing data.
func TestParseDomainList_RejectsMostlyMalformedList(t *testing.T) {
	var body atomic.Value
	body.Store("example.com\n")

	s := newSingleTestDomainDb(t, stringSource(&body))
	if err := s.DownloadAndLoadDatabase(testDb); err != nil {
		t.Fatalf("DownloadAndLoadDatabase failed: %v", err)
	}

	body.Store("<html>\n<body>\nrate limited\n</body>\n</html>\nother.com\n")
	if err := s.DownloadAndLoadDatabase(testDb); err == nil {
		t.Errorf("expected an error for a mostly malformed list")
	}

	mustHave(t, s, "example.com", true)
	mustHave(t, s, "other.com", false)
}

// Surrounding whitespace, CRLF line endings, full-line comments and trailing
// comments are ignored, and domains are normalized both when loading and when
// querying.
func TestParseDomainList_TrimsWhitespaceCommentsAndNormalizes(t *testing.T) {
	s := newSingleTestDomainDb(t, staticSource("# header\r\n  Example.COM  \r\n\tdé.net # unicode\n\n   # indented comment\nfqdn.org.#dot\n"))
	if err := s.DownloadAndLoadDatabase(testDb); err != nil {
		t.Fatalf("DownloadAndLoadDatabase failed: %v", err)
	}

	for _, domain := range []string{"example.com", "EXAMPLE.com.", "xn--d-bga.net", "DÉ.net", "fqdn.org"} {
		mustHave(t, s, domain, true)
	}
	mustHave(t, s, "sub.example.com", false)
}

// A non-200 response produces a well-formed error that includes the status code
// and the start of the body.
func TestDownload_Non200ErrorIncludesStatusAndBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("server exploded"))
	}))
	defer ts.Close()

	s := newSingleTestDomainDb(t, urlSource(ts.URL))

	err := s.DownloadAndLoadDatabase(testDb)
	if err == nil || !errors.Is(err, ErrAllUrlsFailed) {
		t.Fatalf("DownloadAndLoadDatabase returned %v, want ErrAllUrlsFailed", err)
	}
	if strings.Contains(err.Error(), "%!") {
		t.Errorf("malformed error message for non-200 response: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "server exploded") {
		t.Errorf("error message does not include the status code and body: %q", err.Error())
	}
}

// With multiple URLs, a download that is cut off mid-body is skipped entirely,
// so its partial last line is never loaded.
func TestDownload_SkipsTruncatedUrl(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/truncated" {
			_, _ = w.Write([]byte("good.com"))
			return
		}

		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("partial.com\nexample.c"))
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer ts.Close()

	storage := newMemStorage()
	s := newTestDomainDb(t, Options{
		StorageDriver: storage,
		Sources: map[string]*DataSource{
			testDb: urlSource(ts.URL+"/truncated", ts.URL+"/a", ts.URL+"/b"),
		},
	})
	if err := s.DownloadAndLoadDatabase(testDb); err != nil {
		t.Fatalf("DownloadAndLoadDatabase failed: %v", err)
	}

	mustHave(t, s, "good.com", true)
	mustHave(t, s, "partial.com", false)
	mustHave(t, s, "example.c", false)

	if cached, _ := storage.cached(testDb); cached != "good.com\ngood.com\n" {
		t.Errorf("unexpected cached database: %q", cached)
	}
}

// A list with no domains is rejected and leaves both the in-memory database and
// the cache untouched.
func TestDownload_EmptyListDoesNotReplaceDatabase(t *testing.T) {
	var body atomic.Value
	body.Store("example.com\n")

	storage := newMemStorage()
	s := newTestDomainDb(t, Options{
		StorageDriver: storage,
		Sources:       map[string]*DataSource{testDb: stringSource(&body)},
	})
	if err := s.DownloadAndLoadDatabase(testDb); err != nil {
		t.Fatalf("DownloadAndLoadDatabase failed: %v", err)
	}

	for _, empty := range []string{"", "\n\n# nothing here\n"} {
		body.Store(empty)
		if err := s.DownloadAndLoadDatabase(testDb); !errors.Is(err, ErrNoDomains) {
			t.Errorf("DownloadAndLoadDatabase with body %q returned %v, want ErrNoDomains", empty, err)
		}
	}

	mustHave(t, s, "example.com", true)
	if cached, _ := storage.cached(testDb); cached != "example.com\n" {
		t.Errorf("cache was overwritten: %q", cached)
	}
}

// If writing to the cache fails, the error is reported but the complete download
// is still loaded into memory.
func TestDownload_StorageFailureKeepsNewDataInMemory(t *testing.T) {
	storage := newMemStorage()
	storage.failWrites = true

	s := newTestDomainDb(t, Options{
		StorageDriver: storage,
		Sources:       map[string]*DataSource{testDb: staticSource("example.com\n")},
	})

	if err := s.DownloadAndLoadDatabase(testDb); err == nil {
		t.Errorf("expected the storage write failure to be reported")
	}

	mustHave(t, s, "example.com", true)
}

// Databases larger than the maximum size are rejected, whether they come from a
// Get function, a single URL or several URLs merged together.
func TestDownload_EnforcesMaxDatabaseSize(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("aaaa.com\nbbbb.com\n"))
	}))
	defer ts.Close()

	for _, src := range []*DataSource{
		staticSource("aaaa.com\nbbbb.com\n"),
		urlSource(ts.URL),
		// Each URL fits, but the merged database does not.
		urlSource(ts.URL+"/a", ts.URL+"/b"),
	} {
		s := newTestDomainDb(t, Options{
			MaxDatabaseSize: 10,
			Sources:         map[string]*DataSource{testDb: src},
		})
		if len(src.Urls) == 2 {
			s.maxDbSize = 30
		}

		if err := s.DownloadAndLoadDatabase(testDb); !errors.Is(err, ErrDatabaseTooLarge) {
			t.Errorf("DownloadAndLoadDatabase returned %v, want ErrDatabaseTooLarge", err)
		}
	}
}

// Credentials and query parameters (such as API keys) in source URLs never
// appear in errors or logs.
func TestDownload_RedactsUrlSecrets(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer ts.Close()

	var logs bytes.Buffer
	withSecrets := strings.Replace(ts.URL, "http://", "http://user:hunter2@", 1) + "/db?api_key=supersecret#frag"
	// The second URL fails before a response is received.
	unreachable := "http://user:hunter2@127.0.0.1:1/db?api_key=supersecret"

	s := newTestDomainDb(t, Options{
		Logger:  slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Sources: map[string]*DataSource{testDb: urlSource(withSecrets, unreachable)},
	})

	err := s.DownloadAndLoadDatabase(testDb)
	if err == nil || !errors.Is(err, ErrAllUrlsFailed) {
		t.Fatalf("DownloadAndLoadDatabase returned %v, want ErrAllUrlsFailed", err)
	}

	for _, secret := range []string{"hunter2", "supersecret"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error contains secret %q: %v", secret, err)
		}
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain secret %q: %s", secret, logs.String())
		}
	}
	if !strings.Contains(err.Error(), "/db?REDACTED") {
		t.Errorf("error does not identify the failing URL: %v", err)
	}
}

// Invalid options are rejected by NewDomainDb instead of causing panics or
// unusable instances later.
func TestNewDomainDb_RejectsInvalidOptions(t *testing.T) {
	cases := map[string]Options{
		"no storage driver": {
			Sources: map[string]*DataSource{testDb: staticSource("example.com\n")},
		},
		"empty name": {
			StorageDriver: newMemStorage(),
			Sources:       map[string]*DataSource{"": staticSource("example.com\n")},
		},
		"name too long": {
			StorageDriver: newMemStorage(),
			Sources:       map[string]*DataSource{strings.Repeat("a", DbNameMaxSize+1): staticSource("example.com\n")},
		},
		"nil source": {
			StorageDriver: newMemStorage(),
			Sources:       map[string]*DataSource{testDb: nil},
		},
		"source without URLs or Get": {
			StorageDriver: newMemStorage(),
			Sources:       map[string]*DataSource{testDb: {RefreshInterval: time.Hour}},
		},
		"nil URL": {
			StorageDriver: newMemStorage(),
			Sources:       map[string]*DataSource{testDb: {RefreshInterval: time.Hour, Urls: []*url.URL{nil}}},
		},
		"zero refresh interval": {
			StorageDriver: newMemStorage(),
			Sources:       map[string]*DataSource{testDb: {Get: staticSource("example.com\n").Get}},
		},
	}

	for name, options := range cases {
		options.Logger = testLogger()
		if s, err := NewDomainDb(options); err == nil {
			_ = s.Close()
			t.Errorf("%s: NewDomainDb did not fail", name)
		}
	}
}

// If loading fails, NewDomainDb returns the error instead of a nil instance with
// a nil error.
func TestNewDomainDb_InitFailureReturnsError(t *testing.T) {
	src, _ := flakySource(1000, "example.com\n")
	s, err := NewDomainDb(Options{
		StorageDriver: newMemStorage(),
		Logger:        testLogger(),
		Sources:       map[string]*DataSource{testDb: src},
	})
	if err == nil {
		t.Fatalf("NewDomainDb returned instance %v and no error for a source that always fails", s)
	}
	if s != nil {
		t.Errorf("NewDomainDb returned a non-nil instance with an error")
	}
}

// A database downloaded by one instance is loaded from the cache by the next one,
// without downloading it again.
func TestNewDomainDb_RoundTrip(t *testing.T) {
	drv, err := NewFsStorageDriver(t.TempDir())
	if err != nil {
		t.Fatalf("NewFsStorageDriver failed: %v", err)
	}

	src, calls := flakySource(0, "example.com\nexample.org\n")
	options := Options{
		StorageDriver: drv,
		Logger:        testLogger(),
		Sources:       map[string]*DataSource{testDb: src},
	}

	for range 2 {
		s, err := NewDomainDb(options)
		if err != nil {
			t.Fatalf("NewDomainDb failed: %v", err)
		}

		mustHave(t, s, "example.org", true)
		mustHave(t, s, "example.net", false)

		_, err = s.DoesDbHaveDomain("missing", "example.com")
		if _, ok := errors.AsType[*NoSuchDatabaseError](err); !ok {
			t.Errorf("DoesDbHaveDomain for a missing database returned %v, want NoSuchDatabaseError", err)
		}

		if err := s.Close(); err != nil {
			t.Fatalf("Close failed: %v", err)
		}
	}

	// The second instance used the cache; the checkpoint is recent, so no update was due.
	if got := calls.Load(); got != 1 {
		t.Errorf("source was called %d times, want 1", got)
	}

	chk, err := drv.ReadCheckpoints()
	if err != nil {
		t.Fatalf("ReadCheckpoints failed: %v", err)
	}
	if chk.Checkpoints[testDb].LastUpdatedUnix == 0 {
		t.Errorf("no checkpoint was saved for database %q", testDb)
	}
}

// A corrupt cached database does not prevent startup: a fresh copy is downloaded
// and replaces it.
func TestNewDomainDb_RecoversFromCorruptCache(t *testing.T) {
	dir := t.TempDir()
	drv, err := NewFsStorageDriver(dir)
	if err != nil {
		t.Fatalf("NewFsStorageDriver failed: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, testDb+dbFileSuffix), []byte("<html>not a list</html>\n"), 0644); err != nil {
		t.Fatalf("failed to write corrupt database: %v", err)
	}

	options := Options{
		StorageDriver: drv,
		Logger:        testLogger(),
		Sources:       map[string]*DataSource{testDb: staticSource("example.com\n")},
	}

	s, err := NewDomainDb(options)
	if err != nil {
		t.Fatalf("NewDomainDb failed with corrupt cache: %v", err)
	}
	mustHave(t, s, "example.com", true)
	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// The cache was repaired, so it can now be used without downloading.
	options.DisableDownload = true
	options.Sources[testDb].Get = func() (io.ReadCloser, error) {
		t.Errorf("source was used even though downloading is disabled")
		return nil, errors.New("downloading is disabled")
	}

	s, err = NewDomainDb(options)
	if err != nil {
		t.Fatalf("NewDomainDb failed with repaired cache and downloading disabled: %v", err)
	}
	defer func() {
		_ = s.Close()
	}()

	mustHave(t, s, "example.com", true)

	// No temp files may be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), tmpFilePrefix) {
			t.Errorf("temp file was left behind: %s", entry.Name())
		}
	}
}

// With downloading disabled, NewDomainDb fails if there is no usable cached copy.
func TestNewDomainDb_DisableDownloadRequiresUsableCache(t *testing.T) {
	storage := newMemStorage()
	options := Options{
		StorageDriver:   storage,
		Logger:          testLogger(),
		DisableDownload: true,
		Sources:         map[string]*DataSource{testDb: staticSource("example.com\n")},
	}

	if _, err := NewDomainDb(options); !errors.Is(err, ErrNoCacheAndNoDownload) {
		t.Errorf("NewDomainDb without cache returned %v, want ErrNoCacheAndNoDownload", err)
	}

	storage.dbs[testDb] = []byte("not_a_domain!\n")
	if _, err := NewDomainDb(options); err == nil {
		t.Errorf("NewDomainDb with corrupt cache and downloading disabled did not fail")
	}
}

// With LoadDatabasesInBackground, NewDomainDb returns before the databases are
// loaded, and they become usable once background loading finishes.
func TestNewDomainDb_BackgroundInitLoadsDatabases(t *testing.T) {
	release := make(chan struct{})
	storage := newMemStorage()
	s, err := NewDomainDb(Options{
		StorageDriver: storage,
		Logger:        testLogger(),
		Sources: map[string]*DataSource{testDb: {
			RefreshInterval: time.Hour,
			Get: func() (io.ReadCloser, error) {
				<-release
				return io.NopCloser(strings.NewReader("example.com\n")), nil
			},
		}},
		LoadDatabasesInBackground: true,
	})
	if err != nil {
		t.Fatalf("NewDomainDb failed: %v", err)
	}
	defer func() {
		_ = s.Close()
	}()

	_, err = s.DoesDbHaveDomain(testDb, "example.com")
	if _, ok := errors.AsType[*NotInitializedError](err); !ok {
		t.Errorf("DoesDbHaveDomain before loading finished returned %v, want NotInitializedError", err)
	}

	close(release)
	waitFor(t, "background initialization to succeed", func() bool {
		return hasDomain(s, "example.com")
	})
	waitFor(t, "checkpoints to be saved", func() bool {
		chk, err := storage.ReadCheckpoints()
		return err == nil && chk.Checkpoints[testDb].LastUpdatedUnix != 0
	})
}

// With LoadDatabasesInBackground, a failed initialization is not retried: the
// instance is closed, and its methods return ErrDbClosed.
func TestNewDomainDb_BackgroundInitFailureClosesInstance(t *testing.T) {
	src, calls := flakySource(1000, "example.com\n")
	s, err := NewDomainDb(Options{
		StorageDriver:             newMemStorage(),
		Logger:                    testLogger(),
		Sources:                   map[string]*DataSource{testDb: src},
		LoadDatabasesInBackground: true,
	})
	if err != nil {
		t.Fatalf("NewDomainDb failed: %v", err)
	}

	waitFor(t, "the instance to be closed", func() bool {
		_, err := s.DoesDbHaveDomain(testDb, "example.com")
		return errors.Is(err, ErrDbClosed)
	})

	// Close is idempotent, so calling it again waits for the background Close to finish.
	if err := s.Close(); err != nil {
		t.Errorf("Close failed: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("source was called %d times, want 1", got)
	}
	if goroutineStackContains("(*DomainDb).") {
		t.Errorf("a goroutine is still running DomainDb code after the instance was closed")
	}
}

// A failed scheduled update is retried after a short delay, and the next update
// after a success waits for the full refresh interval.
func TestUpdater_RetriesFailedUpdates(t *testing.T) {
	src, calls := flakySource(3, "example.com\n")
	s := newSingleTestDomainDb(t, src)
	s.updateRetryDelay = time.Millisecond

	// The last update was long ago, so the first update is due immediately.
	s.goRun(func() {
		s.runUpdater(s.dbs[testDb], time.Unix(0, 0))
	})

	waitFor(t, "scheduled update to succeed after retries", func() bool {
		return hasDomain(s, "example.com")
	})

	// After a successful update, the next one is a full refresh interval away.
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 4 {
		t.Errorf("source was called %d times, want 4", got)
	}
}

func TestRetryDelay(t *testing.T) {
	cases := []struct {
		failures int
		interval time.Duration
		want     time.Duration
	}{
		{1, time.Hour, time.Minute},
		{2, time.Hour, 2 * time.Minute},
		{6, time.Hour, 32 * time.Minute},
		{7, time.Hour, time.Hour},
		{1000, time.Hour, time.Hour},
		{1, 10 * time.Second, 10 * time.Second},
	}
	for _, c := range cases {
		if got := retryDelay(time.Minute, c.failures, c.interval); got != c.want {
			t.Errorf("retryDelay(1m, %d, %s) = %s, want %s", c.failures, c.interval, got, c.want)
		}
	}
}

// Close can be called while queries and updates are running, and more than once.
func TestClose_ConcurrentWithQueriesAndIdempotent(t *testing.T) {
	s := newSingleTestDomainDb(t, staticSource("example.com\n"))
	if err := s.DownloadAndLoadDatabase(testDb); err != nil {
		t.Fatalf("DownloadAndLoadDatabase failed: %v", err)
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				_, err := s.DoesDbHaveDomain(testDb, "example.com")
				_ = s.DownloadAndLoadDatabase(testDb)
				if err != nil {
					if !errors.Is(err, ErrDbClosed) {
						t.Errorf("DoesDbHaveDomain returned unexpected error: %v", err)
					}
					return
				}
			}
		})
	}

	time.Sleep(20 * time.Millisecond)
	for range 3 {
		if err := s.Close(); err != nil {
			t.Errorf("Close failed: %v", err)
		}
	}
	wg.Wait()

	if _, err := s.DoesDbHaveDomain(testDb, "example.com"); !errors.Is(err, ErrDbClosed) {
		t.Errorf("DoesDbHaveDomain after Close returned %v, want ErrDbClosed", err)
	}
	if err := s.DownloadAndLoadDatabase(testDb); !errors.Is(err, ErrDbClosed) {
		t.Errorf("DownloadAndLoadDatabase after Close returned %v, want ErrDbClosed", err)
	}
}

// Close cancels in-flight downloads and waits for all background goroutines to
// stop before returning.
func TestClose_StopsBackgroundWork(t *testing.T) {
	requestStarted := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hang" {
			select {
			case requestStarted <- struct{}{}:
			default:
			}
			// Hang until the client gives up.
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte("example.com\n"))
	}))
	defer ts.Close()

	storage := newMemStorage()
	// This database is cached, but has no checkpoint, so its updater starts a download immediately.
	storage.dbs["hanging"] = []byte("example.org\n")
	s, err := NewDomainDb(Options{
		StorageDriver: storage,
		Logger:        testLogger(),
		HttpClient:    ts.Client(),
		Sources: map[string]*DataSource{
			testDb:    urlSource(ts.URL + "/ok"),
			"hanging": urlSource(ts.URL + "/hang"),
		},
	})
	if err != nil {
		t.Fatalf("NewDomainDb failed: %v", err)
	}

	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for the updater to start its download")
	}

	closed := make(chan error, 1)
	go func() {
		closed <- s.Close()
	}()

	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Close did not return while a download was hanging")
	}

	if goroutineStackContains("(*DomainDb).") {
		t.Errorf("a goroutine is still running DomainDb code after Close returned")
	}
}

// Lookups run without locking while the database is replaced and the instance is
// closed. Every lookup must see a complete database (old or new), and no database
// may be left installed after Close, even if an update finishes while Close is running.
func TestLookups_ConcurrentWithUpdatesAndClose(t *testing.T) {
	var body atomic.Value
	body.Store("example.com\n")

	s := newSingleTestDomainDb(t, stringSource(&body))
	if err := s.DownloadAndLoadDatabase(testDb); err != nil {
		t.Fatalf("initial load failed: %v", err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for {
				// Both versions of the list contain example.com; they differ in example.org.
				has, err := s.DoesDbHaveDomain(testDb, "example.com")
				if err != nil {
					if !errors.Is(err, ErrDbClosed) {
						t.Errorf("DoesDbHaveDomain returned unexpected error: %v", err)
					}
					return
				}
				if !has {
					t.Errorf("DoesDbHaveDomain(example.com) = false during an update, want true")
					return
				}
			}
		})
	}

	var updater sync.WaitGroup
	updater.Go(func() {
		for n := 0; ; n++ {
			if n%2 == 0 {
				body.Store("example.com\nexample.org\n")
			} else {
				body.Store("example.com\n")
			}
			if err := s.DownloadAndLoadDatabase(testDb); errors.Is(err, ErrDbClosed) {
				return
			} else if err != nil {
				t.Errorf("update failed: %v", err)
				return
			}
		}
	})

	time.Sleep(100 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Errorf("Close failed: %v", err)
	}
	wg.Wait()
	updater.Wait()

	if s.dbs[testDb].isLoaded() {
		t.Errorf("database is still loaded after Close")
	}
}

// With IncludeSubdomains, a domain matches if it or any of its parent domains is
// in the database. Matching is done label by label, and queries are normalized.
// Without it, only exact matches are found.
func TestDoesDbHaveDomainWithOptions_IncludeSubdomains(t *testing.T) {
	s := newSingleTestDomainDb(t, staticSource("yopmail.com\ndé.net\nio\n"))
	if err := s.DownloadAndLoadDatabase(testDb); err != nil {
		t.Fatalf("DownloadAndLoadDatabase failed: %v", err)
	}

	cases := []struct {
		domain    string
		exact     bool
		subdomain bool
	}{
		{"yopmail.com", true, true},
		{"YopMail.COM.", true, true},
		{"mail.yopmail.com", false, true},
		{"a.b.c.yopmail.com", false, true},
		{"MAIL.YOPMAIL.COM", false, true},
		{"sub.dé.net", false, true},
		{"sub.xn--d-bga.net", false, true},
		// Entries for top-level domains cover everything beneath them.
		{"anything.io", false, true},
		// Label boundaries are respected.
		{"notyopmail.com", false, false},
		{"yopmail.com.evil.org", false, false},
		{"yopmail.co", false, false},
		{"com", false, false},
		{"example.org", false, false},
	}

	for _, c := range cases {
		got, err := s.DoesDbHaveDomain(testDb, c.domain)
		if err != nil || got != c.exact {
			t.Errorf("DoesDbHaveDomain(%q) = %v, %v; want %v", c.domain, got, err, c.exact)
		}

		got, err = s.DoesDbHaveDomainWithOptions(testDb, c.domain, LookupOptions{})
		if err != nil || got != c.exact {
			t.Errorf("DoesDbHaveDomainWithOptions(%q, {}) = %v, %v; want %v", c.domain, got, err, c.exact)
		}

		got, err = s.DoesDbHaveDomainWithOptions(testDb, c.domain, LookupOptions{IncludeSubdomains: true})
		if err != nil || got != c.subdomain {
			t.Errorf("DoesDbHaveDomainWithOptions(%q, IncludeSubdomains) = %v, %v; want %v", c.domain, got, err, c.subdomain)
		}
	}
}

// DoesDbHaveDomainWithOptions returns the same errors as DoesDbHaveDomain.
func TestDoesDbHaveDomainWithOptions_Errors(t *testing.T) {
	s := newSingleTestDomainDb(t, staticSource("example.com\n"))
	options := LookupOptions{IncludeSubdomains: true}

	if _, err := s.DoesDbHaveDomainWithOptions(testDb, "example.com", options); err == nil {
		t.Errorf("expected an error before the database is loaded")
	} else if _, ok := errors.AsType[*NotInitializedError](err); !ok {
		t.Errorf("DoesDbHaveDomainWithOptions before loading returned %v, want NotInitializedError", err)
	}

	if err := s.DownloadAndLoadDatabase(testDb); err != nil {
		t.Fatalf("DownloadAndLoadDatabase failed: %v", err)
	}

	_, err := s.DoesDbHaveDomainWithOptions("missing", "example.com", options)
	if _, ok := errors.AsType[*NoSuchDatabaseError](err); !ok {
		t.Errorf("DoesDbHaveDomainWithOptions for a missing database returned %v, want NoSuchDatabaseError", err)
	}
	if _, err := s.DoesDbHaveDomainWithOptions(testDb, "user@example.com", options); err == nil {
		t.Errorf("DoesDbHaveDomainWithOptions accepted an email address as a domain")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if _, err := s.DoesDbHaveDomainWithOptions(testDb, "sub.example.com", options); !errors.Is(err, ErrDbClosed) {
		t.Errorf("DoesDbHaveDomainWithOptions after Close returned %v, want ErrDbClosed", err)
	}
}

// StopAtLabelCount stops the parent domain search when it reaches a parent domain
// with that many labels, without checking it. The looked-up domain itself is always
// checked.
func TestDoesDbHaveDomainWithOptions_StopAtLabelCount(t *testing.T) {
	s := newSingleTestDomainDb(t, staticSource("com\nyopmail.com\nmail.example.org\n"))
	if err := s.DownloadAndLoadDatabase(testDb); err != nil {
		t.Fatalf("DownloadAndLoadDatabase failed: %v", err)
	}

	cases := []struct {
		domain string
		stopAt int
		want   bool
	}{
		// Without a limit, the top-level domain entry matches everything beneath it.
		{"anything.com", 0, true},
		{"anything.com", -1, true},
		// 1 stops at the top-level domain.
		{"anything.com", 1, false},
		{"mail.yopmail.com", 1, true},
		{"a.b.mail.yopmail.com", 1, true},
		// 2 also stops at two-label domains.
		{"mail.yopmail.com", 2, false},
		{"a.mail.example.org", 2, true},
		{"a.b.mail.example.org", 3, false},
		// The looked-up domain itself is always checked.
		{"com", 1, true},
		{"yopmail.com", 2, true},
		{"mail.example.org", 5, true},
	}

	for _, c := range cases {
		got, err := s.DoesDbHaveDomainWithOptions(testDb, c.domain, LookupOptions{IncludeSubdomains: true, StopAtLabelCount: c.stopAt})
		if err != nil || got != c.want {
			t.Errorf("DoesDbHaveDomainWithOptions(%q, StopAtLabelCount: %d) = %v, %v; want %v", c.domain, c.stopAt, got, err, c.want)
		}
	}

	// Ignored without IncludeSubdomains.
	if got, err := s.DoesDbHaveDomainWithOptions(testDb, "mail.yopmail.com", LookupOptions{StopAtLabelCount: 1}); err != nil || got {
		t.Errorf("DoesDbHaveDomainWithOptions without IncludeSubdomains = %v, %v; want false", got, err)
	}
}
