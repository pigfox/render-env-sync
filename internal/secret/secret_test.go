package secret_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/pigfox/render-env-sync/internal/secret"
)

// plaintext is a value that must never appear in any rendered output.
const plaintext = "pf-s80-CANARY-PLAINTEXT-do-not-print-9f8e7d6c5b4a"

// TestNoLeakThroughAnyRenderPath is the leak test. It pushes a known plaintext
// through every formatting, logging and marshalling path a caller might
// plausibly reach for, then asserts the plaintext appears nowhere in the
// captured output.
//
// This test exists because PF-S80 recon leaked two live credentials through a
// redaction helper that read correctly: `${VA:+$(fp "$VA")}${VA:-ABSENT}`
// substitutes the value itself when the variable is set. The lesson is that a
// redaction path which looks right and produces plausible output is exactly
// the one that leaks, so the invariant needs a test rather than a review.
func TestNoLeakThroughAnyRenderPath(t *testing.T) {
	s := secret.New(plaintext)

	var buf bytes.Buffer

	// Every fmt verb a caller might use, including ones that would normally
	// reflect into the struct or reject a string outright.
	for _, verb := range []string{"%v", "%s", "%q", "%#v", "%+v", "%d", "%x", "%T", "%10s", "%-10v"} {
		fmt.Fprintf(&buf, verb+"\n", s)
	}

	// Indirect fmt entry points.
	buf.WriteString(fmt.Sprint(s) + "\n")
	buf.WriteString(fmt.Sprintln(s))
	buf.WriteString(fmt.Sprintf("%v\n", &s))

	// Containers: fmt formats elements individually, so each must be covered
	// by Format rather than by the container's own rendering.
	buf.WriteString(fmt.Sprintf("%v %+v %#v\n", []secret.Secret{s, s}, []secret.Secret{s}, []secret.Secret{s}))
	buf.WriteString(fmt.Sprintf("%v\n", map[string]secret.Secret{"k": s}))
	buf.WriteString(fmt.Sprintf("%v\n", struct{ S secret.Secret }{s}))
	buf.WriteString(fmt.Sprintf("%v\n", &struct{ S *secret.Secret }{&s}))

	// log routes through fmt, but assert it explicitly because it is the most
	// common accidental disclosure path.
	logger := log.New(&buf, "", 0)
	logger.Print(s)
	logger.Printf("%v %s %#v", s, s, s)
	logger.Println([]secret.Secret{s})

	// encoding/json, as a bare value, a map value, a struct field, a slice
	// element, and behind a pointer.
	mustMarshal(t, &buf, s)
	mustMarshal(t, &buf, map[string]secret.Secret{"key": s})
	mustMarshal(t, &buf, struct {
		S secret.Secret `json:"s"`
	}{s})
	mustMarshal(t, &buf, []secret.Secret{s, s})
	mustMarshal(t, &buf, &s)

	// encoding.TextMarshaler, reached by encoders that prefer text.
	text, err := s.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	buf.Write(text)

	// Direct Stringer and GoStringer calls.
	buf.WriteString(s.String() + s.GoString())

	got := buf.String()
	if strings.Contains(got, plaintext) {
		t.Fatalf("plaintext leaked into rendered output:\n%s", got)
	}
	// Guard against the opposite failure: a redaction that emits nothing at
	// all would also pass a naive containment check.
	if !strings.Contains(got, s.Fingerprint()) {
		t.Fatalf("fingerprint %q missing from output:\n%s", s.Fingerprint(), got)
	}
}

func mustMarshal(t *testing.T, buf *bytes.Buffer, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%T): %v", v, err)
	}
	buf.Write(b)
	buf.WriteByte('\n')
}

// TestEqualLengthValuesAreNotEqual pins the finding that motivated
// fingerprint comparison. Both values below are 68 characters, the length of
// a VENDOR_API_KEY, and they differ.
func TestEqualLengthValuesAreNotEqual(t *testing.T) {
	a := secret.New(strings.Repeat("a", 67) + "1")
	b := secret.New(strings.Repeat("a", 67) + "2")

	if len(secret.Reveal(a)) != len(secret.Reveal(b)) {
		t.Fatalf("test fixture is wrong: lengths differ (%d vs %d)",
			len(secret.Reveal(a)), len(secret.Reveal(b)))
	}
	if a.Equal(b) {
		t.Fatal("two distinct values of equal length compared equal")
	}
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("distinct values produced the same fingerprint")
	}
	if !a.Equal(secret.New(secret.Reveal(a))) {
		t.Fatal("a value did not compare equal to itself")
	}
}

// TestSetAuthorization covers the auth sink. It exists so that authenticating
// does not require Reveal, which keeps the exported plaintext escape hatch
// down to the two call sites an audit can check by hand.
func TestSetAuthorization(t *testing.T) {
	h := http.Header{}
	secret.SetAuthorization(h, secret.New(plaintext))

	if got := h.Get("Authorization"); got != "Bearer "+plaintext {
		t.Fatalf("Authorization = %q, want the bearer token", got)
	}

	// Overwrites rather than appends, so a retried request cannot end up
	// with two credentials.
	secret.SetAuthorization(h, secret.New("second"))
	if got := h.Values("Authorization"); len(got) != 1 || got[0] != "Bearer second" {
		t.Fatalf("Authorization values = %v, want exactly one", got)
	}
}

func TestRevealRoundTrips(t *testing.T) {
	if got := secret.Reveal(secret.New(plaintext)); got != plaintext {
		t.Fatalf("Reveal = %q, want %q", got, plaintext)
	}
	if got := secret.Reveal(secret.Secret{}); got != "" {
		t.Fatalf("Reveal(zero) = %q, want empty", got)
	}
}

func TestFingerprintIsStableAndTruncated(t *testing.T) {
	s := secret.New("value")
	fp := s.Fingerprint()
	if len(fp) != secret.FingerprintLen {
		t.Fatalf("fingerprint length = %d, want %d", len(fp), secret.FingerprintLen)
	}
	if fp != s.Fingerprint() {
		t.Fatal("fingerprint is not stable across calls")
	}
	// Known digest prefix for "value", so a change to the hash or the
	// truncation is caught rather than silently accepted.
	const want = "cd42404d52ad"
	if fp != want {
		t.Fatalf("fingerprint = %q, want %q", fp, want)
	}
}

func TestEmpty(t *testing.T) {
	if !secret.New("").Empty() {
		t.Fatal("empty string reported as non-empty")
	}
	if !(secret.Secret{}).Empty() {
		t.Fatal("zero value reported as non-empty")
	}
	if secret.New("x").Empty() {
		t.Fatal("non-empty string reported as empty")
	}
}

func TestFormatVerbs(t *testing.T) {
	s := secret.New("value")
	want := "secret(cd42404d52ad)"

	tests := []struct {
		format string
		want   string
	}{
		{"%v", want},
		{"%s", want},
		{"%+v", want},
		{"%d", want},
		{"%q", `"` + want + `"`},
		{"%#v", `secret.Secret{fp:"cd42404d52ad"}`},
	}
	for _, tc := range tests {
		if got := fmt.Sprintf(tc.format, s); got != tc.want {
			t.Errorf("Sprintf(%q) = %q, want %q", tc.format, got, tc.want)
		}
	}
	if got := s.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got := s.GoString(); got != `secret.Secret{fp:"cd42404d52ad"}` {
		t.Errorf("GoString() = %q", got)
	}
}

func TestMarshalText(t *testing.T) {
	b, err := secret.New("value").MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	if string(b) != "secret(cd42404d52ad)" {
		t.Fatalf("MarshalText = %q", b)
	}
}

func TestMarshalJSON(t *testing.T) {
	b, err := secret.New("value").MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if string(b) != `"secret(cd42404d52ad)"` {
		t.Fatalf("MarshalJSON = %s", b)
	}
}

func TestUnmarshalJSON(t *testing.T) {
	var s secret.Secret
	if err := json.Unmarshal([]byte(`"plain"`), &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if secret.Reveal(s) != "plain" {
		t.Fatalf("Reveal = %q, want %q", secret.Reveal(s), "plain")
	}

	if err := json.Unmarshal([]byte(`123`), &s); err == nil {
		t.Fatal("expected error unmarshalling a number into Secret")
	}
}
