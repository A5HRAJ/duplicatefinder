# Architecture

Duplicate Finder is a Go daemon, a CGI gateway and an ExtJS 3.4 desktop app,
packaged for DSM 7. This document describes how the pieces fit together and
the rules that keep the move flow safe. The [README](../README.md) covers
what the app does from a user's point of view; the
[development guide](development.md) covers building and testing it.

## Components

```
DSM desktop app   spk/ui/DuplicateFinder.js  (shipped under a content-stamped name)
        │  fetch api.cgi/…                   same origin; DSM login required
        ▼
spk/ui/api.cgi    verifies the DSM session through authenticate.cgi,
                  requires an administrator, then execs
        ▼
bin/dupfinder -mode cgi     reverse-proxies the request, adding the daemon token, to
        ▼
bin/dupfinder -mode daemon  127.0.0.1:9807, started by start-stop-status as the package user
```

- **The app** (`spk/ui/DuplicateFinder.js`) is a `SYNO.SDS.AppWindow` built
  from DSM's own widget classes (`SYNO.ux.*`, `SYNO.SDS.ModalWindow`, the
  paging toolbar, the file chooser), so it inherits DSM's theme and behaves
  like File Station. It derives its API base from the URL of its own script
  tag, so the same file serves both package ids (`DuplicateFinder` for a
  hand-built package, `duplicatefinder` for the SynoCommunity build).
- **The gateway** (`spk/ui/api.cgi`) runs under DSM's web server for every
  request. It resolves the package id from its own path, validates the
  caller's DSM session with `authenticate.cgi`, requires membership of the
  administrators group, and execs the Go binary in CGI mode. A request that
  carries a SynoToken must validate it; an empty or invalid token never falls
  back to cookie-only authentication.
- **The CGI proxy** (`server/main.go`, `runCGI`) forwards the request to the
  daemon over loopback, attaching the shared daemon token from
  `var/.authtoken` and the local DSM base URL the session is valid against.
- **The daemon** (`server/`, one Go module, no cgo) serves `/api/*` on
  loopback only. Every request must carry the daemon token, so other local
  processes cannot drive it. Scans, moves, exports and cancels additionally
  require the caller's DSM session, forwarded through the proxy; a raw call
  with the token alone can read state and results, nothing more.

The daemon binds its port before it touches the token file, so a second
instance fails immediately and cannot rotate the token out from under the one
that is serving. Once bound and holding its token it writes
`var/dupfinder.ready`, and the start script waits for that file rather than
sleeping, so Package Center never reports a daemon that then died. The log
(`var/dupfinder.log`) rotates at 2 MB with one generation kept.

## Scanning

A scan runs on one goroutine (`runScan` in `server/scan.go`) with a
per-tool accumulator; the tree is never materialized in memory.

### The walk

Every scan root is opened as a pinned directory handle
(`server/dirhandle_linux.go`) and re-checked for containment from the handle's
canonical path, so a root swapped for a symlink after validation is refused
rather than followed. Enumeration goes through the handle; symlinks below a
root are skipped; Synology system directories (`@eaDir`, `#recycle`,
`#snapshot`) and hidden names are skipped. Overlapping or aliased roots are
de-overlapped by canonical path first, so no file is visited twice under two
names. Content reads (hashing, EXIF) open files through the pinned root with
`O_NOFOLLOW` on every path component (`server/openrel.go`), which works on
DSM's 3.10 kernels without `openat2`.

Unreadable locations are collected into the scan's error list, capped at
fifty entries plus a line saying how many more there were.

### Duplicates

1. **Walk.** Every file's record (root index, size, mtime, root-relative
   path) is appended to an unlinked spill file, and its candidate key (size
   plus any requested match criteria) is counted. The counter is exact up to
   about two million keys, then degrades to a fixed 64 MB table of saturating
   two-bit counters. The degraded form can only over-report a collision,
   which costs a needless hash; it can never hide one.
2. **Distil.** The spill is streamed again and the records whose key collided
   are written to a second, smaller spill. Nothing but one record is in memory
   at a time.
3. **Windows.** Candidates are hashed one partition of the key space at a time
   (about 100,000 candidates per window). Every file sharing a key shares a
   partition, so a group is never split across windows, and peak memory
   follows the window rather than the volume. Within a window, files are
   bucketed by a 64 KiB prefix hash and then by the full BLAKE3 hash.
4. **Skewed keys.** A key with more than 25,000 members (every macOS
   `.DS_Store` is 6148 bytes) cannot be partitioned. It is split by content
   instead, first by prefix hash and then by full hash, so every member is
   examined and nothing is truncated silently. Only when a bucket means
   "identical content" does a cap apply, and that cap is the per-group cap.
5. **Groups** accumulate in a bounded heap ordered by reclaimable space, so
   the per-row work a result needs (IDs, capture dates) is spent only on rows
   that survive the stored-results cap of 100,000 files. No single group may
   exceed that budget either. Anything dropped is counted into a truncation
   report the UI shows.

Created dates, when matching on them, are fetched from File Station per
window; a file File Station cannot answer for gets a per-path sentinel key and
never groups, and the scan says how many were excluded.

### The hash store, and detection over speed

`server/hashcache.go` keeps every scan's full-content hashes on disk
(`var/hashcache.bin`), but it never lets a scan skip a read: `lookup` answers
only for entries recorded under the current scan's generation, so a hash
served to the grouping was computed from bytes read during that scan. Earlier
generations are history. `record` compares each fresh hash against the stored
entry for the same path, size and mtime, and a difference is the bit-rot
evidence the conflicting-files pass uses. Rot anywhere in a file is therefore
caught by the next scan, at the price of re-reading every candidate every
time. The store is capped at 500,000 entries, trimmed oldest generation first
and, mid-scan, current-generation entries first so the history the rest of
the scan still needs survives.

The one sanctioned exception is an explicit **resume** of an interrupted
scan: the interruption marker records the generation the dead run was
recording under, and adopting it makes that run's own reads servable again.
A resume is honoured only while the marker stands and only for a request
identical to the interrupted one (tool, scope, reference folders, recursion,
match criteria). Everything else is a new scan and re-reads everything.

### Conflicting files

The duplicates scan also distils a second candidate set keyed on size and
modified time alone, independent of the match criteria, and runs
`scanCorrupted` (`server/corrupt.go`) over it after the duplicates result is
complete. Members that disagree form a set. Each member is then judged by a
ladder that stops at the first verdict:

1. the read failed with an I/O error, which on a checksummed volume means the
   stored bytes are damaged;
2. the content changed since an earlier scan while its size and modified time
   did not, confirmed by re-checking that both are still current on disk;
3. the file's own container is broken: PNG chunk CRCs, gzip's CRC and every
   ZIP member's CRC convict a single copy; JPEG, PDF and ISO base-media files
   are checked structurally, which catches truncation;
4. the shape of the difference between two copies: a run of NULs on one side
   is an interrupted copy and that side is the damaged one; a single flipped
   bit is reported but convicts neither side.

A copy that verifies its own structure, in a set where another copy is
positively damaged, is marked intact. Everything else stays undetermined.
Sets whose members share a filename are listed first. The category is
read-only: the daemon refuses any move that names it.

### Empty folders, empty files, temporary files

The empty-folder scan keeps one frame per open directory, so its memory
follows tree depth, not tree size. A folder is a candidate when it is the
topmost directory whose subtree holds no real file; junk names (the Temporary
Files list) do not count as content, a zero-byte file does, and a directory
the walk could not read makes it and every ancestor non-empty. Each
candidate is then confirmed through File Station's own listing, which sees
hidden entries the walk skipped: `@eaDir` is accepted as junk, any other
directory rejects the candidate. A confirmation error is reported, never read
as "not empty".

The two flat tools keep bounded top-K heaps ordered by path, capped at 20,000
rows.

## Results

Results live in daemon memory and are served in pages
(`server/results.go`): 100 whole groups per page for the grouped tools, 1000
rows for the flat ones, matching File Station's paging. Search, the
advanced-search options, match refinement and column sorting are all applied
by the daemon over the whole result set, so totals and order are correct even
though the client holds one page. A page also carries a row budget, so one
enormous group arrives trimmed and labelled with its true size.

Reference-folder protection is decided by the daemon per page, on canonical
paths, from the folders the stored results were scanned with, which is the
same comparison the move refuses with. The padlocks, the reclaimable totals
and the refusals can therefore never describe different sets; changing the
reference folders takes effect at the next scan.

## Persistence

Finished scans are saved to `var/results.json.gz` together with the
reference folders, the keep-one survivors and the ID counter, so every
move-safety invariant survives a restart. Writes are atomic: a temporary
file, `fsync`, rename, then a directory sync. Results reach the disk before
the interruption marker is cleared, so a crash during the final save is still
reported as an interruption. A state file from a build that had tools this
one lacks has those results dropped, because the move allowlist is built
from every stored result.

`var/scan.interrupted` records a scan in flight: its tool, generation and
normalized request. Found at daemon start it means the scan died, and the app
offers **Resume** or **Start Over** at the next Scan click (for tools other
than duplicates, re-running simply is the resume). Any admitted scan clears
the marker, and a marker that cannot be removed is neutralized in place.

## Moves and exports

`handleMove` (`server/main.go`) is one long request under a move lock. Its
checks run in this order, and every one of them compares symlink-resolved
paths so an alias cannot slip past a prefix test:

1. The tool must not be read-only, and in preserve mode must have a folder
   name in the daemon's own table; the client's string is only ever a key.
2. No scan may be running (checked cheaply before the destination round trip
   and again, authoritatively, after the lock is taken).
3. The destination is pinned, resolved and checked for containment in the
   volumes, then mapped to File Station's share space through the session's
   share table, which is fetched from File Station and never derived by
   string surgery (external shares mount under a directory that differs from
   their name).
4. Every requested path must be in the allowlist built from every stored
   result, and the identity File Station reports for it now (type, size,
   modified time as an epoch value) must match what some scan recorded. For
   duplicates the 64 KiB prefix is re-read and compared too, and with
   verification on the full hash must equal the scan's.
5. Keep-one: for every duplicate group the request touches, File Station is
   asked which members still exist as recorded; if the request would take the
   last existing copy, one is held back. A directory move counts the group
   members beneath it as requested. Survivors of dissolved groups stay
   protected until the next duplicates scan. Reference-folder files are
   refused.
6. Names File Station cannot address (`.DS_Store` exactly, and any `._`
   AppleDouble name) are refused with a plain explanation rather than handed
   to File Station, which would report "no such file".

Execution is File Station's (`server/fsapi.go`): `CopyMove` with
`remove_src`, polled to completion for up to twelve hours, always without the
`overwrite` parameter. A name collision fails into a staging flow: the file is
moved into a fresh `.dupfinder-tmp-*` folder inside the destination, renamed
to the first free ` (n)` name per File Station's own view, and moved into
place. A lost reply is not a failure: the daemon asks where the file actually
is before reporting. A move that ends with the file parked in the staging
folder reports the full path, and with verification on the parked copy is
verified there. Failures that happen after the source is gone still prune the
row, so it can never linger as a phantom survivor.

In preserve mode the batch's tool folder is allocated lazily, once per
request, after the first file has cleared every check, so an all-refused
request creates nothing. Two cases can leave an empty tool folder behind,
both visible and harmless: a folder-creation reply lost in transit, and a
batch whose every file File Station then fails to move.

Progress is published under the daemon's ordinary state lock, never the move
lock, so `/api/state` stays answerable during a long move; the app polls it
every 700 ms. A second overlapping move is refused with 409 rather than
queued, and scans are refused while a move runs.

Exports upload the CSV through File Station's `Upload` without `overwrite`, so
an existing report gets a numbered sibling. Cells whose first meaningful
character is a formula marker are prefixed with an apostrophe.

## What is asked of File Station, and what stays native

Everything File Station can answer is asked of File Station: collision
probing and ` (n)` naming, creation of the tool folder and the mirrored
origin chain, the move precheck's file info, keep-one survivor existence,
scan-root and destination validation, the empty-folder confirmation, creation
times and the volume overview. Kept native deliberately: content hashing (no
API exists), scan enumeration (`Search` reports no per-path errors and no
symlink indicator, so it would break the empty-folder gate and fabricate
duplicate pairs), EXIF capture dates, the canonical-path containment checks
(a security boundary must describe the filesystem the daemon actually walks)
and volume-root discovery (the `/volume*`, `/volumeUSB*` and `/volumeSATA*`
globs that define those walls).

## Security model

- Administrators only, enforced in `api.cgi` by GID and declared in
  `ui/config` (`allUsers: false`), because the package user's permissions are
  shared by everyone who can open the app.
- The daemon listens on loopback and requires the shared token on every
  request; the token is created atomically with no-replace semantics and is
  readable only by the package user.
- Every mutation requires the caller's DSM session and is executed by File
  Station with that session's permissions.
- Paths are vetted on pinned, symlink-resolved objects; content is read
  through pinned roots with `O_NOFOLLOW`.
- Untrusted text is escaped everywhere it reaches HTML, including tooltip
  attributes, which Ext decodes and renders as markup (hence the double
  encoding in `qtipText`); CSV cells are neutralized against formula
  injection.
- Parsers of untrusted file content (HEIF, QuickTime, EXIF, ZIP, PNG, gzip,
  JPEG) bound every allocation and loop, and express bounds as subtractions
  so they cannot overflow on the 32-bit ARM build.

## Packaging

`build.sh` cross-compiles the daemon for linux/amd64, arm64 and arm (GOARM=7),
generates the icons with `tools/make_icons.py`, and assembles one `.spk` per
architecture family with a single `bin/dupfinder` each.

The UI script ships under a **content-stamped filename**,
`DuplicateFinder-<version with dashes>-<first 8 hex of md5>.js`, with the
single top-level key of `ui/config` regenerated to match. DSM serves package
UI files with no `Cache-Control` under a URL that does not change on upgrade,
so browsers keep running a previous build's script otherwise. The stamp is the
content hash rather than the version, so the URL changes if and only if the
script changes. No unversioned copy is shipped as a fallback, because it would
anchor a URL that never changes again, and no `postupgrade` sweep is needed
because DSM replaces `target/ui` wholesale on upgrade.

No `style.css` is shipped: DSM would inject it into the whole desktop page.
The app injects its own window-scoped stylesheet at runtime instead, and
deliberately restyles nothing DSM's own theme already paints.

`spksrc/` holds the SynoCommunity recipe, which reproduces the stamp with
`md5sum` and `jq` and ships the same start script; see
[spksrc/README.md](../spksrc/README.md).
