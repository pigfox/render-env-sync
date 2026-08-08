// Package config loads renv's configuration.
//
// The schema is project → environment → target, and the environment level is
// load-bearing rather than decorative. Real estates have both shapes: a
// project with genuine dev and prod environments backed by separate services
// and separate databases, and a project with a single environment whose
// configuration is split across two complementary files that are halves of one
// thing rather than two variants of it. Collapsing either into the other
// misrepresents it.
//
// Every value renv would otherwise hard-code — the API base, the page limit,
// the backup timestamp layout, the deny list — is declared here, so that call
// sites contain no magic literals.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pigfox/render-env-sync/internal/delta"
	"github.com/pigfox/render-env-sync/internal/dotenv"
	"github.com/pigfox/render-env-sync/internal/render"
)

// SchemaVersion is the configuration format this build understands.
const SchemaVersion = 1

// EnvConfigPath overrides the configuration location when set.
const EnvConfigPath = "RENV_CONFIG"

// Defaults holds the tunables.
type Defaults struct {
	APIBase         string
	PageLimit       int
	BackupTimestamp string
	APIKeyEnv       string
}

// Environment is one deployment target.
type Environment struct {
	Project string
	Name    string

	// Prod marks a target that requires --yes-prod to write.
	Prod bool

	ServiceID   string
	ServiceName string
	OwnerID     string
	GroupID     string

	EnvFiles  []string
	Manage    map[string]delta.Home
	LocalOnly []string
}

// Target is the "project/environment" string that names this environment.
func (e *Environment) Target() string { return e.Project + "/" + e.Name }

// Project is a named group of environments.
type Project struct {
	Name         string
	Environments map[string]*Environment
	envOrder     []string
}

// EnvironmentNames returns the environment names in declaration order.
func (p *Project) EnvironmentNames() []string { return append([]string(nil), p.envOrder...) }

// Config is the whole file.
type Config struct {
	Version      int
	Defaults     Defaults
	DenyPrefixes []string
	Projects     map[string]*Project

	projectOrder []string
}

// ProjectNames returns project names in declaration order.
func (c *Config) ProjectNames() []string { return append([]string(nil), c.projectOrder...) }

// Error reports a configuration problem, naming the path to the offending
// field so that a large file can be corrected without hunting.
type Error struct {
	Path string
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("config: %s: %s", e.Path, e.Msg) }

func errf(path, format string, args ...any) *Error {
	return &Error{Path: path, Msg: fmt.Sprintf(format, args...)}
}

// DefaultPath returns the configuration location, honouring RENV_CONFIG.
func DefaultPath(getenv func(string) string) (string, error) {
	if p := getenv(EnvConfigPath); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: locating home directory: %w", err)
	}
	return filepath.Join(home, ".config", "renv", "config.yaml"), nil
}

// Load reads and validates a configuration file.
func Load(path string) (*Config, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(src)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse reads and validates configuration from memory.
func Parse(src []byte) (*Config, error) {
	root, err := parseYAML(src)
	if err != nil {
		return nil, err
	}
	if root.kind != mappingNode {
		return nil, errf("", "expected a mapping at the top level")
	}

	cfg := &Config{
		Version: SchemaVersion,
		Defaults: Defaults{
			APIBase:         render.DefaultBaseURL,
			PageLimit:       render.MaxPageLimit,
			BackupTimestamp: dotenv.DefaultBackupLayout,
			APIKeyEnv:       "RENDER_API_KEY",
		},
		Projects: map[string]*Project{},
	}

	if n, ok := root.child("version"); ok {
		v, err := strconv.Atoi(n.str)
		if err != nil {
			return nil, errf("version", "expected an integer, got %q", n.str)
		}
		if v != SchemaVersion {
			return nil, errf("version", "unsupported schema version %d; this build understands %d", v, SchemaVersion)
		}
		cfg.Version = v
	}

	if n, ok := root.child("defaults"); ok {
		if err := cfg.loadDefaults(n); err != nil {
			return nil, err
		}
	}

	if n, ok := root.child("deny_prefixes"); ok {
		list, err := stringList(n, "deny_prefixes")
		if err != nil {
			return nil, err
		}
		cfg.DenyPrefixes = list
	}

	projects, ok := root.child("projects")
	if !ok || projects.kind != mappingNode || len(projects.keys) == 0 {
		return nil, errf("projects", "at least one project is required")
	}
	for _, name := range projects.keys {
		p, err := loadProject(name, projects.m[name])
		if err != nil {
			return nil, err
		}
		cfg.Projects[name] = p
		cfg.projectOrder = append(cfg.projectOrder, name)
	}
	return cfg, nil
}

func (c *Config) loadDefaults(n *node) error {
	if n.kind != mappingNode {
		return errf("defaults", "expected a mapping")
	}
	if v, ok := n.child("api_base"); ok && v.str != "" {
		c.Defaults.APIBase = v.str
	}
	if v, ok := n.child("api_key_env"); ok && v.str != "" {
		c.Defaults.APIKeyEnv = v.str
	}
	if v, ok := n.child("backup_timestamp"); ok && v.str != "" {
		c.Defaults.BackupTimestamp = v.str
	}
	if v, ok := n.child("page_limit"); ok && v.str != "" {
		limit, err := strconv.Atoi(v.str)
		if err != nil {
			return errf("defaults.page_limit", "expected an integer, got %q", v.str)
		}
		if limit < 1 || limit > render.MaxPageLimit {
			return errf("defaults.page_limit",
				"%d is out of range 1..%d; the API rejects anything larger with HTTP 400",
				limit, render.MaxPageLimit)
		}
		c.Defaults.PageLimit = limit
	}
	return nil
}

func loadProject(name string, n *node) (*Project, error) {
	path := "projects." + name
	if n.kind != mappingNode {
		return nil, errf(path, "expected a mapping")
	}
	envs, ok := n.child("environments")
	if !ok || envs.kind != mappingNode || len(envs.keys) == 0 {
		return nil, errf(path+".environments", "at least one environment is required")
	}

	p := &Project{Name: name, Environments: map[string]*Environment{}}
	for _, envName := range envs.keys {
		e, err := loadEnvironment(name, envName, envs.m[envName])
		if err != nil {
			return nil, err
		}
		p.Environments[envName] = e
		p.envOrder = append(p.envOrder, envName)
	}
	return p, nil
}

func loadEnvironment(project, name string, n *node) (*Environment, error) {
	path := fmt.Sprintf("projects.%s.environments.%s", project, name)
	if n.kind != mappingNode {
		return nil, errf(path, "expected a mapping")
	}

	e := &Environment{Project: project, Name: name, Manage: map[string]delta.Home{}}

	if v, ok := n.child("service"); ok {
		e.ServiceID = v.str
	}
	if e.ServiceID == "" {
		return nil, errf(path+".service", "a service id is required")
	}
	if v, ok := n.child("service_name"); ok {
		e.ServiceName = v.str
	}
	if v, ok := n.child("owner"); ok {
		e.OwnerID = v.str
	}
	if v, ok := n.child("group"); ok {
		e.GroupID = v.str
	}
	if v, ok := n.child("prod"); ok {
		b, err := parseBool(v.str)
		if err != nil {
			return nil, errf(path+".prod", "expected true or false, got %q", v.str)
		}
		e.Prod = b
	}

	files, ok := n.child("env_files")
	if !ok {
		return nil, errf(path+".env_files", "at least one env file is required")
	}
	list, err := stringList(files, path+".env_files")
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, errf(path+".env_files", "at least one env file is required")
	}
	for _, f := range list {
		expanded, err := expandPath(f)
		if err != nil {
			return nil, errf(path+".env_files", "%v", err)
		}
		e.EnvFiles = append(e.EnvFiles, expanded)
	}

	if manage, ok := n.child("manage"); ok {
		if manage.kind != mappingNode {
			return nil, errf(path+".manage", "expected a mapping of key to home (service or group)")
		}
		for _, key := range manage.keys {
			home := delta.Home(manage.m[key].str)
			if !home.Valid() {
				return nil, errf(path+".manage."+key,
					"home must be %q or %q, got %q", delta.HomeService, delta.HomeGroup, home)
			}
			e.Manage[key] = home
		}
	}

	if lo, ok := n.child("local_only"); ok {
		list, err := stringList(lo, path+".local_only")
		if err != nil {
			return nil, err
		}
		e.LocalOnly = list
	}
	return e, nil
}

func parseBool(s string) (bool, error) {
	switch strings.ToLower(s) {
	case "true", "yes":
		return true, nil
	case "false", "no", "":
		return false, nil
	default:
		return false, fmt.Errorf("not a boolean")
	}
}

// stringList reads a sequence of scalars, also accepting a single scalar as a
// one-element list.
func stringList(n *node, path string) ([]string, error) {
	switch n.kind {
	case sequenceNode:
		out := make([]string, 0, len(n.seq))
		for _, item := range n.seq {
			out = append(out, item.str)
		}
		return out, nil
	case scalarNode:
		if n.str == "" {
			return nil, nil
		}
		return []string{n.str}, nil
	default:
		return nil, errf(path, "expected a list of strings")
	}
}

// expandPath resolves a leading ~ so that configuration files stay portable
// between machines.
func expandPath(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expanding %q: %w", p, err)
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}

// Resolve maps a target string onto an environment.
//
// "project/environment" is always accepted. A bare "project" resolves only
// when the project has exactly one environment; otherwise the ambiguity is an
// error listing the choices, because guessing which environment was meant is
// how a dev change lands on prod.
func (c *Config) Resolve(target string) (*Environment, error) {
	if target == "" {
		return nil, fmt.Errorf("config: no target given; expected project or project/environment")
	}

	projectName, envName, qualified := strings.Cut(target, "/")
	p, ok := c.Projects[projectName]
	if !ok {
		return nil, fmt.Errorf("config: unknown project %q; known projects: %s",
			projectName, strings.Join(c.ProjectNames(), ", "))
	}

	if qualified {
		e, ok := p.Environments[envName]
		if !ok {
			return nil, fmt.Errorf("config: unknown environment %q in project %q; known environments: %s",
				envName, projectName, strings.Join(p.EnvironmentNames(), ", "))
		}
		return e, nil
	}

	if len(p.envOrder) != 1 {
		return nil, fmt.Errorf("config: project %q has %d environments; name one explicitly as %s/<environment> (%s)",
			projectName, len(p.envOrder), projectName, strings.Join(p.EnvironmentNames(), ", "))
	}
	return p.Environments[p.envOrder[0]], nil
}

// Environments returns every configured environment, ordered by target name.
//
// Commands that walk every target use this rather than re-resolving target
// strings, so that iterating the configuration has no error path to handle or
// leave untested.
func (c *Config) Environments() []*Environment {
	var out []*Environment
	for _, pn := range c.projectOrder {
		p := c.Projects[pn]
		for _, en := range p.envOrder {
			out = append(out, p.Environments[en])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target() < out[j].Target() })
	return out
}

// Targets lists every configured environment as "project/environment", sorted.
func (c *Config) Targets() []string {
	envs := c.Environments()
	out := make([]string, 0, len(envs))
	for _, e := range envs {
		out = append(out, e.Target())
	}
	return out
}

// Manifest builds the allowlist for an environment, folding in the global deny
// list so that a per-environment entry cannot re-enable a denied key.
func (c *Config) Manifest(e *Environment) delta.Manifest {
	return delta.Manifest{
		Manage:       e.Manage,
		LocalOnly:    e.LocalOnly,
		DenyPrefixes: c.DenyPrefixes,
	}
}

// RenderOptions builds the API client options from the configured defaults.
func (c *Config) RenderOptions() render.Options {
	return render.Options{BaseURL: c.Defaults.APIBase, PageLimit: c.Defaults.PageLimit}
}

// WriteOptions builds the .env writer options from the configured defaults.
func (c *Config) WriteOptions() dotenv.WriteOptions {
	return dotenv.WriteOptions{BackupLayout: c.Defaults.BackupTimestamp}
}
