package normalize

import (
	"testing"
)

func newN() *DomainNormalizer {
	return NewDomainNormalizer()
}

func TestNormalizeDomain_BasicASCII(t *testing.T) {
	n := newN()

	got, err := n.NormalizeDomain("Example.COM")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if want := "example.com"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeDomain_TrailingDotRemoved(t *testing.T) {
	n := newN()

	got, err := n.NormalizeDomain("example.com.")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if want := "example.com"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeDomain_UnicodeDots(t *testing.T) {
	n := newN()

	cases := []string{
		"example。com", // U+3002
		"example．com", // U+FF0E
		"example｡com", // U+FF61
	}
	for _, in := range cases {
		got, err := n.NormalizeDomain(in)
		if err != nil {
			t.Fatalf("%q: unexpected err: %v", in, err)
		}
		if want := "example.com"; got != want {
			t.Fatalf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeDomain_ZeroWidthAndBidiControls(t *testing.T) {
	n := newN()

	// Insert U+200B ZERO WIDTH SPACE and U+2060 WORD JOINER
	in := "exa\u200bmple\u2060.com"
	got, err := n.NormalizeDomain(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if want := "example.com"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeDomain_IDNToASCII(t *testing.T) {
	n := newN()

	// "bücher.de" -> "xn--bcher-kva.de"
	in := "bücher.de"
	got, err := n.NormalizeDomain(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if want := "xn--bcher-kva.de"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeDomain_MixedCaseIDN(t *testing.T) {
	n := newN()

	in := "BÜCHER.DE"
	got, err := n.NormalizeDomain(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if want := "xn--bcher-kva.de"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeDomain_EmptyInput(t *testing.T) {
	n := newN()

	if _, err := n.NormalizeDomain(""); err == nil {
		t.Fatal("expected error for empty input, got nil")
	}
}

func TestNormalizeDomain_AllDotsOrEmptyLabels(t *testing.T) {
	n := newN()

	// Just a dot or multiple dots should be invalid
	invalids := []string{
		".",
		"..",
		"...",
		".example.com",
		"example.com..",
		"example..com",
	}
	for _, in := range invalids {
		if _, err := n.NormalizeDomain(in); err == nil {
			t.Fatalf("%q: expected error, got nil", in)
		}
	}
}

func TestNormalizeDomain_LabelLengthAndTotalLength(t *testing.T) {
	n := newN()

	// Label of length 63 is ok
	lbl63 := makeStr('a', 63)
	in := lbl63 + ".com"
	if _, err := n.NormalizeDomain(in); err != nil {
		t.Fatalf("63-char label should be valid, got err: %v", err)
	}

	// Label of length 64 should fail
	lbl64 := makeStr('a', 64)
	in = lbl64 + ".com"
	if _, err := n.NormalizeDomain(in); err == nil {
		t.Fatal("64-char label should be invalid, got nil error")
	}

	// Total length > 253 should fail. Build near-boundary domain.
	// 4 labels of 63 plus 3 dots = 255 + ".com" too big; but we just need >253.
	long := lbl63 + "." + lbl63 + "." + lbl63 + "." + lbl63
	if _, err := n.NormalizeDomain(long); err == nil {
		t.Fatal("domain exceeding 253 chars should be invalid, got nil error")
	}
}

func TestNormalizeDomain_STD3_UnderscoreRejected(t *testing.T) {
	n := newN()

	if _, err := n.NormalizeDomain("ex_ample.com"); err == nil {
		t.Fatal("underscore in ASCII label should be rejected under STD3 rules")
	}
}

func TestNormalizeDomain_HyphensAllowedButEdgesChecked(t *testing.T) {
	n := newN()

	// interior hyphen ok
	if _, err := n.NormalizeDomain("exam-ple.com"); err != nil {
		t.Fatalf("unexpected err for interior hyphen: %v", err)
	}

	// leading hyphen invalid (unless punycode), trailing hyphen invalid
	if _, err := n.NormalizeDomain("-example.com"); err == nil {
		t.Fatal("leading hyphen label should be invalid")
	}
	if _, err := n.NormalizeDomain("example-.com"); err == nil {
		t.Fatal("trailing hyphen label should be invalid")
	}

	// punycode prefix allowed
	if _, err := n.NormalizeDomain("xn--exa-mple.com"); err != nil {
		t.Fatalf("punycode-like label should be allowed: %v", err)
	}
}

func TestNormalizeDomain_UnicodeConfusableMapped(t *testing.T) {
	n := newN()

	// Use Cyrillic small letter ie (U+0435) and small i (U+0456) in place of ASCII e/i.
	// After IDNA mapping, should punycode or be rejected; result should not equal raw Unicode.
	in := "burang\u0435r.\u0456o" // "burangер.іo"
	got, err := n.NormalizeDomain(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got == in {
		t.Fatalf("expected ASCII output different from input, got %q", got)
	}
}

func TestNormalizeDomain_StripsTrailingAndLeadingWhitespace(t *testing.T) {
	n := newN()

	in := " \t\nexample.com \r\n"
	got, err := n.NormalizeDomain(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if want := "example.com"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeDomain_RemovesTrailingDotOnly(t *testing.T) {
	n := newN()

	// internal dots preserved; only final dot removed
	in := "a.b.c."
	got, err := n.NormalizeDomain(in)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if want := "a.b.c"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Helper to build a repeated character string.
func makeStr(ch rune, n int) string {
	b := make([]rune, n)
	for i := 0; i < n; i++ {
		b[i] = ch
	}
	return string(b)
}
