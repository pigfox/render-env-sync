package cli_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigfox/render-env-sync/internal/cli"
	"github.com/pigfox/render-env-sync/internal/config"
	"github.com/pigfox/render-env-sync/internal/render"
	"github.com/pigfox/render-env-sync/internal/secret"
)

// fakeAPI is a scripted Render API. Every write is recorded so that tests can
// assert on what a dry run did *not* do.
type fakeAPI struct {
	services    []render.Service
	service     render.Service
	serviceVars []render.EnvVar
	groups      []render.EnvGroup

	puts    []string
	deletes []string
	deploys int

	errList     error
	errGet      error
	errVars     error
	errGroups   error
	errPut      error
	errDelete   error
	errDeploy   error
	deployID    string
	getServiceN int
}

func (f *fakeAPI) ListServices(context.Context) ([]render.Service, error) {
	return f.services, f.errList
}
func (f *fakeAPI) GetService(_ context.Context, id string) (render.Service, error) {
	f.getServiceN++
	if f.errGet != nil {
		return render.Service{}, f.errGet
	}
	return f.service, nil
}
func (f *fakeAPI) ListServiceEnvVars(context.Context, string) ([]render.EnvVar, error) {
	return f.serviceVars, f.errVars
}
func (f *fakeAPI) GroupsForService(context.Context, string) ([]render.EnvGroup, error) {
	return f.groups, f.errGroups
}
func (f *fakeAPI) PutServiceEnvVar(_ context.Context, _, key string, v secret.Secret) error {
	if f.errPut != nil {
		return f.errPut
	}
	f.puts = append(f.puts, key+"="+v.Fingerprint())
	return nil
}
func (f *fakeAPI) DeleteServiceEnvVar(_ context.Context, _, key string) error {
	if f.errDelete != nil {
		return f.errDelete
	}
	f.deletes = append(f.deletes, key)
	return nil
}
func (f *fakeAPI) Deploy(context.Context, string) (string, error) {
	if f.errDeploy != nil {
		return "", f.errDeploy
	}
	f.deploys++
	if f.deployID == "" {
		return "dep-1", nil
	}
	return f.deployID, nil
}

func vars(pairs ...string) []render.EnvVar {
	out := make([]render.EnvVar, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, render.EnvVar{Key: pairs[i], Value: secret.New(pairs[i+1])})
	}
	return out
}

// harness wires an App to buffers and a temp config.
type harness struct {
	t          *testing.T
	app        *cli.App
	api        *fakeAPI
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	dir        string
	configPath string
	envPath    string
}

const configTemplate = `
version: 1
deny_prefixes:
  - DEMO_*
  - RENDER_API_KEY
projects:
  proj:
    environments:
      default:
        service: srv-TEST0000
        service_name: example.com
        owner: tea-TEST0000
        env_files:
          - %s
        manage:
          SAME_KEY: service
          DIFF_KEY: service
          LOCAL_KEY: service
          REMOTE_KEY: service
          GROUP_KEY: group
`

func newHarness(t *testing.T, envContent string) *harness {
	t.Helper()
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte(envContent), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(fmt.Sprintf(configTemplate, envPath)), 0o600); err != nil {
		t.Fatal(err)
	}

	api := &fakeAPI{
		service: render.Service{ID: "srv-TEST0000", Name: "example.com", OwnerID: "tea-TEST0000", Type: "web_service"},
	}
	h := &harness{
		t: t, api: api, dir: dir, configPath: configPath, envPath: envPath,
		stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
	}
	h.app = &cli.App{
		Stdout: h.stdout,
		Stderr: h.stderr,
		Getenv: func(k string) string {
			if k == "RENDER_API_KEY" {
				return "rnd_TESTKEY"
			}
			return ""
		},
		NewAPI: func(*config.Config, secret.Secret) (cli.API, error) { return api, nil },
	}
	return h
}

func (h *harness) run(args ...string) int {
	h.t.Helper()
	full := append([]string{args[0], "--config", h.configPath}, args[1:]...)
	return h.app.Run(context.Background(), full)
}

func (h *harness) out() string { return h.stdout.String() }
func (h *harness) err() string { return h.stderr.String() }

func TestUsageAndUnknownCommand(t *testing.T) {
	h := newHarness(t, "")
	if code := h.app.Run(context.Background(), nil); code != cli.ExitError {
		t.Errorf("no args exit = %d", code)
	}
	if !strings.Contains(h.err(), "Usage:") {
		t.Error("usage not printed for no args")
	}

	h = newHarness(t, "")
	if code := h.app.Run(context.Background(), []string{"bogus"}); code != cli.ExitError {
		t.Errorf("unknown command exit = %d", code)
	}
	if !strings.Contains(h.err(), "unknown command") {
		t.Error("unknown command not reported")
	}

	h = newHarness(t, "")
	if code := h.app.Run(context.Background(), []string{"help"}); code != cli.ExitOK {
		t.Errorf("help exit = %d", code)
	}
	if !strings.Contains(h.out(), "renv <command>") {
		t.Error("help not printed")
	}
}

func TestBadFlag(t *testing.T) {
	h := newHarness(t, "")
	if code := h.app.Run(context.Background(), []string{"status", "--nope"}); code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
}

func TestInit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	h := newHarness(t, "")

	if code := h.app.Run(context.Background(), []string{"init", "--config", path}); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if !strings.Contains(h.out(), "wrote "+path) {
		t.Errorf("output = %q", h.out())
	}

	// Second run leaves it alone.
	h2 := newHarness(t, "")
	if code := h2.app.Run(context.Background(), []string{"init", "--config", path}); code != cli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(h2.out(), "already exists") {
		t.Errorf("output = %q", h2.out())
	}
}

func TestInitFailure(t *testing.T) {
	h := newHarness(t, "")
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := h.app.Run(context.Background(), []string{"init", "--config", filepath.Join(blocked, "sub", "c.yaml")})
	if code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
}

func TestMissingAPIKey(t *testing.T) {
	h := newHarness(t, "")
	h.app.Getenv = func(string) string { return "" }
	if code := h.run("services"); code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
	if !strings.Contains(h.err(), "RENDER_API_KEY is not set") {
		t.Errorf("stderr = %q", h.err())
	}
}

func TestMissingConfig(t *testing.T) {
	h := newHarness(t, "")
	code := h.app.Run(context.Background(), []string{"status", "--config", filepath.Join(t.TempDir(), "absent.yaml")})
	if code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
}

func TestServices(t *testing.T) {
	h := newHarness(t, "")
	h.api.services = []render.Service{
		{ID: "srv-b", Name: "zeta", Type: "web_service", Suspended: "not_suspended"},
		{ID: "srv-a", Name: "alpha", Type: "static_site", Suspended: "suspended"},
	}
	if code := h.run("services"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	out := h.out()
	if strings.Index(out, "alpha") > strings.Index(out, "zeta") {
		t.Error("services not sorted by name")
	}
	if !strings.Contains(out, "srv-a") || !strings.Contains(out, "suspended") {
		t.Errorf("output = %q", out)
	}
}

func TestServicesAPIError(t *testing.T) {
	h := newHarness(t, "")
	h.api.errList = errors.New("boom")
	if code := h.run("services"); code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
}

func TestDiffClassifiesAndExitsZeroWhenClean(t *testing.T) {
	h := newHarness(t, "SAME_KEY=v\n")
	h.api.serviceVars = vars("SAME_KEY", "v")

	if code := h.run("diff"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s\n%s", code, h.err(), h.out())
	}
	if !strings.Contains(h.out(), "SAME") {
		t.Errorf("output = %q", h.out())
	}
}

// TestDiffExitsOneOnDrift is what makes the command usable in CI.
func TestDiffExitsOneOnDrift(t *testing.T) {
	h := newHarness(t, "SAME_KEY=v\nDIFF_KEY=local\n")
	h.api.serviceVars = vars("SAME_KEY", "v", "DIFF_KEY", "remote")

	if code := h.run("diff"); code != cli.ExitDrift {
		t.Fatalf("exit = %d, want %d", code, cli.ExitDrift)
	}
	if !strings.Contains(h.out(), "DIFFERS") {
		t.Errorf("output = %q", h.out())
	}
}

// TestDiffNeverPrintsPlaintext is the CLI-level leak assertion.
func TestDiffNeverPrintsPlaintext(t *testing.T) {
	const canary = "CANARY-PLAINTEXT-VALUE-abc123"
	h := newHarness(t, "DIFF_KEY="+canary+"\n")
	h.api.serviceVars = vars("DIFF_KEY", "remote-"+canary)

	h.run("diff")
	if strings.Contains(h.out()+h.err(), canary) {
		t.Fatalf("plaintext reached the terminal:\n%s", h.out())
	}
	if !strings.Contains(h.out(), secret.New(canary).Fingerprint()) {
		t.Error("fingerprint not shown")
	}
}

func TestDiffWithExplicitTargetAndGroups(t *testing.T) {
	h := newHarness(t, "SAME_KEY=v\n")
	h.api.serviceVars = vars("SAME_KEY", "v")
	h.api.groups = []render.EnvGroup{{ID: "evg-1", Name: "shared", EnvVars: vars("GROUP_KEY", "g")}}

	if code := h.run("diff", "proj/default"); code != cli.ExitDrift {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if !strings.Contains(h.out(), "groups shared") {
		t.Errorf("group name not shown: %q", h.out())
	}
	if !strings.Contains(h.out(), "REMOTE_ONLY") {
		t.Errorf("group-supplied key not classified: %q", h.out())
	}
}

func TestDiffUnknownTarget(t *testing.T) {
	h := newHarness(t, "")
	if code := h.run("diff", "nope"); code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
}

func TestDiffAPIErrors(t *testing.T) {
	for name, apply := range map[string]func(*fakeAPI){
		"get service": func(f *fakeAPI) { f.errGet = errors.New("x") },
		"list vars":   func(f *fakeAPI) { f.errVars = errors.New("x") },
		"list groups": func(f *fakeAPI) { f.errGroups = errors.New("x") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "SAME_KEY=v\n")
			apply(h.api)
			if code := h.run("diff"); code != cli.ExitError {
				t.Errorf("exit = %d", code)
			}
		})
	}
}

func TestLocalFileErrors(t *testing.T) {
	h := newHarness(t, "not an assignment\n")
	if code := h.run("diff"); code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
	if !strings.Contains(h.err(), "KEY=VALUE") {
		t.Errorf("stderr = %q", h.err())
	}
}

func TestStatus(t *testing.T) {
	h := newHarness(t, "SAME_KEY=v\nDIFF_KEY=l\nUNLISTED=x\n")
	h.api.serviceVars = vars("SAME_KEY", "v", "DIFF_KEY", "r")

	if code := h.run("status"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if !strings.Contains(h.out(), "proj/default") || !strings.Contains(h.out(), "UNMANAGED") {
		t.Errorf("output = %q", h.out())
	}
}

func TestStatusAPIError(t *testing.T) {
	h := newHarness(t, "")
	h.api.errGet = errors.New("boom")
	if code := h.run("status"); code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
}

// TestPushRequiresExplicitTarget pins the rule that there is no default push
// target.
func TestPushRequiresExplicitTarget(t *testing.T) {
	h := newHarness(t, "LOCAL_KEY=v\n")
	if code := h.run("push"); code != cli.ExitError {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(h.err(), "explicit target") {
		t.Errorf("stderr = %q", h.err())
	}
	if len(h.api.puts) != 0 {
		t.Error("a write happened without a target")
	}
}

func TestPushDryRunByDefault(t *testing.T) {
	h := newHarness(t, "LOCAL_KEY=v\n")
	if code := h.run("push", "proj/default"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if !strings.Contains(h.out(), "create") || !strings.Contains(h.out(), "dry run") {
		t.Errorf("output = %q", h.out())
	}
	if len(h.api.puts) != 0 {
		t.Fatalf("dry run wrote %v", h.api.puts)
	}
}

func TestPushApplyCreates(t *testing.T) {
	h := newHarness(t, "LOCAL_KEY=v\n")
	if code := h.run("push", "proj/default", "--apply"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if len(h.api.puts) != 1 || !strings.HasPrefix(h.api.puts[0], "LOCAL_KEY=") {
		t.Fatalf("puts = %v", h.api.puts)
	}
	if !strings.Contains(h.out(), "next deploy") {
		t.Errorf("should explain that a deploy is needed: %q", h.out())
	}
	if h.api.deploys != 0 {
		t.Error("push deployed without being asked")
	}
}

// TestPushLeavesDifferingValuesAloneWithoutUpdate is the second safety gate:
// creating a missing key is one decision, overwriting an existing one is
// another.
func TestPushLeavesDifferingValuesAloneWithoutUpdate(t *testing.T) {
	h := newHarness(t, "DIFF_KEY=local\n")
	h.api.serviceVars = vars("DIFF_KEY", "remote")

	if code := h.run("push", "proj/default", "--apply"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if len(h.api.puts) != 0 {
		t.Fatalf("overwrote a differing value without --update: %v", h.api.puts)
	}
	if !strings.Contains(h.out(), "--update") {
		t.Errorf("output should mention --update: %q", h.out())
	}
}

func TestPushUpdateOverwrites(t *testing.T) {
	h := newHarness(t, "DIFF_KEY=local\n")
	h.api.serviceVars = vars("DIFF_KEY", "remote")

	if code := h.run("push", "proj/default", "--apply", "--update"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if len(h.api.puts) != 1 {
		t.Fatalf("puts = %v", h.api.puts)
	}
	if !strings.Contains(h.out(), "update") {
		t.Errorf("output = %q", h.out())
	}
}

// TestPushRefusesGroupHomedKeys covers the promise that v1 never writes group
// configuration, including indirectly by writing it to the service.
func TestPushRefusesGroupHomedKeys(t *testing.T) {
	h := newHarness(t, "GROUP_KEY=v\n")
	if code := h.run("push", "proj/default", "--apply"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if len(h.api.puts) != 0 {
		t.Fatalf("wrote a group-homed key to the service: %v", h.api.puts)
	}
	if !strings.Contains(h.out(), "renv does not write groups") {
		t.Errorf("output = %q", h.out())
	}
}

func TestPushPruneRequiresConfirmation(t *testing.T) {
	h := newHarness(t, "")
	h.api.serviceVars = vars("REMOTE_KEY", "r")
	h.app.Stdin = strings.NewReader("no\n")

	if code := h.run("push", "proj/default", "--apply", "--prune"); code != cli.ExitError {
		t.Fatalf("exit = %d", code)
	}
	if len(h.api.deletes) != 0 {
		t.Fatalf("deleted despite declining: %v", h.api.deletes)
	}
	if !strings.Contains(h.err(), "aborted") {
		t.Errorf("stderr = %q", h.err())
	}
}

func TestPushPruneConfirmed(t *testing.T) {
	h := newHarness(t, "")
	h.api.serviceVars = vars("REMOTE_KEY", "r")
	h.app.Stdin = strings.NewReader("yes\n")

	if code := h.run("push", "proj/default", "--apply", "--prune"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if len(h.api.deletes) != 1 || h.api.deletes[0] != "REMOTE_KEY" {
		t.Fatalf("deletes = %v", h.api.deletes)
	}
}

func TestPushPruneWithNoStdinDeclines(t *testing.T) {
	h := newHarness(t, "")
	h.api.serviceVars = vars("REMOTE_KEY", "r")
	h.app.Stdin = nil

	if code := h.run("push", "proj/default", "--apply", "--prune"); code != cli.ExitError {
		t.Fatalf("exit = %d", code)
	}
	if len(h.api.deletes) != 0 {
		t.Error("deleted without a confirmation source")
	}
}

func TestPushPruneEmptyStdinDeclines(t *testing.T) {
	h := newHarness(t, "")
	h.api.serviceVars = vars("REMOTE_KEY", "r")
	h.app.Stdin = strings.NewReader("")

	if code := h.run("push", "proj/default", "--apply", "--prune"); code != cli.ExitError {
		t.Fatalf("exit = %d", code)
	}
}

func TestPushDeploy(t *testing.T) {
	h := newHarness(t, "LOCAL_KEY=v\n")
	h.api.deployID = "dep-xyz"
	if code := h.run("push", "proj/default", "--apply", "--deploy"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if h.api.deploys != 1 {
		t.Errorf("deploys = %d", h.api.deploys)
	}
	if !strings.Contains(h.out(), "dep-xyz") {
		t.Errorf("output = %q", h.out())
	}
}

func TestPushWriteErrors(t *testing.T) {
	t.Run("put", func(t *testing.T) {
		h := newHarness(t, "LOCAL_KEY=v\n")
		h.api.errPut = errors.New("boom")
		if code := h.run("push", "proj/default", "--apply"); code != cli.ExitError {
			t.Errorf("exit = %d", code)
		}
	})
	t.Run("delete", func(t *testing.T) {
		h := newHarness(t, "")
		h.api.serviceVars = vars("REMOTE_KEY", "r")
		h.api.errDelete = errors.New("boom")
		h.app.Stdin = strings.NewReader("yes\n")
		if code := h.run("push", "proj/default", "--apply", "--prune"); code != cli.ExitError {
			t.Errorf("exit = %d", code)
		}
	})
	t.Run("deploy", func(t *testing.T) {
		h := newHarness(t, "LOCAL_KEY=v\n")
		h.api.errDeploy = errors.New("boom")
		if code := h.run("push", "proj/default", "--apply", "--deploy"); code != cli.ExitError {
			t.Errorf("exit = %d", code)
		}
	})
}

func TestPushNothingToDo(t *testing.T) {
	h := newHarness(t, "SAME_KEY=v\n")
	h.api.serviceVars = vars("SAME_KEY", "v")
	if code := h.run("push", "proj/default", "--apply"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if !strings.Contains(h.out(), "nothing to do") {
		t.Errorf("output = %q", h.out())
	}
}

func TestPushUnknownTarget(t *testing.T) {
	h := newHarness(t, "")
	if code := h.run("push", "nope/nope"); code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
}

func TestPushAPIErrorDuringResolve(t *testing.T) {
	h := newHarness(t, "LOCAL_KEY=v\n")
	h.api.errGet = errors.New("boom")
	if code := h.run("push", "proj/default"); code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
}

func TestPullCreatesLocally(t *testing.T) {
	h := newHarness(t, "SAME_KEY=v\n")
	h.api.serviceVars = vars("SAME_KEY", "v", "REMOTE_KEY", "fetched")

	if code := h.run("pull", "proj/default", "--apply"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	got, err := os.ReadFile(h.envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "REMOTE_KEY=fetched") {
		t.Fatalf("file = %q", got)
	}
	if !strings.Contains(string(got), "SAME_KEY=v") {
		t.Error("existing content lost")
	}
}

func TestPullDryRun(t *testing.T) {
	h := newHarness(t, "")
	h.api.serviceVars = vars("REMOTE_KEY", "fetched")

	if code := h.run("pull", "proj/default"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	got, err := os.ReadFile(h.envPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "REMOTE_KEY") {
		t.Fatal("dry run modified the file")
	}
	if !strings.Contains(h.out(), "dry run") {
		t.Errorf("output = %q", h.out())
	}
}

func TestPullUpdateAndBackup(t *testing.T) {
	h := newHarness(t, "DIFF_KEY=local\n")
	h.api.serviceVars = vars("DIFF_KEY", "remote")

	if code := h.run("pull", "proj/default", "--apply", "--update"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	got, err := os.ReadFile(h.envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "DIFF_KEY=remote") {
		t.Fatalf("file = %q", got)
	}

	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	backups := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), ".bak.") {
			backups++
		}
	}
	if backups != 1 {
		t.Errorf("expected exactly one backup, found %d", backups)
	}
}

func TestPullPrune(t *testing.T) {
	h := newHarness(t, "LOCAL_KEY=gone\n")
	h.app.Stdin = strings.NewReader("yes\n")

	if code := h.run("pull", "proj/default", "--apply", "--prune"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	got, err := os.ReadFile(h.envPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "LOCAL_KEY") {
		t.Fatalf("key not pruned: %q", got)
	}
}

func TestPullPruneDeclined(t *testing.T) {
	h := newHarness(t, "LOCAL_KEY=stays\n")
	h.app.Stdin = strings.NewReader("no\n")

	if code := h.run("pull", "proj/default", "--apply", "--prune"); code != cli.ExitError {
		t.Fatalf("exit = %d", code)
	}
	got, err := os.ReadFile(h.envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "LOCAL_KEY") {
		t.Error("declined prune still deleted the key")
	}
}

func TestPullRequiresTarget(t *testing.T) {
	h := newHarness(t, "")
	if code := h.run("pull"); code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
}

func TestPullUnknownTarget(t *testing.T) {
	h := newHarness(t, "")
	if code := h.run("pull", "nope"); code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
}

func TestPullNothingToDo(t *testing.T) {
	h := newHarness(t, "SAME_KEY=v\n")
	h.api.serviceVars = vars("SAME_KEY", "v")
	if code := h.run("pull", "proj/default", "--apply", "--update"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if !strings.Contains(h.out(), "nothing to do") {
		t.Errorf("output = %q", h.out())
	}
}

func TestPullAPIError(t *testing.T) {
	h := newHarness(t, "")
	h.api.errVars = errors.New("boom")
	if code := h.run("pull", "proj/default"); code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
}

func TestPullWriteFailure(t *testing.T) {
	h := newHarness(t, "")
	h.api.serviceVars = vars("REMOTE_KEY", "v")
	// Remove the file after config load so the write path fails when it
	// re-reads the sources.
	if err := os.Remove(h.envPath); err != nil {
		t.Fatal(err)
	}
	if code := h.run("pull", "proj/default", "--apply"); code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
}

func TestDoctorHealthy(t *testing.T) {
	h := newHarness(t, "SAME_KEY=v\n")
	if code := h.run("doctor"); code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, h.err())
	}
	if !strings.Contains(h.out(), "all targets resolve") {
		t.Errorf("output = %q", h.out())
	}
	if !strings.Contains(h.out(), "schema version 1") {
		t.Errorf("output = %q", h.out())
	}
}

// TestDoctorDetectsIdentityMismatch covers the assertion that turns a mistyped
// service id into a refusal.
func TestDoctorDetectsIdentityMismatch(t *testing.T) {
	h := newHarness(t, "SAME_KEY=v\n")
	h.api.service.OwnerID = "tea-SOMEONEELSE"

	if code := h.run("doctor"); code != cli.ExitError {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(h.out(), "MISMATCH") {
		t.Errorf("output = %q", h.out())
	}
}

func TestDoctorDetectsNameMismatch(t *testing.T) {
	h := newHarness(t, "SAME_KEY=v\n")
	h.api.service.Name = "someone-elses.com"
	if code := h.run("doctor"); code != cli.ExitError {
		t.Fatalf("exit = %d", code)
	}
}

func TestDoctorUnreachableService(t *testing.T) {
	h := newHarness(t, "SAME_KEY=v\n")
	h.api.errGet = errors.New("404")
	if code := h.run("doctor"); code != cli.ExitError {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(h.out(), "unreachable") {
		t.Errorf("output = %q", h.out())
	}
}

func TestDoctorBrokenLocalFile(t *testing.T) {
	h := newHarness(t, "garbage line\n")
	if code := h.run("doctor"); code != cli.ExitError {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(h.out(), "ERROR") {
		t.Errorf("output = %q", h.out())
	}
}

func TestDoctorMissingConfig(t *testing.T) {
	h := newHarness(t, "")
	code := h.app.Run(context.Background(), []string{"doctor", "--config", filepath.Join(t.TempDir(), "absent.yaml")})
	if code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
}

func TestDoctorMissingKey(t *testing.T) {
	h := newHarness(t, "")
	h.app.Getenv = func(string) string { return "" }
	if code := h.run("doctor"); code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
}

// TestProdRequiresYesProd covers the last gate before a production write.
func TestProdRequiresYesProd(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("LOCAL_KEY=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf(`
version: 1
projects:
  proj:
    environments:
      prod:
        prod: true
        service: srv-TEST0000
        env_files:
          - %s
        manage:
          LOCAL_KEY: service
`, envPath)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	api := &fakeAPI{service: render.Service{ID: "srv-TEST0000", Name: "example.com"}}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := &cli.App{
		Stdout: stdout, Stderr: stderr,
		Getenv: func(string) string { return "rnd_KEY" },
		NewAPI: func(*config.Config, secret.Secret) (cli.API, error) { return api, nil },
	}

	code := app.Run(context.Background(), []string{"push", "--config", cfgPath, "proj/prod", "--apply"})
	if code != cli.ExitError {
		t.Fatalf("exit = %d, want a refusal", code)
	}
	if !strings.Contains(stderr.String(), "--yes-prod") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if len(api.puts) != 0 {
		t.Fatalf("wrote to prod without --yes-prod: %v", api.puts)
	}

	stdout.Reset()
	stderr.Reset()
	code = app.Run(context.Background(), []string{"push", "--config", cfgPath, "proj/prod", "--apply", "--yes-prod"})
	if code != cli.ExitOK {
		t.Fatalf("exit = %d: %s", code, stderr.String())
	}
	if len(api.puts) != 1 {
		t.Fatalf("puts = %v", api.puts)
	}
}

// TestIdentityAssertionBlocksPush is the write-path counterpart to the doctor
// check: a config pointing at the wrong service must not write.
func TestIdentityAssertionBlocksPush(t *testing.T) {
	h := newHarness(t, "LOCAL_KEY=v\n")
	h.api.service.OwnerID = "tea-SOMEONEELSE"

	if code := h.run("push", "proj/default", "--apply"); code != cli.ExitError {
		t.Fatalf("exit = %d", code)
	}
	if len(h.api.puts) != 0 {
		t.Fatalf("wrote to a service with the wrong owner: %v", h.api.puts)
	}
	if !strings.Contains(h.err(), "refusing to write") {
		t.Errorf("stderr = %q", h.err())
	}
}

func TestDefaultAPIConstructor(t *testing.T) {
	// Exercises the production wiring: no NewAPI override.
	h := newHarness(t, "")
	h.app.NewAPI = nil
	h.app.Getenv = func(string) string { return "" }
	if code := h.run("services"); code != cli.ExitError {
		t.Errorf("exit = %d", code)
	}
}

func TestMergeConflictAcrossFilesIsReported(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.env")
	b := filepath.Join(dir, "b.env")
	if err := os.WriteFile(a, []byte("SAME_KEY=one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("SAME_KEY=two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := fmt.Sprintf("version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-x\n        env_files:\n          - %s\n          - %s\n        manage:\n          SAME_KEY: service\n", a, b)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	app := &cli.App{
		Stdout: stdout, Stderr: stderr,
		Getenv: func(string) string { return "rnd_KEY" },
		NewAPI: func(*config.Config, secret.Secret) (cli.API, error) {
			return &fakeAPI{service: render.Service{ID: "srv-x"}}, nil
		},
	}
	if code := app.Run(context.Background(), []string{"diff", "--config", cfgPath}); code != cli.ExitError {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "rather than relying on file order") {
		t.Errorf("stderr = %q", stderr.String())
	}
}
