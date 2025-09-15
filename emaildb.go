package domaindb

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime"
	"syscall"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
)

const defaultHttpClientTimeout = 10 * time.Second

type dbUpdate struct {
	Ts   time.Time
	Name string
}
type dbSrcMap struct {
	Has             bool
	Src             *DataSource
	Mu              *xsync.RBMutex
	Domains         map[string]struct{}
	LastUpdatedUnix int64
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
	storage    StorageDriver
	disableDl  bool
	httpClient *http.Client
	logger     *slog.Logger
	updates    chan dbUpdate

	dbs map[string]*dbSrcMap

	isRunning bool
}

// DataSource stores source information for domain data.
// It includes the URL the data can be fetched from and the time to wait between updating the data from the URL.
type DataSource struct {
	// Urls are the URLs where the domain data is located.
	// Either Get or Urls must be provided; Get takes precedence over Urls.
	// Any URLs that cannot be fetched will result in an error log and be skipped.
	Urls []*url.URL

	// Get is a function to get the domain data.
	// Either Get or Url must be provided; Get takes precedence over Url.
	Get func() (io.ReadCloser, error)

	// RefreshInterval is the interval between updating the data from the source.
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
	// If nil, uses a default HTTP client with a 10-second timeout.
	HttpClient *http.Client

	// If true, disables downloading from sources and only uses cached database files.
	//
	// Important: You must still provide sources for the databases you want to use, regardless of whether download is disabled.
	DisableDownload bool

	// If true, the DomainDb instance will be created without waiting for databases to be loaded.
	// The databases will be loaded asynchronously in the background.
	// This can be useful if you're developing and don't want database loading to block startup.
	// It is NOT recommended for production.
	//
	// Important: Any methods on DomainDb that require databases to be initialized will fail until the databases have loaded.
	LoadDatabasesInBackground bool

	// A mapping of database names to their underlying sources.
	// Each source's URL must point to a file containing a newline-separated list of domain names.
	// Empty lines are ignored.
	Sources map[string]*DataSource
}

// NewDomainDb creates a new DomainDb instance.
// Blocks until the databases are loaded, unless Options.LoadDatabasesInBackground is true.
// There should only be one instance of DomainDb per storage driver or storage location, and ideally only one per process.
// If error is nil, the returned DomainDb instance will never be nil.
func NewDomainDb(options Options) (*DomainDb, error) {
	var httpClient *http.Client
	if options.HttpClient == nil {
		httpClient = &http.Client{
			Timeout: defaultHttpClientTimeout,
		}
	} else {
		httpClient = options.HttpClient
	}

	var logger *slog.Logger
	if options.Logger == nil {
		logger = slog.Default()
	} else {
		logger = options.Logger
	}

	// Create source maps.
	dbs := make(map[string]*dbSrcMap)
	for name, src := range options.Sources {
		dbs[name] = &dbSrcMap{
			Has:             false,
			Src:             src,
			Mu:              xsync.NewRBMutex(),
			Domains:         make(map[string]struct{}),
			LastUpdatedUnix: 0,
		}
	}

	s := &DomainDb{
		storage:    options.StorageDriver,
		disableDl:  options.DisableDownload,
		httpClient: httpClient,
		logger:     logger,
		updates:    make(chan dbUpdate, 8),

		dbs: dbs,

		isRunning: true,
	}

	ctx := context.Background()

	s.logger.Log(ctx, slog.LevelInfo, "initializing DomainDb",
		"service", "domaindb.DomainDb",
	)

	alreadyHadCheckpoints := false
	checkpoints, err := s.storage.ReadCheckpoints()
	if err == nil {
		alreadyHadCheckpoints = true
	} else {
		if errors.Is(err, syscall.ENOENT) {
			checkpoints = &AllCheckpoints{
				Checkpoints: make(map[string]Checkpoint),
			}
		} else {
			return nil, fmt.Errorf("failed to load checkpoints during initialization: %w", err)
		}
	}

	setup := func() error {
		var err error

		toClose := make([]io.Closer, 0, len(dbs))
		defer func() {
			for _, c := range toClose {
				_ = c.Close()
			}
		}()

		for name, data := range dbs {
			// Read databases.
			if !s.isRunning {
				return nil
			}

			var reader io.ReadCloser
			if alreadyHadCheckpoints {
				s.logger.Log(ctx, slog.LevelDebug, "reading database from cache",
					"service", "domaindb.DomainDb",
					"database_name", name,
				)

				reader, err = s.storage.ReadDatabase(name)
				if err != nil && !errors.Is(err, syscall.ENOENT) {
					return fmt.Errorf(`failed to read database with name "%s" during initialization: %w`, name, err)
				}
				toClose = append(toClose, reader)
			}
			if reader == nil {
				// No cached database.
				if s.disableDl {
					return fmt.Errorf(`cannot download database with name "%s" during initialization: %w`, name, ErrNoCacheAndNoDownload)
				}

				// Try downloading it.
				err = s.DownloadAndLoadDatabase(name)
				if err != nil {
					return fmt.Errorf(`failed to download database with name "%s" during initialization: %w`, name, err)
				}

				data.LastUpdatedUnix = time.Now().Unix()
			} else {
				err = s.loadDomainsFromReader(reader, name)
				if err != nil {
					return fmt.Errorf(`failed to load database with name "%s" during initialization: %w`, name, err)
				}
			}
		}

		if !s.isRunning {
			return nil
		}

		// Populate checkpoints as needed.
		for name, data := range dbs {
			var chkPnt Checkpoint
			var has bool
			chkPnt, has = checkpoints.Checkpoints[name]
			if !has {
				chkPnt = Checkpoint{
					LastUpdatedUnix: 0,
				}
			}

			if data.LastUpdatedUnix != 0 {
				chkPnt.LastUpdatedUnix = data.LastUpdatedUnix
			}

			checkpoints.Checkpoints[name] = chkPnt
		}

		// Save checkpoints.
		// This is necessary because there could have been database downloads, or checkpoints have never been saved.
		err = s.storage.WriteCheckpoints(checkpoints)
		if err != nil {
			return fmt.Errorf("failed to save checkpoints after initial load: %w", err)
		}

		if !s.isRunning {
			return nil
		}

		// In the background, save checkpoint updates.
		go func() {
			for update := range s.updates {
				var chkPnt Checkpoint
				var has bool
				chkPnt, has = checkpoints.Checkpoints[update.Name]
				if has {
					chkPnt.LastUpdatedUnix = update.Ts.Unix()
				} else {
					chkPnt = Checkpoint{
						LastUpdatedUnix: update.Ts.Unix(),
					}
				}
				checkpoints.Checkpoints[update.Name] = chkPnt

				err := s.storage.WriteCheckpoints(checkpoints)
				if err != nil {
					s.logger.Log(ctx, slog.LevelError, "failed to save checkpoints after receiving checkpoint update",
						"service", "domaindb.DomainDb",
						"database_name", update.Name,
						"error", err,
					)
				}
			}
		}()

		if !s.disableDl {
			// Start updaters for enabled databases.
			for name, data := range dbs {
				chkPnt := checkpoints.Checkpoints[name]
				go s.runUpdater(
					name,
					time.Unix(chkPnt.LastUpdatedUnix, 0),
					data.Src.RefreshInterval,
				)
			}
		}

		s.logger.Log(ctx, slog.LevelInfo, "finished initializing DomainDb",
			"service", "domaindb.DomainDb",
		)

		return nil
	}

	if options.LoadDatabasesInBackground {
		s.logger.Log(ctx, slog.LevelDebug, "loading databases in the background, as requested by DomainDb options",
			"service", "domaindb.DomainDb",
		)
		go func() {
			if err := setup(); err != nil {
				s.logger.Log(ctx, slog.LevelError, "failed to initialize DomainDb in the background",
					"service", "domaindb.DomainDb",
					"error", err,
				)
			}
		}()
	} else {
		if err := setup(); err != nil {
			return nil, nil
		}
	}

	return s, nil
}

// runUpdater runs the updater for the specified DB type.
func (s *DomainDb) runUpdater(name string, lastUpdate time.Time, updateInterval time.Duration) {
	if !s.isRunning {
		return
	}

	ctx := context.Background()

	s.logger.Log(ctx, slog.LevelDebug, "running updater for database",
		"service", "domaindb.DomainDb",
		"database_name", name,
	)

	update := func() error {
		if err := s.DownloadAndLoadDatabase(name); err != nil {
			return err
		}

		if !s.isRunning {
			return ErrDbClosed
		}
		s.updates <- dbUpdate{
			Ts:   time.Now(),
			Name: name,
		}

		// Databases are big, and we want to limit the amount of garbage in memory.
		// Run the GC manually.
		runtime.GC()

		return nil
	}

	firstUpdateTs := lastUpdate.Add(updateInterval)
	firstTimeout := time.NewTimer(firstUpdateTs.Sub(time.Now()))

	// Wait for next update time.
	<-firstTimeout.C
	if !s.isRunning {
		return
	}

	err := update()
	if err != nil {
		s.logger.Log(ctx, slog.LevelError, "failed to do first scheduled update of database",
			"service", "domaindb.DomainDb",
			"database_name", name,
		)
	}

	ticker := time.NewTicker(updateInterval)
	for s.isRunning {
		<-ticker.C
		if !s.isRunning {
			return
		}

		err = update()
		if err != nil {
			s.logger.Log(ctx, slog.LevelError, "failed to do scheduled update of database",
				"service", "domaindb.DomainDb",
				"database_name", name,
			)
		}
	}
}

// openDataSource opens a data source.
// The caller must close the returned reader.
// If the data source has no sources, ErrDataSourceNoSource is returned.
func (s *DomainDb) openDataSource(src *DataSource) (io.ReadCloser, error) {
	ctx := context.Background()

	var reader io.ReadCloser

	if src.Get != nil {
		s.logger.Log(ctx, slog.LevelDebug, "starting download of database with source Get function",
			"service", "domaindb.DomainDb",
		)

		var err error
		reader, err = src.Get()
		if err != nil {
			return nil, fmt.Errorf(`failed to get database (source Get function): %w`, err)
		}

		s.logger.Log(ctx, slog.LevelDebug, "finished download of database with source Get function",
			"service", "domaindb.DomainDb",
		)
	} else if len(src.Urls) > 0 {
		pipeReader, pipeWriter := io.Pipe()

		go func() {
			var err error
			var resp *http.Response

			failures := make([]error, 0, len(src.Urls))

			for _, srcUrl := range src.Urls {
				func() {
					s.logger.Log(ctx, slog.LevelDebug, "starting download of database",
						"service", "domaindb.DomainDb",
						"source_url", srcUrl,
					)
					req := &http.Request{
						Method: http.MethodGet,
						URL:    srcUrl,
					}
					resp, err = s.httpClient.Do(req)
					if err != nil {
						failures = append(failures, fmt.Errorf(`failed to download database (source URL "%s"): %w`, srcUrl, err))
						s.logger.Log(ctx, slog.LevelError, "failed to download database",
							"service", "domaindb.DomainDb",
							"source_url", srcUrl,
							"error", err,
						)
						return
					}

					defer func() {
						_ = resp.Body.Close()
					}()

					if resp.StatusCode != http.StatusOK {
						const bodyPreviewBytes = 1024
						// Try to read first N bytes of body to get a better error message.
						bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, bodyPreviewBytes))

						bodyStr := string(bodyBytes)
						failures = append(failures, fmt.Errorf(`failed to download database (source URL "%s") because status code was %d (expected 200): %s`, srcUrl, resp.StatusCode, bodyStr))
						s.logger.Log(ctx, slog.LevelError, "failed to download database because status code was not 200",
							"service", "domaindb.DomainDb",
							"source_url", srcUrl,
							"status_code", resp.StatusCode,
							"body", bodyStr,
						)
						return
					}

					bytesWritten, err := io.Copy(pipeWriter, resp.Body)
					if err != nil {
						failures = append(failures, fmt.Errorf(`failed to download database (source URL "%s", bytes written: %d): %w`, srcUrl, bytesWritten, err))
						s.logger.Log(ctx, slog.LevelError, "failed to download database",
							"service", "domaindb.DomainDb",
							"source_url", srcUrl,
							"bytes_written", bytesWritten,
							"error", err,
						)
						return
					}
				}()

				// Write a newline to ensure the next URL body is read on a new line.
				_, _ = pipeWriter.Write([]byte("\n"))
			}

			if len(failures) == len(src.Urls) {
				// All URLs failed; close the pipe writer with ErrAllUrlsFailed and the errors.
				failures = append(failures, ErrAllUrlsFailed)
				_ = pipeWriter.CloseWithError(errors.Join(failures...))
			} else {
				_ = pipeWriter.Close()
			}
		}()

		reader = pipeReader
	} else {
		return nil, ErrDataSourceNoSource
	}

	return reader, nil
}

// loadDomainsFromReader reads all domain names from the reader and loads them to the database with the specified name.
// Domain names with Unicode and non-uppercase are normalized.
// Does not close the reader.
// Assumes the database name exists, panics if not; checking the database name is the responsibility of the caller.
func (s *DomainDb) loadDomainsFromReader(reader io.Reader, name string) error {
	ctx := context.Background()

	data := s.dbs[name]

	domains := make(map[string]struct{})

	const maxFailures = 10
	failures := make([]error, 0, maxFailures)

	goodLines := 0

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() && len(failures) < maxFailures {
		line := scanner.Text()

		// Skip empty lines and comments.
		if line == "" || line[0] == '#' {
			continue
		}

		// Normalize the domain before putting it into the map.
		normalized, err := NormalizeDomainName(line)
		if err != nil {
			s.logger.Log(ctx, slog.LevelError, "failed to normalize domain name",
				"service", "domaindb.DomainDb",
				"domain_name", line,
				"error", err,
			)
			failures = append(failures, fmt.Errorf(`failed to normalize domain name "%s": %w`, line, err))
			continue
		}

		domains[normalized] = struct{}{}

		goodLines++
	}

	if len(failures) > goodLines {
		return fmt.Errorf(`encountered %d parse failures while loading datacenter ranges, but only %d lines were successfully parsed. file is probably malformed; expected newline-separated list of domain names. this error wraps the encountered parse errors: %w`,
			len(failures),
			goodLines,
			errors.Join(failures...),
		)
	}

	data.Mu.Lock()
	data.Domains = domains
	data.Mu.Unlock()

	return nil
}

// DownloadAndLoadDatabase downloads the database with the specified name and loads it into memory.
// You most likely do not need to call this function, as loading databases is handled automatically by the DomainDb instance.
func (s *DomainDb) DownloadAndLoadDatabase(name string) error {
	ctx := context.Background()

	data, has := s.dbs[name]
	if !has {
		return NewNoSuchDatabaseError(name)
	}

	s.logger.Log(ctx, slog.LevelDebug, "downloading and loading database",
		"service", "domaindb.DomainDb",
		"database_name", name,
	)

	reader, err := s.openDataSource(data.Src)
	defer func() {
		if reader != nil {
			_ = reader.Close()
		}
	}()
	if err != nil {
		return fmt.Errorf(`failed to read from source of data with name "%s": %w`, name, err)
	}

	pipeReader, pipeWriter := io.Pipe()

	writeErrChan := make(chan error, 1)
	go func() {
		writeErrChan <- s.storage.WriteDatabase(name, pipeReader)
	}()

	parseReader := noOpReadCloser{io.TeeReader(reader, pipeWriter)}

	err = s.loadDomainsFromReader(parseReader, name)
	if err != nil {
		wrapped := fmt.Errorf(`failed to parse database with name "%s": %w`, name, err)
		_ = pipeWriter.CloseWithError(wrapped)
		return wrapped
	}

	_ = pipeWriter.Close()

	if err := <-writeErrChan; err != nil {
		return fmt.Errorf(`failed to write database with name "%s": %w`, name, err)
	}

	return nil
}

func (s *DomainDb) Close() error {
	close(s.updates)

	s.isRunning = false

	// Dereference databases to allow them to be garbage collected.
	s.dbs = nil

	// Force garbage collection.
	runtime.GC()

	return nil
}

func (s *DomainDb) DoesDbHaveDomain(dbName string, domain string) (bool, error) {
	data, has := s.dbs[dbName]
	if !has {
		return false, NewNoSuchDatabaseError(dbName)
	}

	normalized, err := NormalizeDomainName(domain)
	if err != nil {
		return false, err
	}

	tok := data.Mu.RLock()
	defer data.Mu.RUnlock(tok)

	_, has = data.Domains[normalized]
	return has, nil
}
