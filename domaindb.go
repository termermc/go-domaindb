package domaindb

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/termermc/go-domaindb/normalize"
)

// DefaultMaxDatabaseSize is the default maximum size of a single database, in bytes.
// See Options.MaxDatabaseSize.
const DefaultMaxDatabaseSize int64 = 1 * 1024 * 1024 * 1024 // 1GiB

const (
	// DefaultHttpTimeout is the default HTTP client's timeout for an entire download, including reading the body.
	// It is used as http.Client.Timeout when Options.HttpClient is nil.
	DefaultHttpTimeout = 5 * time.Minute

	// DefaultHttpResponseHeaderTimeout is the default HTTP client's timeout for receiving response headers after
	// sending a request.
	// It is used as http.Transport.ResponseHeaderTimeout when Options.HttpClient is nil.
	DefaultHttpResponseHeaderTimeout = 30 * time.Second
)

const (
	// The delay before the first retry of a failed scheduled update.
	// Doubles on each consecutive failure, up to the source's refresh interval.
	defaultUpdateRetryDelay = 1 * time.Minute

	// The maximum number of domain parse failures to log and include in errors per load.
	maxReportedParseFailures = 10

	// The number of bytes of a non-200 response body to include in errors.
	bodyPreviewBytes = 1024
)

// database is a single in-memory database and its source.
type database struct {
	name string
	src  *DataSource

	// updateMu serializes downloads of the database.
	updateMu sync.Mutex

	// loaded is the loaded database, or nil if it has not been loaded or the instance has been closed.
	//
	// Lookups load the pointer once and use that snapshot without locking.
	// Updates replace the pointer with a new snapshot; lookups still using the old snapshot finish with it,
	// and it is freed by the GC once they are done. Snapshots are never modified after being stored.
	loaded atomic.Pointer[loadedDb]
}

// loadedDb is an immutable snapshot of a loaded database.
type loadedDb struct {
	// domains is the set of normalized domain names in the database.
	domains map[string]struct{}
}

func (db *database) isLoaded() bool {
	return db.loaded.Load() != nil
}

// DomainDb stores and updates domain databases.
//
// Includes functionality to:
//   - Check if a domain falls within a list of domains
//
// Databases are cached on disk and updated periodically from data sources.
// At runtime, databases are stored in-memory.
//
// Caches are not aware of which data sources were used to create them, so adding, removing or changing data source URLs or Get method implementations should be followed by clearing the cache.
//
// Create an instance with NewDomainDb; do not create an empty DomainDb struct and attempt to use it.
//
// There should only be one instance of DomainDb per storage driver or storage location, and ideally only one per process.
// It is safe to use a single instance of DomainDb across multiple goroutines.
type DomainDb struct {
	storage     StorageDriver
	disableDl   bool
	httpClient  *http.Client
	logger      *slog.Logger
	maxDbSize   int64
	retryPolicy RetryPolicy
	normalizer  *normalize.DomainNormalizer

	updateRetryDelay time.Duration

	// The databases.
	// The map is never modified after construction.
	dbs map[string]*database

	// chkMu guards checkpoints and serializes checkpoint writes.
	chkMu       sync.Mutex
	checkpoints *AllCheckpoints

	// ctx is canceled when the instance is closed.
	ctx    context.Context
	cancel context.CancelFunc

	// aliveMu guards starting background goroutines against Close.
	aliveMu   sync.Mutex
	wg        sync.WaitGroup
	closed    atomic.Bool
	closeOnce sync.Once
}

// DataSource stores source information for domain data.
// It includes the URLs the data can be fetched from and the time to wait between updating the data from them.
type DataSource struct {
	// Urls are the URLs where the domain data is located.
	// Either Get or Urls must be provided; Get takes precedence over Urls.
	//
	// The contents of all URLs are merged into a single database.
	// URLs that fail to download or do not contain a valid list are handled by Options.RetryPolicy;
	// by default, they are logged and skipped, and the database is updated from the remaining URLs.
	Urls []*url.URL

	// Get is a function to get the domain data.
	// Either Get or Urls must be provided; Get takes precedence over Urls.
	// The returned reader is closed by the DomainDb instance.
	//
	// Get should return within a reasonable amount of time; DomainDb.Close waits for running calls to finish.
	Get func() (io.ReadCloser, error)

	// RefreshInterval is the interval between updating the data from the source.
	// Required and must be positive, unless Options.DisableDownload is true.
	//
	// If an update fails, it is retried with an increasing delay that starts at one minute and never exceeds RefreshInterval.
	RefreshInterval time.Duration
}

// Options are options for creating an DomainDb instance.
// Any omitted DataSource fields will be disabled and unavailable, even if cached files for them exist.
type Options struct {
	// The storage driver used to store cached databases and checkpoint information.
	// Unless you have a custom driver you want to use, you should most likely use FsStorageDriver.
	// Required.
	StorageDriver StorageDriver

	// By default, DomainDb uses slog.Default.
	// If Logger is specified, it will use it instead.
	Logger *slog.Logger

	// Overrides the default HTTP client if not nil.
	// If nil, uses a default HTTP client based on http.DefaultTransport, with a timeout of DefaultHttpTimeout per
	// download and a timeout of DefaultHttpResponseHeaderTimeout for receiving response headers.
	HttpClient *http.Client

	// If true, disables downloading from sources and only uses cached database files.
	//
	// Important: You must still provide sources for the databases you want to use, regardless of whether download is
	// disabled.
	DisableDownload bool

	// If true, NewDomainDb returns without waiting for databases to be loaded, and they are loaded in the background
	// instead.
	//
	// This option is intended for speeding up startup during development, where waiting for databases to load or
	// download every time the program starts gets in the way. It is unsuitable for production, because:
	//   - Any methods on DomainDb that require databases return a NotInitializedError until the databases have loaded.
	//   - If loading fails, the error is only logged, and the DomainDb instance is closed. Loading is not retried, and
	//     the instance's methods return ErrDbClosed from then on. Without this option, NewDomainDb would instead
	//     return the error, so the program can handle it.
	LoadDatabasesInBackground bool

	// The maximum size of a single database, in bytes.
	// Downloads and cached files larger than this are rejected with ErrDatabaseTooLarge.
	// For databases with multiple URLs, the limit applies to the merged database.
	// If zero or negative, DefaultMaxDatabaseSize is used.
	MaxDatabaseSize int64

	// Decides what happens when a source of a database fails to download: retry it, skip it, or fail the
	// update of the whole database. See RetryPolicy.
	// A download that completes but does not contain a valid list also counts as a failure.
	//
	// If nil, failed sources are logged and skipped without retrying, so a broken URL never stops a database
	// from being updated from its other URLs.
	// See SkipAllSources.
	RetryPolicy RetryPolicy

	// A mapping of database names to their underlying sources.
	// Each source must yield a newline-separated list of domain names.
	// Empty lines are ignored, as is anything following a '#' character.
	// Domain names are normalized; see normalize.DomainNormalizer.
	//
	// Names must not be empty and must be at most DbNameMaxSize bytes long.
	// The storage driver may impose additional restrictions.
	Sources map[string]*DataSource
}

func newDefaultHttpClient() *http.Client {
	transport := http.DefaultTransport
	if t, ok := transport.(*http.Transport); ok {
		clone := t.Clone()
		clone.ResponseHeaderTimeout = DefaultHttpResponseHeaderTimeout
		transport = clone
	}

	return &http.Client{
		Timeout:   DefaultHttpTimeout,
		Transport: transport,
	}
}

// validateDataSource returns an error if the data source cannot be used.
func validateDataSource(src *DataSource, disableDl bool) error {
	if src == nil {
		return errors.New("data source is nil")
	}
	if src.Get == nil && len(src.Urls) == 0 {
		return ErrDataSourceNoSource
	}
	for i, srcUrl := range src.Urls {
		if srcUrl == nil {
			return fmt.Errorf("URL at index %d is nil", i)
		}
	}
	if !disableDl && src.RefreshInterval <= 0 {
		return fmt.Errorf("%w (got %s)", ErrInvalidRefreshInterval, src.RefreshInterval)
	}

	return nil
}

// newDomainDb validates options and creates a DomainDb instance without loading anything or starting any goroutines.
func newDomainDb(options Options) (*DomainDb, error) {
	if options.StorageDriver == nil {
		return nil, errors.New("domaindb: Options.StorageDriver is required")
	}

	httpClient := options.HttpClient
	if httpClient == nil {
		httpClient = newDefaultHttpClient()
	}

	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}

	maxDbSize := options.MaxDatabaseSize
	if maxDbSize <= 0 {
		maxDbSize = DefaultMaxDatabaseSize
	}

	retryPolicy := options.RetryPolicy
	if retryPolicy == nil {
		retryPolicy = SkipAllSources
	}

	nameValidator, _ := options.StorageDriver.(DbNameValidator)

	dbs := make(map[string]*database, len(options.Sources))
	for name, src := range options.Sources {
		switch {
		case name == "":
			return nil, fmt.Errorf("domaindb: %w: database name must not be empty", ErrInvalidDbName)
		case len(name) > DbNameMaxSize:
			return nil, fmt.Errorf(`domaindb: invalid database name "%s": %w`, name, ErrDbNameTooLong)
		}
		if nameValidator != nil {
			if err := nameValidator.ValidateDatabaseName(name); err != nil {
				return nil, fmt.Errorf(`domaindb: database name "%s" was rejected by the storage driver: %w`, name, err)
			}
		}
		if err := validateDataSource(src, options.DisableDownload); err != nil {
			return nil, fmt.Errorf(`domaindb: invalid data source for database "%s": %w`, name, err)
		}

		dbs[name] = &database{
			name: name,
			src:  src,
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &DomainDb{
		storage:     options.StorageDriver,
		disableDl:   options.DisableDownload,
		httpClient:  httpClient,
		logger:      logger,
		maxDbSize:   maxDbSize,
		retryPolicy: retryPolicy,
		normalizer:  normalize.NewDomainNormalizer(),

		updateRetryDelay: defaultUpdateRetryDelay,

		dbs: dbs,

		checkpoints: &AllCheckpoints{
			Checkpoints: make(map[string]Checkpoint),
		},

		ctx:    ctx,
		cancel: cancel,
	}, nil
}

// NewDomainDb creates a new DomainDb instance.
// Blocks until the databases are loaded, unless Options.LoadDatabasesInBackground is true.
// If loading fails, returns the error, or with Options.LoadDatabasesInBackground, closes the returned instance.
// There should only be one instance of DomainDb per storage driver or storage location, and ideally only one per process.
// If error is nil, the returned DomainDb instance will never be nil.
func NewDomainDb(options Options) (*DomainDb, error) {
	s, err := newDomainDb(options)
	if err != nil {
		return nil, err
	}

	s.logger.Info("initializing DomainDb",
		"service", "domaindb.DomainDb",
	)

	checkpoints, err := s.storage.ReadCheckpoints()
	if err == nil {
		if checkpoints != nil && checkpoints.Checkpoints != nil {
			s.checkpoints = checkpoints
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		_ = s.Close()
		return nil, fmt.Errorf("failed to load checkpoints during initialization: %w", err)
	}

	if options.LoadDatabasesInBackground {
		s.logger.Debug("loading databases in the background, as requested by DomainDb options",
			"service", "domaindb.DomainDb",
		)
		s.goRun(s.initInBackground)

		return s, nil
	}

	if err := s.init(); err != nil {
		_ = s.Close()
		return nil, err
	}

	return s, nil
}

// goRun runs the function in a goroutine that Close will wait for.
// Does nothing if the instance is closed.
func (s *DomainDb) goRun(fn func()) {
	s.aliveMu.Lock()
	defer s.aliveMu.Unlock()

	if s.closed.Load() {
		return
	}

	s.wg.Go(fn)
}

// sleep waits for the specified duration.
// Returns false if the instance was closed while waiting.
func (s *DomainDb) sleep(duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// init loads all databases that are not loaded yet, saves checkpoints and starts the updaters.
func (s *DomainDb) init() error {
	for _, db := range s.dbs {
		if s.closed.Load() {
			return ErrDbClosed
		}
		if db.isLoaded() {
			continue
		}
		if err := s.initDb(db); err != nil {
			return err
		}
	}

	// Save checkpoints.
	// This is necessary because checkpoints may never have been saved, or may be missing databases.
	s.chkMu.Lock()
	for name := range s.dbs {
		if _, has := s.checkpoints.Checkpoints[name]; !has {
			s.checkpoints.Checkpoints[name] = Checkpoint{}
		}
	}
	lastUpdates := make(map[string]time.Time, len(s.dbs))
	for name := range s.dbs {
		lastUpdates[name] = time.Unix(s.checkpoints.Checkpoints[name].LastUpdatedUnix, 0)
	}
	err := s.storage.WriteCheckpoints(s.checkpoints)
	s.chkMu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to save checkpoints after initial load: %w", err)
	}

	if !s.disableDl {
		for _, db := range s.dbs {
			s.goRun(func() {
				s.runUpdater(db, lastUpdates[db.name])
			})
		}
	}

	s.logger.Info("finished initializing DomainDb",
		"service", "domaindb.DomainDb",
	)

	return nil
}

// initInBackground runs init once, closing the instance if it fails.
// See Options.LoadDatabasesInBackground.
func (s *DomainDb) initInBackground() {
	err := s.init()
	if err == nil || s.closed.Load() {
		return
	}

	s.logger.Error("failed to initialize DomainDb in the background, closing it; the DomainDb instance will return ErrDbClosed from now on",
		"service", "domaindb.DomainDb",
		"error", err,
	)

	// Close waits for all goroutines started by goRun, including this one, so it must be called from another
	// goroutine to avoid a deadlock. It returns as soon as this goroutine has finished.
	go func() {
		_ = s.Close()
	}()
}

// initDb loads the database from cache, falling back to downloading it if there is no usable cached copy.
func (s *DomainDb) initDb(db *database) error {
	s.logger.Debug("reading database from cache",
		"service", "domaindb.DomainDb",
		"database_name", db.name,
	)

	reader, err := s.storage.ReadDatabase(db.name)
	switch {
	case err == nil && reader != nil:
		var data []byte
		data, err = s.readAllLimited(reader)
		_ = reader.Close()
		if err == nil {
			err = s.load(db, data)
		}
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrDbClosed) {
			return err
		}
		if s.disableDl {
			return fmt.Errorf(`failed to load cached database with name "%s" during initialization, and downloading is disabled: %w`, db.name, err)
		}

		s.logger.Warn("cached database could not be loaded, downloading a fresh copy",
			"service", "domaindb.DomainDb",
			"database_name", db.name,
			"error", err,
		)
	case err == nil || errors.Is(err, fs.ErrNotExist):
		// No cached database.
		if s.disableDl {
			return fmt.Errorf(`cannot download database with name "%s" during initialization: %w`, db.name, ErrNoCacheAndNoDownload)
		}
	default:
		return fmt.Errorf(`failed to read database with name "%s" during initialization: %w`, db.name, err)
	}

	if err = s.refresh(db); err != nil {
		return fmt.Errorf(`failed to download database with name "%s" during initialization: %w`, db.name, err)
	}

	return nil
}

// retryDelay returns the time to wait before retrying an update after the specified number of consecutive failures.
func retryDelay(base time.Duration, failures int, interval time.Duration) time.Duration {
	delay := base << min(max(failures-1, 0), 16)
	if delay <= 0 || delay > interval {
		return interval
	}
	return delay
}

// runUpdater periodically updates the database until the instance is closed.
func (s *DomainDb) runUpdater(db *database, lastUpdate time.Time) {
	interval := db.src.RefreshInterval

	s.logger.Debug("running updater for database",
		"service", "domaindb.DomainDb",
		"database_name", db.name,
	)

	failures := 0
	wait := time.Until(lastUpdate.Add(interval))

	for {
		if !s.sleep(max(wait, 0)) {
			return
		}

		err := s.refresh(db)
		if s.closed.Load() {
			return
		}
		if err == nil {
			// Databases are big, and the update leaves the downloaded data and the replaced database behind as garbage.
			// Run the GC now to free that memory right away, instead of letting it build up until the next GC cycle.
			runtime.GC()

			failures = 0
			// The interval is intentionally measured from the end of the update rather than on a fixed schedule:
			// some sources require a minimum time between requests, which a fixed schedule could violate after a
			// slow update. The schedule drifting by the update's duration is acceptable.
			wait = interval
			continue
		}

		failures++
		wait = retryDelay(s.updateRetryDelay, failures, interval)

		s.logger.Error("failed to do scheduled update of database",
			"service", "domaindb.DomainDb",
			"database_name", db.name,
			"error", err,
			"consecutive_failures", failures,
			"retry_in", wait.String(),
		)
	}
}

// recordUpdate saves a checkpoint for the database with the specified name.
func (s *DomainDb) recordUpdate(name string, ts time.Time) {
	s.chkMu.Lock()
	defer s.chkMu.Unlock()

	chkPnt := s.checkpoints.Checkpoints[name]
	chkPnt.LastUpdatedUnix = ts.Unix()
	s.checkpoints.Checkpoints[name] = chkPnt

	if err := s.storage.WriteCheckpoints(s.checkpoints); err != nil {
		s.logger.Error("failed to save checkpoints after database update",
			"service", "domaindb.DomainDb",
			"database_name", name,
			"error", err,
		)
	}
}

// redactUrl returns the URL as a string without credentials or query parameters, for use in logs and errors.
// Source URLs may contain secrets such as API keys.
func redactUrl(srcUrl *url.URL) string {
	redacted := *srcUrl
	redacted.User = nil
	redacted.Fragment = ""
	redacted.RawFragment = ""
	redacted.ForceQuery = false
	if redacted.RawQuery != "" {
		redacted.RawQuery = "REDACTED"
	}
	return redacted.String()
}

// readAllLimited reads the entire reader, returning ErrDatabaseTooLarge if it is larger than the max database size.
func (s *DomainDb) readAllLimited(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, s.maxDbSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > s.maxDbSize {
		return nil, fmt.Errorf("%w (limit is %d bytes)", ErrDatabaseTooLarge, s.maxDbSize)
	}
	return data, nil
}

// fetchUrl downloads the entire body of the URL.
// An incomplete download results in an error; partial data is never returned.
func (s *DomainDb) fetchUrl(srcUrl *url.URL) ([]byte, error) {
	redacted := redactUrl(srcUrl)

	s.logger.Debug("starting download of database",
		"service", "domaindb.DomainDb",
		"source_url", redacted,
	)

	req, err := http.NewRequestWithContext(s.ctx, http.MethodGet, srcUrl.String(), nil)
	if err != nil {
		return nil, fmt.Errorf(`failed to create request (source URL "%s")`, redacted)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		// url.Error includes the full URL in its message, so unwrap it.
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err
		}
		return nil, fmt.Errorf(`failed to download database (source URL "%s"): %w`, redacted, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		// Try to read first N bytes of body to get a better error message.
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, bodyPreviewBytes))

		return nil, fmt.Errorf(`failed to download database (source URL "%s"): %w`, redacted, &HttpStatusError{
			StatusCode:  resp.StatusCode,
			BodyPreview: string(bodyBytes),
		})
	}

	data, err := s.readAllLimited(resp.Body)
	if err != nil {
		return nil, fmt.Errorf(`error while reading database (source URL "%s"): %w`, redacted, err)
	}

	s.logger.Debug("finished download of database",
		"service", "domaindb.DomainDb",
		"source_url", redacted,
		"bytes", len(data),
	)

	return data, nil
}

// getFromFunc reads the database from the source's Get function.
func (s *DomainDb) getFromFunc(db *database) ([]byte, error) {
	s.logger.Debug("starting download of database with source Get function",
		"service", "domaindb.DomainDb",
		"database_name", db.name,
	)

	reader, err := db.src.Get()
	if err != nil {
		return nil, fmt.Errorf("failed to get database (source Get function): %w", err)
	}
	if reader == nil {
		return nil, errors.New("failed to get database (source Get function): returned reader was nil")
	}

	data, err := s.readAllLimited(reader)
	_ = reader.Close()
	if err != nil {
		return nil, fmt.Errorf("failed to read database (source Get function): %w", err)
	}

	return data, nil
}

// fetchSource runs fetch, consulting the retry policy after each failure, until it succeeds or the policy gives up.
// srcUrl is the URL being fetched, or nil for a Get function.
// If the policy gives up, returns the last error and the action the policy chose (RetryActionSkip or RetryActionFail).
// If the instance is closed, returns ErrDbClosed with RetryActionFail.
func (s *DomainDb) fetchSource(db *database, srcUrl *url.URL, fetch func() error) (RetryAction, error) {
	source := "Get function"
	if srcUrl != nil {
		source = redactUrl(srcUrl)
	}

	for attempt := 1; ; attempt++ {
		if s.closed.Load() {
			return RetryActionFail, ErrDbClosed
		}

		err := fetch()
		if err == nil {
			return RetryActionSkip, nil
		}
		if s.closed.Load() {
			return RetryActionFail, ErrDbClosed
		}

		decision := s.retryPolicy(SourceFailure{
			DbName:  db.name,
			Url:     srcUrl,
			Attempt: attempt,
			Err:     err,
		})

		switch decision.Action {
		case RetryActionRetry:
			delay := max(decision.Delay, 0)
			s.logger.Warn("failed to download database source, retrying",
				"service", "domaindb.DomainDb",
				"database_name", db.name,
				"source", source,
				"attempt", attempt,
				"retry_in", delay.String(),
				"error", err,
			)
			if !s.sleep(delay) {
				return RetryActionFail, ErrDbClosed
			}
		case RetryActionFail:
			s.logger.Error("failed to download database source, failing the database update",
				"service", "domaindb.DomainDb",
				"database_name", db.name,
				"source", source,
				"attempt", attempt,
				"error", err,
			)
			return RetryActionFail, err
		default:
			s.logger.Error("failed to download database source, skipping it",
				"service", "domaindb.DomainDb",
				"database_name", db.name,
				"source", source,
				"attempt", attempt,
				"error", err,
			)
			return RetryActionSkip, err
		}
	}
}

// downloadAndLoad downloads the database from its source and loads it into memory.
// Returns the raw data that was loaded.
// The in-memory database is only replaced if the downloaded data is complete and valid.
func (s *DomainDb) downloadAndLoad(db *database) ([]byte, error) {
	src := db.src

	if src.Get != nil {
		// The Get function is the only source, so skipping it fails the update.
		var data []byte
		var domains map[string]struct{}
		action, err := s.fetchSource(db, nil, func() error {
			var err error
			if data, err = s.getFromFunc(db); err != nil {
				return err
			}
			domains, err = s.parseDomainList(db.name, bytes.NewReader(data))
			return err
		})
		if err != nil {
			if action == RetryActionFail && !errors.Is(err, ErrDbClosed) {
				return nil, fmt.Errorf("%w: %w", ErrFailedByRetryPolicy, err)
			}
			return nil, err
		}

		if err = s.install(db, domains); err != nil {
			return nil, err
		}
		return data, nil
	}

	if len(src.Urls) == 0 {
		return nil, ErrDataSourceNoSource
	}

	// Merge all URLs that yield a valid list.
	// Each list is validated on its own, so one broken URL cannot fail the whole database.
	var failures []error
	var merged bytes.Buffer
	var domains map[string]struct{}
	for _, srcUrl := range src.Urls {
		var data []byte
		var urlDomains map[string]struct{}
		action, err := s.fetchSource(db, srcUrl, func() error {
			var err error
			if data, err = s.fetchUrl(srcUrl); err != nil {
				return err
			}
			if urlDomains, err = s.parseDomainList(db.name, bytes.NewReader(data)); err != nil {
				return fmt.Errorf(`downloaded list was invalid (source URL "%s"): %w`, redactUrl(srcUrl), err)
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, ErrDbClosed) && s.closed.Load() {
				return nil, ErrDbClosed
			}
			failures = append(failures, err)
			if action == RetryActionFail {
				return nil, errors.Join(append(failures, ErrFailedByRetryPolicy)...)
			}
			continue
		}

		if int64(merged.Len())+int64(len(data))+1 > s.maxDbSize {
			return nil, fmt.Errorf("%w (limit is %d bytes)", ErrDatabaseTooLarge, s.maxDbSize)
		}

		merged.Write(data)
		// Write a newline to ensure the next URL body starts on a new line.
		merged.WriteByte('\n')

		if domains == nil {
			domains = urlDomains
		} else {
			maps.Copy(domains, urlDomains)
		}
	}

	if domains == nil {
		return nil, errors.Join(append(failures, ErrAllUrlsFailed)...)
	}

	if err := s.install(db, domains); err != nil {
		return nil, err
	}

	return merged.Bytes(), nil
}

// refresh downloads the database, loads it into memory, writes it to the cache and saves a checkpoint for it.
//
// If the download fails or yields invalid data, the in-memory database and the cache are left untouched.
// If writing to the cache fails, the in-memory database is still updated, but an error is returned and no checkpoint is saved.
func (s *DomainDb) refresh(db *database) error {
	if s.closed.Load() {
		return ErrDbClosed
	}

	db.updateMu.Lock()
	defer db.updateMu.Unlock()

	s.logger.Debug("downloading and loading database",
		"service", "domaindb.DomainDb",
		"database_name", db.name,
	)

	data, err := s.downloadAndLoad(db)
	if err != nil {
		if s.closed.Load() {
			return ErrDbClosed
		}
		return err
	}

	if s.closed.Load() {
		return ErrDbClosed
	}

	err = s.storage.WriteDatabase(db.name, io.NopCloser(bytes.NewReader(data)))
	if err != nil {
		return fmt.Errorf(`database with name "%s" was loaded into memory, but writing it to the cache failed: %w`, db.name, err)
	}

	s.recordUpdate(db.name, time.Now())

	return nil
}

// parseDomainList reads and normalizes all domain names from the reader.
// The entire input is always read; an error is returned if the input cannot be read completely,
// contains no domain names, or contains more malformed lines than valid ones.
func (s *DomainDb) parseDomainList(name string, reader io.Reader) (map[string]struct{}, error) {
	domains := make(map[string]struct{})
	failures := make([]error, 0, maxReportedParseFailures)
	failureCount := 0
	goodLines := 0

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()

		// Strip comments and surrounding whitespace, and skip empty lines.
		// '#' cannot appear in a valid domain name, so anything after it is a comment.
		if idx := strings.IndexByte(line, '#'); idx != -1 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		normalized, err := s.normalizer.NormalizeDomain(line)
		if err != nil {
			failureCount++
			if len(failures) < maxReportedParseFailures {
				failures = append(failures, fmt.Errorf(`failed to normalize domain name "%s": %w`, line, err))
				s.logger.Error("failed to normalize domain name in database",
					"service", "domaindb.DomainDb",
					"database_name", name,
					"domain_name", line,
					"error", err,
				)
			}
			continue
		}

		domains[normalized] = struct{}{}
		goodLines++
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf(`failed to read domain names for database "%s": %w`, name, err)
	}

	if failureCount > goodLines {
		return nil, fmt.Errorf(`encountered %d parse failures while loading domain names for database "%s", but only %d lines were successfully parsed. file is probably malformed; expected newline-separated list of domain names. this error wraps the first %d parse errors. error(s): %w`, failureCount, name, goodLines, len(failures), errors.Join(failures...))
	}
	if goodLines == 0 {
		return nil, fmt.Errorf(`failed to load domain names for database "%s": %w`, name, ErrNoDomains)
	}

	if failureCount > 0 {
		s.logger.Warn("some lines in database could not be parsed and were skipped",
			"service", "domaindb.DomainDb",
			"database_name", name,
			"skipped_lines", failureCount,
			"loaded_lines", goodLines,
		)
	}

	return domains, nil
}

// load validates the raw database data and, if valid, replaces the in-memory database with it.
func (s *DomainDb) load(db *database, data []byte) error {
	domains, err := s.parseDomainList(db.name, bytes.NewReader(data))
	if err != nil {
		return err
	}
	return s.install(db, domains)
}

// install replaces the in-memory database with the specified domains.
func (s *DomainDb) install(db *database, domains map[string]struct{}) error {
	if s.closed.Load() {
		return ErrDbClosed
	}

	snapshot := &loadedDb{domains: domains}
	db.loaded.Store(snapshot)

	// Close may have run between the check above and the store, in which case it may already have cleared the
	// database, and this store would have put it back. Close sets the closed flag before clearing databases,
	// so checking the flag again after storing guarantees that one of the two clears it.
	// CompareAndSwap avoids clearing a newer snapshot stored by someone else.
	if s.closed.Load() {
		db.loaded.CompareAndSwap(snapshot, nil)
		return ErrDbClosed
	}

	return nil
}

// DownloadAndLoadDatabase downloads the database with the specified name, loads it into memory and writes it to the cache.
// You most likely do not need to call this function, as loading databases is handled automatically by the DomainDb instance.
//
// If the download fails or yields invalid data, the existing database is left untouched.
// If the database does not exist, returns a NoSuchDatabaseError.
// If the DomainDb instance has been closed, returns ErrDbClosed.
func (s *DomainDb) DownloadAndLoadDatabase(name string) error {
	db, has := s.dbs[name]
	if !has {
		return NewNoSuchDatabaseError(name)
	}

	return s.refresh(db)
}

// Close stops all background updates and frees all databases.
// Running downloads are canceled, and Close waits for background work to stop before returning.
// The DomainDb instance is no longer usable after closure; its methods return ErrDbClosed.
//
// It is safe to call Close multiple times and from multiple goroutines.
// Subsequent calls do nothing.
// The returned error is currently always nil.
func (s *DomainDb) Close() error {
	s.closeOnce.Do(func() {
		s.aliveMu.Lock()
		s.closed.Store(true)
		s.aliveMu.Unlock()

		s.cancel()
		s.wg.Wait()

		// Clear the databases so they can be freed by the GC once in-progress lookups are done.
		for _, db := range s.dbs {
			db.loaded.Store(nil)
		}
		runtime.GC()
	})

	return nil
}

// LookupOptions are options for DomainDb.DoesDbHaveDomainWithOptions.
// The zero value performs an exact match, the same as DomainDb.DoesDbHaveDomain.
type LookupOptions struct {
	// If true, a domain also matches if any of its parent domains are in the database, so a database entry covers the
	// domain itself and all of its subdomains, like HSTS's includeSubDomains directive.
	// For example, if the database contains "example.com", then "mail.example.com" and "a.b.example.com" match.
	//
	// Domains are compared label by label, so "notexample.com" does not match "example.com".
	// Every parent domain is checked, up to and including the top-level domain, so a database entry such as
	// "com" or a public suffix such as "co.uk" or "github.io" matches all domains beneath it.
	//
	// If false, only exact matches are found: a database containing "example.com" does not contain "sub.example.com".
	IncludeSubdomains bool

	// Limits how far up the domain hierarchy IncludeSubdomains searches.
	// The search stops when it reaches a parent domain with StopAtLabelCount labels, without checking that parent
	// domain or any above it. For example:
	//   - 1 stops at the top-level domain, so "mail.example.com" checks "mail.example.com" and "example.com",
	//     but not "com". This is useful when every looked-up domain is a real Internet domain, since a database entry
	//     for a top-level domain is then never useful.
	//   - 2 also stops at domains with two labels, so "mail.example.com" checks only "mail.example.com".
	//
	// The looked-up domain itself is always checked, regardless of how many labels it has.
	// If zero or negative, every parent domain is checked, up to and including the top-level domain.
	// Ignored if IncludeSubdomains is false.
	StopAtLabelCount int
}

// DoesDbHaveDomain returns whether a domain was found in the specified domain database.
// The domain is normalized before checking; see normalize.DomainNormalizer.
// Only exact matches are found: a database containing "example.com" does not contain "sub.example.com".
// To also match subdomains of domains in the database, use DoesDbHaveDomainWithOptions with
// LookupOptions.IncludeSubdomains.
// If the domain cannot be normalized, returns the normalization error.
// If the database does not exist, returns a NoSuchDatabaseError.
// If the database has not been initialized, returns a NotInitializedError.
// If the DomainDb instance has been closed, returns ErrDbClosed.
func (s *DomainDb) DoesDbHaveDomain(dbName string, domain string) (bool, error) {
	return s.DoesDbHaveDomainWithOptions(dbName, domain, LookupOptions{})
}

// DoesDbHaveDomainWithOptions returns whether a domain was found in the specified domain database,
// using the specified lookup options. See LookupOptions.
// The domain is normalized before checking; see normalize.DomainNormalizer.
// If the domain cannot be normalized, returns the normalization error.
// If the database does not exist, returns a NoSuchDatabaseError.
// If the database has not been initialized, returns a NotInitializedError.
// If the DomainDb instance has been closed, returns ErrDbClosed.
func (s *DomainDb) DoesDbHaveDomainWithOptions(dbName string, domain string, options LookupOptions) (bool, error) {
	if s.closed.Load() {
		return false, ErrDbClosed
	}

	db, has := s.dbs[dbName]
	if !has {
		return false, NewNoSuchDatabaseError(dbName)
	}

	normalized, err := s.normalizer.NormalizeDomain(domain)
	if err != nil {
		return false, err
	}

	loaded := db.loaded.Load()
	if loaded == nil {
		if s.closed.Load() {
			return false, ErrDbClosed
		}
		return false, NewNotInitializedError(dbName)
	}

	if !options.IncludeSubdomains {
		_, has = loaded.domains[normalized]
		return has, nil
	}

	// Check the domain, then each parent domain by removing one label at a time from the left, until reaching the
	// top-level domain or a parent domain with StopAtLabelCount labels.
	labels := strings.Count(normalized, ".") + 1
	for candidate := normalized; ; {
		if _, has = loaded.domains[candidate]; has {
			return true, nil
		}

		_, parent, found := strings.Cut(candidate, ".")
		if !found {
			return false, nil
		}
		labels--
		if labels <= options.StopAtLabelCount {
			return false, nil
		}
		candidate = parent
	}
}
