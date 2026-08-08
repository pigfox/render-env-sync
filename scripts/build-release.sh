#!/usr/bin/env bash
#
# build-release.sh <version> [outdir]
#
# Cross-compiles renv for every released target and packages each one.
#
# This lives in a tracked script rather than inline in the release workflow so
# that the build can be run and verified locally before a tag is pushed. A
# release pipeline that only ever executes on the release itself is a pipeline
# whose first real run is the one that matters.

set -euo pipefail

VERSION="${1:?usage: build-release.sh <version> [outdir]}"
OUT="${2:-dist}"

TARGETS=(
	linux/amd64
	linux/arm64
	darwin/amd64
	darwin/arm64
	windows/amd64
)

rm -rf "$OUT"
mkdir -p "$OUT/stage"

for target in "${TARGETS[@]}"; do
	os="${target%/*}"
	arch="${target#*/}"

	bin="renv"
	[ "$os" = "windows" ] && bin="renv.exe"

	name="renv_${VERSION}_${os}_${arch}"
	stage="$OUT/stage/$name"
	mkdir -p "$stage"

	# CGO_ENABLED=0 keeps the binaries static and free of a libc dependency;
	# -trimpath keeps build machine paths out of them, which matters for a
	# tool whose whole point is not disclosing local paths.
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
		-trimpath \
		-ldflags "-s -w -X main.version=${VERSION}" \
		-o "$stage/$bin" .

	cp README.md LICENSE example.config.yaml "$stage/"

	if [ "$os" = "windows" ]; then
		(cd "$OUT/stage" && zip -qr "../${name}.zip" "$name")
	else
		tar -czf "$OUT/${name}.tar.gz" -C "$OUT/stage" "$name"
	fi
	echo "built $name"
done

rm -rf "$OUT/stage"

# List the archives explicitly rather than globbing everything: a bare * would
# race the checksum file into its own manifest.
(cd "$OUT" && sha256sum renv_*.tar.gz renv_*.zip >SHA256SUMS)

echo
echo "artifacts in $OUT:"
ls -1 "$OUT"
