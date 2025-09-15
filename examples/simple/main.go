package main

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"time"

	"github.com/termermc/go-domaindb"
)

// DbDisposable is the name of the domain database that stores disposable email domains.
const DbDisposable = "disposable"

// DbDisposableFalsePositive is the name of the domain database that stores email domains that are often falsely marked as disposable.
const DbDisposableFalsePositive = "disposable-false-positive"

func mustParseUrl(str string) *url.URL {
	res, err := url.Parse(str)
	if err != nil {
		panic(err)
	}
	return res
}

func main() {
	const domainDbDir = "./domaindb"
	err := os.MkdirAll(domainDbDir, 0755)
	if err != nil {
		panic(err)
	}

	// Use the built-in FsStorageDriver.
	// This stores cached databases and checkpoints in the specified directory.
	// If you want to store them somewhere else, you can implement your own StorageDriver.
	storageDriver, err := domaindb.NewFsStorageDriver(domainDbDir)
	if err != nil {
		panic(err)
	}

	// Use the built-in stdlib slog logger.
	// It logs to the console.
	logger := slog.Default()

	// Create the actual DomainDb instance.
	// You may omit any of the data sources if you do not need them.
	// Read the documentation on the DomainDb struct for more details.
	domainDb, err := domaindb.NewDomainDb(domaindb.Options{
		StorageDriver: storageDriver,
		Logger:        logger,

		// You can optionally provide your own HTTP client to use for downloading databases.
		// By default, it uses a client with a 10-second timeout.
		HttpClient: nil,

		// You can set this to true to disable downloading databases.
		// If true, it will only use cached databases.
		// Keep in mind that if there is no cached database available, it will return an error.
		DisableDownload: false,

		// You can set this to true to do most of the initialization in the background.
		// This is useful if you're developing and don't want database loading to block startup.
		// It is NOT recommended for production.
		// See documentation on this field for more details.
		LoadDatabasesInBackground: false,

		Sources: map[string]*domaindb.DataSource{
			DbDisposable: {
				RefreshInterval: 1 * time.Hour,
				Urls: []*url.URL{
					// We're using disposable-email-domains which provides a frequently-updated list of disposable email domains.
					// It is used by PyPI and presumably many other sites.
					mustParseUrl("https://raw.githubusercontent.com/disposable-email-domains/disposable-email-domains/refs/heads/main/disposable_email_blocklist.conf"),

					// Another good aggregated list of disposable email domains.
					// You can check domains against its constituent lists with this page: https://disposable.github.io/disposable-email-domains/lookup
					mustParseUrl("https://raw.githubusercontent.com/disposable/disposable-email-domains/master/domains.txt"),

					// You can provide more URLs if you have other lists you want to combine.
				},
			},

			DbDisposableFalsePositive: {
				RefreshInterval: 1 * time.Hour,
				Urls: []*url.URL{
					// Some whitelists from various sources.
					mustParseUrl("https://raw.githubusercontent.com/disposable-email-domains/disposable-email-domains/refs/heads/main/allowlist.conf"),
					mustParseUrl("https://raw.githubusercontent.com/disposable/disposable/refs/heads/master/whitelist.txt"),
				},
			},
		},
	})
	if err != nil {
		panic(err)
	}

	// List of domains to check.
	domains := []string{
		// Some random disposable email domains.
		"10minutemail.com",
		"5minutemail.net",
		"obirah.com",

		// Disposable email domain with Unicode.
		// Supports both Unicode and Punycode forms; they are all normalized to Punycode internally.
		"dé.net",
		"xn--53h1310o.ws",

		// Some domains that are obviously not disposable.
		"google.com",
		"encoding.com",
		"gmail.com",

		// Some domains that are not disposable but are often falsely marked as disposable.
		"protonmail.com",
		"proton.me",
		"cock.li",
		"riseup.net",
	}

	for _, domain := range domains {
		looksDisposable, err := domainDb.DoesDbHaveDomain(DbDisposable, domain)
		if err != nil {
			panic(err)
		}

		looksLikeFalsePositive, err := domainDb.DoesDbHaveDomain(DbDisposableFalsePositive, domain)
		if err != nil {
			panic(err)
		}

		fmt.Printf("%s looks disposable: %t, looks like false positive: %t\n", domain, looksDisposable, looksLikeFalsePositive)
	}

	// In this example, the program terminates.
	// In real-world usage, the program will continue to run and databases will be updated in the background.
	// Add `select {}` to the end of the program to leave it open for a few hours to see log messages caused by the automatic updates.

	//select {}

	// Closing the database frees all databases.
	// The DomainDb instance is no longer usable after closure.
	err = domainDb.Close()
	if err != nil {
		panic(err)
	}
}
