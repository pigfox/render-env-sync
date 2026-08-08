# Security Policy

## Supported versions

| Version | Supported | Notes |
| --- | --- | --- |
| v0.1.2 and later | ✅ | Current. |
| v0.1.1 | ❌ | Superseded. See [Known issues](#known-issues). |
| v0.1.0 | ❌ | Superseded. See [Known issues](#known-issues). |

## Known issues

### v0.1.1 — excluded keys reported as absent locally

**Fixed in v0.1.2. This is a correctness bug, not a disclosure one.**

Keys excluded by `deny_prefixes` or `local_only` were rendered with an empty
local column, so a blocked key that also exists on the service read as absent
locally when it was present. Blocked keys are omitted from the report entirely
in v0.1.2. Not a disclosure issue — the values shown were correct; the
local/remote presence was not.

No credential was exposed and no value was misreported. What was wrong was the
claim about where a key existed, in a report used to decide what to push and
what to delete — so a reader could reasonably have concluded a key was missing
from their local files and added it, or that a service held a key their files
did not.

The defect only affected blocked keys that the service *also* carried: a
blocked key absent remotely was omitted for the ordinary reason and looked
correct. It was introduced alongside the v0.1.1 fix that filters blocked keys
out of every source file before the merge, which addressed a blocked key being
able to fail every command through a conflict between two files renv would
never read or write.

### v0.1.0 — `.env` parser printed raw line content in syntax errors

**Fixed in v0.1.1.**

The `.env` parser's `SyntaxError` carried the raw text of the offending line and
included it in the error message. A malformed line is by definition bytes the
parser could not classify, and in practice such a line often *is* a credential —
for example a shell-style `export KEY='...'` assignment, which v0.1.0 did not
accept as valid syntax.

The result was that running `renv diff`, `status`, `doctor`, `push` or `pull`
against a file containing a malformed line printed that line, in clear, to the
terminal. From there it would reach shell history, CI logs, and bug reports.

**If you ran v0.1.0 against a `.env` file that produced a syntax error, treat
any credential on the reported line as exposed and rotate it.**

Two changes in v0.1.1 address this:

- The field holding the line was removed from the error type, rather than the
  message being edited. `%#v` renders struct fields directly, so an error type
  that *holds* untrusted content leaks it regardless of what `Error()` returns.
  Errors now report a line number and one of three classifications.
- No key name is reported for a syntax error either. Where parsing failed, no
  valid identifier exists, so the candidate key is itself untrusted bytes — for
  a stray token line it would be part of the secret.

`export KEY=value` and leading whitespace are also accepted from v0.1.1, which
removes the most common way to hit the error path at all.

## Reporting a vulnerability

Please report privately through GitHub, not in a public issue:

**https://github.com/pigfox/render-env-sync/security/advisories/new**

Include the version (`renv version`), what you observed, and how to reproduce
it. Please do not include real credentials in a report — a fingerprint or a
redacted example is enough.

## Design notes

These are the guarantees the tool is built around. They are what a reader
evaluating it should be checking, and each has a test named for the failure it
prevents.

**Plaintext reaches exactly two sinks.** Credentials are held in a type whose
`String`, `GoString`, `Format`, `MarshalText` and `MarshalJSON` all emit a
SHA-256 fingerprint instead of the value. `Reveal`, the one exported function
returning a plaintext string, is called from exactly two places: the `.env`
writer and the PUT request body. A test drives a known value through every
`fmt` verb, `log`, and `encoding/json` path and asserts it appears nowhere.

**The API credential never touches a request object a caller can format.** It
is not a field on the client. It is attached to a *clone* of each request inside
a `RoundTripper`, so the request the client builds and holds carries no
credential. `*http.Request` is plain stdlib with no redaction — `%v`, `%+v`,
`req.Header` and `httputil.DumpRequestOut` all print a bearer token in clear,
which was verified before the transport was written. Authentication does not
call `Reveal`; it uses a narrow sink that writes into the header without the
plaintext becoming a value the caller holds.

**Comparison is by fingerprint, never by length.** Two distinct 68-character
credentials in sibling files are what motivated this: a length check called them
identical. Values compare by the leading 12 hex characters of their SHA-256
digest.

**The collection PUT endpoint is never called.**
`PUT /v1/services/{id}/env-vars` replaces a service's entire variable set and
returns `200`, so a partial payload silently deletes every omitted key. Writes
go one key at a time to `/env-vars/{key}`. The refusal is enforced below every
method in the client, so a future method that forgets the per-key path cannot
reach the network.

**Dry run is the default.** Every command prints what it would do and changes
nothing. Mutation requires `--apply`; overwriting a differing value additionally
requires `--update`; deleting requires `--prune` and an interactive
confirmation; writing to a target marked `prod` requires `--yes-prod`. Before
any write, the service is fetched and its owner and name are asserted against
the configuration, so a mistyped service id becomes a refusal rather than a
write to someone else's service.

**Error messages are scrubbed.** If the API rejects a value and echoes it back
in the response body, that value is replaced by its fingerprint before the body
is stored on the error. Scrubbing runs before truncation, so a value straddling
the length cap cannot leave a usable prefix behind.
