// Package render is a minimal client for the Render REST API, shaped by what
// PF-S80 recon measured rather than by what the API appears to offer.
//
// Four behaviours of the live API drive this design:
//
//   - PUT /v1/services/{id}/env-vars replaces the entire variable set and
//     returns 200. A partial payload silently deletes every omitted key. This
//     client never issues it, and [Client.do] refuses to construct it.
//   - limit is capped at 100. Larger values return HTTP 400.
//   - GET /v1/services/{id}/env-groups does not exist; it returns 404. Group
//     membership is only discoverable from the group side, via each group's
//     serviceLinks.
//   - GET /v1/env-groups omits envVars entirely, reporting every group as
//     empty. Membership requires a per-group GET.
//
// Any non-2xx response is fatal. There is no empty-result fallback anywhere in
// this package: during recon, two separate swallowed errors produced confident
// and completely wrong conclusions.
package render

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/pigfox/render-env-sync/internal/secret"
)

// DefaultBaseURL is the public Render API root.
const DefaultBaseURL = "https://api.render.com/v1"

// MaxPageLimit is the largest page size the API accepts. Anything above this
// returns HTTP 400 {"message":"invalid limit: too large"}.
const MaxPageLimit = 100

// maxErrorBody bounds how much of a failing response is quoted back.
const maxErrorBody = 512

// Client talks to the Render API.
//
// The credential is not a field here. It lives inside the client's transport,
// so no request object this package hands to other code ever carries it; see
// [authTransport].
type Client struct {
	baseURL string
	hc      *http.Client
	limit   int
}

// authTransport attaches the credential to a clone of each outbound request.
//
// Setting the header on the caller's request would put the token on an object
// that other code holds and could format. *http.Request is plain stdlib with
// no redaction: %v, %+v, req.Header and httputil.DumpRequestOut all print it
// in clear, and no amount of care in [secret.Secret] helps once the string has
// been written into a header map. Cloning inside RoundTrip means the only
// request carrying the credential is the one handed straight to the transport
// and discarded.
type authTransport struct {
	key  secret.Secret
	base http.RoundTripper
}

// RoundTrip implements [http.RoundTripper].
func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	secret.SetAuthorization(clone.Header, t.key)

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// Options configures a [Client]. The zero value is usable.
type Options struct {
	// BaseURL defaults to DefaultBaseURL.
	BaseURL string
	// PageLimit defaults to MaxPageLimit and may not exceed it.
	PageLimit int
	// HTTPClient defaults to http.DefaultClient.
	HTTPClient *http.Client
}

// New builds a Client.
func New(apiKey secret.Secret, opt Options) (*Client, error) {
	if apiKey.Empty() {
		return nil, fmt.Errorf("render: API key is empty")
	}
	if opt.BaseURL == "" {
		opt.BaseURL = DefaultBaseURL
	}
	if opt.PageLimit == 0 {
		opt.PageLimit = MaxPageLimit
	}
	if opt.PageLimit < 1 || opt.PageLimit > MaxPageLimit {
		return nil, fmt.Errorf("render: page limit %d out of range 1..%d", opt.PageLimit, MaxPageLimit)
	}
	base := http.DefaultClient
	if opt.HTTPClient != nil {
		base = opt.HTTPClient
	}
	// Copy the caller's client rather than mutating it: installing a
	// transport on a shared *http.Client would attach this credential to
	// every unrelated request made through it.
	authed := *base
	authed.Transport = &authTransport{key: apiKey, base: base.Transport}

	return &Client{
		baseURL: strings.TrimSuffix(opt.BaseURL, "/"),
		hc:      &authed,
		limit:   opt.PageLimit,
	}, nil
}

// APIError reports a non-2xx response.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("render: %s %s: HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

// CollectionWriteError reports a refused write to a collection endpoint.
type CollectionWriteError struct {
	Method string
	Path   string
}

func (e *CollectionWriteError) Error() string {
	return fmt.Sprintf("render: refusing %s %s: writing the env-vars collection replaces every "+
		"variable on the service and returns 200; use a per-key write instead", e.Method, e.Path)
}

// GroupWriteError reports a refused write to an environment group. renv v1
// reads groups to resolve precedence but never modifies them, so that a
// mistake can never take out configuration shared by several services.
type GroupWriteError struct {
	Method string
	Path   string
}

func (e *GroupWriteError) Error() string {
	return fmt.Sprintf("render: refusing %s %s: renv does not write environment groups", e.Method, e.Path)
}

// guardWrite rejects request shapes this tool must never issue. It runs before
// the request is built, so a caller cannot reach the network by mistake.
func guardWrite(method, path string) error {
	if method == http.MethodGet {
		return nil
	}
	clean := path
	if i := strings.IndexByte(clean, '?'); i >= 0 {
		clean = clean[:i]
	}
	seg := strings.Split(strings.Trim(clean, "/"), "/")

	if seg[len(seg)-1] == "env-vars" {
		return &CollectionWriteError{Method: method, Path: clean}
	}
	if seg[0] == "env-groups" {
		return &GroupWriteError{Method: method, Path: clean}
	}
	return nil
}

// scrubBody removes a just-sent plaintext value from a captured response body.
//
// An API that echoes a rejected value back in its error message would
// otherwise put that value into an error string, which is printed, logged and
// often pasted into a bug report. Scrubbing runs before truncation, so a value
// straddling the truncation point cannot leave a partial behind.
func scrubBody(body, sensitive string) string {
	if sensitive != "" {
		body = strings.ReplaceAll(body, sensitive, secret.New(sensitive).String())
	}
	if len(body) > maxErrorBody {
		body = body[:maxErrorBody] + "…"
	}
	return body
}

// newRequest builds an outbound request.
//
// Deliberately absent: the Authorization header. Everything this function
// returns is safe to format, log, or hand to another package; the credential
// is attached later, to a clone, inside [authTransport.RoundTrip].
func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/"+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("render: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// do issues a request and returns the body, failing on any non-2xx status.
//
// sensitive is the plaintext this request is sending, if any. It is used only
// to scrub an echoed value out of a failing response; pass "" when the request
// carries no secret.
//
// The credential is not set here. It is attached to a clone of the request
// inside [authTransport.RoundTrip], so the request object this function builds
// and holds never carries it.
func (c *Client) do(ctx context.Context, method, path string, body []byte, sensitive string) ([]byte, error) {
	if err := guardWrite(method, path); err != nil {
		return nil, err
	}

	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return nil, err
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("render: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("render: %s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &APIError{
			Method:     method,
			Path:       path,
			StatusCode: resp.StatusCode,
			Body:       scrubBody(string(data), sensitive),
		}
	}
	return data, nil
}

// Service is a Render service.
type Service struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	OwnerID   string `json:"ownerId"`
	Suspended string `json:"suspended"`
}

// EnvVar is a single environment variable. Value is fingerprinted on every
// render path; see [secret.Secret].
type EnvVar struct {
	Key   string        `json:"key"`
	Value secret.Secret `json:"value"`
}

// ServiceLink names a service attached to an environment group.
type ServiceLink struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// EnvGroup is a Render environment group.
type EnvGroup struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	OwnerID      string        `json:"ownerId"`
	EnvVars      []EnvVar      `json:"envVars"`
	ServiceLinks []ServiceLink `json:"serviceLinks"`
}

// decodeList unwraps a Render list response.
//
// List endpoints return elements wrapped under a type-specific key alongside a
// cursor, as in [{"service":{…},"cursor":"…"}]. Some endpoints return the
// object directly. Both shapes are accepted rather than guessed at, because
// getting it wrong yields an empty slice rather than an error.
func decodeList[T any](data []byte, field string) ([]T, string, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, "", fmt.Errorf("decode list: %w", err)
	}

	out := make([]T, 0, len(raw))
	cursor := ""
	for _, el := range raw {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(el, &probe); err != nil {
			return nil, "", fmt.Errorf("decode list element: %w", err)
		}

		// Reuse the element's own bytes when it is not wrapped, rather than
		// re-encoding the probe map, which would add a branch that cannot
		// fail and so cannot be tested.
		inner, wrapped := probe[field]
		if !wrapped {
			inner = el
		}

		var item T
		if err := json.Unmarshal(inner, &item); err != nil {
			return nil, "", fmt.Errorf("decode %s: %w", field, err)
		}
		out = append(out, item)

		if c, ok := probe["cursor"]; ok {
			var s string
			if err := json.Unmarshal(c, &s); err != nil {
				return nil, "", fmt.Errorf("decode cursor: %w", err)
			}
			cursor = s
		}
	}
	return out, cursor, nil
}

// getPaged walks a cursor-paginated collection to completion.
func getPaged[T any](ctx context.Context, c *Client, base, field string) ([]T, error) {
	var out []T
	cursor := ""
	for {
		q := base + "?limit=" + strconv.Itoa(c.limit)
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		data, err := c.do(ctx, http.MethodGet, q, nil, "")
		if err != nil {
			return nil, err
		}
		page, next, err := decodeList[T](data, field)
		if err != nil {
			return nil, fmt.Errorf("render: GET %s: %w", q, err)
		}
		out = append(out, page...)

		// A short page is the last page. An empty cursor with a full page
		// would otherwise loop forever.
		if len(page) < c.limit || next == "" {
			return out, nil
		}
		cursor = next
	}
}

// ListServices returns every service the credential can see.
func (c *Client) ListServices(ctx context.Context) ([]Service, error) {
	return getPaged[Service](ctx, c, "services", "service")
}

// GetService fetches one service. Use it to assert ownership before a write.
func (c *Client) GetService(ctx context.Context, id string) (Service, error) {
	data, err := c.do(ctx, http.MethodGet, "services/"+url.PathEscape(id), nil, "")
	if err != nil {
		return Service{}, err
	}
	var svc Service
	if err := unwrap(data, "service", &svc); err != nil {
		return Service{}, fmt.Errorf("render: GET services/%s: %w", id, err)
	}
	return svc, nil
}

// ListServiceEnvVars returns the service's own variables, excluding anything
// inherited from a linked environment group.
func (c *Client) ListServiceEnvVars(ctx context.Context, serviceID string) ([]EnvVar, error) {
	return getPaged[EnvVar](ctx, c, "services/"+url.PathEscape(serviceID)+"/env-vars", "envVar")
}

// GetEnvGroup fetches one environment group, including its variables and
// service links.
func (c *Client) GetEnvGroup(ctx context.Context, id string) (EnvGroup, error) {
	data, err := c.do(ctx, http.MethodGet, "env-groups/"+url.PathEscape(id), nil, "")
	if err != nil {
		return EnvGroup{}, err
	}
	var g EnvGroup
	if err := unwrap(data, "envGroup", &g); err != nil {
		return EnvGroup{}, fmt.Errorf("render: GET env-groups/%s: %w", id, err)
	}
	return g, nil
}

// ListEnvGroups returns every environment group, fully populated.
//
// The list endpoint omits envVars and reports every group as holding zero
// variables, so each group is fetched individually. This costs one request per
// group and is not an optimisation target: believing the list endpoint's zero
// is how a sync tool concludes there is nothing to reconcile.
func (c *Client) ListEnvGroups(ctx context.Context) ([]EnvGroup, error) {
	stubs, err := getPaged[EnvGroup](ctx, c, "env-groups", "envGroup")
	if err != nil {
		return nil, err
	}
	out := make([]EnvGroup, 0, len(stubs))
	for _, stub := range stubs {
		full, err := c.GetEnvGroup(ctx, stub.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, full)
	}
	return out, nil
}

// GroupsForService returns the environment groups linked to a service.
//
// There is no per-service endpoint for this — GET /v1/services/{id}/env-groups
// returns 404 — so the whole group set is fetched and filtered by serviceLinks.
func (c *Client) GroupsForService(ctx context.Context, serviceID string) ([]EnvGroup, error) {
	all, err := c.ListEnvGroups(ctx)
	if err != nil {
		return nil, err
	}
	var out []EnvGroup
	for _, g := range all {
		for _, link := range g.ServiceLinks {
			if link.ID == serviceID {
				out = append(out, g)
				break
			}
		}
	}
	return out, nil
}

// PutServiceEnvVar upserts a single variable.
//
// This is the only write path to service configuration. It targets
// /env-vars/{key}, never the collection.
func (c *Client) PutServiceEnvVar(ctx context.Context, serviceID, key string, value secret.Secret) error {
	if key == "" {
		return fmt.Errorf("render: refusing to write an empty key: that URL is the destructive collection endpoint")
	}
	path := "services/" + url.PathEscape(serviceID) + "/env-vars/" + url.PathEscape(key)
	return c.putValue(ctx, path, value)
}

// putValue sends a single {"value": …} body.
//
// This is the HTTP request body writer: one of only two regions in the program
// permitted to call [secret.Reveal], the other being the .env writer. The
// credential used to authenticate is handled separately, by [authTransport].
func (c *Client) putValue(ctx context.Context, path string, value secret.Secret) error {
	plaintext := secret.Reveal(value)
	// A map[string]string cannot fail to encode. The error is discarded
	// deliberately rather than returned as a branch no test could ever reach.
	body, _ := json.Marshal(map[string]string{"value": plaintext})
	// plaintext is passed through so that an API which echoes a rejected
	// value back in its error message cannot put it into an error string.
	_, err := c.do(ctx, http.MethodPut, path, body, plaintext)
	return err
}

// DeleteServiceEnvVar removes a single variable.
func (c *Client) DeleteServiceEnvVar(ctx context.Context, serviceID, key string) error {
	if key == "" {
		return fmt.Errorf("render: refusing to delete an empty key: that URL is the destructive collection endpoint")
	}
	path := "services/" + url.PathEscape(serviceID) + "/env-vars/" + url.PathEscape(key)
	_, err := c.do(ctx, http.MethodDelete, path, nil, "")
	return err
}

// Deploy triggers a deploy and returns its id.
//
// Writing a variable does not deploy it. Render applies environment changes on
// the next deploy, and renv makes that a separate, explicit step so that a
// configuration fix never restarts a service as a side effect.
func (c *Client) Deploy(ctx context.Context, serviceID string) (string, error) {
	data, err := c.do(ctx, http.MethodPost,
		"services/"+url.PathEscape(serviceID)+"/deploys", []byte("{}"), "")
	if err != nil {
		return "", err
	}
	var dep struct {
		ID string `json:"id"`
	}
	if err := unwrap(data, "deploy", &dep); err != nil {
		return "", fmt.Errorf("render: POST services/%s/deploys: %w", serviceID, err)
	}
	return dep.ID, nil
}

// unwrap decodes an object that may be wrapped under a single named field.
func unwrap(data []byte, field string, dst any) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("decode object: %w", err)
	}
	if inner, ok := probe[field]; ok {
		return json.Unmarshal(inner, dst)
	}
	return json.Unmarshal(data, dst)
}
