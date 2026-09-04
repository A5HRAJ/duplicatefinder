# Shared pieces of the smoke-test runners (run.sh, run-arm.sh).
# Sourced, not executed; callers set their own shell options.

# fetch_deps: ExtJS 3.4 for the harness plus jsdom. Cached after first run.
# Must be called from the test/ directory.
fetch_deps() {
	mkdir -p ext
	[ -f ext/ext-base.js ] || curl -sL -o ext/ext-base.js \
		"https://cdnjs.cloudflare.com/ajax/libs/extjs/3.4.1-1/adapter/ext/ext-base.js"
	[ -f ext/ext-all.js ] || curl -sL -o ext/ext-all.js \
		"https://cdnjs.cloudflare.com/ajax/libs/extjs/3.4.1-1/ext-all.js"
	[ -d node_modules/jsdom ] || npm install --no-audit --no-fund >/dev/null
}

# wait_daemon <port> <logfile> [seconds]: block until the daemon answers
# /api/info, or fail loudly with its log. Both runners used a bare `sleep 1`
# after every launch; on a slow host the first request then raced the
# startup and a persistence test failed with a diagnosis about markers.
wait_daemon() {
	local PORT="$1" LOG="$2" SECS="${3:-30}" i
	for i in $(seq 1 $((SECS * 4))); do
		curl -sf "http://localhost:$PORT/api/info" >/dev/null 2>&1 && return 0
		sleep 0.25
	done
	echo "daemon failed to start within ${SECS}s:" >&2
	cat "$LOG" >&2
	return 1
}

# make_fixture <volume-dir> <outside-dir> [usb-volume-dir]: the disposable
# test volume. The outside dir holds the symlink-escape target and must live
# outside the volume. The optional third dir becomes a second "volume" whose
# single share directory is "usbshare" — the dev mock advertises shares on
# secondary roots with a "1" suffix ("usbshare1"), reproducing the
# name-differs-from-directory shape of a real USB share. Keep in sync with
# the assertions in smoke.js.
make_fixture() {
	local V="$1" OUTSIDE="$2" USBV="${3:-}"
	if [ -n "$USBV" ]; then
		mkdir -p "$USBV/usbshare"
	fi
	mkdir -p "$V/Backups/A" "$V/Backups/B" "$V/photo"
	head -c 400000 /dev/urandom > "$V/Backups/A/IMG_0001.JPG"
	cp "$V/Backups/A/IMG_0001.JPG" "$V/Backups/B/IMG_0001.JPG"
	head -c 250000 /dev/urandom > "$V/Backups/A/clip.mov"
	cp "$V/Backups/A/clip.mov" "$V/Backups/B/clip.mov"
	# same size, different content — must NOT group.
	# Their modified times are pinned APART on purpose: same size + same mtime
	# + different content is exactly what the corrupted-files scan reports, and
	# these two are written back to back, so on a fast machine they would land
	# in the same second and appear as a corrupted set at random.
	head -c 300000 /dev/zero > "$V/photo/zeros.bin"
	head -c 300000 /dev/urandom > "$V/photo/random.bin"
	touch -t 202401020900.00 "$V/photo/zeros.bin"
	touch -t 202401020901.00 "$V/photo/random.bin"
	# empty-folder fixtures: one truly empty; one unreadable with hidden
	# content (a Hyper Backup vault the package user can't open must NOT
	# count as empty)
	mkdir -p "$V/photo/emptydir" "$V/photo/locked"
	head -c 1000 /dev/urandom > "$V/photo/locked/secret.bin"
	chmod 000 "$V/photo/locked"
	# hidden-DIRECTORY cases, which the walker skips and File Station's
	# listing must therefore adjudicate: a dot-directory holding real content
	# (think .git) must NOT be reported empty; @eaDir is Synology's thumbnail
	# cache, junk by definition (2026-08-10 directive), so a folder holding
	# only that IS empty and the cache rides along when it moves
	mkdir -p "$V/photo/hiddenonly/.hidden" "$V/photo/eadironly/@eaDir"
	head -c 100 /dev/urandom > "$V/photo/hiddenonly/.hidden/thumb.bin"
	# junk-only folders count as empty (same directive): the junk-file names
	# are the Temporary Files tool's own list. One visible to File Station
	# (Thumbs.db), one invisible to it (.DS_Store — DSM filters exactly
	# .DS_Store and ._* from list/getinfo, so on the device the confirmation
	# sees an empty listing; the dev mock lists it and isTempName accepts it —
	# both layers agree either way). junkmixed pins that junk BESIDE a real
	# file protects the folder. Sizes unique in the fixture and mtimes pinned
	# so the same-second/same-size corrupted-set trap above cannot bite.
	mkdir -p "$V/photo/junkonly" "$V/photo/dsonly" "$V/photo/junkmixed"
	head -c 37 /dev/urandom > "$V/photo/junkonly/Thumbs.db"
	head -c 41 /dev/urandom > "$V/photo/dsonly/.DS_Store"
	# AppleDouble sidecar: the OTHER name File Station cannot address. It has
	# to be created here, natively — File Station itself answers 418 when asked
	# to upload or rename to a "._" name, so the API cannot produce one, and
	# this is the only place the "._" half of fsCannotAddress gets exercised
	# against a file that really exists. macOS writes these over SMB/AFP, so a
	# real NAS has them.
	head -c 39 /dev/urandom > "$V/photo/dsonly/._sidecar"
	head -c 43 /dev/urandom > "$V/photo/junkmixed/Thumbs.db"
	head -c 47 /dev/urandom > "$V/photo/junkmixed/keep.txt"
	touch -t 202401021000.00 "$V/photo/junkonly/Thumbs.db"
	touch -t 202401021001.00 "$V/photo/dsonly/.DS_Store"
	touch -t 202401021004.00 "$V/photo/dsonly/._sidecar"
	touch -t 202401021002.00 "$V/photo/junkmixed/Thumbs.db"
	touch -t 202401021003.00 "$V/photo/junkmixed/keep.txt"
	# symlink escape fixture: a link under the volume pointing outside it —
	# moves/exports through it must be rejected
	mkdir -p "$OUTSIDE"
	head -c 100 /dev/urandom > "$OUTSIDE/escapee.bin"
	ln -s "$OUTSIDE" "$V/photo/sneaky"
	# round-2 fixtures: directory aliases inside the volume (a move request
	# may name a protected/duplicate file through them) and probe files
	ln -s "$V/photo" "$V/photoalias"
	ln -s "$V/Backups/B" "$V/Balias"
	mkdir -p "$V/spare"   # not a reference dir, so its files may move
	head -c 100 /dev/urandom > "$V/spare/pres.bin"   # never surfaced by any scan
	: > "$V/spare/pres2.bin"   # zero-byte: an Empty Files scan surfaces it
	: > "$V/spare/ident.bin"   # zero-byte, modified after its scan in smoke.js
	# preserve-mode batch-folder fixtures, all zero-byte so the duplicates
	# scanner skips them (size 0) and the headline group counts are unmoved.
	# spare/sub holds a file, so the empty-folder scan does not report it.
	mkdir -p "$V/spare/sub"
	: > "$V/spare/sub/pres2.bin"  # same basename as spare/pres2.bin: one batch
	                              # must land both, neither renamed
	: > "$V/spare/pres3.bin"      # payload for a SECOND batch, which must get
	                              # its own numbered folder rather than merge
	: > "$V/spare/inplace.bin"    # moved with spare/ itself as the destination:
	                              # refused outright without preserve, mirrored
	                              # into a new tool folder with it
	# paging fixtures: 105 more duplicate pairs (same basename in A/ and B/,
	# so they survive match-by-name refinement) push the group count past the
	# UI's 100-groups-per-page window — the paging toolbar must page
	mkdir -p "$V/Backups/many/A" "$V/Backups/many/B"
	local i n=0
	for i in $(seq -w 1 105); do
		head -c 1024 /dev/urandom > "$V/Backups/many/A/pg_$i.bin"
		cp "$V/Backups/many/A/pg_$i.bin" "$V/Backups/many/B/pg_$i.bin"
		# One minute per PAIR, and both copies of a pair share it. Distinct
		# per pair because all 105 pairs are 1024 bytes with different
		# content: written in one burst they would share a second, and the
		# corrupted-files scan would report all 210 as one enormous set —
		# a real finding under its rule, but a fixture that changes shape
		# with the speed of the machine. Shared WITHIN a pair because
		# match-by-modified must still keep the duplicate pairs together.
		touch -t "$(printf '20240101%02d%02d.00' $((n / 60)) $((n % 60)))" \
			"$V/Backups/many/A/pg_$i.bin" "$V/Backups/many/B/pg_$i.bin"
		n=$((n + 1))
	done
	# corrupted-files fixtures. Nested INSIDE an existing share on purpose:
	# every top-level directory under $V becomes a share in the dev mock, and
	# the default scan scope takes only the first six.
	#
	# Neither pair is byte-identical, so neither adds a duplicate group and the
	# 107/214 headline counts are unmoved. Sizes (4096, 777) are unique in the
	# fixture, so nothing else can join these sets. mtimes are set LAST — cp
	# does not preserve them, and the whole detection keys on them.
	mkdir -p "$V/Backups/damaged/copy"
	# Decidable: the second copy holds zeros where the first holds data, which
	# is the signature of an interrupted transfer — corrupt vs intact.
	head -c 4096 /dev/urandom > "$V/Backups/damaged/archive.dat"
	head -c 2048 "$V/Backups/damaged/archive.dat" > "$V/Backups/damaged/copy/archive.dat"
	head -c 2048 /dev/zero >> "$V/Backups/damaged/copy/archive.dat"
	touch -t 202402100800.00 "$V/Backups/damaged/archive.dat" "$V/Backups/damaged/copy/archive.dat"
	# Undecidable, and under different names: the set is still reported (the
	# rule is size + modified time) but nothing identifies a side, so both
	# rows must come back Undetermined.
	head -c 777 /dev/urandom > "$V/Backups/damaged/notes-a.txt"
	head -c 777 /dev/urandom > "$V/Backups/damaged/notes-b.txt"
	touch -t 202402100900.00 "$V/Backups/damaged/notes-a.txt" "$V/Backups/damaged/notes-b.txt"
}
