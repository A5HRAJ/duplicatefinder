#!/bin/bash
# Self-contained test runner for the Duplicate Finder package.
#
# Builds the dev daemon, creates a disposable fixture volume, serves the
# packaged UI plus an ExtJS harness, and drives the real app headlessly
# (jsdom) through scan → results → selection → dialogs → export.
#
# Requires: Go 1.20+, Node 18+, network on first run (fetches ExtJS 3.4 for
# the harness and installs jsdom; both are cached locally afterwards).
set -euo pipefail
cd "$(dirname "$0")"
. ./lib.sh
ROOT="$(cd .. && pwd)"
PORT="${DUPFINDER_TEST_PORT:-9807}"
WORK="$(mktemp -d)"
DPID=""
trap '[ -n "$DPID" ] && kill "$DPID" 2>/dev/null; chmod -R u+rwx "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT

# Packaging first: it needs no daemon and no fixture, and it is the only check
# that the ASSEMBLED payload is coherent — everything below this line runs
# against spk/ui copied verbatim and would stay green on a broken package.
echo "== packaging (UI cache stamp)"
./stamp.sh

echo "== deps"
fetch_deps

echo "== fixture volume"
V="$WORK/volume1"
make_fixture "$V" "$WORK/outside" "$WORK/volumeUSB1"

echo "== dev daemon"
(cd "$ROOT/server" && go build -tags dev -o "$WORK/dupfinder-dev" .)
UIDIR="$WORK/ui"
mkdir -p "$UIDIR"
# "dir/." copies the CONTENTS on BSD and GNU cp alike; a trailing slash
# alone means contents only on BSD and nests a "ui" directory on Linux.
cp -R "$ROOT/spk/ui/." "$UIDIR/"
cp harness.html ext/ext-base.js ext/ext-all.js "$UIDIR/"
# DUPFINDER_DSM_URL: the daemon delegates file mutations to the File Station
# Web API; in dev that is the daemon's own mock (see dev.go).
# DUPFINDER_STATE: dev stand-in for the package var dir, so persistence
# (results, hash cache, scan marker) is exercised without arming the token.
STATE="$WORK/state"
mkdir -p "$STATE"
# start_daemon <logfile>: the one launcher for every (re)start below, so no
# launch can skip the readiness wait.
start_daemon() {
	DUPFINDER_ROOTS="$V:$WORK/volumeUSB1" DUPFINDER_UI="$UIDIR" DUPFINDER_DSM_URL="http://127.0.0.1:$PORT" \
		DUPFINDER_STATE="$STATE" \
		"$WORK/dupfinder-dev" -mode daemon -port "$PORT" > "$1" 2>&1 &
	DPID=$!
	disown
	wait_daemon "$PORT" "$1" || exit 1
}
start_daemon "$WORK/daemon.log"

echo "== smoke suite"
DUPFINDER_TEST_PORT="$PORT" node smoke.js

echo "== persistence across restart"
# The smoke suite left results behind; a restarted daemon must still serve
# them, and a scan marker left on disk must surface as "interrupted".
kill "$DPID" 2>/dev/null
wait "$DPID" 2>/dev/null || true
echo '{"tool":"duplicates","startedAt":"2026-07-29T00:00:00Z"}' > "$STATE/scan.interrupted"
start_daemon "$WORK/daemon2.log"
# state_field <json-path>: prints one value from /api/state, parsed rather
# than grepped — a grep on the raw JSON froze the marker struct's field order.
state_field() {
	curl -sf "http://localhost:$PORT/api/state" | python3 -c '
import json,sys
v=json.load(sys.stdin)
for k in sys.argv[1].split("."):
    v=v.get(k) if isinstance(v,dict) else None
print("" if v is None else v)' "$1"
}
[ "$(state_field interrupted.tool)" = "duplicates" ] \
	|| { echo "FAIL: interrupted marker not reported: $(curl -sf "http://localhost:$PORT/api/state")" >&2; exit 1; }
curl -sf "http://localhost:$PORT/api/results?tool=duplicates" | grep -q '"scanned":true' \
	|| { echo "FAIL: results did not survive the restart" >&2; exit 1; }
curl -sf "http://localhost:$PORT/api/results?tool=duplicates" | grep -q '"groups":\[{' \
	|| { echo "FAIL: restored results carry no groups" >&2; exit 1; }
echo "PASS  results and scan marker survive a daemon restart"

echo "== resume mechanics (daemon side)"
# The marker's gen is what a resume continues. Plant a marker carrying the
# hash store's current generation — the shape a mid-scan death leaves — and
# prove: (1) resume:true adopts that generation (the store's header still
# reads it after the resumed scan completes); (2) once the notice is gone,
# resume:true is inert and the next scan advances the generation. The header
# layout is hashcache.go's: 8-byte magic, then gen as uint32 LE. The marker
# must carry the request it interrupted (dirs/recurse/match) — a resume is
# honored only for the identical request, so the plant mirrors the POST below.
hc_gen() {
	python3 -c 'import struct,sys;print(struct.unpack("<I",open(sys.argv[1],"rb").read(12)[8:12])[0])' "$STATE/hashcache.bin"
}
wait_idle() {
	for _ in $(seq 1 100); do
		curl -sf "http://localhost:$PORT/api/state" | grep -q '"running":false' && return 0
		sleep 0.3
	done
	echo "FAIL: scan did not finish" >&2; exit 1
}
G1="$(hc_gen)"
kill "$DPID" 2>/dev/null
wait "$DPID" 2>/dev/null || true
echo "{\"tool\":\"duplicates\",\"gen\":$G1,\"dirs\":[\"$V/spare/ct-src\"],\"recurse\":true,\"match\":{},\"startedAt\":\"2026-07-29T00:00:00Z\"}" > "$STATE/scan.interrupted"
start_daemon "$WORK/daemon3.log"
[ "$(state_field interrupted.tool)" = "duplicates" ] && [ "$(state_field interrupted.gen)" = "$G1" ] \
	|| { echo "FAIL: marker generation not reported" >&2; exit 1; }
curl -sf -X POST -H 'Content-Type: application/json' \
	-d "{\"tool\":\"duplicates\",\"dirs\":[\"$V/spare/ct-src\"],\"recurse\":true,\"match\":{},\"resume\":true}" \
	"http://localhost:$PORT/api/scan" >/dev/null
wait_idle
G2="$(hc_gen)"
[ "$G2" = "$G1" ] || { echo "FAIL: resume did not continue gen $G1 (got $G2)" >&2; exit 1; }
echo "PASS  resume continues the interrupted generation ($G1)"
# The UI tells a Stop from a finish by this field; a completed run reports it.
[ "$(state_field lastEnd.completed)" = "True" ] && [ "$(state_field lastEnd.tool)" = "duplicates" ] \
	|| { echo "FAIL: /state does not report how the last run ended" >&2; exit 1; }
echo "PASS  /state reports the completed run's outcome"
curl -sf -X POST -H 'Content-Type: application/json' \
	-d "{\"tool\":\"duplicates\",\"dirs\":[\"$V/spare/ct-src\"],\"recurse\":true,\"match\":{},\"resume\":true}" \
	"http://localhost:$PORT/api/scan" >/dev/null
wait_idle
G3="$(hc_gen)"
[ "$G3" = "$((G1 + 1))" ] || { echo "FAIL: without a notice resume must be inert (want $((G1+1)), got $G3)" >&2; exit 1; }
echo "PASS  resume without an interruption notice is inert (gen advanced to $G3)"
# Negative gate: a notice for a DIFFERENT tool must not let a duplicates scan
# resume — that would serve hashes the flag's own tool never recorded.
kill "$DPID" 2>/dev/null
wait "$DPID" 2>/dev/null || true
echo "{\"tool\":\"temp_files\",\"gen\":$G3,\"startedAt\":\"2026-07-29T00:00:00Z\"}" > "$STATE/scan.interrupted"
start_daemon "$WORK/daemon4.log"
curl -sf -X POST -H 'Content-Type: application/json' \
	-d "{\"tool\":\"duplicates\",\"dirs\":[\"$V/spare/ct-src\"],\"recurse\":true,\"match\":{},\"resume\":true}" \
	"http://localhost:$PORT/api/scan" >/dev/null
wait_idle
G4="$(hc_gen)"
[ "$G4" = "$((G3 + 1))" ] || { echo "FAIL: a foreign tool's notice honored a duplicates resume (want $((G3+1)), got $G4)" >&2; exit 1; }
echo "PASS  a foreign tool's interruption cannot be resumed into (gen advanced to $G4)"
# Params gate: the marker records the interrupted run's request, and a resume
# whose request differs — here, a different scope — is a DIFFERENT scan. It
# must not adopt the dead run's generation: adopted, it would be served reads
# that run made for another scope, and a file rotted under an unchanged
# size+mtime since those reads would stay grouped as bit-for-bit identical.
kill "$DPID" 2>/dev/null
wait "$DPID" 2>/dev/null || true
echo "{\"tool\":\"duplicates\",\"gen\":$G4,\"dirs\":[\"$V/spare\"],\"recurse\":true,\"match\":{},\"startedAt\":\"2026-07-29T00:00:00Z\"}" > "$STATE/scan.interrupted"
start_daemon "$WORK/daemon5.log"
curl -sf -X POST -H 'Content-Type: application/json' \
	-d "{\"tool\":\"duplicates\",\"dirs\":[\"$V/spare/ct-src\"],\"recurse\":true,\"match\":{},\"resume\":true}" \
	"http://localhost:$PORT/api/scan" >/dev/null
wait_idle
G5="$(hc_gen)"
[ "$G5" = "$((G4 + 1))" ] || { echo "FAIL: a rescoped resume adopted the interrupted generation (want $((G4+1)), got $G5)" >&2; exit 1; }
echo "PASS  a resume with a different scope cannot adopt the interrupted generation (gen advanced to $G5)"
