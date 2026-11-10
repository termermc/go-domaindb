package domaindb

// Integration tests that download real domain lists.
// They are skipped with -short, and skipped if the lists cannot be reached.

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/termermc/go-domaindb/normalize"
)

var integrationListUrls = map[string]string{
	"disposable-email-domains": "https://raw.githubusercontent.com/disposable-email-domains/disposable-email-domains/refs/heads/main/disposable_email_blocklist.conf",
	"disposable":               "https://raw.githubusercontent.com/disposable/disposable-email-domains/master/domains.txt",
}

// fetchForIntegration downloads the URL, skipping the test if it cannot be reached.
func fetchForIntegration(t *testing.T, rawUrl string) []byte {
	t.Helper()

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(rawUrl)
	if err != nil {
		t.Skipf("cannot reach %s: %v", rawUrl, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		t.Skipf("cannot download %s: status %d", rawUrl, resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Skipf("cannot download %s: %v", rawUrl, err)
	}

	return data
}

// referenceDomains parses a domain list independently of the DomainDb parser.
func referenceDomains(t *testing.T, data []byte) []string {
	t.Helper()

	n := normalize.NewDomainNormalizer()
	var domains []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		domain, err := n.NormalizeDomain(line)
		if err != nil {
			t.Logf("reference parser skipped line %q: %v", line, err)
			continue
		}
		domains = append(domains, domain)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reference scanner failed: %v", err)
	}

	return domains
}

func checkAgainstReference(t *testing.T, s *DomainDb, dbName string, domains []string) {
	t.Helper()

	missing := 0
	for _, domain := range domains {
		has, err := s.DoesDbHaveDomain(dbName, domain)
		if err != nil {
			t.Fatalf("DoesDbHaveDomain(%s, %s) failed: %v", dbName, domain, err)
		}
		if !has {
			missing++
			if missing <= 10 {
				t.Errorf("DoesDbHaveDomain(%s, %s) = false, want true", dbName, domain)
			}
		}
	}
	if missing > 10 {
		t.Errorf("%d more domains were missing from %s", missing-10, dbName)
	}

	for _, domain := range []string{"google.com", "example.com"} {
		has, err := s.DoesDbHaveDomain(dbName, domain)
		if err != nil {
			t.Fatalf("DoesDbHaveDomain(%s, %s) failed: %v", dbName, domain, err)
		}
		if has {
			t.Errorf("DoesDbHaveDomain(%s, %s) = true, want false", dbName, domain)
		}
	}
}

func TestIntegrationRealDomainLists(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network integration test in short mode")
	}

	// Download the lists independently to build the reference.
	all := make(map[string][]string, len(integrationListUrls))
	var merged []string
	for name, rawUrl := range integrationListUrls {
		domains := referenceDomains(t, fetchForIntegration(t, rawUrl))
		if len(domains) == 0 {
			t.Fatalf("list %s has no domains", name)
		}
		t.Logf("list %s has %d domains", name, len(domains))

		all[name] = domains
		merged = append(merged, domains...)
	}

	sources := make(map[string]*DataSource, len(integrationListUrls)+1)
	mergedSource := &DataSource{RefreshInterval: time.Hour}
	for name, rawUrl := range integrationListUrls {
		parsed, err := url.Parse(rawUrl)
		if err != nil {
			t.Fatalf("failed to parse URL: %v", err)
		}
		sources[name] = &DataSource{RefreshInterval: time.Hour, Urls: []*url.URL{parsed}}
		mergedSource.Urls = append(mergedSource.Urls, parsed)
	}
	sources["merged"] = mergedSource

	drv, err := NewFsStorageDriver(t.TempDir())
	if err != nil {
		t.Fatalf("NewFsStorageDriver failed: %v", err)
	}

	options := Options{
		StorageDriver: drv,
		Logger:        testLogger(),
		Sources:       sources,
	}

	s, err := NewDomainDb(options)
	if err != nil {
		t.Fatalf("NewDomainDb failed: %v", err)
	}
	for name, domains := range all {
		checkAgainstReference(t, s, name, domains)
	}
	checkAgainstReference(t, s, "merged", merged)
	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// The cache written by the first instance must load identically without downloading.
	options.DisableDownload = true
	s, err = NewDomainDb(options)
	if err != nil {
		t.Fatalf("NewDomainDb from cache failed: %v", err)
	}
	defer func() {
		_ = s.Close()
	}()

	for name, domains := range all {
		checkAgainstReference(t, s, name, domains)
	}
	checkAgainstReference(t, s, "merged", merged)
}
