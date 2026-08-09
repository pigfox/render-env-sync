# PF-S80 — session record

The session that produced renv, written up while the reasoning was still
available. Project names, service identifiers and credentials are omitted
deliberately; this file is public and the tool's own pre-push scan is run
against it.

## What shipped

**renv v0.1.2** — a CLI that diffs and syncs environment variables between
local `.env` files and Render services. Go 1.26, standard library only, no
third-party dependencies. Six internal packages, each at 100% statement
coverage. Cross-compiled for five targets with checksums, published from a
tagged release workflow that tests the tagged commit before building.

It was written against a real estate of 18 services and 4 environment groups,
and every design decision below came from something measured there rather than
anticipated.

## Six API behaviours that report success while doing something else

The unifying property: none of these fails. Each returns a plausible result,
and a tool that trusts the result proceeds confidently in the wrong direction.
That is why the client treats any non-2xx as fatal and has no empty-result
fallback anywhere — a swallowed error here is indistinguishable from a true
answer.

**1. The collection PUT deletes everything you omit, and returns 200.**
`PUT /v1/services/{id}/env-vars` replaces a service's entire variable set. A
payload of one key leaves the service holding one key. Measured against a real
service: local sources held 44 keys, the service 35, with 18 in common — a
"push my .env" script on that endpoint would have deleted 17 live keys, payment
credentials among them, and reported success. renv never calls it; writes go
one key at a time and the refusal is enforced below every method.

**2. Length comparison cannot detect drift on fixed-width tokens.** Two
credentials in sibling files, both 68 characters, were different values. Most
API tokens are fixed width, so this is the common case rather than an edge
case. Every comparison is by SHA-256 fingerprint.

**3. `GET /v1/services/{id}/env-groups` does not exist.** It returns 404. Code
that treats a failed lookup as "no groups linked" reports every service as
unlinked, which looks exactly like a correct answer for an estate that mostly
has no groups. Linkage is only discoverable from the group side, via each
group's `serviceLinks`.

**4. The env-group list omits `envVars`.** `GET /v1/env-groups` returns every
group with zero variables. Believing it is how a sync tool concludes there is
nothing to reconcile. Membership requires a per-group GET, one request each.

**5. A security setting PATCH returned 200 and enabled nothing.** Enabling push
protection on a repository where secret scanning is unavailable is accepted and
silently ignored; a follow-up read showed both still disabled. Verifying by
exit code would have reported protection that did not exist.

**6. The obvious verification query reads a field that is not there.** Private
vulnerability reporting does not appear in the repository object's
`security_and_analysis` block at all, so that query returns a confident-looking
result that says nothing either way. It has its own endpoint.

The lesson generalised into a habit worth keeping: **verify by reading back the
state, never by checking the exit code of the thing that set it.** Applied
consistently, it caught 5 and 6.

## Two disclosure bugs, both in redaction code

**A recon script leaked two live credentials through its redaction helper.**
The construct was `${VA:+$(fp "$VA")}${VA:-ABSENT}`, intended to print a
fingerprint or the word ABSENT. `${VA:-ABSENT}` substitutes the *value* when the
variable is set, so it printed the fingerprint followed by the plaintext. The
code reads correctly and its output looks plausible.

**The `.env` parser's `SyntaxError` carried the raw line.** A malformed line is
by definition bytes the parser could not classify, and the line that triggered
it in practice was a shell-style `export KEY='...'` assignment holding a live
API key. The error printed it to the terminal.

Both fixes were driven by tests written to fail against the existing code
first. Two details from the second one are worth carrying forward:

- The field was removed from the error type rather than the message edited.
  `%#v` renders struct fields directly, so a type that *holds* untrusted
  content discloses it regardless of what `Error()` returns.
- No key name is reported for a syntax error either. Where parsing failed no
  valid identifier exists, so the candidate key is itself untrusted bytes — for
  a stray token line it would be part of the secret.

The same audit was applied to the configuration reader, removing five further
places where file content was echoed into an error.

## Design decisions that held

**Allowlist, not denylist.** Only keys named in the manifest are compared or
written; everything else is reported and left alone. The estate measured 44
local keys against 35 remote with 18 in common — most keys legitimately belong
on exactly one side. A denylist would have required enumerating a moving target
and would fail open.

**Fingerprint, not length.** See trap 2. Length is the tempting proxy because
it is free and non-disclosing; it is also wrong.

**Per-key upsert, not collection PUT.** See trap 1. The guard sits at the
lowest level of the client, below every method, so a future method written
without the per-key path cannot reach the network.

**The credential is injected by a RoundTripper onto a cloned request.** It is
not a field on the client. `*http.Request` is plain stdlib with no redaction —
`%v`, `%+v`, `req.Header` and `httputil.DumpRequestOut` all print a bearer
token in clear, which was verified before the transport was written. Cloning
inside `RoundTrip` means the only request carrying the credential is the one
handed to the transport and discarded. Authentication therefore does not use
the plaintext accessor at all, which keeps the audit of that accessor down to
two call sites: the `.env` writer and the request body.

**Halt on conflict, not last-wins.** When two source files define one key
differently, renv refuses rather than picking. In the case that motivated this,
the correct value was in the *later* file — and in a second case discovered
afterwards, in the *earlier* one. No fixed precedence rule would have been
right both times.

**Dry run by default, with stepwise escalation.** Creating a missing key,
overwriting an existing one, and deleting one are three separate decisions and
take three separate flags; production takes a fourth. Before any write the
service is fetched and its owner and name are asserted against the
configuration, so a mistyped identifier becomes a refusal rather than a write
to someone else's service.

## Deliberately not done

**No allowlist for the second project.** Both its targets resolve and diff
cleanly, but every key is still unmanaged. One environment has no local source
file at all and is configured as remote-only; the other's allowlist was not
attempted because the source file's role only became clear late in the session.

**`push` and `pull` were never run for real.** Every command in this session
was a dry run, a diff, or a read. The write paths are covered by tests against
a fake API and by one release-verification run of a published binary, but no
production service was ever written to. The tool is unproven against a live
mutation.

**Six keys referenced nowhere.** A grep of one project's codebase found six
environment variables with zero references outside the `.env` file itself and
no commits touching them. They are deletion candidates, not deletions — an
unreferenced key is sometimes an obsolete auth method and sometimes a feature
that has not shipped.

**One known-stale credential remains.** A vendor API key exists in the sole
configured source with a value the service does not have; the copy that matched
the service lived in a file since deprecated as a source. It is excluded in
both directions so nothing acts on it, but the reconciliation is outstanding.

## Numbers

| | |
| --- | --- |
| Packages at 100% coverage | 6 |
| Third-party dependencies | 0 |
| Releases | v0.1.0, v0.1.1, v0.1.2 |
| Superseded releases with notices | 2 |
| API traps documented | 6 |
| Disclosure bugs found and fixed | 2 |
| Production services written to | 0 |
