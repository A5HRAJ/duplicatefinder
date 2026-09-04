#!/bin/bash
# Build the Duplicate Finder .spk set for Synology DSM 7.x.
# Requires: Go 1.20+, Python 3. Produces one spk per architecture family
# (dist/DuplicateFinder-<version>-<arch>.spk), each carrying a single
# bin/dupfinder — the packaging model the DSM developer guide documents
# (one spk per arch; no multi-platform binary bundles).
set -euo pipefail
cd "$(dirname "$0")"

PKG="DuplicateFinder"
VERSION="${DUPFINDER_VERSION:-1.0.0-0136}"   # env override exists for test/stamp.sh only
BUILD="build"
export COPYFILE_DISABLE=1   # keep macOS ._* metadata out of the tars

# ---------------------------------------------------------------- cache stamp
# DSM serves ui/<script> with ETag and Last-Modified but NO Cache-Control, under
# a URL stamped only with DSM's own global `v=` value — which does NOT change
# when a package is upgraded. Browsers therefore cache the app's JS
# heuristically (roughly 10% of the file's age when fetched) and can keep
# running a previous build's code without even revalidating: measured on the
# DS916+, a normal reload after upgrading served the old script with
# transferSize 0. The FILENAME is the only part of that URL this package
# controls, so the shipped script carries a stamp.
#
# The stamp is the content hash, not the version. Deriving it from VERSION
# alone would mean a rebuild that forgot to bump build.sh's literal silently
# reproduces the very bug this exists to fix — and identical rebuilds would
# churn the cache for no reason. Hashing the bytes makes the URL change if and
# only if the script changes. The version rides along purely so the filename is
# readable in a network log.
#
# The stem stays dot-free (1.0.0-0104 -> 1-0-0-0104): DSM does token
# substitution inside config values and its loader's filename handling is not
# documented, so extra dots in a basename are a gratuitous unknown.
JS_HASH=$( (md5 -q spk/ui/$PKG.js 2>/dev/null || md5sum spk/ui/$PKG.js | cut -d' ' -f1) | cut -c1-8 )
JS_NAME="$PKG-${VERSION//./-}-$JS_HASH.js"

# stage_ui <dest> — copy spk/ui into a package payload with the UI script
# renamed to its stamped name and ui/config keyed to match.
#
# The rename happens HERE, in the payload, and never in spk/. The source tree
# keeps the canonical DuplicateFinder.js, so the dev daemon (DUPFINDER_UI),
# test/run.sh and test/harness.html stay version-unaware and need no edits.
#
# ui/config's single top-level key is the script's path under the UI dir, and
# it is the ONLY thing in the whole package that names the file. If the key and
# the filename ever diverge, the app still registers on the DSM desktop and
# then 404s when launched — so the config is REGENERATED with a JSON parser
# rather than patched with sed. sed exits 0 when its pattern misses, which
# would ship exactly that divergence silently; json.load fails loudly instead.
#
# Deliberately NOT shipped: an unversioned copy of the script at the old path.
# It would look like a safe fallback for a browser holding a stale app registry,
# but it anchors a URL that never changes again, and any browser that ever
# loaded through it would reacquire the heuristic cache entry this whole
# mechanism exists to defeat.
stage_ui() {
  local DEST="$1"
  rm -rf "$DEST"
  cp -R spk/ui "$DEST"
  mv "$DEST/$PKG.js" "$DEST/$JS_NAME"
  JS_NAME="$JS_NAME" PKG="$PKG" python3 - "$DEST/config" <<'PY'
import json, os, sys
path = sys.argv[1]
want, pkg = os.environ["JS_NAME"], os.environ["PKG"]
with open(path) as fh:
    cfg = json.load(fh)          # malformed config fails the build here
key = pkg + ".js"
if key not in cfg:
    sys.exit("ui/config: expected top-level key %r, got %r" % (key, list(cfg)))
cfg[want] = cfg.pop(key)
with open(path, "w") as fh:
    json.dump(cfg, fh, indent=1)
    fh.write("\n")
PY
}

# assert_ui_stamped <dest> — the postcondition that keeps a broken package from
# shipping green. Nothing in test/run.sh ever sees the assembled payload (it
# copies spk/ui verbatim), so this is the only check that the thing actually
# shipped is coherent: the config's own key must name a file that exists.
assert_ui_stamped() {
  local DEST="$1" KEY
  KEY=$(python3 -c 'import json,sys; c=json.load(open(sys.argv[1])); k=list(c); sys.exit("config must hold exactly one key, got %r" % (k,)) if len(k)!=1 else print(k[0])' "$DEST/config")
  [ -f "$DEST/$KEY" ] || { echo "payload: ui/config names '$KEY' but no such file in $DEST" >&2; exit 1; }
  [ ! -f "$DEST/$PKG.js" ] || { echo "payload: unstamped $PKG.js must not ship (it re-anchors the cached URL)" >&2; exit 1; }
}

# Test hook: stage only the UI payload and print the stamped filename, so
# test/stamp.sh exercises the real staging code instead of a copy of it.
# Sits above `rm -rf "$BUILD"` so a test run can never wipe a real build.
if [ "${1:-}" = "--stage-ui" ]; then
  stage_ui "$2"
  assert_ui_stamped "$2"
  echo "$JS_NAME"
  exit 0
fi

rm -rf "$BUILD"
mkdir -p "$BUILD/bin" dist

echo "== Compiling backend (linux amd64 / arm64 / armv7)…"
(
  cd server
  # -X stamps the real package version into the daemon, so /api/info can answer
  # "which build is live?" without going through Package Center.
  LD="-s -w -X main.appVersion=$VERSION"
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="$LD" -o "../$BUILD/bin/dupfinder.amd64" .
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
    go build -trimpath -ldflags="$LD" -o "../$BUILD/bin/dupfinder.arm64" .
  CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 \
    go build -trimpath -ldflags="$LD" -o "../$BUILD/bin/dupfinder.arm" .
)

echo "== Generating icons…"
mkdir -p "$BUILD/icons"
python3 tools/make_icons.py "$BUILD/icons" >/dev/null

# build_spk <label> <goarch-binary-suffix> <INFO arch value>
build_spk() {
  local LABEL="$1" BINSUF="$2" ARCHVAL="$3"
  local ROOT="$BUILD/pkgroot-$LABEL" SPKROOT="$BUILD/spkroot-$LABEL"
  echo "== [$LABEL] Assembling package payload…"
  mkdir -p "$ROOT/bin" "$SPKROOT"
  cp "$BUILD/bin/dupfinder.$BINSUF" "$ROOT/bin/dupfinder"
  stage_ui "$ROOT/ui"
  mkdir -p "$ROOT/ui/images"
  cp "$BUILD"/icons/icon_*.png "$ROOT/ui/images/"
  chmod 755 "$ROOT/ui/api.cgi" "$ROOT/bin/dupfinder"
  find "$ROOT" -name '.DS_Store' -delete
  # before the tar, so checksum and extractsize below still describe what ships
  assert_ui_stamped "$ROOT/ui"

  tar -czf "$SPKROOT/package.tgz" -C "$ROOT" .

  local CHECKSUM EXTRACTSIZE
  CHECKSUM=$(md5 -q "$SPKROOT/package.tgz" 2>/dev/null || md5sum "$SPKROOT/package.tgz" | cut -d' ' -f1)
  EXTRACTSIZE=$(du -sk "$ROOT" | cut -f1)
  cat > "$SPKROOT/INFO" <<EOF
package="$PKG"
version="$VERSION"
arch="$ARCHVAL"
displayname="Duplicate Finder"
description="Find and clean up duplicate files, empty folders, empty files and temporary junk on your DiskStation, and spot conflicting files — copies that share a size and modified time but no longer hold the same content. Duplicates are matched by Blake3 content hash; selected files are moved (never deleted) to a folder you choose."
maintainer="Ashwin Rajani"
os_min_ver="7.0-41890"
dsmuidir="ui"
dsmappname="SYNO.SDS.DuplicateFinder.Application"
silent_upgrade="no"
checksum="$CHECKSUM"
extractsize="$EXTRACTSIZE"
EOF

  cp "$BUILD"/icons/PACKAGE_ICON.PNG "$BUILD"/icons/PACKAGE_ICON_256.PNG "$SPKROOT/"
  cp -R spk/scripts "$SPKROOT/scripts"
  cp -R spk/conf "$SPKROOT/conf"
  chmod 755 "$SPKROOT/scripts/"*
  find "$SPKROOT" -name '.DS_Store' -delete

  local SPK="dist/$PKG-$VERSION-$LABEL.spk"
  rm -f "$SPK"
  tar -cf "$SPK" -C "$SPKROOT" \
    INFO PACKAGE_ICON.PNG PACKAGE_ICON_256.PNG package.tgz scripts conf
  echo "== Done: $SPK"
}

build_spk x86_64 amd64 "x86_64"
build_spk armv8  arm64 "armv8"
build_spk armv7  arm   "armv7 armada38x"
