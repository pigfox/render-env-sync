#!/usr/bin/env bash
#
# pre-push: refuse to push credentials, machine-specific paths, or a red build.
#
# WHY THIS EXISTS
#
# GitHub secret scanning and push protection are free on public repositories
# but require a paid plan on private ones. This repository is private, so the
# server-side net is not there. This hook is the substitute.
#
# HOOKS ARE LOCAL ONLY
#
# Git hooks live in .git/hooks, which is NOT part of the repository. A fresh
# clone has no hooks and no protection. This tracked copy is the source of
# truth; install it after cloning with:
#
#     cp scripts/pre-push.sh .git/hooks/pre-push && chmod +x .git/hooks/pre-push
#
# WHAT IT SCANS
#
# The committed blobs being pushed, not the working directory. A working-tree
# scan would miss a secret that was committed and then edited out of the file
# on disk, which is exactly the case that matters.

set -uo pipefail

PATTERNS='/home/peter|pigfox2|marketverdict|ost_[A-Za-z0-9]|OUTSTAND|BREVO|TURNSTILE|rnd_[A-Za-z0-9_-]{20,}|srv-[a-zA-Z0-9]{15,}|evg-[a-zA-Z0-9]{15,}|tea-[a-zA-Z0-9]{15,}'

# Deliberate placeholders and synthetic test constants. A line containing any
# of these is exempt. Keep this list short: every entry is a hole in the net.
ALLOW='EXAMPLE|rnd_CANARY|rnd_TESTKEY'

# Determine what is actually being pushed. Before the first push there is no
# upstream, so fall back to every tracked file rather than scanning nothing.
if git rev-parse --abbrev-ref '@{push}' >/dev/null 2>&1; then
	files=$(git diff --name-only '@{push}..HEAD')
	scope="files changed since @{push}"
else
	files=$(git ls-files)
	scope="all tracked files (no upstream yet)"
fi

hits=0
report=""

while IFS= read -r f; do
	[ -n "$f" ] || continue
	# This script necessarily contains every pattern it searches for, so
	# scanning it would block every push. It is excluded by path rather than
	# by an ALLOW entry, because an ALLOW entry broad enough to cover it
	# would punch the same hole in every other file.
	[ "$f" = "scripts/pre-push.sh" ] && continue
	# Skip paths deleted in HEAD: they have no blob to scan.
	git cat-file -e "HEAD:$f" 2>/dev/null || continue

	while IFS= read -r match; do
		[ -n "$match" ] || continue
		report+="  $f:$match"$'\n'
		hits=$((hits + 1))
	done < <(git show "HEAD:$f" 2>/dev/null | grep -anE "$PATTERNS" | grep -vE "$ALLOW")
done <<<"$files"

if [ "$hits" -gt 0 ]; then
	{
		echo "pre-push: BLOCKED — $hits match(es) in $scope"
		echo
		printf '%s' "$report"
		echo
		echo "Each line above is a committed blob, not your working copy."
		echo "If a hit is a deliberate placeholder, make the line say EXAMPLE,"
		echo "or widen ALLOW in scripts/pre-push.sh and reinstall the hook."
		echo "Do not pass --no-verify: the point of this hook is the case where"
		echo "you are certain it is a false positive and it is not."
	} >&2
	exit 1
fi

# A red build should not reach a branch anyone else pulls.
if command -v go >/dev/null 2>&1; then
	testlog=$(mktemp)
	trap 'rm -f "$testlog"' EXIT
	if ! go test ./... -race >"$testlog" 2>&1; then
		{
			echo "pre-push: BLOCKED — go test ./... -race failed"
			echo
			cat "$testlog"
		} >&2
		exit 1
	fi
fi

exit 0
