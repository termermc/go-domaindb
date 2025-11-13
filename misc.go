package domaindb

import (
	"io"

	"github.com/termermc/go-domaindb/normalize"
)

type noOpReadCloser struct {
	io.Reader
}

func (n noOpReadCloser) Close() error {
	return nil
}

// NormalizeDomainName normalizes the provided domain name by making it lowercase and converting any non-ASCII characters to ASCII punycode.
//
// Deprecated: Use normalize.DomainNormalizer instead.
func NormalizeDomainName(domain string) (string, error) {
	n := normalize.NewDomainNormalizer()
	return n.NormalizeDomain(domain)
}
