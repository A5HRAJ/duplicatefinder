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
#   empty folders   a folder holding only empty folders (or junk) is listed and
#                   moves, verified, to another volume; a hidden folder or a
#                   real file at any depth keeps it off the list, the empty
#                   folders beside one are still listed, and a file that
#                   arrives two levels down after the scan stops its move
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

ALL_TOOLS='["duplicates","empty_folders","empty_files","temp_files","corrupted_files"]'

echo "== a scan only reads"
# Every entry on both volumes is fingerprinted before and after a scan with
# all five tools: type, permissions, size, modified time, inode change time
# (which any write, rename, chmod or extended-attribute change moves, and no
# program can set back), link count, inode and content. The fixture gives
# each tool something to read: a temporary file, empty folders, a hard link,
# and a pair of ZIPs sharing size and modified time with one damaged, so the
# Conflicting Files check opens and decodes both.
mkdir -p "$SHARE/e9/empty dir" "$SHARE/e9/deep/er"
printf 'half a download' > "$SHARE/e9/film.part"
ln "$SHARE/a.txt" "$SHARE/e9/a-link.txt"
python3 - "$SHARE/e9" <<'PY'
import os,sys,zipfile
d=sys.argv[1]
z1=os.path.join(d,"z1.zip"); z2=os.path.join(d,"z2.zip")
with zipfile.ZipFile(z1,"w",zipfile.ZIP_STORED) as z: z.writestr("notes.txt", "a line of notes\n"*300)
b=bytearray(open(z1,"rb").read()); b[2000]^=1; open(z2,"wb").write(b)
st=os.stat(z1); os.utime(z2, ns=(st.st_atime_ns, st.st_mtime_ns))
PY
fingerprint() { # fingerprint <out>
	python3 - "$V" "$USB" > "$1" <<'PY'
import hashlib,os,stat,sys
for top in sys.argv[1:]:
    for d,dirs,files in os.walk(top):
        dirs.sort()
        for n in sorted(dirs+files):
            p=os.path.join(d,n); st=os.lstat(p); h=""
            if stat.S_ISREG(st.st_mode) and st.st_size<=64<<20:
                with open(p,"rb") as f: h=hashlib.sha256(f.read()).hexdigest()
            print(repr(p), stat.S_IFMT(st.st_mode), oct(st.st_mode&0o7777), st.st_size,
                  st.st_mtime_ns, st.st_ctime_ns, st.st_nlink, st.st_ino, h)
PY
}
fingerprint "$WORK/fp-before"
scan "$ALL_TOOLS"
fingerprint "$WORK/fp-after"
diff "$WORK/fp-before" "$WORK/fp-after" > "$WORK/fp-diff" || fail "a scan changed something on disk: $(head -6 "$WORK/fp-diff")"
api GET '/api/results?tool=temp_files' | grep -q 'film.part' || fail "fixture: the temporary file was not found, so the scan did not read the fixture"
api GET '/api/results?tool=empty_folders' | grep -q 'empty dir' || fail "fixture: the empty folder was not found"
api GET '/api/results?tool=corrupted_files' | grep -q 'z2.zip' || fail "fixture: the ZIP pair was not checked"
pass "a scan with every tool changes nothing on disk ($(wc -l < "$WORK/fp-before" | tr -d ' ') entries fingerprinted)"
rm -rf "$SHARE/e9"

echo "== recycle bin and snapshots"
# Copies in the recycle bin or a snapshot disappear on their own. Met inside
# a scanned folder they are skipped, so the one live copy is not a duplicate
# of anything; chosen directly — as a scan folder, a reference folder, or
# through a shortcut — they are refused.
SNAP="$SHARE/#snapshot/GMT+00_2026.11.01-00.00.00"
mkdir -p "$SHARE/#recycle/old/emptydir" "$SNAP/sub"
printf 'one live copy, two that will vanish' > "$SHARE/r1.txt"
cp -p "$SHARE/r1.txt" "$SHARE/#recycle/old/r1.txt"
cp -p "$SHARE/r1.txt" "$SNAP/sub/r1.txt"
: > "$SHARE/#recycle/old/empty.txt"
printf 'x' > "$SHARE/#recycle/old/junk.tmp"
scan "$ALL_TOOLS"
for tool in duplicates empty_folders empty_files temp_files corrupted_files; do
	out=$(api GET "/api/results?tool=$tool")
	echo "$out" | grep -q -e '#recycle' -e '#snapshot' && fail "$tool lists something inside the recycle bin or a snapshot: $out"
done
api GET '/api/results?tool=duplicates' | grep -q 'r1.txt' && fail "the live copy was paired with its copies in the bin or a snapshot"
ln -s "$SHARE/#recycle/old" "$SHARE/binlink"
for body in \
	"{\"tool\":\"duplicates\",\"tools\":[\"duplicates\"],\"dirs\":[\"$SHARE/#recycle\"],\"recurse\":true,\"match\":{}}" \
	"{\"tool\":\"duplicates\",\"tools\":[\"duplicates\"],\"dirs\":[\"$SNAP/sub\"],\"recurse\":true,\"match\":{}}" \
	"{\"tool\":\"duplicates\",\"tools\":[\"duplicates\"],\"dirs\":[\"$SHARE/binlink\"],\"recurse\":true,\"match\":{}}" \
	"{\"tool\":\"duplicates\",\"tools\":[\"duplicates\"],\"dirs\":[\"$SHARE\"],\"refDirs\":[\"$SNAP\"],\"recurse\":true,\"match\":{}}"; do
	r=$(api POST /api/scan "$body")
	echo "$r" | grep -q 'never scanned' || fail "a scan reaching into the recycle bin or a snapshot was not refused: $body → $r"
done
r=$(api POST /api/refs "{\"refDirs\":[\"$SNAP\"]}")
echo "$r" | grep -q 'never scanned' || fail "a snapshot was accepted as a reference folder: $r"
rm -rf "$SHARE/#recycle" "$SHARE/#snapshot" "$SHARE/binlink" "$SHARE/r1.txt"
pass "the recycle bin and snapshots are never scanned, met inside a folder or chosen directly"

echo "== moves into a reference folder"
# The reference folder is read-only both ways: nothing is moved into it,
# directly, into a folder inside it, or through a shortcut to it — and the
# refusal comes before anything is created there.
mkdir -p "$SHARE/master/inner"
ln -s "$SHARE/master" "$SHARE/masterlink"
r=$(api POST /api/scan "{\"tool\":\"duplicates\",\"tools\":[\"duplicates\"],\"dirs\":[\"$SHARE\"],\"refDirs\":[\"$SHARE/master\"],\"recurse\":true,\"match\":{}}")
echo "$r" | grep -q '"started":true' || fail "scan refused: $r"
wait_idle
for dest in master master/inner masterlink; do
	for preserve in false true; do
		r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/b.txt\"],\"dest\":\"$SHARE/$dest\",\"verify\":true,\"preserve\":$preserve}")
		echo "$r" | grep -q 'inside a read-only reference folder' || fail "a move into the reference folder ($dest, preserve $preserve) was not refused: $r"
	done
done
[ -f "$SHARE/b.txt" ] || fail "the file moved despite the refusal"
[ -z "$(find "$SHARE/master" -mindepth 1 ! -path "$SHARE/master/inner")" ] || fail "something was created inside the reference folder: $(find "$SHARE/master")"
r=$(api POST /api/refs '{"refDirs":[]}')
echo "$r" | grep -q -e '"refDirs":\[\]' -e '"refDirs":null' || fail "the reference folder could not be cleared: $r"
rm -rf "$SHARE/master" "$SHARE/masterlink"
pass "nothing is moved into a reference folder, a folder inside it, or through a shortcut to it"

echo "== a folder moved into itself"
# Moving an empty folder into itself is refused before any batch folder is
# made; so is moving it into a folder made inside it after the scan.
# (TestMoveRefusesADestinationInsideTheFolderBeingMoved proves the guard
# itself for a folder already holding the destination.)
mkdir -p "$SHARE/ef"
scan '["empty_folders"]'
api GET '/api/results?tool=empty_folders' | grep -q '"name":"ef"' || fail "fixture: the empty folder was not listed: $(api GET '/api/results?tool=empty_folders')"
for preserve in false true; do
	r=$(api POST /api/move "{\"tool\":\"empty_folders\",\"files\":[\"$SHARE/ef\"],\"dest\":\"$SHARE/ef\",\"verify\":true,\"preserve\":$preserve}")
	echo "$r" | grep -q 'inside the folder being moved' || fail "moving a folder into itself (preserve $preserve) was not refused: $r"
done
[ -z "$(ls -A "$SHARE/ef")" ] || fail "something was created inside the folder: $(find "$SHARE/ef")"
mkdir "$SHARE/ef/inner"
for preserve in false true; do
	r=$(api POST /api/move "{\"tool\":\"empty_folders\",\"files\":[\"$SHARE/ef\"],\"dest\":\"$SHARE/ef/inner\",\"verify\":true,\"preserve\":$preserve}")
	echo "$r" | grep -q '"moved":\[\]\|"moved":null' || fail "moving a folder into a folder inside it (preserve $preserve) was not refused: $r"
done
[ -d "$SHARE/ef/inner" ] && [ -z "$(ls -A "$SHARE/ef/inner")" ] || fail "the folder moved, or something was created inside it: $(find "$SHARE/ef")"
rm -rf "$SHARE/ef"
pass "a folder is never moved into itself or a folder inside it"

echo "== the destination fills up mid-move"
# Something else fills the destination partway through: the mock writes half
# of each file there and fails the copy, with File Station's "no space left"
# code or with the bare failure code. Verified or not, the original stays
# byte for byte, its row stays listed and is not taken for a moved copy, no
# staging folder is left, and the report says where any incomplete copy is.
sha() { python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$1"; }
for f in g5-fills-dest g5-fills-dest-generic; do
	python3 -c 'import os,sys; open(sys.argv[1],"wb").write(os.urandom(256*1024))' "$SHARE/$f.bin"
	cp -p "$SHARE/$f.bin" "$SHARE/$f-twin.bin"
done
sum1=$(sha "$SHARE/g5-fills-dest.bin"); sum2=$(sha "$SHARE/g5-fills-dest-generic.bin")
scan '["duplicates"]'
for verify in true false; do
	for f in g5-fills-dest g5-fills-dest-generic; do
		r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/$f.bin\"],\"dest\":\"$USB/usbshare\",\"verify\":$verify,\"preserve\":false}")
		echo "$r" | python3 -c '
import json,sys
r=json.load(sys.stdin); verify=sys.argv[1]=="true"; name=sys.argv[2]
e=r["errors"][0]["error"] if len(r.get("errors") or [])==1 else ""
ok=not r.get("moved") and "original" in e and "parked" not in e
if not verify: ok=ok and ("incomplete copy at /usbshare1/"+name+".bin") in e
sys.exit(0 if ok else 1)' "$verify" "$f" || fail "a move onto a destination that filled up (verify $verify, $f) was not reported as not done: $r"
		[ "$(sha "$SHARE/$f.bin")" = "$([ $f = g5-fills-dest ] && echo "$sum1" || echo "$sum2")" ] || fail "the original changed or left (verify $verify, $f)"
		[ -z "$(find "$USB/usbshare" -name '.dupfinder-*')" ] || fail "a staging folder was left behind: $(find "$USB/usbshare")"
		if [ "$verify" = true ]; then
			[ -e "$USB/usbshare/$f.bin" ] && fail "a verified move left an incomplete copy at the destination"
		else
			rm "$USB/usbshare/$f.bin" || fail "fixture: the mock left no incomplete copy"
		fi
	done
done
api GET '/api/results?tool=duplicates' | grep -q 'g5-fills-dest-generic.bin' || fail "the row of a file that never left was dropped"
scan '["duplicates"]'
api POST /api/results '{"tool":"duplicates","offset":0,"limit":100}' | python3 -c '
import json,sys
rows={f["name"]: f.get("out",False) for g in json.load(sys.stdin)["groups"] for f in g["files"] if f["name"].startswith("g5-")}
sys.exit(0 if len(rows)==4 and not any(rows.values()) else "rows wrong: %r" % rows)' || fail "after a failed move, the pairs are not intact originals"
rm -f "$SHARE"/g5-*
pass "a destination that fills up mid-move loses nothing, verified or not, and the report says so"

echo "== folders holding the last copy"
# A folder holding nothing but junk is offered as empty, and moving it takes
# the junk along. When that junk is a duplicate, the folder counts as moving
# its copy: two such folders holding the same file never both leave — not in
# one request, not one after the other, and not with a rescan in between that
# pairs the moved copy with the one left behind.
for d in j1 j2 j3 j4 j5 j6; do mkdir -p "$SHARE/jf/$d"; done
printf 'a real file, so each folder below is offered on its own' > "$SHARE/jf/keep.txt"
printf 'thumbnail cache, pair one' > "$SHARE/jf/j1/Thumbs.db"; cp -p "$SHARE/jf/j1/Thumbs.db" "$SHARE/jf/j2/Thumbs.db"
printf 'thumbnail cache, pair two!' > "$SHARE/jf/j3/Thumbs.db"; cp -p "$SHARE/jf/j3/Thumbs.db" "$SHARE/jf/j4/Thumbs.db"
printf 'thumbnail cache, pair three!!' > "$SHARE/jf/j5/Thumbs.db"; cp -p "$SHARE/jf/j5/Thumbs.db" "$SHARE/jf/j6/Thumbs.db"
mkdir -p "$SHARE/jfdest"
scan '["duplicates","empty_folders"]'
listed=$(api GET '/api/results?tool=empty_folders' | python3 -c 'import json,sys; print(" ".join(sorted(f["name"] for f in json.load(sys.stdin)["files"] if f["name"].startswith("j") and f["name"][1:].isdigit())))')
[ "$listed" = "j1 j2 j3 j4 j5 j6" ] || fail "fixture: the junk-only folders are not all offered as empty: $listed"
efmove() { # efmove <folder>... — one empty_folders request into jfdest
	local files="" f
	for f in "$@"; do files="$files${files:+,}\"$SHARE/jf/$f\""; done
	api POST /api/move "{\"tool\":\"empty_folders\",\"files\":[$files],\"dest\":\"$SHARE/jfdest\",\"verify\":false,\"preserve\":false}"
}
r=$(efmove j1 j2)
echo "$r" | python3 -c '
import json,sys
r=json.load(sys.stdin)
ok=len(r.get("moved") or [])==1 and len(r["errors"])==1 and "keeping one copy" in r["errors"][0]["error"]
sys.exit(0 if ok else 1)' || fail "moving both folders of a pair in one request did not hold one back: $r"
[ "$(ls "$SHARE"/jf/j1/Thumbs.db "$SHARE"/jf/j2/Thumbs.db 2>/dev/null | wc -l | tr -d ' ')" = 1 ] || fail "the pair's copies did not end with exactly one in place"
r=$(efmove j3); echo "$r" | grep -q '"moved":\["' || fail "fixture: the first folder of pair two did not move: $r"
r=$(efmove j4); echo "$r" | grep -q 'keeping' || fail "after one folder moved, the folder holding the last copy moved too: $r"
[ -f "$SHARE/jf/j4/Thumbs.db" ] || fail "the last copy of pair two left its folder"
r=$(efmove j5); echo "$r" | grep -q '"moved":\["' || fail "fixture: the first folder of pair three did not move: $r"
scan '["duplicates","empty_folders"]'
r=$(efmove j6); echo "$r" | grep -q 'keeping' || fail "after a rescan, the folder holding the last copy moved too: $r"
[ -f "$SHARE/jf/j6/Thumbs.db" ] || fail "the last copy of pair three left its folder"
rm -rf "$SHARE/jf" "$SHARE/jfdest"
pass "a folder holding the last copy of a duplicate is held back, in one request, one after another, and after a rescan"

echo "== a reference folder inside a folder offered as empty"
# A folder holding nothing but a junk-only folder is offered as empty. When
# that inner folder is a read-only reference folder, the outer one must not
# be offered, and must not move if the reference was set after the scan —
# not even alongside a twin folder whose copy the reference was keeping.
mkdir -p "$SHARE/rf/A/reference" "$SHARE/rf/B"
printf 'a real file, so A and B are offered on their own' > "$SHARE/rf/keep.txt"
printf 'thumbnail cache kept by a reference' > "$SHARE/rf/A/reference/Thumbs.db"; cp -p "$SHARE/rf/A/reference/Thumbs.db" "$SHARE/rf/B/Thumbs.db"
mkdir -p "$SHARE/rfdest"
r=$(api POST /api/scan "{\"tool\":\"duplicates\",\"tools\":[\"duplicates\",\"empty_folders\"],\"dirs\":[\"$SHARE\"],\"refDirs\":[\"$SHARE/rf/A/reference\"],\"recurse\":true,\"match\":{}}")
echo "$r" | grep -q '"started":true' || fail "scan refused: $r"
wait_idle
api GET '/api/results?tool=empty_folders' | python3 -c '
import json,sys
rows=[f["path"]+"/"+f["name"] for f in json.load(sys.stdin)["files"]]
sys.exit(1 if any(r.endswith("/rf/A") for r in rows) else 0)' || fail "a folder holding a reference folder was offered as empty"
api POST /api/refs '{"refDirs":[]}' >/dev/null
scan '["duplicates","empty_folders"]'
api GET '/api/results?tool=empty_folders' | grep -q '"name":"A"' || fail "fixture: without a reference, A is not offered"
r=$(api POST /api/refs "{\"refDirs\":[\"$SHARE/rf/A/reference\"]}")
echo "$r" | grep -q 'reference' || fail "fixture: the reference could not be set: $r"
r=$(api POST /api/move "{\"tool\":\"empty_folders\",\"files\":[\"$SHARE/rf/A\",\"$SHARE/rf/B\"],\"dest\":\"$SHARE/rfdest\",\"verify\":false,\"preserve\":false}")
echo "$r" | grep -q 'read-only reference folder' || fail "a folder holding a reference folder was not refused: $r"
[ -f "$SHARE/rf/A/reference/Thumbs.db" ] || fail "the reference folder's file left with its parent"
api POST /api/refs '{"refDirs":[]}' >/dev/null
rm -rf "$SHARE/rf" "$SHARE/rfdest"
pass "a folder holding a reference folder is neither offered as empty nor moved"

echo "== copies that went away after the scan"
# Which copies still exist is asked of File Station at move time. A copy
# deleted since the scan, or rewritten so it no longer has the scan's size
# and modified time, does not count as the copy that stays. Verification is
# off, so this is the existence check alone, not the read of B5.
printf 'b7 pair one' > "$SHARE/b7a.txt"; cp -p "$SHARE/b7a.txt" "$SHARE/b7b.txt"
printf 'b7 pair two' > "$SHARE/b7c.txt"; cp -p "$SHARE/b7c.txt" "$SHARE/b7d.txt"
scan '["duplicates"]'
rm "$SHARE/b7b.txt"
printf 'b7 pair two, since rewritten' > "$SHARE/b7d.txt"
for f in b7a b7c; do
	r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/$f.txt\"],\"dest\":\"$SHARE/dest\",\"verify\":false,\"preserve\":false}")
	echo "$r" | grep -q 'keeping one copy' || fail "$f.txt moved although its only other copy changed or went away after the scan: $r"
	[ -f "$SHARE/$f.txt" ] || fail "$f.txt left despite the refusal"
done
rm -f "$SHARE"/b7*.txt
pass "a copy deleted or rewritten after the scan never counts as the copy that stays"

echo "== a name too long for ' (1)'"
# A clash at the destination is settled with " (1)", but a name already at
# the length limit has no room for it. The file must end up somewhere it can
# be found — a holding folder at the destination, named in the report — and
# the file already there must stay as it was.
LONG=$(python3 -c 'print("n"*251 + ".bin")')
mkdir -p "$SHARE/longdest" "$SHARE/longsrc"
printf 'already at the destination' > "$SHARE/longdest/$LONG"
printf 'the file being moved, with a long name' > "$SHARE/longsrc/$LONG"; cp -p "$SHARE/longsrc/$LONG" "$SHARE/longtwin.bin"
want=$(sha "$SHARE/longsrc/$LONG")
scan '["duplicates"]'
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/longsrc/$LONG\"],\"dest\":\"$SHARE/longdest\",\"verify\":true,\"preserve\":false}")
echo "$r" | python3 -c '
import json,sys
r=json.load(sys.stdin); e=r["errors"][0]["error"] if len(r.get("errors") or [])==1 else ""
sys.exit(0 if "parked in /share/longdest/.dupfinder-tmp-" in e and "verified intact" in e else 1)' || fail "the file with no room for \" (1)\" was not reported in a holding folder: $r"
[ "$(cat "$SHARE/longdest/$LONG")" = 'already at the destination' ] || fail "the file already at the destination changed"
held=$(find "$SHARE/longdest" -path '*/.dupfinder-tmp-*' -name "$LONG")
[ -n "$held" ] && [ "$(sha "$held")" = "$want" ] || fail "the moved file is not intact in the holding folder: $(find "$SHARE/longdest")"
rm -rf "$SHARE/longdest" "$SHARE/longsrc" "$SHARE/longtwin.bin"
pass "a file with no room for \" (1)\" is kept, intact, in a holding folder the report names"

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

echo "== folders holding only empty folders"
# A folder whose only content is another, empty folder is listed as one row:
# the walk offers only the topmost folder whose subtree holds no file, so a
# confirmation that refused any folder inside would list neither it nor the
# folder inside. Beside it, junk two folders down (listed), and what must
# still keep a folder off the list at any depth: a real file, a hidden folder
# (.git), and a folder with a junk name (.DS_Store/) holding a real file.
# Then the moves: across volumes with verification on (copy, check every
# level, remove), and a folder that gained a file two levels down since the
# scan, which must be refused.
mkdir -p "$SHARE/efnest/inner" "$SHARE/efjunk/a/b" "$SHARE/efreal/inner" \
	"$SHARE/efgit/inner/.git" "$SHARE/efds/inner/.DS_Store"
printf 'junk two folders down' > "$SHARE/efjunk/a/b/Thumbs.db"
printf 'a real file' > "$SHARE/efreal/inner/keep.txt"
printf '[core]' > "$SHARE/efgit/inner/.git/config"
printf 'a photo in a folder with a junk name' > "$SHARE/efds/inner/.DS_Store/precious.jpg"
# An empty folder BESIDE what keeps its parent off the list is listed on its
# own: beside a hidden folder holding a file, beside a folder with a junk
# name (itself empty, so listed too), and beside a symlink. The walk passes
# over or walks into each of these without finding a file, so it has to
# count them as content itself; leaving that to the confirmation offers the
# parent, refuses it, and never offers the empty folder beside it.
mkdir -p "$SHARE/efside/a" "$SHARE/efside/b/.git" "$SHARE/efside2/a" "$SHARE/efside2/cache.tmp" "$SHARE/efside3/a"
printf '[core]' > "$SHARE/efside/b/.git/config"
ln -s "$SHARE/dest" "$SHARE/efside3/link"
scan '["empty_folders"]'
api POST /api/results '{"tool":"empty_folders","offset":0,"limit":100}' | python3 -c '
import json,sys
share=sys.argv[1]
d=json.load(sys.stdin)
rows={f["path"]+"/"+f["name"] for f in d.get("files") or []}
mine={r for r in rows if r.startswith(share+"/ef")}
want={share+"/efnest", share+"/efjunk", share+"/efside/a", share+"/efside2/a",
      share+"/efside2/cache.tmp", share+"/efside3/a"}
if mine!=want: sys.exit("listed %r, want %r; errors %r" % (sorted(mine), sorted(want), d.get("errors")))' "$SHARE" || fail "folders holding only empty folders are not listed as they should be"
r=$(api POST /api/move "{\"tool\":\"empty_folders\",\"files\":[\"$SHARE/efnest\"],\"dest\":\"$USB/usbshare\",\"verify\":true,\"preserve\":false}")
echo "$r" | grep -q '"moved":\["' || fail "a verified move of a folder of empty folders to another volume failed: $r"
[ -d "$USB/usbshare/efnest/inner" ] || fail "the folder inside did not arrive at the other volume"
[ -e "$SHARE/efnest" ] && fail "the original is still in place after the verified move"
printf 'arrived after the scan' > "$SHARE/efjunk/a/b/late.txt"
r=$(api POST /api/move "{\"tool\":\"empty_folders\",\"files\":[\"$SHARE/efjunk\"],\"dest\":\"$SHARE/dest\",\"verify\":false,\"preserve\":false}")
echo "$r" | grep -q 'no longer empty' || fail "a folder that gained a file two levels down since the scan was not refused: $r"
[ -f "$SHARE/efjunk/a/b/late.txt" ] || fail "the late file moved despite the refusal"
rm -rf "$SHARE/efnest" "$SHARE/efjunk" "$SHARE/efreal" "$SHARE/efgit" "$SHARE/efds" \
	"$SHARE/efside" "$SHARE/efside2" "$SHARE/efside3" "$USB/usbshare/efnest"
pass "a folder holding only empty folders (or junk) is listed and moves, verified, across volumes; hidden or real content at any depth keeps it off the list, and the empty folders beside such content are listed"

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
# While that move runs, the daemon itself refuses a second one — not only the
# app's window — and the refusal comes before anything moves.
r=$(curl -s -m 20 -X POST -H 'Content-Type: application/json' \
	-d "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/c2.txt\"],\"dest\":\"$SHARE/dest\",\"verify\":false,\"preserve\":true}" \
	"http://127.0.0.1:$PORT/api/move") || fail "a second move waited for the running one instead of being refused (no answer in 20s)"
echo "$r" | grep -q 'a move is already running' || fail "a second move was not refused while one was running: $r"
[ -f "$SHARE/c2.txt" ] && [ -z "$(find "$SHARE/dest" -name c2.txt)" ] || fail "the refused second move moved its file"
pass "a second move is refused while one is running, and moves nothing"
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

echo "== move records gone entirely"
# Package Center's Repair, or the files carried to a new NAS: every record the
# app keeps is gone, which it cannot tell from a fresh install. A flat move's
# own record, left in the folder the files landed in, is what still says the
# copy there is one Duplicate Finder moved — so its original stays put.
printf 'kept by the record in its folder' > "$SHARE/w1.txt"; cp -p "$SHARE/w1.txt" "$SHARE/w1-copy.txt"
printf 'a file of the user'"'"'s own' > "$SHARE/w2.txt"
mkdir -p "$SHARE/flatdest"; cp -p "$SHARE/w2.txt" "$SHARE/flatdest/w2.txt"
# And a junk-only folder moved flat (an Empty Folders row): its Thumbs.db is
# a duplicate of one left in place, and must still be known as moved.
mkdir -p "$SHARE/wj" "$SHARE/wtwin"
printf 'thumbnail cache carried by a folder move' > "$SHARE/wj/Thumbs.db"; cp -p "$SHARE/wj/Thumbs.db" "$SHARE/wtwin/Thumbs.db"
scan '["duplicates","empty_folders"]'
r=$(api POST /api/move "{\"tool\":\"empty_folders\",\"files\":[\"$SHARE/wj\"],\"dest\":\"$SHARE/flatdest\",\"verify\":true,\"preserve\":false}")
echo "$r" | grep -q '"moved":\["' || fail "fixture: the flat folder move failed: $r"
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/w1-copy.txt\"],\"dest\":\"$SHARE/flatdest\",\"verify\":true,\"preserve\":false}")
echo "$r" | grep -q '"moved":\["' || fail "the flat move for the records check failed: $r"
echo "$r" | grep -q 'recordError' && fail "the record was not left: $r"
python3 - "$SHARE/flatdest" <<'PY' || fail "the destination holds no record of the move"
import glob,json,os,sys
recs=[json.load(open(p)) for p in glob.glob(os.path.join(sys.argv[1], ".duplicate-finder-moved", "*.json"))]
assert sorted(e["name"] for r in recs for e in r["moved"])==["w1-copy.txt","wj/Thumbs.db"], recs
PY
stop_daemon
rm -rf "$STATE"; mkdir -p "$STATE"
FREESPACE=1000 start_daemon "$WORK/daemon3.log"
[ -z "$(api GET /api/state | field recordsWarning)" ] || fail "fixture: a wiped state was announced as damaged records"
scan '["duplicates","empty_folders"]'
api POST /api/results '{"tool":"duplicates","offset":0,"limit":100}' | python3 -c '
import json,sys
share=sys.argv[1]
out={f["path"]+"/"+f["name"]: f.get("out",False) for g in json.load(sys.stdin)["groups"] for f in g["files"]}
want={share+"/w1.txt":False, share+"/flatdest/w1-copy.txt":True, share+"/w2.txt":False, share+"/flatdest/w2.txt":False,
      share+"/flatdest/wj/Thumbs.db":True, share+"/wtwin/Thumbs.db":False}
bad={p:(out.get(p),w) for p,w in want.items() if out.get(p)!=w}
if bad: sys.exit("flags wrong after the records were lost (got, want): %r" % bad)' "$SHARE" || fail "the folder record did not identify the moved copy"
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/w1.txt\"],\"dest\":\"$SHARE/dest\",\"verify\":false,\"preserve\":true}")
echo "$r" | grep -q 'already moved' || fail "with every record lost, the original was allowed to follow its moved copy: $r"
[ -f "$SHARE/w1.txt" ] || fail "the original moved despite the refusal"
r=$(api POST /api/move "{\"tool\":\"empty_folders\",\"files\":[\"$SHARE/wtwin\"],\"dest\":\"$SHARE/dest\",\"verify\":false,\"preserve\":true}")
echo "$r" | grep -q 'keeping' || fail "with every record lost, the folder holding the last copy followed its moved twin: $r"
[ -f "$SHARE/wtwin/Thumbs.db" ] || fail "the last copy left its folder"
stop_daemon
FREESPACE=1000 start_daemon "$WORK/daemon3.log"
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/w1.txt\"],\"dest\":\"$SHARE/dest\",\"verify\":false,\"preserve\":true}")
echo "$r" | grep -q 'already moved' || fail "what the folder record restored was not kept across a restart: $r"
pass "with every record of the app's lost, the record a flat move left in its folder keeps the original in place"

echo "== a folder record copied beside the original"
# A record speaks for the folder it was written in. Copied anywhere else —
# beside the original, say, by copying a cleanup folder's contents back — it
# must not mark the original as moved: with every copy marked, any copy may
# stay, and the last original could follow the others.
mkdir -p "$SHARE/lib" "$SHARE/other" "$SHARE/clean"
printf 'the library photo' > "$SHARE/lib/p.jpg"; cp -p "$SHARE/lib/p.jpg" "$SHARE/other/p.jpg"
scan '["duplicates"]'
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/other/p.jpg\"],\"dest\":\"$SHARE/clean\",\"verify\":false,\"preserve\":false}")
echo "$r" | grep -q '"moved":\["' || fail "fixture: the flat move failed: $r"
cp -Rp "$SHARE/clean/.duplicate-finder-moved" "$SHARE/lib/"
scan '["duplicates"]'
api POST /api/results '{"tool":"duplicates","offset":0,"limit":100}' | python3 -c '
import json,sys
share=sys.argv[1]
out={f["path"]+"/"+f["name"]: f.get("out",False) for g in json.load(sys.stdin)["groups"] for f in g["files"]}
want={share+"/lib/p.jpg":False, share+"/clean/p.jpg":True}
bad={p:(out.get(p),w) for p,w in want.items() if out.get(p)!=w}
if bad: sys.exit("flags wrong (got, want): %r" % bad)' "$SHARE" || fail "a record copied beside the original marked the original as moved"
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/lib/p.jpg\"],\"dest\":\"$SHARE/dest\",\"verify\":false,\"preserve\":true}")
echo "$r" | grep -q 'already moved' || fail "the original moved past its moved copy: $r"
[ -f "$SHARE/lib/p.jpg" ] || fail "the original left despite the refusal"
rm -rf "$SHARE/lib" "$SHARE/other" "$SHARE/clean"
pass "a folder record copied into another folder does not speak for that folder"

echo "== protection recovered by a scan that is then stopped"
# A scan that recovers a moved copy from its folder record, and is then
# stopped before any result is published, must still keep what it
# recovered: after a restart, the groups stored earlier must not treat the
# moved copy as an original. The 2 GiB sparse images make the hashing slow
# enough for the Stop to land after the walk.
mkdir -p "$SHARE/sv/clean"
printf 'recovered, then the scan was stopped' > "$SHARE/sv/o.dat"; cp -p "$SHARE/sv/o.dat" "$SHARE/sv/o-copy.dat"
scan '["duplicates"]'
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/sv/o-copy.dat\"],\"dest\":\"$SHARE/sv/clean\",\"verify\":false,\"preserve\":false}")
echo "$r" | grep -q '"moved":\["' || fail "fixture: the flat move failed: $r"
stop_daemon
rm -rf "$STATE"; mkdir -p "$STATE"
mv "$SHARE/sv/clean/.duplicate-finder-moved" "$SHARE/sv/clean/.held-aside"
FREESPACE=1000 start_daemon "$WORK/daemon3.log"
scan '["duplicates"]'
mv "$SHARE/sv/clean/.held-aside" "$SHARE/sv/clean/.duplicate-finder-moved"
r=$(api POST /api/scan "{\"tool\":\"duplicates\",\"tools\":[\"duplicates\"],\"dirs\":[\"$SHARE\"],\"recurse\":true,\"match\":{}}")
echo "$r" | grep -q '"started":true' || fail "scan refused: $r"
for _ in $(seq 1 400); do api GET /api/state | grep -qi 'hash' && break; sleep 0.025; done
api POST /api/cancel '{}' >/dev/null
wait_idle
[ "$(api GET /api/state | field lastEnd.completed)" = "False" ] || fail "fixture: the scan finished before the Stop landed"
stop_daemon
FREESPACE=1000 start_daemon "$WORK/daemon3.log"
r=$(api POST /api/move "{\"tool\":\"duplicates\",\"files\":[\"$SHARE/sv/o.dat\"],\"dest\":\"$SHARE/dest\",\"verify\":false,\"preserve\":true}")
echo "$r" | grep -q 'already moved' || fail "what a stopped scan recovered was forgotten, and the original may follow its moved copy: $r"
[ -f "$SHARE/sv/o.dat" ] || fail "the original moved despite the refusal"
rm -rf "$SHARE/sv"
pass "protection a scan recovers from a folder record survives the scan being stopped and a restart"

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
