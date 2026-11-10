package domaindb

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// listServer serves "/good" and "/good2" as valid lists, "/missing" as a 404, "/html" as an HTML page with
// status 200, and "/flaky" as a 503 for the first flakyFailures requests and a valid list after that.
func listServer(t *testing.T, flakyFailures int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var flakyCalls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/good":
			_, _ = w.Write([]byte("good.com\n"))
		case "/good2":
			_, _ = w.Write([]byte("good2.com\n"))
		case "/html":
			_, _ = w.Write([]byte("<html>\n<body>\nWe moved!\n</body>\n</html>\n"))
		case "/flaky":
			if flakyCalls.Add(1) <= flakyFailures {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("flaky.com\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)

	return ts, &flakyCalls
}

// With the default policy, a URL that 404s or serves something that is not a list
// is skipped, and the database is updated from its other URLs.
func TestRetryPolicy_DefaultSkipsBrokenUrls(t *testing.T) {
	ts, _ := listServer(t, 0)

	storage := newMemStorage()
	s := newTestDomainDb(t, Options{
		StorageDriver: storage,
		Sources: map[string]*DataSource{
			testDb: urlSource(ts.URL+"/missing", ts.URL+"/good", ts.URL+"/html", ts.URL+"/good2"),
		},
	})
	if err := s.DownloadAndLoadDatabase(testDb); err != nil {
		t.Fatalf("DownloadAndLoadDatabase failed: %v", err)
	}

	mustHave(t, s, "good.com", true)
	mustHave(t, s, "good2.com", true)
	if cached, _ := storage.cached(testDb); cached != "good.com\n\ngood2.com\n\n" {
		t.Errorf("unexpected cached database: %q", cached)
	}
}

// If every URL is skipped, the update fails and the database keeps its data.
func TestRetryPolicy_AllUrlsSkippedKeepsData(t *testing.T) {
	ts, _ := listServer(t, 0)

	src := urlSource(ts.URL + "/good")
	s := newSingleTestDomainDb(t, src)
	if err := s.DownloadAndLoadDatabase(testDb); err != nil {
		t.Fatalf("DownloadAndLoadDatabase failed: %v", err)
	}

	src.Urls = urlSource(ts.URL+"/missing", ts.URL+"/html").Urls
	err := s.DownloadAndLoadDatabase(testDb)
	if !errors.Is(err, ErrAllUrlsFailed) {
		t.Fatalf("DownloadAndLoadDatabase returned %v, want ErrAllUrlsFailed", err)
	}
	if statusErr, ok := errors.AsType[*HttpStatusError](err); !ok || statusErr.StatusCode != http.StatusNotFound {
		t.Errorf("error does not wrap an HttpStatusError with status 404: %v", err)
	}

	mustHave(t, s, "good.com", true)
}

// The policy can retry a source, and receives the database name, URL, attempt
// number and error for each failure.
func TestRetryPolicy_Retry(t *testing.T) {
	ts, flakyCalls := listServer(t, 2)

	var mu sync.Mutex
	var seen []SourceFailure
	s := newTestDomainDb(t, Options{
		Sources: map[string]*DataSource{
			testDb: urlSource(ts.URL+"/flaky", ts.URL+"/good"),
		},
		RetryPolicy: func(f SourceFailure) RetryDecision {
			mu.Lock()
			seen = append(seen, f)
			mu.Unlock()

			if statusErr, ok := errors.AsType[*HttpStatusError](f.Err); ok && statusErr.StatusCode >= 500 && f.Attempt < 5 {
				return RetryDecision{Action: RetryActionRetry, Delay: time.Millisecond}
			}
			return RetryDecision{Action: RetryActionSkip}
		},
	})
	if err := s.DownloadAndLoadDatabase(testDb); err != nil {
		t.Fatalf("DownloadAndLoadDatabase failed: %v", err)
	}

	mustHave(t, s, "flaky.com", true)
	mustHave(t, s, "good.com", true)
	if got := flakyCalls.Load(); got != 3 {
		t.Errorf("flaky URL was requested %d times, want 3", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("policy was called %d times, want 2", len(seen))
	}
	for i, f := range seen {
		if f.DbName != testDb || f.Url == nil || f.Url.Path != "/flaky" || f.Attempt != i+1 {
			t.Errorf("unexpected failure passed to policy: %+v", f)
		}
	}
}

// RetryActionFail fails the whole update immediately: the remaining URLs are not
// downloaded and the database keeps its data.
func TestRetryPolicy_Fail(t *testing.T) {
	var goodCalls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/good" {
			goodCalls.Add(1)
			_, _ = w.Write([]byte("good.com\n"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	src := urlSource(ts.URL + "/good")
	s := newTestDomainDb(t, Options{
		Sources: map[string]*DataSource{testDb: src},
		RetryPolicy: func(f SourceFailure) RetryDecision {
			return RetryDecision{Action: RetryActionFail}
		},
	})
	if err := s.DownloadAndLoadDatabase(testDb); err != nil {
		t.Fatalf("DownloadAndLoadDatabase failed: %v", err)
	}

	src.Urls = urlSource(ts.URL+"/missing", ts.URL+"/good").Urls
	err := s.DownloadAndLoadDatabase(testDb)
	if !errors.Is(err, ErrFailedByRetryPolicy) {
		t.Fatalf("DownloadAndLoadDatabase returned %v, want ErrFailedByRetryPolicy", err)
	}
	if _, ok := errors.AsType[*HttpStatusError](err); !ok {
		t.Errorf("error does not wrap the source's error: %v", err)
	}
	if got := goodCalls.Load(); got != 1 {
		t.Errorf("URL after the failed one was downloaded %d times in total, want 1", got)
	}

	mustHave(t, s, "good.com", true)
}

// The policy applies to Get functions, which are passed to it with a nil URL.
func TestRetryPolicy_GetSource(t *testing.T) {
	src, calls := flakySource(2, "good.com\n")
	s := newTestDomainDb(t, Options{
		Sources: map[string]*DataSource{testDb: src},
		RetryPolicy: func(f SourceFailure) RetryDecision {
			if f.Url != nil {
				t.Errorf("policy got URL %v for a Get source, want nil", f.Url)
			}
			return RetryDecision{Action: RetryActionRetry}
		},
	})
	if err := s.DownloadAndLoadDatabase(testDb); err != nil {
		t.Fatalf("DownloadAndLoadDatabase failed: %v", err)
	}

	mustHave(t, s, "good.com", true)
	if got := calls.Load(); got != 3 {
		t.Errorf("Get was called %d times, want 3", got)
	}
}

// Closing the instance cuts a retry delay short.
func TestRetryPolicy_CloseInterruptsRetryDelay(t *testing.T) {
	ts, _ := listServer(t, 0)

	policyCalled := make(chan struct{}, 1)
	s := newTestDomainDb(t, Options{
		Sources: map[string]*DataSource{testDb: urlSource(ts.URL + "/missing")},
		RetryPolicy: func(f SourceFailure) RetryDecision {
			select {
			case policyCalled <- struct{}{}:
			default:
			}
			return RetryDecision{Action: RetryActionRetry, Delay: time.Hour}
		},
	})

	done := make(chan error, 1)
	go func() {
		done <- s.DownloadAndLoadDatabase(testDb)
	}()

	<-policyCalled
	closeDone := make(chan struct{})
	go func() {
		_ = s.Close()
		close(closeDone)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrDbClosed) {
			t.Errorf("DownloadAndLoadDatabase returned %v, want ErrDbClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retry delay was not interrupted by Close")
	}
	<-closeDone
}

// One database's broken source does not stop other databases from updating.
func TestRetryPolicy_BrokenDatabaseDoesNotBlockOthers(t *testing.T) {
	ts, _ := listServer(t, 0)

	s := newTestDomainDb(t, Options{
		Sources: map[string]*DataSource{
			"broken": urlSource(ts.URL + "/missing"),
			testDb:   urlSource(ts.URL + "/good"),
		},
		RetryPolicy: func(f SourceFailure) RetryDecision {
			return RetryDecision{Action: RetryActionRetry, Delay: time.Hour}
		},
	})

	go func() {
		_ = s.DownloadAndLoadDatabase("broken")
	}()

	if err := s.DownloadAndLoadDatabase(testDb); err != nil {
		t.Fatalf("DownloadAndLoadDatabase failed: %v", err)
	}
	mustHave(t, s, "good.com", true)
}
