# renv — render-env-sync

Keep local `.env` files and [Render](https://render.com) service configuration in
step, without ever issuing the API call that deletes the difference.

Go 1.26, standard library only, no third-party dependencies.

---

## The call this tool will not make

```
PUT /v1/services/{id}/env-vars
```

That endpoint replaces a service's **entire** environment variable set. Any key
absent from the payload is deleted. It returns `200 OK` and no warning, so a
partial payload looks exactly like a successful sync.

The failure is not hypothetical. Measured against one real service: the local
files held 44 keys, the service held 35, and only 18 were common. A
"push my .env to Render" script built on that endpoint would have deleted 17
live keys — every Stripe production and test credential among them — and
reported success.

**renv never calls it.** Writes go one key at a time:

```
PUT /v1/services/{id}/env-vars/{key}
```

The refusal is enforced at the lowest level of the client, below every method,
so a future method that forgets the per-key path cannot reach the network:

```go
if seg[len(seg)-1] == "env-vars" {
    return &CollectionWriteError{Method: method, Path: clean}
}
```

## Everything else that keeps a mistake small

**Dry run is the default.** Every command prints what it would do and changes
nothing. Mutation needs `--apply`.

**Escalation is stepwise.** Creating a missing key is one decision; overwriting
an existing one is another (`--update`); deleting one is a third (`--prune`,
plus typing `yes`). Writing to a target marked `prod` needs `--yes-prod` on top.

**`push` has no default target.** It always requires an explicit
`project/environment`, because a default target eventually becomes production.

**Identity is asserted before any write.** A service id in a config file is a
bare string with no error correction. renv fetches the service and refuses to
write if the owner or name disagrees with the configuration — turning a typo
into a refusal rather than a write to someone else's service.

**The manifest is an allowlist.** Only keys named under `manage` are compared or
written. Everything else is reported as `UNMANAGED` and left alone in both
directions. `deny_prefixes` blocks keys globally and cannot be overridden by
naming them in `manage` — signing keys (`DEMO_*`) and the Render credential
itself are denied out of the box.

**Values are compared by fingerprint, never by length.** Two 68-character
credentials in sibling files turned out to be different values; a length check
called them identical. renv compares the first 12 hex characters of the SHA-256
digest.

**Plaintext never reaches a log or a terminal.** Secrets are wrapped in a type
whose `String`, `GoString`, `Format`, `MarshalText` and `MarshalJSON` all emit a
fingerprint. Plaintext leaves through one function, `Reveal`, called from
exactly two places: the request **body** writer and the `.env` writer. A test
pushes a known value through every `fmt` verb, `log`, and `encoding/json` path
and asserts it appears nowhere.

**The API key never touches a request object anyone can format.** It is not a
field on the client. It is attached to a *clone* of each request inside a
`RoundTripper`, so the request the client builds and holds carries no
credential. This matters because `*http.Request` is plain stdlib with no
redaction — `%v`, `%+v`, `req.Header` and `httputil.DumpRequestOut` all print a
bearer token in clear, which was verified before the transport was written.
Authentication therefore does not call `Reveal` at all; it goes through a
narrow sink, `secret.SetAuthorization`, that writes into the header without the
plaintext ever becoming a value the caller holds.

**Error messages are scrubbed.** If the API rejects a value and echoes it back
in the response body, that value is replaced by its fingerprint before the body
is stored on the error. Scrubbing runs before truncation, so a value straddling
the length cap cannot leave a usable prefix behind.

**Writes do not deploy.** Render applies environment changes on the next deploy.
renv makes that a separate `--deploy` flag so a configuration fix never restarts
a service as a side effect.

**renv v1 never writes environment groups.** It reads them — it has to, to
resolve precedence — but a key whose declared home is the group is reported and
skipped rather than written to the service, where it would silently shadow the
group's copy.

## Install

```
go install github.com/pigfox/render-env-sync@latest
```

The binary is named `renv`.

## Quick start

```sh
export RENDER_API_KEY=rnd_...        # never stored in the config file
renv init                            # writes ~/.config/renv/config.yaml
renv services                        # list service ids the key can see
$EDITOR ~/.config/renv/config.yaml   # fill in ids and the manage allowlist
renv doctor                          # check every target resolves and matches
renv diff                            # see the differences
renv push myproject/dev --apply      # write them
```

## Commands

| Command | Purpose |
| --- | --- |
| `renv init` | Write a starter config. Never overwrites an existing one. |
| `renv services` | List every service the API key can see. |
| `renv status` | One summary row per configured target. |
| `renv diff [target]` | Show per-key differences. **Exits 1 on drift**, so it works in CI. |
| `renv push <target>` | Send local values to a service. Target is mandatory. |
| `renv pull <target>` | Write service values into local `.env` files. |
| `renv doctor` | Validate config, check credentials, assert each target's identity. |

### Flags

| Flag | Effect |
| --- | --- |
| `--config <path>` | Config file. Default `$RENV_CONFIG`, else `~/.config/renv/config.yaml`. |
| `--apply` | Perform changes. Without it, everything is a dry run. |
| `--update` | Also overwrite values that differ. |
| `--prune` | Also delete values missing from the source. Asks for confirmation. |
| `--yes-prod` | Required to write to a target marked `prod`. |
| `--deploy` | Trigger a deploy after a successful push. |

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success, no drift |
| 1 | `diff` found drift |
| 2 | Error |

Drift is distinct from failure so a CI job can tell a configuration difference
apart from a broken run.

## Key statuses

| Status | Meaning |
| --- | --- |
| `SAME` | Local and remote agree, by fingerprint. |
| `DIFFERS` | Both sides have the key, with different values. |
| `LOCAL_ONLY` | Managed, present locally, absent remotely. |
| `REMOTE_ONLY` | Managed, present remotely, absent locally. |
| `SHADOW` | Set on both the service and a linked group. The service wins, so the group's copy is inert while still looking correct in the dashboard. |
| `UNMANAGED` | Not in the allowlist, or denied. Reported, never touched. |

A service's effective environment is its own variables unioned with those of
every linked environment group, service winning. renv resolves that union before
comparing, so a key supplied by a group is not reported as missing.

## Configuration

See [`example.config.yaml`](example.config.yaml), which is exactly what
`renv init` writes. Every identifier in it is a placeholder.

The schema is `project → environment → target`. The environment level is
load-bearing: some projects have genuine `dev` and `prod` environments backed by
separate services and databases, while others have a single environment whose
configuration is split across two complementary files that are halves of one
thing rather than two variants of it.

For multi-file environments, a key present in more than one file with **different
values** is an error, not last-wins. In the case that motivated this rule the
correct survivor was in the later file, and in the general case no fixed
precedence rule can know which one is right.

The config file holds ids and paths, never credentials. The API key is read from
the environment variable named by `defaults.api_key_env` (default
`RENDER_API_KEY`).

## Render API notes

Four behaviours worth knowing if you extend this tool, each verified against the
live API:

- `PUT /v1/services/{id}/env-vars` is a full-collection replace that returns 200.
- `limit` caps at 100. Larger values return HTTP 400 `invalid limit: too large`.
- `GET /v1/services/{id}/env-groups` **does not exist** — it returns 404. Group
  membership is discoverable only from the group side, via `serviceLinks`.
- `GET /v1/env-groups` omits `envVars` entirely, reporting every group as empty.
  Membership requires a per-group `GET`.

The last two are the dangerous pair: both produce plausible, confident, wrong
answers rather than errors. renv treats any non-2xx as fatal and has no
empty-result fallback anywhere.

## Development

```sh
go build ./...
go vet ./...
go test ./... -race -cover
```

### Install the pre-push hook

Git hooks are not part of a repository, so a fresh clone has none. Install this
one before your first push:

```sh
cp scripts/pre-push.sh .git/hooks/pre-push && chmod +x .git/hooks/pre-push
```

It scans the committed blobs being pushed — not the working directory, so a
secret that was committed and later edited out of the file on disk is still
caught — and refuses the push on a match or a failing `go test ./... -race`.

Every package under `internal/` is held at 100% statement coverage. `main.go` is
dispatch only.

## License

MIT. See [LICENSE](LICENSE).
