#!/bin/bash
# End-to-end checks for the classes of defect a filesystem can hand the
# daemon, run against the REAL dev daemon over HTTP — the way each of them
# was found. The unit tests prove the pieces; these prove the behaviour the
# user gets:
#
#   special files   a named pipe or a socket in the scan scope is not listed
#                   as an Empty File, and a row that has BECOME one since the
#                   scan fails its verification at once instead of hanging
#                   the daemon's move lock forever
#   sparse files    two sparse images that occupy nothing on disk add nothing
#                   to the reclaimable figure
#   byte names      a file whose name is not UTF-8 is listed with its bytes
#                   intact, survives a restart, and can be moved and verified
#                   (Linux only: macOS refuses to create such a name)
#   Stop            /api/cancel ends a running scan
#   free space      a move onto a volume that cannot hold the files is refused
#                   before anything is created; a move within the volume is not
#   SIGTERM         the daemon stops cleanly, inside the package's stop window
#
# Requires: Go 1.20+, Python 3, curl. No Node, no network. check.sh runs it.
# DUPFINDER_AUDIT_BIN names a dev daemon built elsewhere (a cross-compiled
# one for a container that has no Go), skipping the build.
set -euo pipefail
cd "$(dirname "$0")"
. ./lib.sh
ROOT="$(cd .. && pwd)"
PORT="${DUPFINDER_AUDIT_PORT:-9809}"
WORK="$(mktemp -d)"
DPID=""
trap '[ -n "$DPID" ] && kill "$DPID" 2>/dev/null; rm -rf "$WORK"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS  $*"; }
api() { # api <method> <path> [json-body] — prints the response body
	if [ $# -ge 3 ]; then
		curl -s -m 60 -X "$1" -H 'Content-Type: application/json' -d "$3" "http://127.0.0.1:$PORT$2"
	else
		curl -s -m 60 -X "$1" "http://127.0.0.1:$PORT$2"
	fi
}
field() { # field <json-path> — one value from stdin JSON, "" if absent
	python3 -c '
import json,sys
v=json.load(sys.stdin)
for k in sys.argv[1].split("."):
    v=v.get(k) if isinstance(v,dict) else None
print("" if v is None else v)' "$1"
}
wait_idle() {
	for _ in $(seq 1 200); do
		api GET /api/state | grep -q '"running":false' && return 0
		sleep 0.25
	done
	fail "scan did not finish"
}
scan() { # scan <tools-json-array> — runs the scan over the share and waits
	local r
	r=$(api POST /api/scan "{\"tool\":\"duplicates\",\"tools\":$1,\"dirs\":[\"$SHARE\"],\"recurse\":true,\"match\":{}}")
	echo "$r" | grep -q '"started":true' || fail "scan refused: $r"
	wait_idle
}

echo "== dev daemon"
if [ -n "${DUPFINDER_AUDIT_BIN:-}" ]; then
	cp "$DUPFINDER_AUDIT_BIN" "$WORK/dupfinder-dev"
else
	(cd "$ROOT/server" && go build -tags dev -o "$WORK/dupfinder-dev" .)
fi
V="$WORK/volume1"; USB="$WORK/volumeUSB1"; SHARE="$V/share"; STATE="$WORK/state"
mkdir -p "$SHARE/dest" "$USB/usbshare" "$STATE" "$WORK/ui"
# The daemon is kept as a child (no disown): the SIGTERM check waits on it.
start_daemon() { # start_daemon <logfile>
	DUPFINDER_ROOTS="$V:$USB" DUPFINDER_UI="$WORK/ui" DUPFINDER_DSM_URL="http://127.0.0.1:$PORT" \
		DUPFINDER_STATE="$STATE" DUPFINDER_DEV_FREESPACE="${FREESPACE:-}" \
		"$WORK/dupfinder-dev" -mode daemon -port "$PORT" > "$1" 2>&1 &
	DPID=$!
	wait_daemon "$PORT" "$1" || exit 1
}
stop_daemon() {
	kill "$DPID" 2>/dev/null || true
	wait "$DPID" 2>/dev/null || true
	DPID=""
}

echo "== fixture"
printf 'hello world content' > "$SHARE/a.txt"
cp "$SHARE/a.txt" "$SHARE/b.txt"
: > "$SHARE/empty.txt"
touch -t 202401020900.00 "$SHARE/empty.txt"
mkfifo "$SHARE/pipe.fifo"
python3 -c 'import socket,sys; socket.socket(socket.AF_UNIX, socket.SOCK_STREAM).bind(sys.argv[1])' "$SHARE/live.sock"
# Two sparse "disk images": 2 GiB apparent each, nothing allocated. Also what
# makes the duplicates scan take long enough for a Stop to land in it.
python3 -c 'import sys; f=open(sys.argv[1],"wb"); f.truncate(2<<30)' "$SHARE/vm1.img"
python3 -c 'import sys; f=open(sys.argv[1],"wb"); f.truncate(2<<30)' "$SHARE/vm2.img"
start_daemon "$WORK/daemon.log"

echo "== special files"
scan '["empty_files","duplicates"]'
listed=$(api GET '/api/results?tool=empty_files' | python3 -c 'import json,sys; print(" ".join(f["name"] for f in json.load(sys.stdin).get("files") or []))')
case " $listed " in
	*" pipe.fifo "*|*" live.sock "*) fail "a named pipe or socket is listed as an Empty File: $listed" ;;
esac
echo "$listed" | grep -q 'empty.txt' || fail "the empty file itself is not listed: $listed"
pass "named pipes and sockets are not listed as Empty Files"
# The row the scan recorded is now a FIFO with the same size and mtime, so
# the identity check passes and verification has to open it. It must refuse
# at once — a plain open blocked forever here.
rm "$SHARE/empty.txt"; mkfifo "$SHARE/empty.txt"; touch -t 202401020900.00 "$SHARE/empty.txt"
r=$(curl -s -m 20 -X POST -H 'Content-Type: application/json' \
	-d "{\"tool\":\"empty_files\",\"files\":[\"$SHARE/empty.txt\"],\"dest\":\"$SHARE/dest\",\"verify\":true,\"preserve\":false}" \
	"http://127.0.0.1:$PORT/api/move") || fail "verifying a named pipe HUNG the move (no answer in 20s)"
echo "$r" | grep -q 'could not read the file to verify it' || fail "unexpected answer to verifying a named pipe: $r"
[ -p "$SHARE/empty.txt" ] || fail "the named pipe was moved"
r=$(api POST /api/scan "{\"tool\":\"duplicates\",\"tools\":[\"empty_files\"],\"dirs\":[\"$SHARE\"],\"recurse\":true,\"match\":{}}")
echo "$r" | grep -q '"started":true' || fail "daemon wedged after the refused verification: $r"
wait_idle
pass "verifying a row that became a named pipe fails at once, and the daemon is not wedged"

echo "== sparse files"
page=$(api POST /api/results '{"tool":"duplicates","offset":0,"limit":50}')
recl=$(echo "$page" | field total.reclaimable)
[ "$recl" = "19" ] || fail "reclaimable total is $recl, want 19 (the 2 GiB sparse pair must add nothing): $page"
echo "$page" | python3 -c '
import json,sys
d=json.load(sys.stdin)
for g in d["groups"]:
    names=sorted(f["name"] for f in g["files"])
    if names==["vm1.img","vm2.img"]:
        assert g["reclaim"]==0, g
        assert all(f.get("alloc")==0 for f in g["files"]), g
    elif names==["a.txt","b.txt"]:
        assert g["reclaim"]==19, g
        assert all("alloc" not in f for f in g["files"]), g
    else:
        sys.exit("unexpected group %r" % names)
' || fail "per-group reclaim/alloc wrong"
pass "sparse duplicates add nothing to the reclaimable figure (total and per group)"

echo "== names outside UTF-8"
if [ "$(uname)" = Linux ]; then
	BAD="$SHARE/$(printf 'caf\351.txt')"
	cp "$SHARE/a.txt" "$BAD"
	scan '["duplicates"]'
	res=$(api GET '/api/results?tool=duplicates')
	echo "$res" | grep -qF '�' && fail "the name lost its bytes on the way out: $res"
	stop_daemon; start_daemon "$WORK/daemon2.log"
	api GET '/api/results?tool=duplicates' > "$WORK/after.json"
	python3 - "$WORK/after.json" "$SHARE" <<'PY' || fail "the name did not survive a restart intact"
import json,sys
d=json.load(open(sys.argv[1], encoding="utf-8"))
names=[f["name"] for g in d["groups"] for f in g["files"]]
esc=[n for n in names if "\U0010fee9" in n]
assert len(esc)==1, names
PY
	# The row is marked unmovable (File Station cannot address the name),
	# and a move that names it with the exact string the daemon served is
	# refused with that reason — not with "no such file", and not with a
	# "rescan" that would change nothing.
	python3 - "$WORK/after.json" "$SHARE" "$PORT" <<'PY' || fail "a name outside UTF-8 is not handled as described"
import json,sys,urllib.request,os
d=json.load(open(sys.argv[1], encoding="utf-8")); share=sys.argv[2]; port=sys.argv[3]
row=[f for g in d["groups"] for f in g["files"] if "\U0010fee9" in f["name"]][0]
assert row.get("nomove") is True, row
body=json.dumps({"tool":"duplicates","files":[row["path"]+"/"+row["name"]],"dest":share+"/dest","verify":True,"preserve":False}).encode()
r=urllib.request.urlopen(urllib.request.Request("http://127.0.0.1:%s/api/move"%port, data=body, headers={"Content-Type":"application/json"}))
out=json.loads(r.read().decode("utf-8"))
assert not out["moved"] and len(out["errors"])==1 and "not valid Unicode" in out["errors"][0]["error"], out
assert os.path.exists(os.path.join(share.encode(), b"caf\xe9.txt"))
PY
	pass "a name outside UTF-8 is listed with its bytes, survives a restart, and is refused with the reason"
else
	echo "SKIP  names outside UTF-8: this filesystem refuses to create one (Linux only; CI covers it)"
fi

echo "== Stop"
r=$(api POST /api/scan "{\"tool\":\"duplicates\",\"tools\":[\"duplicates\"],\"dirs\":[\"$SHARE\"],\"recurse\":true,\"match\":{}}")
echo "$r" | grep -q '"started":true' || fail "scan refused: $r"
c=$(api POST /api/cancel '{}')
echo "$c" | grep -q '"ok":true' || fail "/api/cancel failed: $c"
wait_idle
if echo "$c" | grep -q '"cancelled":true'; then
	[ "$(api GET /api/state | field lastEnd.completed)" = "False" ] || fail "a cancelled scan reported itself completed"
	pass "Stop ends a running scan"
else
	pass "Stop answered (the scan had already finished; nothing to end)"
fi

echo "== free space"
stop_daemon
FREESPACE=1000 start_daemon "$WORK/daemon3.log"
scan '["duplicates"]'
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/vm1.img\"],\"dest\":\"$USB/usbshare\",\"verify\":false,\"preserve\":false}")
echo "$r" | grep -q 'free' || fail "a move onto a volume with 1000 bytes free was not refused: $r"
[ -f "$SHARE/vm1.img" ] || fail "the file was moved despite the refusal"
pass "a move onto a volume that cannot hold the files is refused: $(echo "$r" | field error)"
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/b.txt\"],\"dest\":\"$SHARE/dest\",\"verify\":true,\"preserve\":false}")
echo "$r" | grep -q '"moved":\["' || fail "a move within the volume was refused: $r"
pass "a move within the volume is not held to another volume's space"

echo "== SIGTERM"
kill -TERM "$DPID"
for i in $(seq 1 20); do kill -0 "$DPID" 2>/dev/null || break; sleep 0.25; done
kill -0 "$DPID" 2>/dev/null && fail "daemon still running 5s after SIGTERM"
st=0; wait "$DPID" || st=$?
DPID=""
[ "$st" = 0 ] || fail "daemon exited with status $st on SIGTERM"
grep -q 'stopping (terminated)' "$WORK/daemon3.log" && grep -q '^.*stopped$' "$WORK/daemon3.log" || fail "shutdown not logged: $(tail -3 "$WORK/daemon3.log")"
pass "SIGTERM stops the daemon cleanly (status 0, within 5s)"

echo
echo "AUDIT CHECKS PASSED"
