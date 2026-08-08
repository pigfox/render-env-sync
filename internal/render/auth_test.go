package render_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"testing"

	"github.com/pigfox/render-env-sync/internal/render"
	"github.com/pigfox/render-env-sync/internal/secret"
)

// canaryKey is a credential that must never appear in anything a caller can
// format, log, or dump.
const canaryKey = "rnd_CANARYCANARYCANARYCANARY0001"

// TestCredentialNeverReachesARequestObjectACallerCanFormat is named for the
// failure it prevents: someone debugging a failed call writes
// fmt.Errorf("request %+v failed", req) — or reaches for
// httputil.DumpRequestOut — and ships the API key to a log.
//
// *http.Request is plain stdlib with no redaction. Every rendering below was
// verified to print a bearer token in clear when the header is set on the
// request the client holds, which is why the credential is attached to a clone
// inside the transport instead.
func TestCredentialNeverReachesARequestObjectACallerCanFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	c, err := render.New(secret.New(canaryKey), render.Options{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req, err := c.NewRequestForTest(context.Background(), http.MethodPut,
		"services/srv-x/env-vars/KEY", []byte(`{"value":"v"}`))
	if err != nil {
		t.Fatalf("NewRequestForTest: %v", err)
	}

	dump, err := httputil.DumpRequestOut(req, true)
	if err != nil {
		t.Fatalf("DumpRequestOut: %v", err)
	}

	renderings := map[string]string{
		"%v":               fmt.Sprintf("%v", req),
		"%+v":              fmt.Sprintf("%+v", req),
		"%#v":              fmt.Sprintf("%#v", req),
		"req.Header %v":    fmt.Sprintf("%v", req.Header),
		"req.Header.Get":   req.Header.Get("Authorization"),
		"DumpRequestOut":   string(dump),
		"Sprint(req)":      fmt.Sprint(req),
		"Sprintf(%s, hdr)": fmt.Sprintf("%s", req.Header),
	}
	for name, got := range renderings {
		if strings.Contains(got, canaryKey) {
			t.Errorf("credential leaked via %s:\n%s", name, got)
		}
	}
	if req.Header.Get("Authorization") != "" {
		t.Errorf("request carries an Authorization header: %q", req.Header.Get("Authorization"))
	}
}

// TestCredentialDoesReachTheServer is the other half: the clone must actually
// carry the header, or the redaction above would be achieved by simply never
// authenticating.
func TestCredentialDoesReachTheServer(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	c, err := render.New(secret.New(canaryKey), render.Options{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.ListServices(context.Background()); err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if seen != "Bearer "+canaryKey {
		t.Fatalf("server received Authorization %q, want the bearer token", seen)
	}
}

// capturingTransport records the request it is handed.
type capturingTransport struct {
	got *http.Request
}

func (c *capturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.got = req
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// TestAuthTransportClonesRatherThanMutating pins the mechanism. If RoundTrip
// set the header in place, the caller's request would carry the credential
// after the call and every rendering above would start leaking again.
func TestAuthTransportClonesRatherThanMutating(t *testing.T) {
	base := &capturingTransport{}
	rt := render.NewAuthTransportForTest(secret.New(canaryKey), base)

	req, err := http.NewRequest(http.MethodGet, "https://api.example.test/v1/services", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("caller's request was mutated: Authorization = %q", got)
	}
	if got := base.got.Header.Get("Authorization"); got != "Bearer "+canaryKey {
		t.Errorf("clone did not carry the credential: %q", got)
	}
	if base.got == req {
		t.Error("transport passed the caller's request through instead of a clone")
	}
}

// TestAuthTransportFallsBackToDefaultTransport covers a nil base, which is
// what a caller-supplied http.Client with no Transport produces.
func TestAuthTransportFallsBackToDefaultTransport(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	// An http.Client with a nil Transport: New must not panic, and the
	// transport must fall through to http.DefaultTransport.
	c, err := render.New(secret.New(canaryKey), render.Options{
		BaseURL:    srv.URL,
		HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.ListServices(context.Background()); err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if seen != "Bearer "+canaryKey {
		t.Fatalf("server received Authorization %q", seen)
	}
}

// TestNewDoesNotMutateTheCallersHTTPClient checks that installing the
// credential transport does not attach it to a client the caller also uses for
// unrelated requests.
func TestNewDoesNotMutateTheCallersHTTPClient(t *testing.T) {
	original := &http.Client{}
	if _, err := render.New(secret.New(canaryKey), render.Options{HTTPClient: original}); err != nil {
		t.Fatalf("New: %v", err)
	}
	if original.Transport != nil {
		t.Fatal("New installed a credential-bearing transport on the caller's client")
	}
}

// TestErrorBodyScrubsAnEchoedValue covers an API that reflects a rejected
// value back in its error message. Without scrubbing, that value lands in an
// error string, which is printed, logged, and pasted into bug reports.
func TestErrorBodyScrubsAnEchoedValue(t *testing.T) {
	const submitted = "s3cr3t-value-the-api-echoes-back"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"message":"invalid value %q for key KEY"}`, submitted)
	}))
	defer srv.Close()

	c, err := render.New(secret.New(canaryKey), render.Options{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	err = c.PutServiceEnvVar(context.Background(), "srv-x", "KEY", secret.New(submitted))
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if strings.Contains(msg, submitted) {
		t.Fatalf("echoed value survived into the error:\n%s", msg)
	}
	if !strings.Contains(msg, secret.New(submitted).Fingerprint()) {
		t.Errorf("scrubbed value should be replaced by its fingerprint:\n%s", msg)
	}
	if !strings.Contains(msg, "invalid value") {
		t.Errorf("the rest of the message should survive:\n%s", msg)
	}
}

// TestErrorBodyScrubbingRunsBeforeTruncation checks the ordering. Truncating
// first could cut an echoed secret in half and leave a usable prefix behind.
func TestErrorBodyScrubbingRunsBeforeTruncation(t *testing.T) {
	const submitted = "STRADDLE-THE-TRUNCATION-BOUNDARY-0123456789"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// Position the value so that it spans the 512-character cutoff.
		fmt.Fprint(w, strings.Repeat("x", 500)+submitted+strings.Repeat("y", 500))
	}))
	defer srv.Close()

	c, err := render.New(secret.New(canaryKey), render.Options{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	err = c.PutServiceEnvVar(context.Background(), "srv-x", "KEY", secret.New(submitted))
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if strings.Contains(msg, submitted) {
		t.Fatalf("full value survived:\n%s", msg)
	}
	// The dangerous case: a partial prefix left behind by truncating first.
	for n := 12; n < len(submitted); n++ {
		if strings.Contains(msg, submitted[:n]) {
			t.Fatalf("a %d-character prefix of the value survived truncation:\n%s", n, msg)
		}
	}
}

// TestErrorBodyWithoutSensitiveValueIsUnchanged checks that a plain GET
// failure is reported verbatim; scrubbing must not mangle ordinary messages.
func TestErrorBodyWithoutSensitiveValueIsUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"message":"invalid limit: too large"}`)
	}))
	defer srv.Close()

	c, err := render.New(secret.New(canaryKey), render.Options{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ListServices(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid limit: too large") {
		t.Fatalf("err = %v", err)
	}
}
