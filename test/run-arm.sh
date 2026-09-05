#!/bin/bash
# ARM smoke-test runner: executes the full smoke suite against the dev
# daemon cross-compiled for linux/arm64 (armv8 spk) and linux/arm/v7
# (armv7 spk), each running inside a Docker container of that platform.
#
# On Apple Silicon, arm64 runs natively in Docker's VM; arm/v7 runs under
# QEMU (M-series CPUs dropped 32-bit ARM), so that pass is slower — it is
# also the run that catches classic 32-bit issues such as misaligned
# 64-bit atomics.
#
# The node/jsdom harness stays on the host: the fixture volume is
# bind-mounted into the container at the SAME absolute path, so the daemon
# (in the container) and smoke.js's direct filesystem assertions (on the
# host) agree about every path. The daemon binds all interfaces via the
# dev-only DUPFINDER_BIND_HOST hook, because Docker's published ports
# forward to the container's interface, never to its loopback.
#
# Requires: Docker (running), Go 1.20+, Node 18+. First run pulls the
# alpine base images for both platforms. The work dir lives under test/
# (not the system temp dir) because Docker Desktop shares /Users by
# default, keeping identical-path bind mounts dependable.
set -euo pipefail
cd "$(dirname "$0")"
. ./lib.sh
ROOT="$(cd .. && pwd)"
PORT="${DUPFINDER_TEST_PORT:-9807}"
IMAGE="alpine:3.20"

command -v docker >/dev/null || { echo "docker not found" >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "docker daemon not running" >&2; exit 1; }

echo "== deps"
fetch_deps

echo "== cross-compiling dev daemons"
BINDIR="$(pwd)/.arm-bin"
mkdir -p "$BINDIR"
(
	cd "$ROOT/server"
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
		go build -tags dev -o "$BINDIR/dupfinder.arm64" .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
		go build -tags dev -o "$BINDIR/dupfinder.arm" .
)

# Cleanup on EVERY exit, not only on a function return: an interrupt during
# the 60 s daemon wait or the smoke pass would otherwise leave the container
# (still publishing 127.0.0.1:9807, so the next run.sh fails to bind), the
# cross-compiled binaries and the work dirs behind.
CIDS=""
cleanup() {
	for c in $CIDS; do docker rm -f "$c" >/dev/null 2>&1; done
	for d in "$(pwd)"/.armwork.*; do
		[ -d "$d" ] || continue
		chmod -R u+rwx "$d" 2>/dev/null
		rm -rf "$d"
	done
	rm -rf "$BINDIR"
}
trap cleanup EXIT INT TERM

FAILED=0
run_one() {
	local LABEL="$1" PLATFORM="$2" BIN="$3"
	echo ""
	echo "==== $LABEL ($PLATFORM) ===="
	local WORK CID=""
	WORK="$(mktemp -d "$(pwd)/.armwork.XXXXXX")"

	local V="$WORK/volume1"
	make_fixture "$V" "$WORK/outside" "$WORK/volumeUSB1"
	local UIDIR="$WORK/ui"
	mkdir -p "$UIDIR"
	cp -R "$ROOT/spk/ui/." "$UIDIR/"   # "dir/." — contents on BSD and GNU alike
	cp harness.html ext/ext-base.js ext/ext-all.js "$UIDIR/"
	cp "$BIN" "$WORK/dupfinder-dev"
	# Persistence on too: the state file, hash store and marker are exactly
	# the binary-layout code a 32-bit runner exists to exercise.
	mkdir -p "$WORK/state"

	CID="$(docker run -d --rm --platform "$PLATFORM" \
		-v "$WORK:$WORK" \
		-p "127.0.0.1:$PORT:$PORT" \
		-e DUPFINDER_ROOTS="$V:$WORK/volumeUSB1" \
		-e DUPFINDER_UI="$UIDIR" \
		-e DUPFINDER_DSM_URL="http://127.0.0.1:$PORT" \
		-e DUPFINDER_BIND_HOST="0.0.0.0" \
		-e DUPFINDER_STATE="$WORK/state" \
		"$IMAGE" "$WORK/dupfinder-dev" -mode daemon -port "$PORT")"
	CIDS="$CIDS $CID"

	# QEMU startup can be slow; give the daemon up to 60s to answer.
	local up=0 i
	for i in $(seq 1 60); do
		if curl -sf "http://localhost:$PORT/api/info" >/dev/null 2>&1; then up=1; break; fi
		sleep 1
	done
	if [ "$up" != 1 ]; then
		echo "daemon failed to start on $PLATFORM:" >&2
		docker logs "$CID" >&2 || true
		FAILED=1
		return
	fi
	echo "-- daemon up, running smoke suite"
	if ! DUPFINDER_TEST_PORT="$PORT" node smoke.js; then
		FAILED=1
	fi
	docker rm -f "$CID" >/dev/null 2>&1
	chmod -R u+rwx "$WORK" 2>/dev/null
	rm -rf "$WORK"
}

run_one "armv8" "linux/arm64" "$BINDIR/dupfinder.arm64"
run_one "armv7" "linux/arm/v7" "$BINDIR/dupfinder.arm"

echo ""
if [ "$FAILED" = 1 ]; then
	echo "ARM SMOKE: FAILURES"
	exit 1
fi
echo "ARM SMOKE: ALL PASSED (arm64 + armv7)"
