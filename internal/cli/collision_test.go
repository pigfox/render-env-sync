package cli_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pigfox/render-env-sync/internal/cli"
	"github.com/pigfox/render-env-sync/internal/secret"
)

// diffRow is one parsed line of the diff table.
type diffRow struct {
	status string
	key    string
	local  string
	remote string
}

// parseDiffRows reads the rendered diff table back out of stdout. Asserting on
// the report as printed is the point: a misalignment introduced by the report
// builder would be invisible to a test that inspected delta.Entry values.
func parseDiffRows(t *testing.T, out string) map[string]diffRow {
	t.Helper()
	rows := map[string]diffRow{}
	// STATUS KEY HOME LOCAL REMOTE, whitespace-separated, HOME may be empty.
	re := regexp.MustCompile(`^\s+(SAME|DIFFERS|LOCAL_ONLY|REMOTE_ONLY|SHADOW|UNMANAGED)\s+(\S+)\s+(.*)$`)
	for _, line := range strings.Split(out, "\n") {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		fields := strings.Fields(m[3])
		if len(fields) < 2 {
			t.Fatalf("cannot parse row %q", line)
		}
		// The last two fields are LOCAL and REMOTE; anything before is HOME.
		rows[m[2]] = diffRow{
			status: m[1],
			key:    m[2],
			local:  fields[len(fields)-2],
			remote: fields[len(fields)-1],
		}
	}
	return rows
}

// TestDiffNeverReportsOneKeysFingerprintUnderAnotherKey is named for the
// failure it prevents: a key/value misalignment somewhere between the parser
// and the printed table, which would show key A carrying key B's fingerprint.
//
// That class of bug is invisible to an eyeball review — every fingerprint on
// screen is a real fingerprint of a real value, just attached to the wrong
// row — and it would make the whole report untrustworthy while looking
// entirely normal. The keys below are declared in non-alphabetical order, and
// the report is sorted, so any code that pairs a sorted key list against an
// unsorted value slice mismatches here.
//
// It also exercises the full path rather than delta alone: parse, merge across
// two files, the manifest filter, delta.Compare, and the report builder.
func TestDiffNeverReportsOneKeysFingerprintUnderAnotherKey(t *testing.T) {
	// Deliberately not in sorted order, and split across two files so the
	// merge participates.
	firstFile := map[string]string{
		"ZULU_KEY":  "local-value-zulu",
		"ALPHA_KEY": "local-value-alpha",
	}
	secondFile := map[string]string{
		"MIKE_KEY":   "local-value-mike",
		"BRAVO_KEY":  "local-value-bravo",
		"IGNORED_ME": "local-value-ignored",
	}
	remoteValues := map[string]string{
		"ZULU_KEY":  "remote-value-zulu",
		"ALPHA_KEY": "remote-value-alpha",
		"MIKE_KEY":  "remote-value-mike",
		"BRAVO_KEY": "remote-value-bravo",
	}

	dir := t.TempDir()
	writeEnv := func(name string, kv map[string]string) string {
		p := filepath.Join(dir, name)
		var b strings.Builder
		// Emit in a fixed but non-alphabetical order.
		for _, k := range sortedForFixture(kv) {
			fmt.Fprintf(&b, "%s=%s\n", k, kv[k])
		}
		if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	firstPath := writeEnv("first.env", firstFile)
	secondPath := writeEnv("second.env", secondFile)

	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`
version: 1
projects:
  proj:
    environments:
      default:
        service: srv-TEST0000
        env_files:
          - %s
          - %s
        manage:
          ZULU_KEY: service
          ALPHA_KEY: service
          MIKE_KEY: service
          BRAVO_KEY: service
        local_only:
          - IGNORED_ME
`, firstPath, secondPath)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, "")
	var remotePairs []string
	for _, k := range sortedKeys(remoteValues) {
		remotePairs = append(remotePairs, k, remoteValues[k])
	}
	h.api.serviceVars = vars(remotePairs...)

	if code := h.app.Run(context.Background(), []string{"diff", "--config", cfgPath}); code != cli.ExitDrift {
		t.Fatalf("exit = %d, want drift; stderr: %s", code, h.err())
	}

	rows := parseDiffRows(t, h.out())

	localValues := map[string]string{}
	for k, v := range firstFile {
		localValues[k] = v
	}
	for k, v := range secondFile {
		localValues[k] = v
	}

	seen := map[string]string{}
	for key, want := range remoteValues {
		row, ok := rows[key]
		if !ok {
			t.Fatalf("%s missing from the report:\n%s", key, h.out())
		}

		// Each column must carry that key's own value, not a neighbour's.
		wantLocal := secret.New(localValues[key]).Fingerprint()
		wantRemote := secret.New(want).Fingerprint()
		if row.local != wantLocal {
			t.Errorf("%s local fingerprint = %s, want %s (this key's own value)", key, row.local, wantLocal)
		}
		if row.remote != wantRemote {
			t.Errorf("%s remote fingerprint = %s, want %s (this key's own value)", key, row.remote, wantRemote)
		}
		if row.status != "DIFFERS" {
			t.Errorf("%s status = %s, want DIFFERS", key, row.status)
		}

		// No fingerprint may appear under two different keys, in either
		// column, since every value in this fixture is distinct.
		for col, fp := range map[string]string{"local": row.local, "remote": row.remote} {
			if prev, dup := seen[fp]; dup {
				t.Errorf("fingerprint %s reported for both %s and %s (%s column)", fp, prev, key, col)
			}
			seen[fp] = key
		}
	}

	// The filtered key must not appear at all, and must not have contributed
	// a fingerprint that could collide with a managed one.
	if _, present := rows["IGNORED_ME"]; present {
		t.Errorf("a local_only key appeared in the report:\n%s", h.out())
	}
}

// TestIdenticalValuesUnderDifferentKeysDoShareAFingerprint is the control for
// the test above. Equal values must produce equal fingerprints — that is what
// makes the collision check meaningful rather than vacuous, and it is also the
// real-world case that prompted this investigation: two keys legitimately
// holding the same placeholder.
func TestIdenticalValuesUnderDifferentKeysDoShareAFingerprint(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("SITE_KEY=disabled\nSECRET_KEY=disabled\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf("version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-TEST0000\n        env_files:\n          - %s\n        manage:\n          SITE_KEY: service\n          SECRET_KEY: service\n", envPath)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, "")
	h.api.serviceVars = vars("SITE_KEY", "real-site", "SECRET_KEY", "real-secret")

	if code := h.app.Run(context.Background(), []string{"diff", "--config", cfgPath}); code != cli.ExitDrift {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	rows := parseDiffRows(t, h.out())

	site, secretRow := rows["SITE_KEY"], rows["SECRET_KEY"]
	if site.local != secretRow.local {
		t.Errorf("identical local values produced different fingerprints: %s vs %s", site.local, secretRow.local)
	}
	if site.remote == secretRow.remote {
		t.Errorf("distinct remote values produced the same fingerprint: %s", site.remote)
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Reverse-alphabetical, so callers never accidentally depend on the
	// report's own ordering matching the fixture's.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] > out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func sortedForFixture(m map[string]string) []string { return sortedKeys(m) }
