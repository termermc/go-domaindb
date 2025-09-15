package domaindb

import (
	"io"
	"strings"

	"golang.org/x/net/idna"
)

type noOpReadCloser struct {
	io.Reader
}

func (n noOpReadCloser) Close() error {
	return nil
}

// NormalizeDomainName normalizes the provided domain name by making it lowercase and converting any non-ASCII characters to ASCII punycode.
func NormalizeDomainName(domain string) (string, error) {
	return idna.ToASCII(strings.ToLower(domain))
}
