#!/bin/bash
# Runs every fuzz target in the module for FUZZTIME each (default 30s). Not
# part of the release gate: fuzzing is open-ended, and the gate has to stay
# deterministic. This is a local pre-release check and a scheduled CI job
# (.github/workflows/fuzz.yml). When a target finds a crasher, `go test`
# saves the input under <package>/testdata/fuzz/<Target>/ and replays it as
# an ordinary test from then on; commit it together with the fix.
set -euo pipefail
cd "$(dirname "$0")/../server"

FUZZTIME="${FUZZTIME:-30s}"
failed=0
for pkg in $(go list ./...); do
	for target in $(go test -list '^Fuzz' "$pkg" | grep '^Fuzz' || true); do
		echo "== $pkg $target for $FUZZTIME"
		if ! go test -run '^$' -fuzz "^${target}\$" -fuzztime "$FUZZTIME" "$pkg"; then
			failed=1
		fi
	done
done
if [ "$failed" = 1 ]; then
	echo "FUZZING FOUND A CRASHER (see server/*/testdata/fuzz)"
	exit 1
fi
echo "ALL FUZZ TARGETS CLEAN"
