// Package secret holds credential values that must never reach a log, a
// terminal, or a serialized document.
//
// A Secret wraps an unexported string. Every fmt verb, every encoding/json
// path, and every encoding.TextMarshaler path is overridden to emit a
// fingerprint instead of the plaintext. Plaintext leaves the package as a
// string through exactly one exported function, [Reveal], and reaches an
// Authorization header without becoming a string at all via
// [SetAuthorization].
//
// Values are compared by fingerprint, never by length. PF-S80 recon found two
// distinct 68-character credentials in sibling .env files; a length comparison
// reported them as identical.
package secret

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

// FingerprintLen is the number of hex characters retained from the SHA-256
// digest when fingerprinting a value. Twelve hex characters is 48 bits, far
// beyond the collision risk of a credential set numbered in the hundreds,
// while staying short enough to scan in a terminal.
const FingerprintLen = 12

// Secret wraps a credential value.
//
// The zero Secret wraps the empty string and is safe to use.
type Secret struct {
	// v is unexported so that no reflection-free code path outside this
	// package can read it, and so encoding/json skips it even if the
	// MarshalJSON override were ever removed.
	v string
}

// New wraps plaintext in a Secret.
func New(plaintext string) Secret { return Secret{v: plaintext} }

// Reveal returns the wrapped plaintext.
//
// This is the only exported route that yields a plaintext string. Call it from
// the HTTP request body writer and the .env file writer, and nowhere else.
// Authentication does not need it; use [SetAuthorization].
func Reveal(s Secret) string { return s.v }

// SetAuthorization writes s into h as a bearer token.
//
// This is a sink, not an accessor: the plaintext is written straight into the
// header and never becomes a value the caller holds. Authenticating through it
// rather than through [Reveal] keeps the exported plaintext escape hatch down
// to the two places that genuinely need a string — the .env writer and the
// request body writer — so an audit of Reveal call sites stays short enough to
// read.
//
// Callers must not hand h to anything that formats it. See the transport in
// internal/render for the cloning discipline that goes with this.
func SetAuthorization(h http.Header, s Secret) {
	h.Set("Authorization", "Bearer "+s.v)
}

// Fingerprint returns the leading [FingerprintLen] hex characters of the
// SHA-256 digest of the plaintext.
func (s Secret) Fingerprint() string {
	sum := sha256.Sum256([]byte(s.v))
	return hex.EncodeToString(sum[:])[:FingerprintLen]
}

// Empty reports whether the wrapped value is the empty string.
//
// Empty is deliberately the only property of the plaintext other than its
// fingerprint that this package exposes. Length is not available, because
// callers reach for it as a cheap equality proxy and it is not one.
func (s Secret) Empty() bool { return s.v == "" }

// Equal reports whether two Secrets wrap the same value, comparing
// fingerprints rather than lengths or raw bytes.
func (s Secret) Equal(o Secret) bool { return s.Fingerprint() == o.Fingerprint() }

// String renders the fingerprint form.
func (s Secret) String() string { return "secret(" + s.Fingerprint() + ")" }

// GoString renders the fingerprint form for the %#v verb.
func (s Secret) GoString() string {
	return "secret.Secret{fp:" + strconv.Quote(s.Fingerprint()) + "}"
}

// Format implements [fmt.Formatter] so that every verb — including ones that
// would otherwise reflect over the struct, such as %#v, and ones that make no
// sense for a string, such as %d — resolves to the fingerprint form.
//
// Implementing Formatter rather than only Stringer matters: fmt consults
// Formatter first, so this method is the single chokepoint for all of fmt.
func (s Secret) Format(f fmt.State, verb rune) {
	switch {
	case verb == 'v' && f.Flag('#'):
		io.WriteString(f, s.GoString())
	case verb == 'q':
		io.WriteString(f, strconv.Quote(s.String()))
	default:
		io.WriteString(f, s.String())
	}
}

// MarshalText implements [encoding.TextMarshaler] with the fingerprint form,
// covering map keys and any encoder that prefers text over JSON.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// MarshalJSON implements [json.Marshaler] with the fingerprint form.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// UnmarshalJSON reads a plaintext JSON string into a Secret. It is the inverse
// of the wire format Render uses, not of [Secret.MarshalJSON]; a Secret that
// is marshalled and unmarshalled does not round-trip, by design.
func (s *Secret) UnmarshalJSON(b []byte) error {
	var plaintext string
	if err := json.Unmarshal(b, &plaintext); err != nil {
		return err
	}
	s.v = plaintext
	return nil
}
