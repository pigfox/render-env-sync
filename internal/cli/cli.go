// Package cli implements renv's commands.
//
// Two rules govern every command that can change something:
//
// Dry run is the default. Every command prints exactly what it would do and
// changes nothing until --apply is given. Escalating beyond that is explicit:
// --update to overwrite a differing remote value, --prune plus an interactive
// confirmation to delete one, --yes-prod to touch a production target.
//
// Before any write, the service is fetched and its owner and name are checked
// against the configuration. A service id in a config file is a bare string
// with no error correction; the assertion is what turns a typo into a refusal
// instead of a write to someone else's service.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/pigfox/render-env-sync/internal/config"
	"github.com/pigfox/render-env-sync/internal/delta"
	"github.com/pigfox/render-env-sync/internal/dotenv"
	"github.com/pigfox/render-env-sync/internal/render"
	"github.com/pigfox/render-env-sync/internal/secret"
)

// Exit codes. Drift is distinct from failure so that a CI job can tell a
// configuration difference apart from a broken run.
const (
	ExitOK    = 0
	ExitDrift = 1
	ExitError = 2
)

// API is the slice of the Render client the commands use.
type API interface {
	ListServices(ctx context.Context) ([]render.Service, error)
	GetService(ctx context.Context, id string) (render.Service, error)
	ListServiceEnvVars(ctx context.Context, id string) ([]render.EnvVar, error)
	GroupsForService(ctx context.Context, id string) ([]render.EnvGroup, error)
	PutServiceEnvVar(ctx context.Context, id, key string, v secret.Secret) error
	DeleteServiceEnvVar(ctx context.Context, id, key string) error
	Deploy(ctx context.Context, id string) (string, error)
}

// App holds the process environment the commands depend on, so that tests can
// substitute all of it.
type App struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	Getenv func(string) string

	// Version is the build version, injected into main and passed through.
	// Empty means an unstamped build and reports as DevVersion.
	Version string

	// NewAPI builds the API client. Defaults to a real Render client.
	NewAPI func(cfg *config.Config, key secret.Secret) (API, error)
}

const usage = `renv — sync .env files with Render service configuration

Usage:
  renv <command> [flags]

Commands:
  init                 write a starter config to ~/.config/renv/config.yaml
  services             list every service the API key can see
  status               summarise every configured target
  diff [target]        show differences; exits 1 when any target has drift
  push <target>        send local values to a service
  pull <target>        write service values into local .env files
  doctor               validate config and check each target resolves
  version              print the build version

Flags:
  --config <path>      configuration file (default $RENV_CONFIG or ~/.config/renv/config.yaml)
  --apply              perform changes; without it every command is a dry run
  --update             also overwrite values that differ (push, pull)
  --prune              also delete values missing from the source; asks first
  --yes-prod           required to write to a target marked prod
  --deploy             trigger a deploy after a successful push

A target is "project" or "project/environment". push and pull always require
one: there is no default target, because the default would eventually be
production.
`

// Run dispatches a command and returns the process exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	if a.NewAPI == nil {
		a.NewAPI = defaultAPI
	}
	if len(args) == 0 {
		fmt.Fprint(a.Stderr, usage)
		return ExitError
	}

	cmd, rest := args[0], args[1:]
	var err error
	code := ExitOK

	switch cmd {
	case "init":
		err = a.cmdInit(rest)
	case "services":
		err = a.cmdServices(ctx, rest)
	case "status":
		err = a.cmdStatus(ctx, rest)
	case "diff":
		code, err = a.cmdDiff(ctx, rest)
	case "push":
		err = a.cmdPush(ctx, rest)
	case "pull":
		err = a.cmdPull(ctx, rest)
	case "doctor":
		err = a.cmdDoctor(ctx, rest)
	case "version", "--version", "-v":
		fmt.Fprintln(a.Stdout, a.version())
		return ExitOK
	case "help", "-h", "--help":
		fmt.Fprint(a.Stdout, usage)
		return ExitOK
	default:
		fmt.Fprintf(a.Stderr, "renv: unknown command %q\n\n%s", cmd, usage)
		return ExitError
	}

	if err != nil {
		fmt.Fprintf(a.Stderr, "renv: %v\n", err)
		return ExitError
	}
	return code
}

// DevVersion is reported by a binary built without a version stamp.
const DevVersion = "dev"

// version reports the build version, or DevVersion when unstamped.
func (a *App) version() string {
	if a.Version == "" {
		return DevVersion
	}
	return a.Version
}

// options are the flags shared across commands.
type options struct {
	configPath string
	apply      bool
	update     bool
	prune      bool
	yesProd    bool
	deploy     bool
}

// valueFlags names the flags that consume the following argument, so that
// flags and positionals can be interleaved.
var valueFlags = map[string]bool{"config": true}

// permute moves flags ahead of positional arguments.
//
// The flag package stops parsing at the first non-flag argument, which would
// make `renv push proj/prod --apply` silently drop --apply — and silently
// dropping --apply on a push is the difference between a dry run and a write.
func permute(args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			positional = append(positional, args[i+1:]...)
			return append(flags, positional...)
		case len(arg) > 1 && arg[0] == '-':
			flags = append(flags, arg)
			name := strings.TrimLeft(arg, "-")
			if !strings.Contains(name, "=") && valueFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		default:
			positional = append(positional, arg)
		}
	}
	return append(flags, positional...)
}

// newFlagSet registers every flag renv accepts.
//
// It is a separate function so that a test can walk it and assert that
// valueFlags lists exactly the flags that consume an argument. Getting those
// two out of step makes permute mis-sort silently: a value flag it does not
// know about, appearing after a positional, swallows the positional as its
// value and no error is reported.
func newFlagSet(name string, o *options, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&o.configPath, "config", "", "configuration file")
	fs.BoolVar(&o.apply, "apply", false, "perform changes")
	fs.BoolVar(&o.update, "update", false, "overwrite differing values")
	fs.BoolVar(&o.prune, "prune", false, "delete values missing from the source")
	fs.BoolVar(&o.yesProd, "yes-prod", false, "allow writing to a prod target")
	fs.BoolVar(&o.deploy, "deploy", false, "deploy after a successful push")
	return fs
}

func (a *App) parse(name string, args []string) (*options, []string, error) {
	o := &options{}
	fs := newFlagSet(name, o, a.Stderr)
	if err := fs.Parse(permute(args)); err != nil {
		return nil, nil, err
	}
	return o, fs.Args(), nil
}

func (a *App) configPath(o *options) (string, error) {
	if o.configPath != "" {
		return o.configPath, nil
	}
	return config.DefaultPath(a.Getenv)
}

func (a *App) loadConfig(o *options) (*config.Config, error) {
	path, err := a.configPath(o)
	if err != nil {
		return nil, err
	}
	return config.Load(path)
}

// apiKey reads the credential from the environment variable named in the
// configuration. It is never read from the configuration file itself: that
// file is a map of ids and paths, and is far more likely to be shared or
// committed than a shell environment.
func (a *App) apiKey(cfg *config.Config) (secret.Secret, error) {
	name := cfg.Defaults.APIKeyEnv
	v := a.Getenv(name)
	if v == "" {
		return secret.Secret{}, fmt.Errorf("%s is not set; export the Render API key before running this command", name)
	}
	return secret.New(v), nil
}

func defaultAPI(cfg *config.Config, key secret.Secret) (API, error) {
	return render.New(key, cfg.RenderOptions())
}

func (a *App) client(cfg *config.Config) (API, error) {
	key, err := a.apiKey(cfg)
	if err != nil {
		return nil, err
	}
	return a.NewAPI(cfg, key)
}

func (a *App) cmdInit(args []string) error {
	o, _, err := a.parse("init", args)
	if err != nil {
		return err
	}
	path, err := a.configPath(o)
	if err != nil {
		return err
	}
	res, err := config.Init(path)
	if err != nil {
		return err
	}
	if res.Created {
		fmt.Fprintf(a.Stdout, "wrote %s\nEdit it to replace the placeholder ids, then run: renv services\n", res.Path)
		return nil
	}
	fmt.Fprintf(a.Stdout, "%s already exists; leaving it alone\n", res.Path)
	return nil
}

func (a *App) cmdServices(ctx context.Context, args []string) error {
	o, _, err := a.parse("services", args)
	if err != nil {
		return err
	}
	cfg, err := a.loadConfig(o)
	if err != nil {
		return err
	}
	api, err := a.client(cfg)
	if err != nil {
		return err
	}
	services, err := api.ListServices(ctx)
	if err != nil {
		return err
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })

	tw := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTYPE\tNAME\tSTATE")
	for _, s := range services {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.ID, s.Type, s.Name, s.Suspended)
	}
	return tw.Flush()
}

// resolution is everything needed to compare one target.
type resolution struct {
	env     *config.Environment
	entries []delta.Entry
	service render.Service
	groups  []render.EnvGroup

	// sources are the parsed local files, kept so that a write reuses the
	// bytes the comparison was made from rather than re-reading them and
	// racing an edit made in between.
	sources []dotenv.Source
}

// resolve loads local files, fetches remote state, and classifies.
func (a *App) resolve(ctx context.Context, cfg *config.Config, api API, env *config.Environment) (*resolution, error) {
	sources, local, err := loadLocal(env)
	if err != nil {
		return nil, err
	}

	svc, err := api.GetService(ctx, env.ServiceID)
	if err != nil {
		return nil, err
	}
	serviceVars, err := api.ListServiceEnvVars(ctx, env.ServiceID)
	if err != nil {
		return nil, err
	}
	groups, err := api.GroupsForService(ctx, env.ServiceID)
	if err != nil {
		return nil, err
	}

	groupVars := delta.Set{}
	for _, g := range groups {
		for _, v := range g.EnvVars {
			groupVars[v.Key] = v.Value
		}
	}
	svcSet := delta.Set{}
	for _, v := range serviceVars {
		svcSet[v.Key] = v.Value
	}

	remote := delta.ResolveRemote(svcSet, groupVars)
	entries := delta.Compare(local, remote, cfg.Manifest(env))
	return &resolution{env: env, entries: entries, service: svc, groups: groups, sources: sources}, nil
}

// loadLocal parses and merges an environment's source files, returning both
// the parsed files and the merged view.
func loadLocal(env *config.Environment) ([]dotenv.Source, delta.Set, error) {
	sources := make([]dotenv.Source, 0, len(env.EnvFiles))
	for _, path := range env.EnvFiles {
		f, err := dotenv.ParseFile(path)
		if err != nil {
			return nil, nil, err
		}
		sources = append(sources, dotenv.Source{Path: path, File: f})
	}
	merged, err := dotenv.Merge(sources)
	if err != nil {
		return nil, nil, err
	}
	return sources, delta.Set(merged), nil
}

// assertIdentity refuses to proceed when the fetched service does not match
// what the configuration claims. A mistyped id is otherwise indistinguishable
// from a correct one until the write lands.
func assertIdentity(env *config.Environment, svc render.Service) error {
	if env.OwnerID != "" && svc.OwnerID != env.OwnerID {
		return fmt.Errorf("%s: service %s belongs to owner %s but the config expects %s; refusing to write",
			env.Target(), env.ServiceID, svc.OwnerID, env.OwnerID)
	}
	if env.ServiceName != "" && svc.Name != env.ServiceName {
		return fmt.Errorf("%s: service %s is named %q but the config expects %q; refusing to write",
			env.Target(), env.ServiceID, svc.Name, env.ServiceName)
	}
	return nil
}

func (a *App) cmdStatus(ctx context.Context, args []string) error {
	o, _, err := a.parse("status", args)
	if err != nil {
		return err
	}
	cfg, err := a.loadConfig(o)
	if err != nil {
		return err
	}
	api, err := a.client(cfg)
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TARGET\tSERVICE\tSAME\tDIFFERS\tLOCAL\tREMOTE\tSHADOW\tUNMANAGED")
	for _, env := range cfg.Environments() {
		res, err := a.resolve(ctx, cfg, api, env)
		if err != nil {
			return err
		}
		c := delta.Counts(res.entries)
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\n",
			env.Target(), res.service.Name,
			c[delta.StatusSame], c[delta.StatusDiffers], c[delta.StatusLocalOnly],
			c[delta.StatusRemoteOnly], c[delta.StatusShadow], c[delta.StatusUnmanaged])
	}
	return tw.Flush()
}

func (a *App) cmdDiff(ctx context.Context, args []string) (int, error) {
	o, rest, err := a.parse("diff", args)
	if err != nil {
		return ExitError, err
	}
	cfg, err := a.loadConfig(o)
	if err != nil {
		return ExitError, err
	}
	api, err := a.client(cfg)
	if err != nil {
		return ExitError, err
	}

	envs := cfg.Environments()
	if len(rest) > 0 {
		env, err := cfg.Resolve(rest[0])
		if err != nil {
			return ExitError, err
		}
		envs = []*config.Environment{env}
	}

	drift := false
	for _, env := range envs {
		res, err := a.resolve(ctx, cfg, api, env)
		if err != nil {
			return ExitError, err
		}
		a.printEntries(env.Target(), res)
		if delta.HasDrift(res.entries) {
			drift = true
		}
	}
	if drift {
		return ExitDrift, nil
	}
	return ExitOK, nil
}

// printEntries renders a comparison. Values appear only as fingerprints.
func (a *App) printEntries(target string, res *resolution) {
	fmt.Fprintf(a.Stdout, "\n%s  (service %s", target, res.service.Name)
	if len(res.groups) > 0 {
		names := make([]string, 0, len(res.groups))
		for _, g := range res.groups {
			names = append(names, g.Name)
		}
		fmt.Fprintf(a.Stdout, ", groups %s", strings.Join(names, ", "))
	}
	fmt.Fprintln(a.Stdout, ")")

	tw := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  STATUS\tKEY\tHOME\tLOCAL\tREMOTE")
	for _, e := range res.entries {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
			e.Status, e.Key, e.Home, fingerprint(e.Local, e.HasLocal), fingerprint(e.Remote, e.HasRemote))
	}
	tw.Flush()
}

// fingerprint renders a value for display, never its plaintext.
func fingerprint(s secret.Secret, present bool) string {
	if !present {
		return "-"
	}
	return s.Fingerprint()
}

// planItem is one intended change.
type planItem struct {
	key    string
	action string // "create", "update", "delete"
	entry  delta.Entry
}

func (a *App) cmdPush(ctx context.Context, args []string) error {
	o, rest, err := a.parse("push", args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("push requires an explicit target; there is no default, because the default would eventually be production")
	}
	cfg, err := a.loadConfig(o)
	if err != nil {
		return err
	}
	env, err := cfg.Resolve(rest[0])
	if err != nil {
		return err
	}
	if env.Prod && !o.yesProd {
		return fmt.Errorf("%s is marked prod; pass --yes-prod to write to it", env.Target())
	}
	api, err := a.client(cfg)
	if err != nil {
		return err
	}
	res, err := a.resolve(ctx, cfg, api, env)
	if err != nil {
		return err
	}
	if err := assertIdentity(env, res.service); err != nil {
		return err
	}

	var plan []planItem
	var refused []string
	for _, e := range res.entries {
		// A key whose declared home is the group is never pushed: renv v1
		// does not write groups, and writing it to the service instead
		// would create the shadow the classifier warns about.
		if e.Home == delta.HomeGroup && (e.Status == delta.StatusDiffers || e.Status == delta.StatusLocalOnly) {
			refused = append(refused, e.Key)
			continue
		}
		switch e.Status {
		case delta.StatusLocalOnly:
			plan = append(plan, planItem{key: e.Key, action: "create", entry: e})
		case delta.StatusDiffers:
			if o.update {
				plan = append(plan, planItem{key: e.Key, action: "update", entry: e})
			}
		case delta.StatusRemoteOnly:
			if o.prune {
				plan = append(plan, planItem{key: e.Key, action: "delete", entry: e})
			}
		}
	}

	for _, key := range refused {
		fmt.Fprintf(a.Stdout, "  skip    %s  (declared home is the group; renv does not write groups)\n", key)
	}
	a.printPlan(env.Target(), plan, o)
	if len(plan) == 0 {
		return nil
	}
	if !o.apply {
		fmt.Fprintln(a.Stdout, "\ndry run; re-run with --apply to make these changes")
		return nil
	}
	if hasDeletes(plan) && !a.confirm(fmt.Sprintf("delete %d remote variable(s) from %s", countDeletes(plan), env.Target())) {
		return errors.New("aborted")
	}

	for _, item := range plan {
		if item.action == "delete" {
			if err := api.DeleteServiceEnvVar(ctx, env.ServiceID, item.key); err != nil {
				return err
			}
			fmt.Fprintf(a.Stdout, "  deleted %s\n", item.key)
			continue
		}
		if err := api.PutServiceEnvVar(ctx, env.ServiceID, item.key, item.entry.Local); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "  wrote   %s  %s\n", item.key, item.entry.Local.Fingerprint())
	}

	// Render applies environment changes on the next deploy. Doing that
	// implicitly would turn a configuration fix into a restart.
	if o.deploy {
		id, err := api.Deploy(ctx, env.ServiceID)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "\ndeploy %s triggered\n", id)
		return nil
	}
	fmt.Fprintln(a.Stdout, "\nchanges are staged; Render applies them on the next deploy (--deploy triggers one)")
	return nil
}

func (a *App) cmdPull(ctx context.Context, args []string) error {
	o, rest, err := a.parse("pull", args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("pull requires an explicit target")
	}
	cfg, err := a.loadConfig(o)
	if err != nil {
		return err
	}
	env, err := cfg.Resolve(rest[0])
	if err != nil {
		return err
	}
	api, err := a.client(cfg)
	if err != nil {
		return err
	}
	res, err := a.resolve(ctx, cfg, api, env)
	if err != nil {
		return err
	}

	var plan []planItem
	for _, e := range res.entries {
		switch e.Status {
		case delta.StatusRemoteOnly:
			plan = append(plan, planItem{key: e.Key, action: "create", entry: e})
		case delta.StatusDiffers:
			if o.update {
				plan = append(plan, planItem{key: e.Key, action: "update", entry: e})
			}
		case delta.StatusLocalOnly:
			if o.prune {
				plan = append(plan, planItem{key: e.Key, action: "delete", entry: e})
			}
		}
	}

	a.printPlan(env.Target(), plan, o)
	if len(plan) == 0 {
		return nil
	}
	if !o.apply {
		fmt.Fprintln(a.Stdout, "\ndry run; re-run with --apply to make these changes")
		return nil
	}
	if hasDeletes(plan) && !a.confirm(fmt.Sprintf("delete %d local variable(s) for %s", countDeletes(plan), env.Target())) {
		return errors.New("aborted")
	}
	return a.applyPull(cfg, res, plan)
}

// applyPull writes the planned changes into the environment's files.
//
// An existing key is updated in whichever file already defines it. A new key
// goes to the last file listed, which is the one a multi-file environment
// treats as the extension of the set.
func (a *App) applyPull(cfg *config.Config, res *resolution, plan []planItem) error {
	files := res.sources
	changed := make([]bool, len(files))
	for _, item := range plan {
		idx := len(files) - 1
		for i, src := range files {
			if _, ok := src.File.Get(item.key); ok {
				idx = i
				break
			}
		}
		if item.action == "delete" {
			if files[idx].File.Delete(item.key) {
				changed[idx] = true
			}
			continue
		}
		files[idx].File.Set(item.key, item.entry.Remote)
		changed[idx] = true
	}

	for i, dirty := range changed {
		if !dirty {
			continue
		}
		if err := files[i].File.WriteAtomic(files[i].Path, cfg.WriteOptions()); err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "  updated %s\n", files[i].Path)
	}
	return nil
}

func (a *App) printPlan(target string, plan []planItem, o *options) {
	if len(plan) == 0 {
		fmt.Fprintf(a.Stdout, "%s: nothing to do\n", target)
		if !o.update {
			fmt.Fprintln(a.Stdout, "(values that differ are left alone unless --update is given)")
		}
		return
	}
	fmt.Fprintf(a.Stdout, "%s:\n", target)
	tw := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	for _, item := range plan {
		switch item.action {
		case "create":
			fmt.Fprintf(tw, "  create\t%s\t%s\n", item.key, item.entry.Local.Fingerprint())
		case "update":
			fmt.Fprintf(tw, "  update\t%s\t%s -> %s\n", item.key,
				item.entry.Remote.Fingerprint(), item.entry.Local.Fingerprint())
		default:
			fmt.Fprintf(tw, "  delete\t%s\t%s\n", item.key, item.entry.Remote.Fingerprint())
		}
	}
	tw.Flush()
}

func hasDeletes(plan []planItem) bool { return countDeletes(plan) > 0 }

func countDeletes(plan []planItem) int {
	n := 0
	for _, item := range plan {
		if item.action == "delete" {
			n++
		}
	}
	return n
}

// confirm asks before a deletion. Deletions are the one operation with no
// undo: a wrong write can be corrected from the local file, a wrong delete
// cannot.
func (a *App) confirm(what string) bool {
	fmt.Fprintf(a.Stdout, "\nAbout to %s.\nType yes to continue: ", what)
	if a.Stdin == nil {
		return false
	}
	scanner := bufio.NewScanner(a.Stdin)
	if !scanner.Scan() {
		return false
	}
	return strings.TrimSpace(scanner.Text()) == "yes"
}

func (a *App) cmdDoctor(ctx context.Context, args []string) error {
	o, _, err := a.parse("doctor", args)
	if err != nil {
		return err
	}
	path, err := a.configPath(o)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "config: %s\n", path)

	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "schema version %d, %d project(s), %d target(s)\n",
		cfg.Version, len(cfg.Projects), len(cfg.Targets()))

	api, err := a.client(cfg)
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(a.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TARGET\tSERVICE\tOWNER\tIDENTITY\tFILES")
	problems := 0
	for _, env := range cfg.Environments() {
		target := env.Target()
		svc, err := api.GetService(ctx, env.ServiceID)
		if err != nil {
			fmt.Fprintf(tw, "%s\t%s\t-\tunreachable\t-\n", target, env.ServiceID)
			problems++
			continue
		}

		identity := "ok"
		if err := assertIdentity(env, svc); err != nil {
			identity = "MISMATCH"
			problems++
		}

		files := "ok"
		if _, _, err := loadLocal(env); err != nil {
			files = "ERROR"
			problems++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", target, svc.Name, svc.OwnerID, identity, files)
	}
	tw.Flush()

	if problems > 0 {
		return fmt.Errorf("%d problem(s) found", problems)
	}
	fmt.Fprintln(a.Stdout, "\nall targets resolve and match their configured identity")
	return nil
}
