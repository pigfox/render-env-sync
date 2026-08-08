package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigfox/render-env-sync/internal/config"
	"github.com/pigfox/render-env-sync/internal/delta"
	"github.com/pigfox/render-env-sync/internal/render"
)

const minimal = `
version: 1
projects:
  proj:
    environments:
      default:
        service: srv-EXAMPLE0000000000
        env_files:
          - /tmp/example/.env
`

func parse(t *testing.T, src string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

func parseErr(t *testing.T, src string) error {
	t.Helper()
	_, err := config.Parse([]byte(src))
	if err == nil {
		t.Fatal("expected an error")
	}
	return err
}

// TestShippedExampleMatchesInitOutput keeps the documented example and the
// file `renv init` writes from ever drifting apart.
func TestShippedExampleMatchesInitOutput(t *testing.T) {
	onDisk, err := os.ReadFile(filepath.Join("..", "..", "example.config.yaml"))
	if err != nil {
		t.Fatalf("reading shipped example: %v", err)
	}
	if string(onDisk) != config.ExampleYAML {
		t.Fatal("example.config.yaml differs from config.ExampleYAML; regenerate it")
	}
}

// TestExampleContainsNoRealIdentifiers guards the promise that nothing in the
// repository names a real service, owner, group, or credential.
func TestExampleContainsNoRealIdentifiers(t *testing.T) {
	cfg := parse(t, config.ExampleYAML)
	for _, target := range cfg.Targets() {
		e, err := cfg.Resolve(target)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", target, err)
		}
		for _, id := range []string{e.ServiceID, e.OwnerID, e.GroupID} {
			if id != "" && !strings.Contains(id, "EXAMPLE") {
				t.Errorf("%s: identifier %q does not look like a placeholder", target, id)
			}
		}
	}
	if strings.Contains(config.ExampleYAML, "rnd_") {
		t.Error("example contains something shaped like a Render API key")
	}
}

func TestExampleParsesAndResolves(t *testing.T) {
	cfg := parse(t, config.ExampleYAML)

	want := []string{"example-multi/dev", "example-multi/prod", "example-single/default"}
	got := cfg.Targets()
	if len(got) != len(want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("targets = %v, want %v", got, want)
		}
	}

	prod, err := cfg.Resolve("example-multi/prod")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !prod.Prod {
		t.Error("prod environment not marked prod")
	}
	if prod.Manage["EXTERNAL_DATABASE_URL"] != delta.HomeGroup {
		t.Errorf("EXTERNAL_DATABASE_URL home = %q", prod.Manage["EXTERNAL_DATABASE_URL"])
	}
	if prod.Target() != "example-multi/prod" {
		t.Errorf("Target() = %q", prod.Target())
	}

	single, err := cfg.Resolve("example-single")
	if err != nil {
		t.Fatalf("bare project with one environment should resolve: %v", err)
	}
	if len(single.EnvFiles) != 2 {
		t.Errorf("expected two complementary env files, got %v", single.EnvFiles)
	}
	if strings.HasPrefix(single.EnvFiles[0], "~") {
		t.Errorf("leading ~ was not expanded: %q", single.EnvFiles[0])
	}
}

func TestDefaults(t *testing.T) {
	cfg := parse(t, minimal)
	if cfg.Defaults.APIBase != render.DefaultBaseURL {
		t.Errorf("APIBase = %q", cfg.Defaults.APIBase)
	}
	if cfg.Defaults.PageLimit != render.MaxPageLimit {
		t.Errorf("PageLimit = %d", cfg.Defaults.PageLimit)
	}
	if cfg.Defaults.APIKeyEnv != "RENDER_API_KEY" {
		t.Errorf("APIKeyEnv = %q", cfg.Defaults.APIKeyEnv)
	}
	if cfg.Version != config.SchemaVersion {
		t.Errorf("Version = %d", cfg.Version)
	}

	opts := cfg.RenderOptions()
	if opts.BaseURL != render.DefaultBaseURL || opts.PageLimit != render.MaxPageLimit {
		t.Errorf("RenderOptions = %+v", opts)
	}
	if cfg.WriteOptions().BackupLayout != cfg.Defaults.BackupTimestamp {
		t.Error("WriteOptions did not carry the configured backup layout")
	}
}

func TestDefaultsOverride(t *testing.T) {
	cfg := parse(t, `
version: 1
defaults:
  api_base: https://api.example.test/v1
  page_limit: 25
  backup_timestamp: 2006-01-02
  api_key_env: MY_KEY
projects:
  p:
    environments:
      e:
        service: srv-x
        env_files: /tmp/a.env
`)
	if cfg.Defaults.APIBase != "https://api.example.test/v1" {
		t.Errorf("APIBase = %q", cfg.Defaults.APIBase)
	}
	if cfg.Defaults.PageLimit != 25 {
		t.Errorf("PageLimit = %d", cfg.Defaults.PageLimit)
	}
	if cfg.Defaults.BackupTimestamp != "2006-01-02" {
		t.Errorf("BackupTimestamp = %q", cfg.Defaults.BackupTimestamp)
	}
	if cfg.Defaults.APIKeyEnv != "MY_KEY" {
		t.Errorf("APIKeyEnv = %q", cfg.Defaults.APIKeyEnv)
	}
	// A single scalar is accepted where a list is expected.
	e, err := cfg.Resolve("p/e")
	if err != nil {
		t.Fatal(err)
	}
	if len(e.EnvFiles) != 1 || e.EnvFiles[0] != "/tmp/a.env" {
		t.Errorf("EnvFiles = %v", e.EnvFiles)
	}
}

// TestPageLimitAboveAPIMaximumIsRejected keeps a configuration file from
// requesting the page size that returns HTTP 400.
func TestPageLimitAboveAPIMaximumIsRejected(t *testing.T) {
	err := parseErr(t, strings.Replace(minimal, "version: 1", "version: 1\ndefaults:\n  page_limit: 200", 1))
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("error should explain the API limit: %v", err)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"no projects", "version: 1\n", "at least one project"},
		{"empty projects", "version: 1\nprojects:\n", "at least one project"},
		{"no environments", "version: 1\nprojects:\n  p:\n    environments:\n", "at least one environment"},
		{"missing environments key", "version: 1\nprojects:\n  p:\n    other: x\n", "at least one environment"},
		{"no service", "version: 1\nprojects:\n  p:\n    environments:\n      e:\n        env_files: /a\n", "service id is required"},
		{"bad version", "version: nine\n", "expected an integer"},
		{"unsupported version", "version: 2\n", "unsupported schema version"},
		{"bad page limit", "version: 1\ndefaults:\n  page_limit: many\n", "expected an integer"},
		{"bad prod flag", "version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-x\n        prod: maybe\n        env_files: /a\n", "expected true or false"},
		{"bad home", "version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-x\n        env_files: /a\n        manage:\n          K: elsewhere\n", "home must be"},
		{"manage not a mapping", "version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-x\n        env_files: /a\n        manage:\n          - K\n", "mapping of key to home"},
		{"manage scalar", "version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-x\n        env_files: /a\n        manage: nonsense\n", "mapping of key to home"},
		{"defaults not a mapping", "version: 1\ndefaults:\n  - a\n", "expected a mapping"},
		{"project not a mapping", "version: 1\nprojects:\n  p:\n    - a\n", "expected a mapping"},
		{"environment not a mapping", "version: 1\nprojects:\n  p:\n    environments:\n      e:\n        - a\n", "expected a mapping"},
		{"env_files not a list", "version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-x\n        env_files:\n          k: v\n", "expected a list of strings"},
		{"top level sequence", "- a\n- b\n", "mapping at the top level"},
		{"malformed yaml", "a:\n\tb: 1\n", "tab indentation"},
		{"deny_prefixes not a list", "version: 1\ndeny_prefixes:\n  k: v\n", "expected a list of strings"},
		{"local_only not a list", "version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-x\n        env_files: /a\n        local_only:\n          k: v\n", "expected a list of strings"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := parseErr(t, tc.src)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestErrorNamesTheField(t *testing.T) {
	err := parseErr(t, "version: 1\nprojects:\n  myproj:\n    environments:\n      staging:\n        service: srv-x\n        env_files: /a\n        manage:\n          MY_KEY: nowhere\n")
	if !strings.Contains(err.Error(), "projects.myproj.environments.staging.manage.MY_KEY") {
		t.Errorf("error should name the field path: %v", err)
	}
}

func TestBoolForms(t *testing.T) {
	for _, form := range []string{"true", "TRUE", "yes"} {
		cfg := parse(t, "version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-x\n        prod: "+form+"\n        env_files: /a\n")
		e, _ := cfg.Resolve("p/e")
		if !e.Prod {
			t.Errorf("%q did not parse as true", form)
		}
	}
	for _, form := range []string{"false", "no"} {
		cfg := parse(t, "version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-x\n        prod: "+form+"\n        env_files: /a\n")
		e, _ := cfg.Resolve("p/e")
		if e.Prod {
			t.Errorf("%q did not parse as false", form)
		}
	}
}

func TestResolveErrors(t *testing.T) {
	cfg := parse(t, `
version: 1
projects:
  solo:
    environments:
      only:
        service: srv-a
        env_files: /a
  multi:
    environments:
      dev:
        service: srv-b
        env_files: /b
      prod:
        service: srv-c
        env_files: /c
`)
	if _, err := cfg.Resolve(""); err == nil || !strings.Contains(err.Error(), "no target given") {
		t.Errorf("empty target: %v", err)
	}
	if _, err := cfg.Resolve("nope"); err == nil || !strings.Contains(err.Error(), "unknown project") {
		t.Errorf("unknown project: %v", err)
	}
	if _, err := cfg.Resolve("solo/missing"); err == nil || !strings.Contains(err.Error(), "unknown environment") {
		t.Errorf("unknown environment: %v", err)
	}

	// The ambiguity that matters: a bare project name with two environments
	// must not silently pick one.
	_, err := cfg.Resolve("multi")
	if err == nil {
		t.Fatal("ambiguous bare project resolved without an error")
	}
	for _, want := range []string{"multi/<environment>", "dev", "prod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}

	if e, err := cfg.Resolve("solo"); err != nil || e.Name != "only" {
		t.Errorf("solo should resolve: %v %v", e, err)
	}
}

func TestProjectAndEnvironmentNames(t *testing.T) {
	cfg := parse(t, config.ExampleYAML)
	names := cfg.ProjectNames()
	if len(names) != 2 || names[0] != "example-multi" {
		t.Fatalf("ProjectNames = %v", names)
	}
	envs := cfg.Projects["example-multi"].EnvironmentNames()
	if len(envs) != 2 || envs[0] != "dev" || envs[1] != "prod" {
		t.Fatalf("EnvironmentNames = %v", envs)
	}
}

func TestManifestFoldsInGlobalDenyList(t *testing.T) {
	cfg := parse(t, `
version: 1
deny_prefixes:
  - DEMO_*
  - RENDER_API_KEY
projects:
  p:
    environments:
      e:
        service: srv-x
        env_files: /a
        manage:
          DEMO_DEPLOYER_PK: service
          KEEP: service
        local_only:
          - SCRATCH_*
`)
	e, err := cfg.Resolve("p/e")
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Manifest(e)

	if !m.Blocked("DEMO_DEPLOYER_PK") {
		t.Error("a denied key stayed manageable because it was named in manage")
	}
	if !m.Blocked("RENDER_API_KEY") {
		t.Error("RENDER_API_KEY not blocked")
	}
	if !m.Blocked("SCRATCH_THING") {
		t.Error("local_only pattern not applied")
	}
	if _, ok := m.HomeOf("KEEP"); !ok {
		t.Error("KEEP should be manageable")
	}
}

func TestLoadFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if _, err := config.Load(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("expected error for a missing file")
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := func() error { _, e := config.Load(bad); return e }()
	if err == nil || !strings.Contains(err.Error(), "bad.yaml") {
		t.Errorf("error should name the file: %v", err)
	}
}

func TestDefaultPath(t *testing.T) {
	got, err := config.DefaultPath(func(string) string { return "/custom/path.yaml" })
	if err != nil {
		t.Fatal(err)
	}
	if got != "/custom/path.yaml" {
		t.Errorf("DefaultPath = %q", got)
	}

	got, err = config.DefaultPath(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, filepath.Join(".config", "renv", "config.yaml")) {
		t.Errorf("DefaultPath = %q", got)
	}
}

func TestInit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.yaml")

	res, err := config.Init(path)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !res.Created {
		t.Error("first Init did not report creation")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}

	// A second Init must not overwrite: the file maps local paths to live
	// services, and replacing it with placeholders would be silent damage.
	if err := os.WriteFile(path, []byte("version: 1\n# user edits\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = config.Init(path)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if res.Created {
		t.Error("second Init reported creation")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "user edits") {
		t.Error("Init overwrote an existing configuration")
	}
}

func TestInitErrors(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A regular file where a directory needs to be.
	if _, err := config.Init(filepath.Join(blocked, "sub", "config.yaml")); err == nil {
		t.Error("expected error creating a directory under a file")
	}

	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(ro, 0o700) })

	// Directory creation fails.
	if _, err := config.Init(filepath.Join(ro, "sub", "config.yaml")); err == nil {
		t.Skip("running as a user that can write to a read-only directory")
	}
	// The directory exists but the file cannot be written.
	if _, err := config.Init(filepath.Join(ro, "config.yaml")); err == nil {
		t.Error("expected error writing into a read-only directory")
	}
}

func TestDefaultPathWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	if _, err := config.DefaultPath(func(string) string { return "" }); err == nil {
		t.Skip("home directory still resolvable on this platform")
	}
}

func TestEnvFilePathExpansionFailure(t *testing.T) {
	t.Setenv("HOME", "")
	_, err := config.Parse([]byte("version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-x\n        env_files: ~/a.env\n"))
	if err == nil {
		t.Skip("home directory still resolvable on this platform")
	}
	if !strings.Contains(err.Error(), "env_files") {
		t.Errorf("error should name the field: %v", err)
	}
}

// TestRemoteOnlyEnvironment covers an environment with no local source. Such a
// target exists to be inspected: diff can report what the service holds even
// when nothing local corresponds to it.
func TestRemoteOnlyEnvironment(t *testing.T) {
	for _, src := range []string{
		"version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-x\n",
		"version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-x\n        env_files:\n",
	} {
		cfg := parse(t, src)
		e, err := cfg.Resolve("p/e")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if !e.RemoteOnly() {
			t.Errorf("environment with no env_files should be remote-only")
		}
		if len(e.EnvFiles) != 0 {
			t.Errorf("EnvFiles = %v", e.EnvFiles)
		}
	}

	cfg := parse(t, minimal)
	e, err := cfg.Resolve("proj/default")
	if err != nil {
		t.Fatal(err)
	}
	if e.RemoteOnly() {
		t.Error("environment with an env_file reported remote-only")
	}
}

// TestBareManageIsAnEmptyAllowlist covers the natural way to write "nothing is
// managed yet" while building the allowlist from a real diff.
func TestBareManageIsAnEmptyAllowlist(t *testing.T) {
	cfg := parse(t, "version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-x\n        env_files: /a\n        manage:\n        local_only:\n          - X\n")
	e, err := cfg.Resolve("p/e")
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Manage) != 0 {
		t.Errorf("Manage = %v, want empty", e.Manage)
	}
	if len(e.LocalOnly) != 1 {
		t.Errorf("local_only after a bare manage = %v", e.LocalOnly)
	}
}
