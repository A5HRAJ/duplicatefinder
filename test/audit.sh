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
#   moved copies    a copy the app moved, rescanned beside its original, is
#                   flagged as moved and never lets the original follow it —
#                   recorded for a flat move, recognised by layout for an
#                   older "keep folder structure" move, and kept across a restart
#   staying copy    with verification on, the copy a move would leave behind is
#                   read too: one changed since the scan (size and modified time
#                   standing) keeps its twin in place
#   self-mount      a shared folder of this NAS mounted back into itself is not
#                   scanned, never moved out of or into, and a copy seen through
#                   it never counts as the copy that stays
#   move records    a damaged record of moved copies is set aside and rebuilt
#                   from the results file; with every copy unreadable the loss
#                   is announced, across restarts, until it is acknowledged
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
: > "$WORK/mounts" # the stand-in for /proc/mounts: no network mounts yet
# The daemon is kept as a child (no disown): the SIGTERM check waits on it.
start_daemon() { # start_daemon <logfile>
	DUPFINDER_ROOTS="$V:$USB" DUPFINDER_UI="$WORK/ui" DUPFINDER_DSM_URL="http://127.0.0.1:$PORT" \
		DUPFINDER_STATE="$STATE" DUPFINDER_DEV_FREESPACE="${FREESPACE:-}" DUPFINDER_DEV_MOUNTS="$WORK/mounts" \
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

echo "== moved copies"
# b.txt now sits in $SHARE/dest, inside the scanned share, beside its original
# a.txt. An older preserve-mode move is recreated by hand: the daemon never
# recorded it, so only the batch folder's layout can identify the copy.
printf 'legacy content' > "$SHARE/x.txt"
mkdir -p "$SHARE/Old/Duplicates (2)/volume1/share"
cp -p "$SHARE/x.txt" "$SHARE/Old/Duplicates (2)/volume1/share/x.txt"
moved_check() { # moved_check <label>
	scan '["duplicates"]'
	api POST /api/results '{"tool":"duplicates","offset":0,"limit":100}' | python3 -c '
import json,sys
share=sys.argv[1]
out={f["path"]+"/"+f["name"]: f.get("out",False) for g in json.load(sys.stdin)["groups"] for f in g["files"]}
want={share+"/a.txt":False, share+"/dest/b.txt":True, share+"/x.txt":False,
      share+"/Old/Duplicates (2)/volume1/share/x.txt":True}
bad={p:(out.get(p),w) for p,w in want.items() if out.get(p)!=w}
if bad: sys.exit("moved-copy flags wrong (got, want): %r — all rows: %r" % (bad, out))' "$SHARE" || fail "$1: rows not flagged correctly"
	for orig in a.txt x.txt; do
		r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/$orig\"],\"dest\":\"$SHARE/dest\",\"verify\":false,\"preserve\":true}")
		echo "$r" | grep -q 'already moved' || fail "$1: moving the original $orig past its moved copy was not refused: $r"
		[ -f "$SHARE/$orig" ] || fail "$1: the original $orig moved despite the refusal"
	done
}
moved_check "before a restart"
pass "a moved copy is flagged (flat move recorded, older batch folder recognised), and moving its original is refused"
stop_daemon
FREESPACE=1000 start_daemon "$WORK/daemon3.log"
moved_check "after a restart"
pass "the record of moved copies survives a restart"
stop_daemon
printf 'not json' > "$STATE/moves.json"
FREESPACE=1000 start_daemon "$WORK/daemon3.log"
[ -f "$STATE/moves.json.damaged" ] || fail "the unreadable move record was not set aside"
[ -z "$(api GET /api/state | field recordsWarning)" ] || fail "a record the results file could rebuild was announced as lost"
moved_check "with the move record damaged"
pass "a damaged move record is set aside and rebuilt from the results file"
# The moved copy itself may still move: it is the original that must stay.
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/dest/b.txt\"],\"dest\":\"$USB/usbshare\",\"verify\":true,\"preserve\":false}")
echo "$r" | grep -q '"moved":\["' || fail "moving the moved copy on was refused: $r"
pass "a moved copy can still be moved on"

echo "== moved copies after their folder is renamed"
# The record must follow the moved file, not the folder's name: rename the
# folder a flat move landed in, and the copy inside it is still a moved copy.
printf 'renamed-folder content' > "$SHARE/y1.txt"
cp -p "$SHARE/y1.txt" "$SHARE/y2.txt"
mkdir -p "$SHARE/kept"
scan '["duplicates"]'
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/y2.txt\"],\"dest\":\"$SHARE/kept\",\"verify\":false,\"preserve\":false}")
echo "$r" | grep -q '"moved":\["' || fail "the flat move for the rename check failed: $r"
mv "$SHARE/kept" "$SHARE/kept renamed"
scan '["duplicates"]'
api POST /api/results '{"tool":"duplicates","offset":0,"limit":100}' | python3 -c '
import json,sys
share=sys.argv[1]
out={f["path"]+"/"+f["name"]: f.get("out",False) for g in json.load(sys.stdin)["groups"] for f in g["files"]}
want={share+"/y1.txt":False, share+"/kept renamed/y2.txt":True}
bad={p:(out.get(p),w) for p,w in want.items() if out.get(p)!=w}
if bad: sys.exit("moved-copy flags wrong after the rename (got, want): %r" % bad)' "$SHARE" || fail "renaming the folder lost the moved-copy record"
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/y1.txt\"],\"dest\":\"$SHARE/dest\",\"verify\":false,\"preserve\":true}")
echo "$r" | grep -q 'already moved' || fail "moving the original past its renamed-folder copy was not refused: $r"
[ -f "$SHARE/y1.txt" ] || fail "the original moved despite the refusal"
pass "a moved copy stays a moved copy after its folder is renamed, and its original stays put"

echo "== a changed copy that would stay"
# Two identical files; after the scan one is altered in place with its size
# and modified time put back — what File Station checks cannot tell. Moving
# the OTHER one with verification on must keep it where it is: it is the
# only copy that still holds what the scan found.
python3 -c 'import sys; open(sys.argv[1],"wb").write(bytes(range(256))*400)' "$SHARE/s1.bin"
cp -p "$SHARE/s1.bin" "$SHARE/s2.bin"
scan '["duplicates"]'
python3 -c '
import os,sys
p=sys.argv[1]; st=os.stat(p)
with open(p,"r+b") as f: f.seek(90000); b=f.read(1); f.seek(90000); f.write(bytes([b[0]^1]))
os.utime(p, ns=(st.st_atime_ns, st.st_mtime_ns))' "$SHARE/s2.bin"
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/s1.bin\"],\"dest\":\"$SHARE/dest\",\"verify\":true,\"preserve\":false}")
echo "$r" | grep -q 'would have stayed no longer matches' || fail "moving the good copy past a changed twin was not refused: $r"
[ -f "$SHARE/s1.bin" ] || fail "the good copy moved despite the refusal"
pass "with verification on, a copy that changed since the scan never stays in place of its good twin"

echo "== a shared folder of this NAS mounted into itself"
# $SHARE/mnt stands for a CIFS mount of this NAS's own folder (DSM refuses its
# own addresses but not a name that resolves to one). Its m1.txt is the same
# file as $SHARE/m1.txt: scanned before the mount was known, the two pair up.
printf 'one file, two paths' > "$SHARE/m1.txt"
mkdir -p "$SHARE/mnt"
cp -p "$SHARE/m1.txt" "$SHARE/mnt/m1.txt"
scan '["duplicates"]'
api POST /api/results '{"tool":"duplicates","offset":0,"limit":100}' | grep -q 'mnt' || fail "fixture: the mount's copy did not pair up before the mount was known"
printf '//nas.example.nip.io/share %s cifs rw,addr=127.0.0.1 0 0\n' "$SHARE/mnt" > "$WORK/mounts"
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/m1.txt\"],\"dest\":\"$SHARE/dest\",\"verify\":true,\"preserve\":false}")
echo "$r" | grep -q 'keeping one copy' || fail "the file moved although its only other copy is itself seen through the mount: $r"
[ -f "$SHARE/m1.txt" ] || fail "the file left despite the refusal"
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/mnt/m1.txt\"],\"dest\":\"$SHARE/dest\",\"verify\":true,\"preserve\":false}")
echo "$r" | grep -q 'mounted from this same NAS' || fail "a path inside the mount was not refused: $r"
[ -f "$SHARE/mnt/m1.txt" ] || fail "the mount's path moved despite the refusal"
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/s1.bin\"],\"dest\":\"$SHARE/mnt\",\"verify\":false,\"preserve\":false}")
echo "$r" | grep -q 'destination cannot be used' || fail "a destination inside the mount was not refused: $r"
scan '["duplicates","empty_folders"]'
res=$(api POST /api/results '{"tool":"duplicates","offset":0,"limit":100}')
echo "$res" | python3 -c '
import json,sys
rows=[f["path"] for g in json.load(sys.stdin)["groups"] for f in g["files"]]
sys.exit(1 if any(p.endswith("/mnt") or "/mnt/" in p for p in rows) else 0)' || fail "the mount was scanned: $res"
echo "$res" | grep -q 'not scanned' || fail "the skipped mount is not reported: $res"
api POST /api/results '{"tool":"empty_folders","offset":0,"limit":100}' | grep -q '"name":"mnt"' && fail "the mount point is offered as an empty folder"
: > "$WORK/mounts"
pass "a mount of this NAS's own folder is not scanned, not moved out of or into, and never the copy that stays"

echo "== move records lost"
stop_daemon
for f in moves.json moves.json.prev results.json.gz; do printf 'not json' > "$STATE/$f"; done
FREESPACE=1000 start_daemon "$WORK/daemon3.log"
api GET /api/state | field recordsWarning | grep -q 'could not read' || fail "a lost move record was not announced: $(api GET /api/state)"
stop_daemon
FREESPACE=1000 start_daemon "$WORK/daemon3.log"
[ -n "$(api GET /api/state | field recordsWarning)" ] || fail "the announcement did not survive a restart"
api POST /api/records/ack '{}' | grep -q '"ok":true' || fail "the acknowledgement was refused"
[ -z "$(api GET /api/state | field recordsWarning)" ] || fail "the acknowledged announcement is still shown"
stop_daemon
FREESPACE=1000 start_daemon "$WORK/daemon3.log"
[ -z "$(api GET /api/state | field recordsWarning)" ] || fail "the acknowledged announcement came back after a restart"
pass "a lost move record is announced, across restarts, until acknowledged"

echo "== a move cut short by a stop"
# Two flat moves into a folder inside the scanned share. The second never
# finishes (the mock's never-lands hook — a copy longer than the stop's
# grace), so the daemon is stopped with the batch half done. The first file's
# record must survive: a moved copy forgotten would read as an original and
# let its original follow it into the cleanup folder.
printf 'cut-short one' > "$SHARE/c1.txt"; cp -p "$SHARE/c1.txt" "$SHARE/c1-copy.txt"
printf 'cut-short two' > "$SHARE/c2.txt"; cp -p "$SHARE/c2.txt" "$SHARE/c2-never-lands.txt"
mkdir -p "$SHARE/cut"
scan '["duplicates"]'
curl -s -m 60 -X POST -H 'Content-Type: application/json' \
	-d "{\"tool\":\"duplicates\",\"id\":\"cut1\",\"files\":[\"$SHARE/c1-copy.txt\",\"$SHARE/c2-never-lands.txt\"],\"dest\":\"$SHARE/cut\",\"verify\":false,\"preserve\":false}" \
	"http://127.0.0.1:$PORT/api/move" > "$WORK/cut.json" 2>&1 &
CPID=$!
for _ in $(seq 1 80); do api GET /api/state | grep -q 'never-lands' && break; sleep 0.25; done
api GET /api/state | grep -q 'never-lands' || fail "fixture: the second move never started: $(api GET /api/state)"
[ -f "$SHARE/cut/c1-copy.txt" ] || fail "fixture: the first file did not move"
kill -TERM "$DPID"; wait "$DPID" 2>/dev/null || true; DPID=""
wait "$CPID" 2>/dev/null || true
[ -f "$STATE/moves.journal" ] || fail "the cut-short move left no journal"
FREESPACE=1000 start_daemon "$WORK/daemon3.log"
[ -f "$STATE/moves.journal" ] && fail "the journal is still there after its replay"
[ "$(api GET /api/state | field lastMove.id)" = cut1 ] || fail "the cut-short move's report is not offered: $(api GET /api/state)"
api GET '/api/move/report?id=cut1' | python3 -c '
import json,sys
share=sys.argv[1]
r=json.load(sys.stdin)["report"]
if not r.get("interrupted"): sys.exit("report not marked interrupted: %r" % r)
if r["moved"]!=[share+"/c1-copy.txt"]: sys.exit("moved list wrong: %r" % r["moved"])
e={x["path"]:x["error"] for x in r["errors"]}
if "may have finished" not in e.get(share+"/c2-never-lands.txt",""): sys.exit("in-flight file not reported: %r" % e)' "$SHARE" || fail "the cut-short move's report is wrong"
api POST /api/results '{"tool":"duplicates","offset":0,"limit":100}' | grep -q "\"c1-copy.txt\"" && fail "the moved file's row is back after the restart"
scan '["duplicates"]'
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/c1.txt\"],\"dest\":\"$SHARE/dest\",\"verify\":false,\"preserve\":true}")
echo "$r" | grep -q 'already moved' || fail "the original of a file moved before the stop was not protected: $r"
[ -f "$SHARE/c1.txt" ] || fail "the original moved despite the refusal"
pass "a move cut short by a stop keeps the record of what it moved, prunes it, and reports what was in flight"

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
