package render

import (
	"context"
	"net/http"

	"github.com/pigfox/render-env-sync/internal/secret"
)

// GuardWriteForTest exposes the request-shape guard so that the refusals can
// be asserted directly, without needing a caller that would otherwise be
// impossible to write through the exported API.
func GuardWriteForTest(method, path string) error { return guardWrite(method, path) }

// DoForTest reaches the low-level request path directly.
//
// No exported method can construct a collection write — the per-key helpers
// reject an empty key first — so the guard inside do is unreachable through
// the public API by design. That is exactly why it needs its own test: it is
// the layer that would catch a future method added without the key check.
func (c *Client) DoForTest(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	return c.do(ctx, method, path, body, "")
}

// NewRequestForTest builds an outbound request exactly as do does, so that the
// object do holds can be inspected for the credential.
func (c *Client) NewRequestForTest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	return c.newRequest(ctx, method, path, body)
}

// NewAuthTransportForTest exposes the credential-attaching transport so that
// the clone-versus-mutate behaviour can be asserted directly.
func NewAuthTransportForTest(key secret.Secret, base http.RoundTripper) http.RoundTripper {
	return &authTransport{key: key, base: base}
}
