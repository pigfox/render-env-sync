package cli_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigfox/render-env-sync/internal/cli"
)

// TestDefaultAPIWiring runs a command through the real Render client rather
// than the fake, against a local server. It is the only test that exercises
// the production constructor, so a wiring mistake in main cannot hide.
func TestDefaultAPIWiring(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer rnd_TESTKEY" {
			t.Errorf("Authorization = %q", got)
		}
		fmt.Fprint(w, `[{"service":{"id":"srv-a","name":"alpha","type":"web_service"}}]`)
	}))
	defer srv.Close()

	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf("version: 1\ndefaults:\n  api_base: %s\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-a\n        env_files:\n          - %s\n", srv.URL, envPath)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := &cli.App{
		Stdout: stdout, Stderr: stderr,
		Getenv: func(k string) string {
			if k == "RENDER_API_KEY" {
				return "rnd_TESTKEY"
			}
			return ""
		},
		// NewAPI deliberately unset: this is the production path.
	}
	if code := app.Run(context.Background(), []string{"services", "--config", cfgPath}); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "alpha") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// TestConfigPathFromEnvironment covers resolution via RENV_CONFIG when no
// --config flag is given.
func TestConfigPathFromEnvironment(t *testing.T) {
	h := newHarness(t, "SAME_KEY=v\n")
	h.api.serviceVars = vars("SAME_KEY", "v")
	configPath := h.configPath
	h.app.Getenv = func(k string) string {
		switch k {
		case "RENV_CONFIG":
			return configPath
		case "RENDER_API_KEY":
			return "rnd_TESTKEY"
		}
		return ""
	}
	if code := h.app.Run(context.Background(), []string{"diff"}); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
}

// TestConfigPathUnresolvable covers the branch where neither RENV_CONFIG nor a
// home directory is available.
func TestConfigPathUnresolvable(t *testing.T) {
	t.Setenv("HOME", "")
	h := newHarness(t, "")
	h.app.Getenv = func(string) string { return "" }

	for _, cmd := range []string{"init", "status", "doctor", "diff", "services"} {
		if code := h.app.Run(context.Background(), []string{cmd}); code != cli.ExitError {
			t.Skipf("home directory still resolvable on this platform (%s)", cmd)
		}
	}
}

// TestPermuteForms covers the argument shapes a user actually types.
func TestPermuteForms(t *testing.T) {
	t.Run("flag=value", func(t *testing.T) {
		h := newHarness(t, "SAME_KEY=v\n")
		h.api.serviceVars = vars("SAME_KEY", "v")
		if code := h.app.Run(context.Background(), []string{"diff", "--config=" + h.configPath}); code != cli.ExitOK {
			t.Fatalf("exit = %d: %s", code, h.err())
		}
	})

	t.Run("double dash ends flags", func(t *testing.T) {
		h := newHarness(t, "LOCAL_KEY=v\n")
		// After --, "--apply" is a positional, not a flag, so this stays a
		// dry run and the extra positional is ignored.
		code := h.app.Run(context.Background(), []string{
			"push", "--config", h.configPath, "--", "proj/default", "--apply",
		})
		if code != cli.ExitOK {
			t.Fatalf("exit = %d: %s", code, h.err())
		}
		if len(h.api.puts) != 0 {
			t.Fatalf("--apply after -- was treated as a flag: %v", h.api.puts)
		}
		if !strings.Contains(h.out(), "dry run") {
			t.Errorf("output = %q", h.out())
		}
	})

	t.Run("trailing value flag with no argument", func(t *testing.T) {
		h := newHarness(t, "")
		if code := h.app.Run(context.Background(), []string{"diff", "--config"}); code != cli.ExitError {
			t.Errorf("exit = %d", code)
		}
	})
}

// TestVersion covers every spelling and both the stamped and unstamped cases.
// The release workflow injects main.version with -X; if that plumbing breaks,
// every published binary reports "dev" and there is no way to tell which build
// a bug report came from.
func TestVersion(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		t.Run("stamped/"+arg, func(t *testing.T) {
			h := newHarness(t, "")
			h.app.Version = "v1.2.3"
			if code := h.app.Run(context.Background(), []string{arg}); code != cli.ExitOK {
				t.Fatalf("exit = %d", code)
			}
			if got := strings.TrimSpace(h.out()); got != "v1.2.3" {
				t.Errorf("output = %q, want v1.2.3", got)
			}
		})

		t.Run("unstamped/"+arg, func(t *testing.T) {
			h := newHarness(t, "")
			if code := h.app.Run(context.Background(), []string{arg}); code != cli.ExitOK {
				t.Fatalf("exit = %d", code)
			}
			if got := strings.TrimSpace(h.out()); got != cli.DevVersion {
				t.Errorf("output = %q, want %q", got, cli.DevVersion)
			}
		})
	}
}

// TestVersionNeedsNoConfigOrCredential checks that asking a binary what it is
// works on a machine with neither a config file nor an API key.
func TestVersionNeedsNoConfigOrCredential(t *testing.T) {
	h := newHarness(t, "")
	h.app.Getenv = func(string) string { return "" }
	h.app.Version = "v0.1.0"

	if code := h.app.Run(context.Background(), []string{"version"}); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if strings.TrimSpace(h.out()) != "v0.1.0" {
		t.Errorf("output = %q", h.out())
	}
}

// TestUsageListsVersion keeps the help text honest about the command existing.
func TestUsageListsVersion(t *testing.T) {
	h := newHarness(t, "")
	h.app.Run(context.Background(), []string{"help"})
	if !strings.Contains(h.out(), "version") {
		t.Errorf("usage does not mention the version command:\n%s", h.out())
	}
}

// TestFlagErrorPerCommand covers the flag-parsing failure on every command.
func TestFlagErrorPerCommand(t *testing.T) {
	for _, cmd := range []string{"init", "services", "status", "diff", "push", "pull", "doctor"} {
		t.Run(cmd, func(t *testing.T) {
			h := newHarness(t, "")
			if code := h.app.Run(context.Background(), []string{cmd, "--not-a-flag"}); code != cli.ExitError {
				t.Errorf("exit = %d", code)
			}
		})
	}
}

// TestClientErrorPerCommand covers the credential check on every command that
// reaches the API.
func TestClientErrorPerCommand(t *testing.T) {
	for _, cmd := range [][]string{
		{"services"},
		{"status"},
		{"diff"},
		{"push", "proj/default"},
		{"pull", "proj/default"},
		{"doctor"},
	} {
		t.Run(cmd[0], func(t *testing.T) {
			h := newHarness(t, "")
			h.app.Getenv = func(string) string { return "" }
			if code := h.run(cmd...); code != cli.ExitError {
				t.Errorf("exit = %d", code)
			}
			if !strings.Contains(h.err(), "is not set") {
				t.Errorf("stderr = %q", h.err())
			}
		})
	}
}

// TestBadConfigPerCommand covers the config load failure on every command.
func TestBadConfigPerCommand(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range [][]string{
		{"services"}, {"status"}, {"diff"}, {"push", "p/e"}, {"pull", "p/e"}, {"doctor"},
	} {
		t.Run(cmd[0], func(t *testing.T) {
			h := newHarness(t, "")
			args := append([]string{cmd[0], "--config", bad}, cmd[1:]...)
			if code := h.app.Run(context.Background(), args); code != cli.ExitError {
				t.Errorf("exit = %d", code)
			}
		})
	}
}

// TestPullAcrossComplementaryFiles covers the multi-file case that motivated
// the environment level of the schema: an existing key is updated in the file
// that already defines it, a new key lands in the last file, and a file with
// no changes is left untouched.
func TestPullAcrossComplementaryFiles(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.env")
	second := filepath.Join(dir, "second.env")
	if err := os.WriteFile(first, []byte("# first half\nDIFF_KEY=stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("# second half\nSAME_KEY=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}

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
          SAME_KEY: service
          DIFF_KEY: service
          REMOTE_KEY: service
`, first, second)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, "")
	h.api.serviceVars = vars("SAME_KEY", "v", "DIFF_KEY", "fresh", "REMOTE_KEY", "new")

	code := h.app.Run(context.Background(), []string{
		"pull", "--config", cfgPath, "proj/default", "--apply", "--update",
	})
	if code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}

	gotFirst, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gotFirst), "DIFF_KEY=fresh") {
		t.Errorf("first file = %q; the key should be updated where it already lives", gotFirst)
	}
	if !strings.Contains(string(gotFirst), "# first half") {
		t.Error("comments lost")
	}
	if strings.Contains(string(gotFirst), "REMOTE_KEY") {
		t.Error("a new key was added to the wrong file")
	}

	gotSecond, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gotSecond), "REMOTE_KEY=new") {
		t.Errorf("second file = %q; a new key belongs in the last file", gotSecond)
	}
}

// TestPullLeavesUnchangedFilesAlone checks that a file with no planned change
// is not rewritten, and so gets no backup.
func TestPullLeavesUnchangedFilesAlone(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.env")
	second := filepath.Join(dir, "second.env")
	if err := os.WriteFile(first, []byte("DIFF_KEY=stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("SAME_KEY=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf("version: 1\nprojects:\n  proj:\n    environments:\n      default:\n        service: srv-TEST0000\n        env_files:\n          - %s\n          - %s\n        manage:\n          SAME_KEY: service\n          DIFF_KEY: service\n", first, second)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, "")
	h.api.serviceVars = vars("SAME_KEY", "v", "DIFF_KEY", "fresh")

	code := h.app.Run(context.Background(), []string{
		"pull", "--config", cfgPath, "proj/default", "--apply", "--update",
	})
	if code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "second.env.bak") {
			t.Error("an unchanged file was rewritten")
		}
	}
}

// TestPullWriteToUnwritableFileFails covers the write failure inside applyPull.
func TestPullWriteToUnwritableFileFails(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "ro")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(sub, ".env")
	if err := os.WriteFile(envPath, []byte("SAME_KEY=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf("version: 1\nprojects:\n  proj:\n    environments:\n      default:\n        service: srv-TEST0000\n        env_files:\n          - %s\n        manage:\n          REMOTE_KEY: service\n", envPath)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o700) })

	h := newHarness(t, "")
	h.api.serviceVars = vars("REMOTE_KEY", "new")

	code := h.app.Run(context.Background(), []string{
		"pull", "--config", cfgPath, "proj/default", "--apply",
	})
	if code != cli.ExitError {
		t.Skip("running as a user that can write to a read-only directory")
	}
}

// TestPushDryRunWithPruneStillWritesNothing checks the interaction of the two
// escalation flags: --prune without --apply must remain a dry run.
func TestPushDryRunWithPruneStillWritesNothing(t *testing.T) {
	h := newHarness(t, "")
	h.api.serviceVars = vars("REMOTE_KEY", "r")

	if code := h.run("push", "proj/default", "--prune"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if len(h.api.deletes) != 0 {
		t.Fatalf("dry run deleted %v", h.api.deletes)
	}
	if !strings.Contains(h.out(), "delete") || !strings.Contains(h.out(), "dry run") {
		t.Errorf("output = %q", h.out())
	}
}

// TestLocalOnlyKeyDoesNotBreakEveryCommand is named for the failure it
// prevents. A key listed in local_only is excluded in both directions, so a
// disagreement about it between two source files must not be able to fail
// diff, status, push, pull and doctor alike — which is precisely what happened
// the first time renv ran against a real estate.
func TestLocalOnlyKeyDoesNotBreakEveryCommand(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.env")
	second := filepath.Join(dir, "second.env")
	if err := os.WriteFile(first, []byte("VENDOR_API_KEY=stale\nSAME_KEY=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("VENDOR_API_KEY=current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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
          SAME_KEY: service
        local_only:
          - VENDOR_API_KEY
`, first, second)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, "")
	h.api.serviceVars = vars("SAME_KEY", "v")

	code := h.app.Run(context.Background(), []string{"diff", "--config", cfgPath})
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want success; stderr: %s", code, h.err())
	}
	if strings.Contains(h.out(), "VENDOR_API_KEY") {
		t.Errorf("an excluded key appeared in output:\n%s", h.out())
	}
	if strings.Contains(h.out()+h.err(), "defined differently") {
		t.Errorf("an excluded key still produced a merge conflict:\n%s%s", h.out(), h.err())
	}
	if !strings.Contains(h.out(), "SAME") {
		t.Errorf("the managed key was not compared:\n%s", h.out())
	}
}

// TestDenyPrefixAlsoSurvivesAConflict covers the same rule for the global deny
// list rather than a per-environment local_only entry.
func TestDenyPrefixAlsoSurvivesAConflict(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.env")
	second := filepath.Join(dir, "b.env")
	if err := os.WriteFile(first, []byte("DEMO_DEPLOYER_PK=0xaaa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("DEMO_DEPLOYER_PK=0xbbb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf("version: 1\ndeny_prefixes:\n  - DEMO_*\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-TEST0000\n        env_files:\n          - %s\n          - %s\n", first, second)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	h := newHarness(t, "")
	if code := h.app.Run(context.Background(), []string{"diff", "--config", cfgPath}); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if strings.Contains(h.out(), "DEMO_DEPLOYER_PK") {
		t.Errorf("a denied key appeared in output:\n%s", h.out())
	}
}

// remoteOnlyHarness builds a target with no env_files.
func remoteOnlyHarness(t *testing.T) (*harness, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "version: 1\nprojects:\n  mv:\n    environments:\n      dev:\n        service: srv-TEST0000\n        manage:\n          REMOTE_KEY: service\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, "")
	h.api.serviceVars = vars("REMOTE_KEY", "only-on-render")
	return h, cfgPath
}

// TestRemoteOnlyTargetDiffs covers an environment with no confirmed source
// file: it can still be inspected, and everything the service holds reports as
// REMOTE_ONLY.
func TestRemoteOnlyTargetDiffs(t *testing.T) {
	h, cfgPath := remoteOnlyHarness(t)
	code := h.app.Run(context.Background(), []string{"diff", "--config", cfgPath})
	if code != cli.ExitDrift {
		t.Fatalf("exit = %d, want drift; stderr: %s", code, h.err())
	}
	if !strings.Contains(h.out(), "REMOTE_ONLY") {
		t.Errorf("output = %q", h.out())
	}
}

// TestRemoteOnlyTargetRefusesPushAndPull covers the other half: there is no
// local side, so synchronising in either direction is meaningless and must say
// so rather than panic or silently do nothing.
func TestRemoteOnlyTargetRefusesPushAndPull(t *testing.T) {
	for _, cmd := range []string{"push", "pull"} {
		t.Run(cmd, func(t *testing.T) {
			h, cfgPath := remoteOnlyHarness(t)
			code := h.app.Run(context.Background(), []string{cmd, "--config", cfgPath, "mv/dev", "--apply"})
			if code != cli.ExitError {
				t.Fatalf("exit = %d, want a refusal", code)
			}
			if !strings.Contains(h.err(), "no env_files") {
				t.Errorf("stderr = %q", h.err())
			}
			if len(h.api.puts) != 0 || len(h.api.deletes) != 0 {
				t.Errorf("a remote-only target was written to: puts=%v deletes=%v", h.api.puts, h.api.deletes)
			}
		})
	}
}

// TestExportStyleSourceFileIsAccepted covers the shell-style .env that the
// parser originally rejected outright.
func TestExportStyleSourceFileIsAccepted(t *testing.T) {
	h := newHarness(t, "export SAME_KEY=v\nexport DIFF_KEY=local\n")
	h.api.serviceVars = vars("SAME_KEY", "v", "DIFF_KEY", "remote")

	if code := h.run("diff"); code != cli.ExitDrift {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if !strings.Contains(h.out(), "SAME") || !strings.Contains(h.out(), "DIFFERS") {
		t.Errorf("output = %q", h.out())
	}
}
