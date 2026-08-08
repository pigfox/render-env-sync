package render_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pigfox/render-env-sync/internal/render"
	"github.com/pigfox/render-env-sync/internal/secret"
)

const testKey = "rnd_TESTKEYTESTKEYTESTKEYTEST"

// recorder captures every request a test issues so that assertions can be made
// about requests that were *not* sent, which is the point of most of this
// package's guarantees.
type recorder struct {
	t        *testing.T
	requests []string
	handler  http.HandlerFunc
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.requests = append(r.requests, req.Method+" "+req.URL.Path)

	// The collection PUT is the single most destructive request against this
	// API. Fail loudly here rather than relying on a client-side guard alone.
	if req.Method != http.MethodGet && strings.HasSuffix(req.URL.Path, "/env-vars") {
		r.t.Errorf("client issued the destructive collection write: %s %s", req.Method, req.URL.Path)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+testKey {
		r.t.Errorf("Authorization = %q", got)
	}
	r.handler(w, req)
}

func newServer(t *testing.T, h http.HandlerFunc) (*render.Client, *recorder) {
	t.Helper()
	rec := &recorder{t: t, handler: h}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	c, err := render.New(secret.New(testKey), render.Options{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, rec
}

func TestNewValidation(t *testing.T) {
	if _, err := render.New(secret.New(""), render.Options{}); err == nil {
		t.Error("expected error for empty key")
	}
	for _, limit := range []int{-1, 101, 1000} {
		if _, err := render.New(secret.New(testKey), render.Options{PageLimit: limit}); err == nil {
			t.Errorf("expected error for page limit %d", limit)
		}
	}
	c, err := render.New(secret.New(testKey), render.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c == nil {
		t.Fatal("New returned nil client")
	}
	if _, err := render.New(secret.New(testKey), render.Options{PageLimit: render.MaxPageLimit}); err != nil {
		t.Errorf("max page limit rejected: %v", err)
	}
}

// TestPageLimitNeverExceedsAPIMaximum pins the recon finding that limit=200
// returns HTTP 400.
func TestPageLimitNeverExceedsAPIMaximum(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("limit = %q, want 100", got)
		}
		fmt.Fprint(w, `[]`)
	})
	if _, err := c.ListServices(context.Background()); err != nil {
		t.Fatalf("ListServices: %v", err)
	}
}

func TestListServicesPaginates(t *testing.T) {
	page := 0
	rec := &recorder{t: t}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	rec.handler = func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		switch page {
		case 0:
			if cursor != "" {
				t.Errorf("first page sent cursor %q", cursor)
			}
			fmt.Fprint(w, `[{"service":{"id":"srv-1","name":"one"},"cursor":"c1"},
			                {"service":{"id":"srv-2","name":"two"},"cursor":"c2"}]`)
		case 1:
			if cursor != "c2" {
				t.Errorf("second page cursor = %q, want c2", cursor)
			}
			fmt.Fprint(w, `[{"service":{"id":"srv-3","name":"three"},"cursor":"c3"}]`)
		default:
			t.Fatalf("unexpected extra page request")
		}
		page++
	}

	client, err := render.New(secret.New(testKey), render.Options{
		BaseURL: srv.URL, PageLimit: 2, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ListServices(context.Background())
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d services, want 3", len(got))
	}
	if got[2].ID != "srv-3" {
		t.Errorf("third service = %q", got[2].ID)
	}
}

func TestListServicesStopsOnEmptyCursor(t *testing.T) {
	calls := 0
	rec := &recorder{t: t}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	rec.handler = func(w http.ResponseWriter, r *http.Request) {
		calls++
		// A full page with no cursor: without the guard this loops forever.
		fmt.Fprint(w, `[{"service":{"id":"srv-1"}},{"service":{"id":"srv-2"}}]`)
	}
	c, err := render.New(secret.New(testKey), render.Options{
		BaseURL: srv.URL, PageLimit: 2, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListServices(context.Background()); err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if calls != 1 {
		t.Fatalf("made %d requests, want 1", calls)
	}
}

func TestDecodeListAcceptsUnwrappedElements(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"key":"A","value":"one"},{"key":"B","value":"two"}]`)
	})
	got, err := c.ListServiceEnvVars(context.Background(), "srv-x")
	if err != nil {
		t.Fatalf("ListServiceEnvVars: %v", err)
	}
	if len(got) != 2 || got[0].Key != "A" {
		t.Fatalf("got %+v", got)
	}
	if secret.Reveal(got[1].Value) != "two" {
		t.Errorf("value = %q", secret.Reveal(got[1].Value))
	}
}

func TestGetService(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"srv-x","name":"example.com","type":"web_service",
		                "ownerId":"tea-owner","suspended":"not_suspended"}`)
	})
	svc, err := c.GetService(context.Background(), "srv-x")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if svc.OwnerID != "tea-owner" || svc.Name != "example.com" {
		t.Fatalf("got %+v", svc)
	}
}

func TestGetServiceWrapped(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"service":{"id":"srv-x","name":"wrapped"}}`)
	})
	svc, err := c.GetService(context.Background(), "srv-x")
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if svc.Name != "wrapped" {
		t.Fatalf("got %+v", svc)
	}
}

// TestListEnvGroupsRefetchesEachGroup pins the recon finding that the list
// endpoint reports every group as empty.
func TestListEnvGroupsRefetchesEachGroup(t *testing.T) {
	c, rec := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/env-groups":
			// Exactly what the real list endpoint does: no envVars.
			fmt.Fprint(w, `[{"envGroup":{"id":"evg-1","name":"grp","envVars":[]}}]`)
		case r.URL.Path == "/env-groups/evg-1":
			fmt.Fprint(w, `{"envGroup":{"id":"evg-1","name":"grp",
			  "envVars":[{"key":"SHARED","value":"v"}],
			  "serviceLinks":[{"id":"srv-a","name":"a","type":"web"}]}}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	})

	got, err := c.ListEnvGroups(context.Background())
	if err != nil {
		t.Fatalf("ListEnvGroups: %v", err)
	}
	if len(got) != 1 || len(got[0].EnvVars) != 1 {
		t.Fatalf("group not populated: %+v", got)
	}
	if len(rec.requests) != 2 {
		t.Fatalf("requests = %v, want a list plus a per-group GET", rec.requests)
	}
}

func TestGroupsForService(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/env-groups":
			fmt.Fprint(w, `[{"envGroup":{"id":"evg-1"}},{"envGroup":{"id":"evg-2"}}]`)
		case "/env-groups/evg-1":
			fmt.Fprint(w, `{"id":"evg-1","serviceLinks":[{"id":"srv-a"},{"id":"srv-b"}]}`)
		case "/env-groups/evg-2":
			fmt.Fprint(w, `{"id":"evg-2","serviceLinks":[{"id":"srv-z"}]}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	})

	got, err := c.GroupsForService(context.Background(), "srv-b")
	if err != nil {
		t.Fatalf("GroupsForService: %v", err)
	}
	if len(got) != 1 || got[0].ID != "evg-1" {
		t.Fatalf("got %+v", got)
	}

	none, err := c.GroupsForService(context.Background(), "srv-none")
	if err != nil {
		t.Fatalf("GroupsForService: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("got %+v, want no groups", none)
	}
}

// TestPutIsPerKeyNeverCollection is the central safety assertion of this
// package. The recorder fails the test if a collection write is ever seen.
func TestPutIsPerKeyNeverCollection(t *testing.T) {
	var gotPath, gotBody string
	c, rec := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	})

	err := c.PutServiceEnvVar(context.Background(), "srv-x", "MY_KEY", secret.New("plaintext"))
	if err != nil {
		t.Fatalf("PutServiceEnvVar: %v", err)
	}
	if gotPath != "/services/srv-x/env-vars/MY_KEY" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotBody != `{"value":"plaintext"}` {
		t.Fatalf("body = %q", gotBody)
	}
	if len(rec.requests) != 1 || rec.requests[0] != "PUT /services/srv-x/env-vars/MY_KEY" {
		t.Fatalf("requests = %v", rec.requests)
	}
}

func TestCollectionWriteIsRefusedBeforeTheNetwork(t *testing.T) {
	c, rec := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("a refused request reached the network")
	})

	err := c.PutServiceEnvVar(context.Background(), "srv-x", "", secret.New("v"))
	if err == nil {
		t.Fatal("expected refusal for an empty key")
	}
	if !strings.Contains(err.Error(), "collection endpoint") {
		t.Errorf("error = %v", err)
	}

	err = c.DeleteServiceEnvVar(context.Background(), "srv-x", "")
	if err == nil {
		t.Fatal("expected refusal for an empty key on delete")
	}

	if len(rec.requests) != 0 {
		t.Fatalf("requests = %v, want none", rec.requests)
	}
}

func TestDeleteServiceEnvVar(t *testing.T) {
	c, rec := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.DeleteServiceEnvVar(context.Background(), "srv-x", "KEY"); err != nil {
		t.Fatalf("DeleteServiceEnvVar: %v", err)
	}
	if rec.requests[0] != "DELETE /services/srv-x/env-vars/KEY" {
		t.Fatalf("requests = %v", rec.requests)
	}
}

func TestDeployIsExplicit(t *testing.T) {
	c, rec := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"dep-123"}`)
	})
	id, err := c.Deploy(context.Background(), "srv-x")
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if id != "dep-123" {
		t.Fatalf("id = %q", id)
	}
	if rec.requests[0] != "POST /services/srv-x/deploys" {
		t.Fatalf("requests = %v", rec.requests)
	}
}

func TestDeployWrapped(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"deploy":{"id":"dep-wrapped"}}`)
	})
	id, err := c.Deploy(context.Background(), "srv-x")
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if id != "dep-wrapped" {
		t.Fatalf("id = %q", id)
	}
}

// TestWritesToEnvGroupsAreRefused covers the v1 promise that renv reads
// environment groups to resolve precedence but never modifies them, so a
// mistake cannot take out configuration shared by several services.
func TestWritesToEnvGroupsAreRefused(t *testing.T) {
	err := render.GuardWriteForTest(http.MethodPut, "env-groups/evg-1")
	if err == nil {
		t.Fatal("expected refusal")
	}
	var ge *render.GroupWriteError
	if !errors.As(err, &ge) {
		t.Fatalf("error = %v, want GroupWriteError", err)
	}
	if !strings.Contains(ge.Error(), "does not write environment groups") {
		t.Errorf("message = %q", ge.Error())
	}
}

// TestDoRefusesCollectionWriteEvenWhenCalledDirectly covers the innermost
// guard. The exported API cannot reach it, which is the point: it is what
// would stop a future method that forgot the per-key check.
func TestDoRefusesCollectionWriteEvenWhenCalledDirectly(t *testing.T) {
	c, rec := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("a refused request reached the network")
	})

	_, err := c.DoForTest(context.Background(), http.MethodPut,
		"services/srv-x/env-vars", []byte(`{"envVars":[]}`))
	var ce *render.CollectionWriteError
	if !errors.As(err, &ce) {
		t.Fatalf("error = %v, want CollectionWriteError", err)
	}
	if len(rec.requests) != 0 {
		t.Fatalf("requests = %v, want none", rec.requests)
	}
}

func TestGuardWrite(t *testing.T) {
	tests := []struct {
		method, path string
		wantErr      bool
		wantKind     any
	}{
		{http.MethodGet, "services/srv-x/env-vars", false, nil},
		{http.MethodGet, "env-groups/evg-1", false, nil},
		{http.MethodPut, "services/srv-x/env-vars", true, &render.CollectionWriteError{}},
		{http.MethodPut, "services/srv-x/env-vars?limit=100", true, &render.CollectionWriteError{}},
		{http.MethodDelete, "services/srv-x/env-vars", true, &render.CollectionWriteError{}},
		{http.MethodPost, "env-groups", true, &render.GroupWriteError{}},
		// Anything under env-groups is refused as a group write, including
		// per-key paths that would be permitted on a service.
		{http.MethodPut, "env-groups/evg-1/env-vars/K", true, &render.GroupWriteError{}},
		{http.MethodPut, "services/srv-x/env-vars/KEY", false, nil},
		{http.MethodPost, "services/srv-x/deploys", false, nil},
	}
	for _, tc := range tests {
		err := render.GuardWriteForTest(tc.method, tc.path)
		if tc.wantErr && err == nil {
			t.Errorf("%s %s: expected refusal", tc.method, tc.path)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s %s: unexpected refusal: %v", tc.method, tc.path, err)
		}
		if tc.wantErr {
			switch tc.wantKind.(type) {
			case *render.CollectionWriteError:
				var e *render.CollectionWriteError
				if !errors.As(err, &e) {
					t.Errorf("%s %s: error = %v, want CollectionWriteError", tc.method, tc.path, err)
				} else if !strings.Contains(e.Error(), "returns 200") {
					t.Errorf("message should explain the 200: %q", e.Error())
				}
			case *render.GroupWriteError:
				var e *render.GroupWriteError
				if !errors.As(err, &e) {
					t.Errorf("%s %s: error = %v, want GroupWriteError", tc.method, tc.path, err)
				}
			}
		}
	}
}

func TestNonSuccessIsFatalWithNoEmptyFallback(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 429, 500, 503} {
		c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"message":"failure %d"}`, status)
		})
		got, err := c.ListServices(context.Background())
		if err == nil {
			t.Fatalf("status %d returned no error", status)
		}
		if got != nil {
			t.Fatalf("status %d returned %d services alongside the error", status, len(got))
		}
		var ae *render.APIError
		if !errors.As(err, &ae) {
			t.Fatalf("status %d: error = %v, want APIError", status, err)
		}
		if ae.StatusCode != status {
			t.Errorf("APIError.StatusCode = %d, want %d", ae.StatusCode, status)
		}
		if !strings.Contains(ae.Error(), fmt.Sprintf("HTTP %d", status)) {
			t.Errorf("message = %q", ae.Error())
		}
	}
}

// TestPageLimitTooLargeIsSurfaced reproduces the exact 400 recon hit.
func TestPageLimitTooLargeIsSurfaced(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"message":"invalid limit: too large"}`)
	})
	_, err := c.ListServices(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid limit: too large") {
		t.Fatalf("err = %v", err)
	}
}

func TestLongErrorBodyIsTruncated(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, strings.Repeat("x", 2000))
	})
	_, err := c.ListServices(context.Background())
	var ae *render.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v", err)
	}
	if len(ae.Body) > 600 {
		t.Fatalf("body not truncated: %d chars", len(ae.Body))
	}
	if !strings.HasSuffix(ae.Body, "…") {
		t.Errorf("truncation marker missing")
	}
}

func TestMalformedJSONIsAnError(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{not json`)
	})
	if _, err := c.ListServices(context.Background()); err == nil {
		t.Fatal("expected decode error for a list")
	}
	if _, err := c.GetService(context.Background(), "srv-x"); err == nil {
		t.Fatal("expected decode error for an object")
	}
	if _, err := c.GetEnvGroup(context.Background(), "evg-1"); err == nil {
		t.Fatal("expected decode error for a group")
	}
	if _, err := c.Deploy(context.Background(), "srv-x"); err == nil {
		t.Fatal("expected decode error for a deploy")
	}
}

func TestMalformedListElements(t *testing.T) {
	cases := []string{
		`[{"service":"not-an-object"}]`,
		`[{"id":123}]`,
		`[{"service":{"id":"a"},"cursor":99}]`,
		`[1,2]`, // elements that are not objects at all
	}
	for _, body := range cases {
		c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, body)
		})
		if _, err := c.ListServices(context.Background()); err == nil {
			t.Errorf("body %s: expected error", body)
		}
	}
}

// TestEveryCallSurfacesANonSuccessStatus checks that no method quietly
// degrades to an empty result. Recon produced two confidently wrong
// conclusions from exactly this failure mode.
func TestEveryCallSurfacesANonSuccessStatus(t *testing.T) {
	newFailing := func(t *testing.T) *render.Client {
		c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"boom"}`)
		})
		return c
	}
	ctx := context.Background()

	if _, err := newFailing(t).GetService(ctx, "srv-x"); err == nil {
		t.Error("GetService swallowed a 500")
	}
	if _, err := newFailing(t).ListServiceEnvVars(ctx, "srv-x"); err == nil {
		t.Error("ListServiceEnvVars swallowed a 500")
	}
	if _, err := newFailing(t).GetEnvGroup(ctx, "evg-1"); err == nil {
		t.Error("GetEnvGroup swallowed a 500")
	}
	if _, err := newFailing(t).ListEnvGroups(ctx); err == nil {
		t.Error("ListEnvGroups swallowed a 500")
	}
	if _, err := newFailing(t).GroupsForService(ctx, "srv-x"); err == nil {
		t.Error("GroupsForService swallowed a 500")
	}
	if err := newFailing(t).PutServiceEnvVar(ctx, "srv-x", "K", secret.New("v")); err == nil {
		t.Error("PutServiceEnvVar swallowed a 500")
	}
	if err := newFailing(t).DeleteServiceEnvVar(ctx, "srv-x", "K"); err == nil {
		t.Error("DeleteServiceEnvVar swallowed a 500")
	}
	if _, err := newFailing(t).Deploy(ctx, "srv-x"); err == nil {
		t.Error("Deploy swallowed a 500")
	}
}

// TestListEnvGroupsFailsIfAGroupFetchFails covers the per-group GET, where a
// partial result would look exactly like a group with no variables.
func TestListEnvGroupsFailsIfAGroupFetchFails(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/env-groups" {
			fmt.Fprint(w, `[{"envGroup":{"id":"evg-1"}}]`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"nope"}`)
	})
	got, err := c.ListEnvGroups(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if got != nil {
		t.Fatalf("returned %d groups alongside the error", len(got))
	}
}

func TestUnreadableBodyIsAnError(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Promise more bytes than are written, then hang up: the client's
		// read of the body fails partway through.
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"service":{"id":"srv-1"}}]`))
	})
	if _, err := c.ListServices(context.Background()); err == nil {
		t.Fatal("expected a body read error")
	}
}

func TestTransportErrorIsSurfaced(t *testing.T) {
	c, err := render.New(secret.New(testKey), render.Options{
		BaseURL: "http://127.0.0.1:1", // nothing listening
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListServices(context.Background()); err == nil {
		t.Fatal("expected transport error")
	}
}

func TestRequestBuildErrorIsSurfaced(t *testing.T) {
	c, err := render.New(secret.New(testKey), render.Options{BaseURL: "http://\x7f invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListServices(context.Background()); err == nil {
		t.Fatal("expected request build error")
	}
}

func TestContextCancellation(t *testing.T) {
	c, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ListServices(ctx); err == nil {
		t.Fatal("expected cancellation error")
	}
}
