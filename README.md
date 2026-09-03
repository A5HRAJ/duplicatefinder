# Duplicate Finder — Synology DSM package

A DSM 7.x package that finds **duplicate files** (BLAKE3 content hash),
**conflicting files**, **empty folders**, **empty files** and **temporary junk**
on your DiskStation, from a native app inside DSM's web desktop. Selected files
are **moved, never deleted**, to a folder you choose — by default into a new
folder there named after the tool that found them, with each file's original
folder path mirrored inside it; results can be exported as CSV.
(Conflicting Files is the one exception: it is a report only — see below.)

## Install

1. Build (or grab) the newest `DuplicateFinder-<version>-x86_64.spk` in `dist/`
   (the build also emits `-armv8` and `-armv7` — pick the one matching your NAS).
2. DSM → **Package Center → Manual Install** → pick the `.spk`.
   (Allow installation of packages from *Any publisher* in Package Center
   settings, since this package is not signed by Synology.)
3. **Grant folder access** — the package runs as its own low-privilege user
   (`DuplicateFinder`), so give it permissions on the shared folders you want
   to scan or move files into:
   *Control Panel → Shared Folder → \<folder\> → Edit → Permissions →
   switch the dropdown to **System internal user** → `DuplicateFinder` →
   Read/Write.*
4. Open **Duplicate Finder** from the DSM main menu (admin users only).

## How it works

```
DSM browser UI (ui/index.html + app.js)
        │  fetch api.cgi/…            (same-origin, DSM login required)
        ▼
ui/api.cgi  – verifies the DSM session via authenticate.cgi,
              requires an administrator account, then execs
        ▼
bin/dupfinder -mode cgi  – reverse-proxies the request to
        ▼
bin/dupfinder -mode daemon  – 127.0.0.1:9807, started by
                              start-stop-status as the package user
```

- **Duplicates**: files are grouped by size, then by a 64 KiB prefix hash,
  then confirmed with a full BLAKE3 hash — only byte-identical files group.
  Every scan re-reads every candidate in full: a group reported by a scan was
  proven identical by that scan's own reads, never by a hash remembered from
  an earlier one, so a file that rots between scans can never keep riding in
  a group on a stale hash.
  Optional extra match criteria (name / modified / created) refine the groups
  in the UI. *Date captured* is read natively from file headers: EXIF in JPEG
  and the TIFF-family raws (`.tif`/`.dng`/`.nef`/`.cr2`/`.arw`), the embedded
  EXIF item in HEIF/HEIC/AVIF, and the QuickTime creation date in
  `.mov`/`.mp4`/`.m4v`.
- **Conflicting Files** is filled in by the same duplicates scan and lists files
  that claim to be the same file — identical size *and* identical modified time
  — but hold different content. Nothing that edits a file leaves both of those
  untouched, so a difference underneath them points at bit rot, a bad sector or
  an interrupted copy rather than at an edit. Its candidate key is size +
  modified time whatever the duplicates match criteria are set to, so the
  category means the same thing however you have those checkboxes.

  The category is named for what the scan can actually prove — that two files
  *disagree* — and not for a verdict on all of them. On a real NAS most sets end
  Undetermined; **Corrupted** is a per-file verdict that has to be earned, so it
  stays on the row rather than in the category name.

  Where the evidence allows, each copy is marked **Corrupted** or **Intact**,
  with the reason shown beside it. In order of strength:
  - the read failed with an I/O error — on a checksummed volume the drive is
    reporting that the stored bytes no longer match what was written;
  - the content changed since an earlier scan while its size and modified time
    did not (each scan keeps the previous scan's full-content hashes as
    history — a timestamped before and after, sensitive to rot anywhere in
    the file, not just its first bytes);
  - the file's own container is broken: PNG carries a CRC32 on every chunk,
    gzip one over the whole stream, and every ZIP member (so `.docx`, `.xlsx`,
    `.odt`, `.jar`) its own — these convict a single copy with nothing to
    compare it to. JPEG, PDF and the ISO base-media formats (MP4/MOV/HEIC/AVIF)
    are checked structurally instead, which catches truncation;
  - the shape of the difference: a run of zeros on one side where the other
    holds data is an interrupted copy, and that side is the damaged one. A
    single flipped bit is reported but convicts neither side — it is
    symmetric, and guessing would be worse than saying so.

  Anything else is left **Undetermined** with both copies listed. Two files can
  share a size and a modified time by coincidence (anything unpacked from one
  archive, or copied with `cp -p`), so sets whose members also share a filename
  — the real bit-rot-between-backups signature — are listed first.

  **It is a report: no checkboxes, no Move button**, and the daemon refuses a
  move naming it, not just the UI. Which copy to keep is a judgement, the
  evidence is sometimes silent, and a wrong guess destroys the good copy. Use
  the CSV export, then act in File Station.

  *A deliberate trade:* every scan re-reads every candidate in full, so rot
  anywhere in a file — first byte or last — is caught the next time a scan
  reads it, at the price of rescans doing the full-content work every time.
  (Resuming an *interrupted* scan continues that same scan's own reads
  rather than repeating them — that is what resuming means, and it is
  offered as an explicit choice, never done silently.)
  An earlier build served a remembered hash for files whose size, modified
  time and first 64 KiB still matched, which was faster but structurally
  blind to rot beyond those first 64 KiB; perfect detection was chosen over
  speed, and the remembered hashes now serve only as the before-picture that
  convicts a changed file.
- **Reference folders** are scanned but read-only: their files can't be
  selected or moved (enforced in the UI *and* the backend).
- **Move** never overwrites: the daemon vets every request, then delegates
  execution to DSM's File Station Web API as the logged-in admin (see
  Notes below) — always without the `overwrite` parameter, so a collision
  can only fail into the app's ` (n)`-renaming recovery instead of
  clobbering anything, and moved files keep all their Synology metadata.
- **Where moved files land** is asked *before* the folder picker opens,
  because it changes what the folder you pick means — and the picker's Select
  button starts the move, so there is no confirmation step after it. The
  dialog offers two mutually exclusive layouts:
  - **Preserve original folder structure** (the default) — the whole batch
    goes into **one new folder** created at the destination and named after
    the tool that found it: `Duplicates`, `Empty Folders`, `Empty Files`
    or `Temporary Files`, or the next free ` (n)` variant when
    that name is already taken. Every item is filed inside it under its
    original folder path, so a batch never merges into an earlier one and
    moved items always keep their own names.
  - **Move all files into same folder** — they go straight into the folder
    you picked, where a name clash gets the ` (n)` treatment described below.
- The daemon binds to localhost only; every UI request goes through DSM's
  authenticated web server and an admin check in `api.cgi`. The daemon
  additionally requires a shared secret (`var/.authtoken`, readable only by
  the package user) on every API request, so other local processes cannot
  drive it directly.

## Build from source

Requires Go ≥ 1.20 and Python 3 (icon generation). On macOS or Linux:

```sh
./build.sh          # → dist/DuplicateFinder-<version>-{x86_64,armv8,armv7}.spk
```

One spk per architecture family, each with a single static `bin/dupfinder`
(the packaging model the DSM developer guide documents). Install the one
matching your model — x86_64 for Intel models (e.g. DS916+), armv8 for
aarch64 models, armv7 for 32-bit ARM models incl. armada38x.

## Development

Off-NAS testing requires the `dev` build tag, which adds a few env-var hooks
(present only in dev builds — a release binary ignores every one of them). A
release binary reads just two variables of its own, neither meant to be set by
hand: `DUPFINDER_PORT`, the daemon's listen port (the package's start script
reads the same variable and passes it on), and `DUPFINDER_VAR`, the package
var directory that `api.cgi` exports so the CGI process can find the daemon's
auth token. The dev hooks:

```sh
cd server && go build -tags dev -o ../build/dupfinder-dev .
DUPFINDER_ROOTS=/path/to/fake/volume \
DUPFINDER_UI=../spk/ui \
DUPFINDER_DSM_URL=http://localhost:9807 \
  ../build/dupfinder-dev -mode daemon
# open http://localhost:9807
```

- `DUPFINDER_ROOTS` (colon-separated) substitutes for `/volume*`.
- `DUPFINDER_UI` serves the packaged UI files alongside the API.
- `DUPFINDER_DSM_URL` stands in for the DSM session (points File Station
  calls at the daemon's own mock Web API) — required, since every scan and
  mutation rides a DSM session.
- `DUPFINDER_BIND_HOST` widens the daemon's loopback-only bind (the ARM
  test runner reaches a containerized daemon through Docker's published
  ports, which never forward to container loopback). Release builds always
  bind 127.0.0.1.
- `DUPFINDER_STATE` stands in for the package var dir as the persistence
  location (results, hash cache, scan marker) — dev runs pass no `-var`,
  since a var dir would also arm the daemon token, which the harness runs
  without. Unset, dev runs simply persist nothing.

## Tests

```sh
test/run.sh
```

Fully self-contained, in five phases. It first stages a package payload and
checks the UI cache stamp (`test/stamp.sh`, driving `build.sh`'s real
`--stage-ui` code — the only check that the *assembled* payload is coherent,
since everything after it runs against `spk/ui` copied verbatim). Then it
builds the dev daemon, creates a disposable fixture volume (duplicate pairs
plus a same-size/different-content trap), and drives the real
`DuplicateFinder.js` headlessly — actual ExtJS 3.4 plus stubs for the `SYNO.*`
desktop classes (`test/harness.html`) — through scan, grouping, selection
rules, reference-folder protection, the move flow and export
(`test/smoke.js`, ~290 assertions). Finally it restarts the daemon to confirm
stored results and the interrupted-scan marker survive. Needs Go, Python 3,
Node 18+, and network on first run (fetches ExtJS and jsdom, cached
afterwards).

```sh
test/run-arm.sh
```

Runs the same smoke suite against the daemon cross-compiled for the two ARM
spk targets, each executing in a Docker container of that platform —
`linux/arm64` (armv8) and `linux/arm/v7` — so the ARM binaries are exercised
without ARM hardware. On Apple Silicon the arm64 pass runs natively and the
armv7 pass runs under QEMU (which is also the pass that would catch classic
32-bit issues like misaligned 64-bit atomics). The node harness stays on the
host; the fixture is bind-mounted at an identical path so host-side
assertions and the containerized daemon agree. Requires Docker. DSM-side
integration (CGI, lifecycle scripts, UI) is architecture-independent and
covered by the normal suite plus on-device testing.

## Layout

```
server/        Go backend (daemon + CGI proxy + scanners; dev.go is dev-only)
spk/ui/        Native DSM app (DuplicateFinder.js — renamed to a stamped
               filename at package time), app config, api.cgi
spk/scripts/   Package lifecycle scripts (start-stop-status, …)
spk/conf/      privilege (run-as package user)
test/          Self-contained headless test suite (test/run.sh)
tools/         Icon generator (pure-Python PNG writer)
build.sh       Builds binaries, icons, package.tgz and the final .spk
```

The UI script ships under a **stamped filename** — `DuplicateFinder-1-0-0-0110-1ab4d030.js`
— with `ui/config`'s single top-level key generated to match. DSM serves
`ui/<script>` with an ETag but **no `Cache-Control`**, under a URL carrying only
DSM's own global `v=` value, which does not change when a package is upgraded.
Browsers therefore cache the app's JS heuristically and can keep running a
previous build's code without revalidating (measured on a DS916+: a normal
reload after an upgrade served the old script with `transferSize 0`). The
filename is the only part of that URL the package controls.

The stamp is `<version>-<first 8 hex of the script's md5>`, so the URL changes
if and only if the script's bytes change: a rebuild that forgot to bump
`VERSION` still busts the cache, and an identical rebuild does not churn it.
The rename happens in the **package payload** (`stage_ui` in `build.sh`), never
in `spk/`, so the source tree keeps the canonical `DuplicateFinder.js` and the
dev daemon, `test/run.sh` and `test/harness.html` stay version-unaware.
`test/stamp.sh` covers it, and `build.sh` refuses to package a payload whose
config key names a file that is not there.

Two deliberate omissions. No unversioned copy is shipped as a fallback: it
would anchor a URL that never changes again and re-acquire the very cache entry
this defeats. And no `postupgrade` sweep removes older stamped scripts — none
is needed: DSM **replaces** `target/ui` wholesale on upgrade (verified on-device
during the 0126 install: the previous build's stamped JS answered 404 afterwards
while the new one answered 200), so older stamped scripts never accumulate and
a sweep would have nothing to delete.

No `style.css` is shipped. DSM would inject `<uidir>/style.css` into the whole
desktop page while the package runs (global rules there would corrupt other
apps' layout), so the app instead injects its own window-scoped styles at
runtime via `Ext.util.CSS.createStyleSheet` in `DuplicateFinder.js`.

## Notes / limitations

- DSM 7 does not let third-party packages read all volumes by default —
  folder access must be granted once per shared folder (step 3 above).
- "Created Date" is **File Station's own creation time**, fetched from the
  `SYNO.FileStation.List` API after each scan (so it matches File Station
  exactly, even on kernels without statx). It is never approximated
  natively; anything the API cannot answer stays blank — and under
  match-by-created, files with no answer are excluded from grouping (an
  unknown date is not a confirmed match) and the scan says how many were.
  Creation times are fetched in parallel batches with live progress
  ("Fetching creation times… (x of y)"), so large scans don't stall
  silently; Stop also interrupts the fetch.
- **Long moves and exports don't time out client-side**: a move across
  volumes or onto ext4 copies the data and takes as long as File Station
  needs, so those requests run with an effectively unlimited timeout behind
  a busy dialog. If the connection is lost mid-operation the app says the
  outcome is unknown — the NAS keeps working — and refreshes the results
  rather than falsely reporting failure. A second overlapping move is
  refused by the app itself ("A move is already running"): the dialog masks
  only the app window, and a folder chooser opened beforehand sits outside
  that mask, so the mask alone would not stop one. The daemon also executes
  move requests strictly one at a time.
- **A move reports its own progress**: `POST /api/move` is a single long
  blocking request and so cannot report anything in its own response. The
  daemon publishes progress separately instead — `{done, total, name}` under
  a `move` key in `/api/state`, present only while a move is in flight — and
  the busy dialog polls that every 700 ms, showing `12 of 40 — clip.mov`
  against a bar that starts indeterminate and becomes a real fraction on the
  first update. The file is announced *before* it is processed, so the name
  on screen is the one currently costing the time. Progress is published
  under the daemon's ordinary state lock, never the move lock, so the poll
  stays answerable throughout a move that holds that lock for minutes.
- **Long result lists are paged, exactly like a big folder in File
  Station**: the grid carries DSM's own `SYNO.ux.PagingToolbar` along its
  bottom edge — numbered pages, ±5-page jumps, first/last, the item count
  and a refresh button — and it hides its navigation when everything fits
  on one page. A scan of a very large NAS therefore never overwhelms the
  browser. Duplicates page in whole groups (100 per page, so the
  keep-one-per-group rules always see a complete group; the daemon never
  splits one across pages); the flat lists page in 1000 rows like File
  Station. Because a page counts *groups*, a page also carries a row budget:
  a single enormous group — a NAS with tens of thousands of copies of one
  file — would otherwise be one enormous page, so such a group arrives
  trimmed, labelled with its true size and "showing the first N". The
  totals, and the daemon's keep-one enforcement, always describe the whole
  group rather than the part on screen. As in File Station, a selection belongs to the page it was made
  on. The search box, the magnifier menu's advanced-search options (location,
  file type/extension, a date field with a From–To range, and a size
  comparison — File Station's own search form), the match refinement, the Big
  Files ≥ N MB/GB filter and column sorting are all applied by the daemon
  across the whole result
  set — the grid only ever holds one page — and the toolbar summary always
  describes the full set. The raw localhost API reads results the same way
  (`POST /api/results` with `{tool, offset, limit, q, match, minSize, sort,
  dir, refDirs}`, plus the search-options fields `{loc, ftype, extq, dateBy,
  dateFrom, dateTo, sizeOp, sizeMB}`) or as the legacy full dump
  (`GET /api/results?tool=…`).
- Stored results are capped, and every cap says how much more it found
  instead of truncating silently — narrowing the scan scope surfaces the
  rest. Duplicates keep the groups with the most reclaimable space, up to
  100 000 files — and no single group exceeds that budget either, so a
  million copies of one file cannot slip through the cap whole; empty/temp
  scans and the empty-folder scan cap at 20 000 rows.
- **Scans stream — daemon memory does not grow with the volume, or with how
  much of it is duplicated.** The walk hands every entry to a small per-tool
  accumulator instead of loading the tree into RAM: the flat tools keep only
  their capped result sets, the empty-folder scan keeps one frame per
  *open* directory — tree depth, not tree size, because a directory's fate
  is decided the moment the walk leaves it — and the duplicates scan spools
  compact records to a private,
  auto-cleaned spill file (in the package var directory) alongside a
  collision counter that is exact while the key population is small and
  falls back to a fixed 64 MB table beyond it (that fallback can only ever
  over-report a collision — it hashes a file needlessly, it never hides a
  duplicate). The collision candidates are then distilled into a second,
  smaller spill and hashed one partition of the key space at a time: every
  file sharing a candidate key shares a partition, so a group is never split,
  and a volume with ten million duplicate candidates simply runs more
  windows. Partitioning cannot split a single key, and keys are not evenly
  populated (every macOS `.DS_Store` is 6148 bytes, so one key can cover
  hundreds of thousands of files) — such a key is not truncated, which would
  silently lose whatever duplicates lived in the dropped part; it is
  re-partitioned by *content prefix* instead, since two files can only be
  duplicates if their first 64 KiB agree. Every member is examined either
  way. Groups accumulate in a bounded heap of the best-yet results, so
  the per-file work a result row costs — its ID and its EXIF capture date —
  is only ever spent on rows that survive the cap. Overlapping or
  symlink-aliased scope folders are de-overlapped by canonical path before
  walking, so the same file is never scanned twice under two names.
- **Results survive a daemon restart.** Finished scans (with the reference
  folders and keep-one survivor protections) are saved — atomically,
  private to the package user — into the var directory and restored when
  the package starts. A scan that a restart or reboot killed is reported
  when the app opens, and — for the duplicates scan, the only one that
  reads file contents and so the only one with progress worth continuing —
  the next Scan click asks what you want: **Resume** or **Start Over**.
  The other tools' scans simply run again, which for them amounts to the
  same thing. Resume continues the interrupted run —
  files it had already read in full are not read again, because resuming
  *is* that same scan carrying on, not a new scan borrowing old answers.
  Start Over discards that progress and re-reads everything. The choice is
  honored only while the interruption notice stands and only for a request
  identical to the interrupted run's — same tool, same scope and reference
  folders, same match criteria. Change any of those and the interrupted
  run's reads no longer describe the scan being asked for, so the dialog
  shows Resume disabled (restore the settings to re-enable it) and the
  daemon ignores the flag from any caller regardless — the scan simply
  re-reads everything. A **completed** scan can never be resumed into — a fresh scan
  always re-reads every candidate, deliberately: a hash computed by an
  earlier run is never reused, because reusing it is exactly where rot in
  an unchanged-looking file could hide. What IS kept across scans is a
  hash **history** — each scan's full-content hashes, compared against on
  the next scan to catch content that moved while size and mtime stood
  still. The history is capped at 500 000 entries in memory as well as on
  disk, oldest scan generations first, so a scan of millions of files
  cannot grow it without bound. The daemon's log rotates at 2 MB, one
  generation kept.
- The app is admin-only (`allUsers: false` + an administrators check in
  `api.cgi`), because the package user's permissions are shared by everyone
  who can open the app.
- "Empty" means the folder holds **nothing you could miss**: zero directory
  entries, or nothing but disposable junk — files the Temporary Files tool
  itself would list (`Thumbs.db`, `.DS_Store`, `desktop.ini`, editor
  droppings; one shared list decides what "junk" means everywhere) and
  Synology's `@eaDir` thumbnail cache, which DSM regenerates on demand. The
  junk simply rides along when the folder is moved. Any *other* content
  keeps a folder off the list: a folder whose only child is `.git` is a
  repository, not clutter, and a zero-byte file is not junk — a `.gitkeep`
  exists precisely to keep its folder non-empty. The confirmation goes
  through File Station's own listing, which was checked against DSM 7
  before replacing the native read: `.keep`, `.git`, `.htaccess` and
  `#recycle` all appear in it, so no real content can hide from the check
  (DSM filters exactly two names from that listing — `.DS_Store` and
  AppleDouble `._*` files — and the scanner's own walk classifies those
  same two as junk, so nothing slips through the gap).
- CSV **export never overwrites**: an existing report gets a ` (n)`-suffixed
  sibling instead, and the app reports the path actually written. Cells
  whose first meaningful character is a spreadsheet formula marker (`=`,
  `+`, `-`, `@` — even behind leading whitespace or control characters)
  are prefixed with an apostrophe, so a malicious filename can never
  execute as a formula when the report is opened.
- The daemon rejects any move/export destination (and any move source parent)
  that resolves through a **symlink to outside the volumes**, and requires
  the shared auth token from `var/.authtoken` on every request (dev builds
  run without `-var` and skip token enforcement). Scan content reads
  (hashing, EXIF) open files **through the pinned scan-root handles** with
  component-wise `O_NOFOLLOW` (works on kernel 3.10 — no `openat2`
  needed), so a directory swapped for a symlink after enumeration fails
  the read instead of redirecting it outside the volumes.
- **Moves and exports are executed by DSM's own File Station** (the
  documented `SYNO.FileStation.*` Web API), with the logged-in
  administrator's session — so they behave *exactly* like a move done in
  File Station: the real Created Date is preserved (Synology's private
  creation-time handling included), ACLs and metadata carry over, and
  moves between shared folders on one btrfs volume use the instant
  copy-on-write fastcopy. The daemon remains the safety gate — it validates
  every request (canonical paths, reference-folder protection,
  keep-one-per-group, volume containment, and scan-results membership:
  only files a scan actually surfaced can move) before File Station is
  asked to execute it. The only things the daemon creates at a destination
  are the preserve-mode tool folder, the mirrored origin chain beneath it,
  and — only when a name collision forces the staging recovery described
  below — a `.dupfinder-tmp-*` folder, all inside the destination that
  vetting already pinned. The tool folder is allocated lazily, after the
  first file has cleared every check, so a request whose files are all
  refused leaves nothing behind. Two cases can still leave an *empty* tool
  folder behind, both visible and harmless: a folder-creation reply lost in
  transit (File Station has no create-and-return-path call, so the daemon
  cannot tell whether that folder exists, takes the next free name, and the
  stray one stays), and a batch whose every file File Station itself then
  fails to move — by then the folder was already allocated, and the
  response still names it. Keep-one re-verifies its survivors against File
  Station at move time, so a copy deleted externally since the scan can
  never excuse taking the last remaining one — and each moved file must
  still match the size, type, and modification time its scan recorded, so
  a file replaced or modified since the scan is refused ("rescan and try
  again") rather than moved as something it no longer is. For duplicates —
  the one result that asserts something about a file's *content* — the scan
  also records the file's 64 KiB content fingerprint, and the move re-reads
  and re-checks it: a file rewritten in place with content of the same
  length and its modification time put back still matches on size and mtime,
  and is still refused. A move is also refused outright while a scan is
  running, rather than acting on the results it is about to replace.

  **Full move verification** is a checkbox on the move dialog ("Verify file
  contents after moving"), ticked by default. With it on, every moved *file*
  is read in full and hashed just before the move — for duplicates the hash
  must still equal the one the scan verified, which closes even the
  rot-past-the-fingerprint case; rows the scan recorded no hash for (empty
  and temporary files) are verified against that fresh pre-move read
  instead — and the copy at the destination is read back and hashed after
  it. A move that ends with the file parked in the destination's staging
  folder (a collision recovery that could not finish) is verified right
  there, in the staging folder: "parked" never means "unverified". A mismatched read-back is reported as exactly
  that ("does not match the original content"), distinct from a destination
  the package user merely cannot read ("could not be read back to verify") —
  the first is possible damage in transit, the second is a permissions gap,
  and confusing them would send someone deleting good copies. Folders are
  moved without content verification: their move is a single rename.

  Whether to verify is left to you each time, because it depends on plans
  the app cannot know: if you intend to *keep* the moved files — above all
  when the destination is a remote folder — verification is the proof the
  data survived the trip; if you are about to delete them anyway, it is
  pure cost. One fact worth knowing when you decide: a move **within the
  same shared folder** only renames directory pointers — no data blocks are
  rewritten — so verification there re-reads the same blocks and will
  usually be worth turning off. The app deliberately never makes that call
  for you: a path can *look* like it is inside the same shared folder and
  still be a remote mount underneath, and guessing wrong in that direction
  is precisely the unverified remote move the option exists to prevent.
  Move requests execute one at a time, and when a duplicate
  group shrinks to a single copy that survivor stays protected from later
  moves until the next duplicates scan — no sequence of requests, however
  interleaved, can drain a group.
- **Nothing is ever overwritten**: File Station calls are made without the
  `overwrite` parameter, so a name collision can only fail — the app then
  stages the file through a `.dupfinder-tmp-*` folder created next to where
  the file is going and renames it to the next free ` (n)` name. In
  preserve mode that is inside the tool folder's mirrored subfolder, not at
  the top of the folder you picked — and because that tool folder is always
  new, a preserve-mode move cannot collide on a filename at all. If a
  multi-step collision recovery is interrupted, the file sits intact in
  that `.dupfinder-tmp-*` folder and the error message names its full path.
- The app is operated through the DSM UI only: scans, moves and exports
  all require the caller's DSM session. The daemon's raw API (localhost
  with the shared token but no DSM session) can read cached state and
  results, nothing more.
- Everything File Station's API can answer is asked of File Station, not
  the filesystem: collision probing and ` (n)` name selection, for files and
  for the preserve-mode tool folder alike (`List getinfo`, asked about the
  specific names rather than listing the folder, so each probe costs the
  same whatever the destination holds), creation of the tool folder and of
  the mirrored origin chain (`CreateFolder`), the move precheck's
  file info and keep-one survivor existence
  (`List getinfo`), scan-root and move/export destination validation
  (`List getinfo` — the same view the folder picker showed), the
  empty-folder confirmation (`List list`), creation times (`List
  getinfo`), and the volume/share overview incl. disk usage (`List
  list_share` + `volume_status`). What remains native was checked against
  the full File Station API surface and kept native deliberately: content
  hashing (no API exists — `MD5` is one-file-per-async-task, no prefix
  reads, weaker hash), scan enumeration (`Search` has no per-path error
  reporting for unreadable subtrees and no symlink indicator, so it would
  break the empty-folder safety gate and create phantom duplicate pairs),
  EXIF capture dates (no API exposes image metadata), the canonical-path
  vetting (`CheckPermission`
  answers "may this user write here", not "does this symlink-resolved path
  stay inside the volume"), and volume-root discovery (the `/volume[0-9]*`
  glob — it defines the walls the safety vetting encloses every path in,
  and a security boundary must describe the filesystem the daemon actually
  walks, not an API answer). Scan roots must sit inside a shared folder —
  a bare volume root has no share-space address for File Station to answer
  about, so it is refused rather than checked natively (the UI's picker
  cannot produce one anyway).
