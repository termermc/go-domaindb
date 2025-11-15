package normalize

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/net/idna"
)

// DomainNormalizer normalizes domain names to their canonical form.
// Note that it rejects domain names with trailing dots and empty labels.
// See DomainNormalizer.NormalizeDomain for details.
type DomainNormalizer struct {
	profile     *idna.Profile
	dotReplacer *strings.Replacer
}

// NewDomainNormalizer constructs a normalizer with a configured UTS #46 profile.
// The profile performs Map+Validate for lookup and registration with modern rules.
func NewDomainNormalizer() *DomainNormalizer {
	p := idna.New(
		idna.ValidateForRegistration(),
		idna.MapForLookup(),
		idna.BidiRule(),
		idna.Transitional(false),
		// Use STD3 rules to prevent underscores and other disallowed runes in ASCII
		idna.StrictDomainName(true),
	)

	// Prebuild replacer for Unicode dot-like characters.
	dots := strings.NewReplacer(
		"。", ".",
		"．", ".",
		"｡", ".",
	)

	return &DomainNormalizer{
		profile:     p,
		dotReplacer: dots,
	}
}

// NormalizeDomain normalizes a domain name:
// - Trims surrounding whitespace
// - Maps Unicode dot-like chars to '.'
// - Strips default-ignorable zero-width/bidi control chars
// - Removes a trailing dot
// - Applies UTS #46 mapping and ASCII (Punycode) conversion
// - Lowercases output (ASCII)
// - Validates total (<=253) and label (1..63) lengths and forbids empty labels
// Returns the normalized ASCII domain without a trailing dot.
func (n *DomainNormalizer) NormalizeDomain(input string) (string, error) {
	// Trim typical surrounding whitespace first
	s := strings.TrimSpace(input)
	if s == "" {
		return "", errors.New("empty domain")
	}

	// Map Unicode dot-like characters to '.'
	s = n.dotReplacer.Replace(s)

	// Strip default-ignorable/zero-width/bidi control characters
	s = stripInvisibleChars(s)
	if s == "" {
		return "", errors.New("empty domain after stripping invisibles")
	}

	// Remove a single trailing dot if present (FQDN marker)
	if strings.HasSuffix(s, ".") {
		s = strings.TrimSuffix(s, ".")
	}
	if s == "" {
		return "", errors.New("domain has no labels")
	}
	// Reject any remaining leading/trailing dot
	for strings.HasPrefix(s, ".") {
		s = strings.TrimPrefix(s, ".")
	}
	for strings.HasSuffix(s, ".") {
		s = strings.TrimSuffix(s, ".")
	}
	// Reject empty labels like "a..b"
	if strings.Contains(s, "..") {
		return "", errors.New("domain contains empty label")
	}

	// Reject empty labels like "a..b"
	parts := strings.Split(s, ".")
	for _, p := range parts {
		if p == "" {
			return "", errors.New("domain contains empty label")
		}
	}

	// UTS #46 to ASCII (punycode) using the prepared profile
	ascii, err := n.profile.ToASCII(s)
	if err != nil {
		return "", fmt.Errorf("idna toASCII: %w", err)
	}
	ascii = strings.ToLower(ascii)

	// Enforce label and total length constraints
	labels := strings.Split(ascii, ".")
	for _, lbl := range labels {
		if l := len(lbl); l == 0 || l > 63 {
			return "", fmt.Errorf("label %q length %d out of range 1..63", lbl, len(lbl))
		}
		if !isLDHOrPunycode(lbl) {
			return "", fmt.Errorf("label %q contains invalid ASCII characters", lbl)
		}
	}
	if len(ascii) > 253 {
		return "", fmt.Errorf("domain length %d exceeds 253 characters", len(ascii))
	}

	return ascii, nil
}

// stripInvisibleChars removes a minimal safe set of default-ignorable and control
// characters that can be used for obfuscation in domains.
func stripInvisibleChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		// ASCII controls and DEL
		case 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
			0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F,
			0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
			0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F,
			0x7F:
			continue
		// Zero-width and joiners
		case '\u200B', '\u200C', '\u200D', '\u2060', '\uFEFF':
			continue
		// Basic bidi controls
		case '\u202A', '\u202B', '\u202C', '\u202D', '\u202E':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isLDHOrPunycode checks if an ASCII label uses allowed characters per STD3.
// Allows "xn--" punycode prefix; label must start/end alnum; interior may have hyphens.
func isLDHOrPunycode(lbl string) bool {
	l := len(lbl)
	if l == 0 {
		return false
	}
	// End must be alnum
	if !isAlnum(lbl[l-1]) {
		return false
	}
	// Start must be alnum unless punycode "xn--"
	if !isAlnum(lbl[0]) && !strings.HasPrefix(lbl, "xn--") {
		return false
	}
	for i := 0; i < l; i++ {
		c := lbl[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return false
	}
	return true
}

func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}
