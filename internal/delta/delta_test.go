package delta_test

import (
	"strings"
	"testing"

	"github.com/pigfox/render-env-sync/internal/delta"
	"github.com/pigfox/render-env-sync/internal/secret"
)

func set(pairs map[string]string) delta.Set {
	out := delta.Set{}
	for k, v := range pairs {
		out[k] = secret.New(v)
	}
	return out
}

func entryFor(t *testing.T, entries []delta.Entry, key string) delta.Entry {
	t.Helper()
	for _, e := range entries {
		if e.Key == key {
			return e
		}
	}
	t.Fatalf("no entry for %q", key)
	return delta.Entry{}
}

func TestResolveRemoteServiceWins(t *testing.T) {
	service := set(map[string]string{"BOTH": "from-service", "ONLY_SVC": "s"})
	group := set(map[string]string{"BOTH": "from-group", "ONLY_GRP": "g"})

	got := delta.ResolveRemote(service, group)
	if len(got) != 3 {
		t.Fatalf("resolved %d keys, want 3", len(got))
	}

	both := got["BOTH"]
	if secret.Reveal(both.Value) != "from-service" {
		t.Errorf("BOTH = %q, want the service value to win", secret.Reveal(both.Value))
	}
	if !both.InService || !both.InGroup {
		t.Errorf("BOTH origin = service:%v group:%v, want both", both.InService, both.InGroup)
	}

	if g := got["ONLY_GRP"]; !g.InGroup || g.InService {
		t.Errorf("ONLY_GRP origin = service:%v group:%v", g.InService, g.InGroup)
	}
	if s := got["ONLY_SVC"]; !s.InService || s.InGroup {
		t.Errorf("ONLY_SVC origin = service:%v group:%v", s.InService, s.InGroup)
	}
}

func TestManifestMatching(t *testing.T) {
	m := delta.Manifest{
		Manage: map[string]delta.Home{
			"KEEP":              delta.HomeService,
			"DEMO_ARBITER_PK":   delta.HomeService,
			"RENDER_API_KEY":    delta.HomeService,
			"LOCAL_SCRATCH_ONE": delta.HomeService,
		},
		DenyPrefixes: []string{"DEMO_*", "RENDER_API_KEY"},
		LocalOnly:    []string{"LOCAL_SCRATCH_*"},
	}

	// Deny wins over an explicit allowlist entry: naming a key in manage
	// must not be able to re-enable a globally denied prefix.
	for _, key := range []string{"DEMO_ARBITER_PK", "DEMO_OPERATOR_ADDR", "RENDER_API_KEY", "LOCAL_SCRATCH_ONE"} {
		if !m.Blocked(key) {
			t.Errorf("%s should be blocked", key)
		}
		if _, ok := m.HomeOf(key); ok {
			t.Errorf("%s should have no home", key)
		}
	}

	if m.Blocked("KEEP") {
		t.Error("KEEP should not be blocked")
	}
	h, ok := m.HomeOf("KEEP")
	if !ok || h != delta.HomeService {
		t.Errorf("HomeOf(KEEP) = %q, %v", h, ok)
	}
	if _, ok := m.HomeOf("NEVER_MENTIONED"); ok {
		t.Error("an unlisted key should not be managed")
	}
	// "RENDER_API_KEY" is an exact deny pattern, not a prefix one.
	if m.Blocked("RENDER_API_KEY_EXTRA") {
		t.Error("exact deny pattern should not match by prefix")
	}
}

func TestHomeValid(t *testing.T) {
	if !delta.HomeService.Valid() || !delta.HomeGroup.Valid() {
		t.Error("declared homes should be valid")
	}
	if delta.Home("elsewhere").Valid() {
		t.Error("unknown home should be invalid")
	}
}

func TestCompareStatuses(t *testing.T) {
	local := set(map[string]string{
		"SAME_KEY":         "v",
		"DIFF_KEY":         "local",
		"LOCAL_ONLY_KEY":   "l",
		"SHADOWED":         "v",
		"NOT_IN_MANIFEST":  "x",
		"DEMO_DEPLOYER_PK": "0xdeadbeef",
	})
	service := set(map[string]string{
		"SAME_KEY": "v",
		"DIFF_KEY": "remote",
		"SHADOWED": "v",
	})
	group := set(map[string]string{
		"SHADOWED":        "v",
		"REMOTE_ONLY_KEY": "r",
	})
	remote := delta.ResolveRemote(service, group)

	m := delta.Manifest{
		Manage: map[string]delta.Home{
			"SAME_KEY":        delta.HomeService,
			"DIFF_KEY":        delta.HomeService,
			"LOCAL_ONLY_KEY":  delta.HomeService,
			"REMOTE_ONLY_KEY": delta.HomeGroup,
			"SHADOWED":        delta.HomeService,
		},
		DenyPrefixes: []string{"DEMO_*"},
	}

	got := delta.Compare(local, remote, m)

	want := map[string]delta.Status{
		"SAME_KEY":         delta.StatusSame,
		"DIFF_KEY":         delta.StatusDiffers,
		"LOCAL_ONLY_KEY":   delta.StatusLocalOnly,
		"REMOTE_ONLY_KEY":  delta.StatusRemoteOnly,
		"SHADOWED":         delta.StatusShadow,
		"NOT_IN_MANIFEST":  delta.StatusUnmanaged,
		"DEMO_DEPLOYER_PK": delta.StatusUnmanaged,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for key, status := range want {
		if e := entryFor(t, got, key); e.Status != status {
			t.Errorf("%s status = %q, want %q", key, e.Status, status)
		}
	}

	// The declared home is carried only for managed keys.
	if e := entryFor(t, got, "REMOTE_ONLY_KEY"); e.Home != delta.HomeGroup {
		t.Errorf("REMOTE_ONLY_KEY home = %q", e.Home)
	}
	if e := entryFor(t, got, "NOT_IN_MANIFEST"); e.Home != "" {
		t.Errorf("unmanaged key carried home %q", e.Home)
	}
}

// TestEqualLengthValuesDiffer is the delta-level guard on the finding that a
// stale and a current credential can share a length.
func TestEqualLengthValuesDiffer(t *testing.T) {
	stale := strings.Repeat("k", 67) + "1"
	current := strings.Repeat("k", 67) + "2"

	local := set(map[string]string{"VENDOR_API_KEY": stale})
	remote := delta.ResolveRemote(set(map[string]string{"VENDOR_API_KEY": current}), nil)
	m := delta.Manifest{Manage: map[string]delta.Home{"VENDOR_API_KEY": delta.HomeService}}

	got := delta.Compare(local, remote, m)
	if got[0].Status != delta.StatusDiffers {
		t.Fatalf("status = %q, want DIFFERS for equal-length distinct values", got[0].Status)
	}
}

func TestCompareIsSortedAndStable(t *testing.T) {
	local := set(map[string]string{"ZED": "1", "ALPHA": "2", "MIKE": "3"})
	m := delta.Manifest{Manage: map[string]delta.Home{
		"ZED": delta.HomeService, "ALPHA": delta.HomeService, "MIKE": delta.HomeService,
	}}
	got := delta.Compare(local, nil, m)

	want := []string{"ALPHA", "MIKE", "ZED"}
	for i, key := range want {
		if got[i].Key != key {
			t.Fatalf("entry %d = %q, want %q", i, got[i].Key, key)
		}
	}
}

func TestCompareEmpty(t *testing.T) {
	got := delta.Compare(nil, nil, delta.Manifest{})
	if len(got) != 0 {
		t.Fatalf("got %d entries, want none", len(got))
	}
	if delta.HasDrift(got) {
		t.Error("empty comparison reported drift")
	}
}

func TestIsDriftAndHasDrift(t *testing.T) {
	drifting := []delta.Status{
		delta.StatusDiffers, delta.StatusLocalOnly, delta.StatusRemoteOnly, delta.StatusShadow,
	}
	for _, s := range drifting {
		e := delta.Entry{Status: s}
		if !e.IsDrift() {
			t.Errorf("%s should count as drift", s)
		}
		if !delta.HasDrift([]delta.Entry{{Status: delta.StatusSame}, e}) {
			t.Errorf("HasDrift missed %s", s)
		}
	}
	for _, s := range []delta.Status{delta.StatusSame, delta.StatusUnmanaged} {
		if (delta.Entry{Status: s}).IsDrift() {
			t.Errorf("%s should not count as drift", s)
		}
	}
	if delta.HasDrift([]delta.Entry{{Status: delta.StatusSame}, {Status: delta.StatusUnmanaged}}) {
		t.Error("HasDrift reported drift for a clean comparison")
	}
}

func TestCounts(t *testing.T) {
	got := delta.Counts([]delta.Entry{
		{Status: delta.StatusSame},
		{Status: delta.StatusSame},
		{Status: delta.StatusDiffers},
	})
	if got[delta.StatusSame] != 2 || got[delta.StatusDiffers] != 1 {
		t.Fatalf("counts = %v", got)
	}
	if got[delta.StatusShadow] != 0 {
		t.Errorf("absent status counted %d", got[delta.StatusShadow])
	}
}

// TestGroupSuppliedKeyIsNotReportedMissing covers the case that a
// service-only reader gets wrong: a key present in a linked group and absent
// from the service is already resolved at runtime, not missing.
func TestGroupSuppliedKeyIsNotReportedMissing(t *testing.T) {
	local := set(map[string]string{"EXTERNAL_DATABASE_URL": "postgres://x"})
	remote := delta.ResolveRemote(nil, set(map[string]string{"EXTERNAL_DATABASE_URL": "postgres://x"}))
	m := delta.Manifest{Manage: map[string]delta.Home{"EXTERNAL_DATABASE_URL": delta.HomeGroup}}

	got := delta.Compare(local, remote, m)
	if got[0].Status != delta.StatusSame {
		t.Fatalf("status = %q, want SAME; a group-supplied key is not missing", got[0].Status)
	}
	if got[0].InService {
		t.Error("key should not be marked as service-supplied")
	}
}
