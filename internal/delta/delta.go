// Package delta compares a local key set against what a Render service
// actually resolves at runtime, and classifies each key.
//
// Two properties of the live estate shape this package:
//
// A service's effective environment is its own variables unioned with those of
// every linked environment group, with the service's own value winning. A tool
// that reads only service-level variables will keep re-pushing keys the group
// already supplies, and will report a group-supplied key as missing.
//
// The set of local keys that belong on a service is much smaller than the set
// of local keys. Recon measured 44 local keys against 35 remote ones with only
// 18 in common: the remainder are local-only signing material on one side and
// remote-only payment credentials on the other. So the manifest is an
// allowlist, and a key not named in it is [StatusUnmanaged] rather than
// something to add or delete.
package delta

import (
	"sort"
	"strings"

	"github.com/pigfox/render-env-sync/internal/secret"
)

// Home declares where a key is supposed to live.
type Home string

const (
	// HomeService means the key belongs on the service itself.
	HomeService Home = "service"
	// HomeGroup means the key belongs in a linked environment group. renv
	// reads these to resolve precedence but never writes them.
	HomeGroup Home = "group"
)

// Valid reports whether h is a recognised home.
func (h Home) Valid() bool { return h == HomeService || h == HomeGroup }

// Status classifies one key.
type Status string

const (
	// StatusSame means local and remote agree, by fingerprint.
	StatusSame Status = "SAME"
	// StatusDiffers means both sides have the key with different values.
	StatusDiffers Status = "DIFFERS"
	// StatusLocalOnly means the key is managed but absent remotely.
	StatusLocalOnly Status = "LOCAL_ONLY"
	// StatusRemoteOnly means the key is managed but absent locally.
	StatusRemoteOnly Status = "REMOTE_ONLY"
	// StatusShadow means the key is set on both the service and a linked
	// group. The service value wins, so the group's copy is inert while
	// still appearing correct in the dashboard.
	StatusShadow Status = "SHADOW"
	// StatusUnmanaged means the key is not in the allowlist, or is blocked.
	// renv reports it and touches it in neither direction.
	StatusUnmanaged Status = "UNMANAGED"
)

// Set is a flat key to value mapping.
type Set map[string]secret.Secret

// Resolved is one key of a service's effective environment.
type Resolved struct {
	Value secret.Secret
	// InService reports that the service carries the key directly.
	InService bool
	// InGroup reports that a linked environment group carries the key.
	InGroup bool
}

// ResolveRemote unions a service's own variables with those of its linked
// groups, service winning, and records where each key came from.
func ResolveRemote(service, group Set) map[string]Resolved {
	out := make(map[string]Resolved, len(service)+len(group))

	for k, v := range group {
		out[k] = Resolved{Value: v, InGroup: true}
	}
	for k, v := range service {
		r := out[k]
		r.Value = v // service wins
		r.InService = true
		out[k] = r
	}
	return out
}

// Manifest declares which keys renv may touch and where each one lives.
type Manifest struct {
	// Manage is the allowlist. A key absent from it is unmanaged.
	Manage map[string]Home
	// LocalOnly blocks a key in both directions. A trailing * makes an
	// entry a prefix match.
	LocalOnly []string
	// DenyPrefixes blocks keys globally, checked before everything else.
	DenyPrefixes []string
}

// matches reports whether key satisfies a pattern, where a trailing asterisk
// is a prefix wildcard and anything else is an exact match.
func matches(pattern, key string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(key, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == key
}

// Blocked reports whether a key is refused in both directions.
func (m Manifest) Blocked(key string) bool {
	for _, p := range m.DenyPrefixes {
		if matches(p, key) {
			return true
		}
	}
	for _, p := range m.LocalOnly {
		if matches(p, key) {
			return true
		}
	}
	return false
}

// HomeOf returns the declared home for a key, if it is managed and not
// blocked.
func (m Manifest) HomeOf(key string) (Home, bool) {
	if m.Blocked(key) {
		return "", false
	}
	h, ok := m.Manage[key]
	return h, ok
}

// Entry is the comparison result for one key.
type Entry struct {
	Key    string
	Status Status
	// Home is the declared home, empty for unmanaged keys.
	Home Home

	Local     secret.Secret
	HasLocal  bool
	Remote    secret.Secret
	HasRemote bool

	InService bool
	InGroup   bool
}

// IsDrift reports whether an entry represents a difference worth a non-zero
// exit code. Unmanaged keys are reported but never count as drift: they are
// the expected steady state for most of the estate.
func (e Entry) IsDrift() bool {
	switch e.Status {
	case StatusDiffers, StatusLocalOnly, StatusRemoteOnly, StatusShadow:
		return true
	default:
		return false
	}
}

// Compare classifies the union of local and resolved-remote keys, sorted by
// key so that output is stable across runs.
func Compare(local Set, remote map[string]Resolved, m Manifest) []Entry {
	keys := make(map[string]struct{}, len(local)+len(remote))
	for k := range local {
		keys[k] = struct{}{}
	}
	for k := range remote {
		keys[k] = struct{}{}
	}

	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	out := make([]Entry, 0, len(sorted))
	for _, k := range sorted {
		lv, hasLocal := local[k]
		rv, hasRemote := remote[k]

		e := Entry{
			Key:       k,
			Local:     lv,
			HasLocal:  hasLocal,
			Remote:    rv.Value,
			HasRemote: hasRemote,
			InService: rv.InService,
			InGroup:   rv.InGroup,
		}

		home, managed := m.HomeOf(k)
		switch {
		case !managed:
			e.Status = StatusUnmanaged
		default:
			e.Home = home
			e.Status = classify(e)
		}
		out = append(out, e)
	}
	return out
}

// classify resolves a managed key's status.
//
// The shadow check comes first because it is a configuration fault rather than
// a value difference: whatever the values are, one of the two definitions is
// silently doing nothing.
func classify(e Entry) Status {
	switch {
	case e.InService && e.InGroup:
		return StatusShadow
	case e.HasLocal && e.HasRemote:
		if e.Local.Equal(e.Remote) {
			return StatusSame
		}
		return StatusDiffers
	case e.HasLocal:
		return StatusLocalOnly
	default:
		return StatusRemoteOnly
	}
}

// HasDrift reports whether any entry counts as drift, which the diff command
// turns into a non-zero exit status for CI.
func HasDrift(entries []Entry) bool {
	for _, e := range entries {
		if e.IsDrift() {
			return true
		}
	}
	return false
}

// Counts tallies entries by status, for summary output.
func Counts(entries []Entry) map[Status]int {
	out := map[Status]int{}
	for _, e := range entries {
		out[e.Status]++
	}
	return out
}
