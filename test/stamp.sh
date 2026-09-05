#!/bin/bash
# Packaging test for the UI script's cache stamp.
#
# The rest of the suite never sees the assembled payload — test/run.sh copies
# spk/ui verbatim and the harness loads DuplicateFinder.js by its canonical
# name — so without this, a package whose ui/config names a file that does not
# exist would ship with every other test green. It drives the REAL build.sh
# staging code (via its --stage-ui hook), never a copy of the logic.
set -euo pipefail
cd "$(dirname "$0")/.."

PASS=0; FAIL=0
check() { if [ "$2" = 1 ]; then PASS=$((PASS+1)); echo "PASS  $1"; else FAIL=$((FAIL+1)); echo "FAIL  $1"; fi; }
eq() { [ "$1" = "$2" ] && echo 1 || { echo "        want: $2" >&2; echo "        got:  $1" >&2; echo 0; }; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# --- stage once at the declared version -------------------------------------
# The expected version is asked of build.sh itself: a literal here would go
# stale at the first release bump and fail every run after it.
VERSION=$(./build.sh --version)
check "build.sh reports its version" "$([ -n "$VERSION" ] && echo 1 || echo 0)"
NAME_A=$(./build.sh --stage-ui "$WORK/a")
check "staging prints the stamped filename" "$([ -n "$NAME_A" ] && echo 1 || echo 0)"
check "stamped file exists in the payload" "$([ -f "$WORK/a/$NAME_A" ] && echo 1 || echo 0)"
check "stamped name carries the package version" \
  "$(grep -qF -- "-${VERSION//./-}-" <<<"$NAME_A" && echo 1 || echo 0)"
check "stamped stem is dot-free apart from .js" \
  "$(eq "$(tr -cd '.' <<<"$NAME_A" | wc -c | tr -d ' ')" "1")"

# The unstamped name must NOT ship: it would anchor a URL that never changes
# again, reinstating the heuristic caching this whole mechanism defeats.
check "unstamped DuplicateFinder.js is absent from the payload" \
  "$([ ! -f "$WORK/a/DuplicateFinder.js" ] && echo 1 || echo 0)"

# --- the config must name exactly the file that shipped ---------------------
KEY_A=$(python3 -c 'import json,sys;c=json.load(open(sys.argv[1]));k=list(c);print(k[0] if len(k)==1 else "MULTI:%r"%(k,))' "$WORK/a/config")
check "ui/config holds exactly one top-level key naming the shipped file" "$(eq "$KEY_A" "$NAME_A")"
check "ui/config is valid JSON" \
  "$(python3 -c 'import json,sys;json.load(open(sys.argv[1]))' "$WORK/a/config" >/dev/null 2>&1 && echo 1 || echo 0)"
check "ui/config kept the app class entry" \
  "$(python3 -c 'import json,sys;c=json.load(open(sys.argv[1]));print(1 if "SYNO.SDS.DuplicateFinder.Application" in list(c.values())[0] else 0)' "$WORK/a/config")"
check "ui/config kept the MainWindow lib entry" \
  "$(python3 -c 'import json,sys;c=json.load(open(sys.argv[1]));print(1 if "SYNO.SDS.DuplicateFinder.MainWindow" in list(c.values())[0] else 0)' "$WORK/a/config")"

# --- the shipped script is the source script, byte for byte -----------------
check "staged script is byte-identical to spk/ui/DuplicateFinder.js" \
  "$(cmp -s spk/ui/DuplicateFinder.js "$WORK/a/$NAME_A" && echo 1 || echo 0)"
check "api.cgi still ships" "$([ -f "$WORK/a/api.cgi" ] && echo 1 || echo 0)"

# --- the source tree must stay canonical (this is what keeps dev/test alive) -
check "source tree keeps the canonical name" \
  "$([ -f spk/ui/DuplicateFinder.js ] && echo 1 || echo 0)"
check "no stamped copy leaked into the source tree" \
  "$([ -z "$(find spk/ui -name 'DuplicateFinder-*.js' -print -quit)" ] && echo 1 || echo 0)"
check "source config still keyed by the canonical name" \
  "$(grep -q '"DuplicateFinder.js"' spk/ui/config && echo 1 || echo 0)"

# --- the stamp must track the VERSION ---------------------------------------
NAME_B=$(DUPFINDER_VERSION=9.9.9-9999 ./build.sh --stage-ui "$WORK/b")
check "a different version yields a different filename" \
  "$([ "$NAME_A" != "$NAME_B" ] && echo 1 || echo 0)"
check "the new version appears in the new filename" \
  "$(grep -q -- '-9-9-9-9999-' <<<"$NAME_B" && echo 1 || echo 0)"
check "the config followed the rename" \
  "$(eq "$(python3 -c 'import json,sys;print(list(json.load(open(sys.argv[1])))[0])' "$WORK/b/config")" "$NAME_B")"

# --- the stamp must track CONTENT, so a forgotten version bump still busts ---
cp spk/ui/DuplicateFinder.js "$WORK/orig.js"
restore() { cp "$WORK/orig.js" spk/ui/DuplicateFinder.js; rm -rf "$WORK"; }
# INT/TERM too: the source file is mutated below, and an interrupted run
# must not leave the trailer in the shipped script (a SIGKILL still would —
# the next build then stamps a file that differs from the source by one
# comment; check `git status` after any hard kill).
trap restore EXIT INT TERM
printf '\n/* stamp test */\n' >> spk/ui/DuplicateFinder.js
NAME_C=$(./build.sh --stage-ui "$WORK/c")
check "same version but changed content yields a different filename" \
  "$([ "$NAME_A" != "$NAME_C" ] && echo 1 || echo 0)"
cp "$WORK/orig.js" spk/ui/DuplicateFinder.js
NAME_D=$(./build.sh --stage-ui "$WORK/d")
check "restoring the content restores the filename (identical rebuilds do not churn)" \
  "$(eq "$NAME_D" "$NAME_A")"

echo
if [ "$FAIL" -gt 0 ]; then echo "$FAIL FAILURES ($PASS passed)"; exit 1; fi
echo "ALL PASSED ($PASS assertions)"
