#!/bin/bash
# The release gate. CI runs exactly this script, and a release is cut only
# from a commit on which it passed, so what the maintainer runs locally and
# what CI runs never differ.
#
#   1. gofmt          — every Go file formatted
#   2. go vet         — for the host and for each shipped target
#                       (linux/amd64, linux/arm64, linux/arm), release and dev
#   3. staticcheck    — the same targets; the version is pinned to the last
#                       release that supports the module's Go version
#   4. go test -race  — the unit tests under the race detector
#   5. test/run.sh    — packaging check plus the full headless UI/API suite
#
# Requires: Go 1.20+, Node 18+, Python 3, and network on the first run
# (staticcheck is fetched into the module cache; run.sh fetches ExtJS and
# jsdom). Docker is not needed; the ARM runtime suite is test/run-arm.sh.
set -euo pipefail
cd "$(dirname "$0")/.."

STATICCHECK="honnef.co/go/tools/cmd/staticcheck@v0.4.7"
TARGETS="amd64 arm64 arm"

TOOLS=$(mktemp -d)
trap 'rm -rf "$TOOLS"' EXIT

echo "== gofmt"
unformatted=$(cd server && gofmt -l .)
if [ -n "$unformatted" ]; then
	echo "gofmt: these files are not formatted:" >&2
	echo "$unformatted" >&2
	exit 1
fi

echo "== go vet (host and linux/{$TARGETS}; release and dev builds)"
(
	cd server
	go vet ./...
	go vet -tags dev ./...
	for arch in $TARGETS; do
		GOOS=linux GOARCH="$arch" go vet ./...
		GOOS=linux GOARCH="$arch" go vet -tags dev ./...
	done
)

echo "== staticcheck ($STATICCHECK)"
# Installed for the HOST, then pointed at each target through GOOS/GOARCH:
# `go run` would cross-compile the tool itself and fail to execute it.
GOBIN="$TOOLS" go install "$STATICCHECK"
(
	cd server
	"$TOOLS/staticcheck" ./...
	"$TOOLS/staticcheck" -tags dev ./...
	for arch in $TARGETS; do
		GOOS=linux GOARCH="$arch" "$TOOLS/staticcheck" ./...
		GOOS=linux GOARCH="$arch" "$TOOLS/staticcheck" -tags dev ./...
	done
)

echo "== go test -race"
(cd server && go test -race ./...)

echo "== packaging check and headless suite"
test/run.sh

echo
echo "ALL CHECKS PASSED"
