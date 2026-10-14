# Changelog

Notable changes to Duplicate Finder. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions are the
package versions shown in Package Center.

## [1.1.8] - 2026-10-14

### Fixed
- A shared folder of this same NAS mounted into another one as a remote
  folder made every file in it look like its own duplicate. File Station
  refuses the NAS's own addresses for Mount Remote Folder, but accepts a
  name that resolves to one. Moving either path then took the other with
  it, so the copy meant to stay in place went too. The app now recognises
  such mounts: it skips them when scanning and lists them among the scan
  issues, refuses to move files out of or into them, and never counts a
  copy seen through one as the copy that stays. Remote folders on other
  servers are unaffected.

## [1.1.7] - 2026-10-13

### Fixed
- A verified duplicates move checked the copy it left behind only by its
  size and modified time. A copy that had changed since the scan with both
  standing — damage on a disk without checksums, or a program that restores
  the date — could be the one kept while the good copy moved away. With
  **Verify file contents after moving** ticked, the copy that stays is now
  read too and must still match the scan; if it does not, a copy that still
  matches stays in its place instead. Unverified moves are unchanged.

## [1.1.6] - 2026-10-12

### Fixed
- A verified move onto another volume, a USB disk or another shared folder
  could leave only a damaged copy. File Station moves across filesystems by
  copying and then deleting the original, and verification ran afterwards,
  so a copy damaged on the way was found only once the original was gone.
  The app now copies first, reads the copy back, and removes the original
  only when the copy matches; a damaged copy is discarded and the original
  is left untouched. The files inside a nearly empty folder are checked the
  same way. Moves within one volume, and unverified moves, are unchanged.
- Renaming or reorganising the folder Duplicate Finder had moved copies
  into made those copies look like originals again, so a later Keep Newest
  or move could send the real original into a second cleanup folder. Moved
  copies are now recognised by the file itself, wherever they go within
  their shared folder; a moved file put back where it came from is an
  original again.
- When the connection to a long move dropped, or the app was closed during
  one, its per-file report reached only the daemon log, so a warning such
  as "the copy does not match" could go unseen. The NAS now keeps the
  report of the latest move until the app has shown it: the app shows it
  when the move finishes, follows a move already running when it opens, and
  shows a report nobody has seen the next time it opens.

## [1.1.5] - 2026-10-12

### Fixed
- Empty Folders could list a folder that was not empty. A hidden subfolder
  named like a temporary file (".DS_Store", "._photos") was taken for junk,
  and File Station's own listing hides such names, so a folder holding one —
  with real files inside — was shown as empty and offered for moving. Any
  subfolder other than Synology's thumbnail cache now means the folder is
  not empty.
- A file whose name contains one of a small range of rare private-use
  characters could not be moved: the app read its name back as a different
  name and refused it as "not valid Unicode text". Such names now come back
  exactly as they are.
- A move could be abandoned while File Station was still carrying it out.
  If DSM's web server dropped a single reply — to the request starting the
  move, or to one of the progress checks during a long copy to another
  volume — the app reported the file as failed although it went on to
  arrive: never verified even with verification on, still listed in the
  results, and not remembered as a copy the app had moved, so a later
  Keep Newest could take it for an original. The app now keeps checking
  through up to two minutes of silence and follows the move to its end.

## [1.1.4] - 2026-10-12

### Fixed
- Empty Folders showed every folder's size as "Empty", even a folder that
  still held files. A folder counts as empty when everything left in it has
  a temporary-file name, and those names include *.bak and *.old, which are
  also common names for real backups. Such a folder now shows the total size
  of what it holds, and its tooltip names the files, so a folder of backups
  cannot pass as a bare one.
- Removing the app's own temporary and batch folders after a move could have
  deleted anything still inside them. They are now removed only when empty:
  DSM itself refuses to delete one that still holds a file, and the folder
  stays where you can see it.
- A copy Duplicate Finder had already moved could count as "the copy that
  stays". When the move destination is inside the scanned folders, the next
  scan pairs each moved copy with its original, and Keep Newest / Keep Oldest
  could keep the moved copy and tick the original — so every copy ended up
  in folders meant to be emptied. The app now remembers where it moves
  things (and recognises its older "Duplicates/volume1/…" folders); while an
  original exists, one original always stays in place, moved copies are
  never the one kept and are not ticked to move again, and their location
  says Duplicate Finder moved them there.

## [1.1.3] - 2026-10-11

### Fixed
- The footer read "0 B reclaimable" for almost every selection, whatever
  was ticked: a file that occupies its full size carries no separate on-disk
  figure, and the footer counted that missing figure as zero. It now adds up
  what the selected files actually hold.
- Inside a read-only reference folder, only Duplicate Files showed the
  padlock. Empty Folders, Empty Files and Temporary Files listed the same
  folder's contents with an ordinary checkbox, and moving them could only be
  refused. Those rows now show the padlock and cannot be selected, in every
  view.

## [1.1.2] - 2026-10-11

### Fixed
- A named pipe or a socket in the scan scope (a Docker container's socket, a
  backup of a Linux system) was listed as an Empty File, and moving it with
  Verify on hung the daemon for good: opening a pipe to read it never
  returns, so the move never finished and every later scan and move was
  refused until the package was restarted. Only regular files and folders are
  scanned now, and the content readers refuse anything else at once.
- Sparse files — virtual machine disks, preallocated downloads — were counted
  at their apparent size, so two 1 GB images that occupied nothing were
  reported as 1 GB reclaimable. Reclaimable space is now measured in bytes
  held on disk, per copy, and never promises more than removing all but the
  largest copy would free.
- A file whose name is not valid UTF-8 (a Latin-1 or Shift-JIS name from an
  old Windows or Linux machine) was listed but could never be moved: the
  name lost its bytes on the way to the app, every move was refused with
  "not part of the current scan results", two such files showed as the same
  row twice, and the bytes were lost again in the saved results. Names now
  travel byte for byte in both directions and in the state file, the app
  draws each undecodable byte as �, and such a row is marked unmovable with
  the reason — DSM's File Station cannot address the name — instead of
  failing the move with a message that sends the user in circles.
- A move onto another volume that could not hold the files failed partway,
  one file at a time; it is now refused before anything is created, with the
  free and needed amounts.
- The daemon had no handler for the stop signal, so a package stop, a DSM
  reboot or an upgrade killed it on the spot: a move batch in progress now
  finishes the file it is on, reports the rest as not attempted and saves what
  it moved before exiting. The HTTP server also gained header and idle
  timeouts.

### Added
- `test/audit.sh`, run by the release gate: the real daemon against named
  pipes and sockets, sparse files, names outside UTF-8, Stop, a full
  destination volume and SIGTERM.

## [1.1.1] - 2026-09-05

### Fixed
- The sidebar clipped the longer tool names ("Duplicate Files", "Temporary
  Files", "Conflicting Files") once the tool checkboxes were added beside
  them. Found on a DS916+; the rail is now wide enough for every name with
  its badge at full width, and the toolbar still fits at the window's
  minimum width.

## [1.1.0] - 2026-09-05

### Added
- The scan scope lives in the sidebar, and each tool has a checkbox: one
  Scan runs every ticked tool over a single walk of the scope, and each
  tool's results appear as its pass finishes. Conflicting Files can be
  scanned for on its own.
- Reference folders are one setting shared by every tool and view, owned by
  the daemon; a change takes effect at once, without a rescan.
- Hard links are recognised: rows that share their data with other names
  are labelled, and reclaimable space counts each file once.
- The summary bar links to a dialog listing every issue a scan recorded, not
  only the first one the toast quotes; a state save that failed is reported.
- Conflicting Files decodes JPEGs completely and verifies the checksum
  inside every compressed PDF stream, so Intact means a check covered the
  payload. A corpus test runs the validators over a directory of real files
  and fails on any conviction.
- A release gate, `test/check.sh`, that runs formatting, vet, staticcheck,
  the race-detector unit tests and the headless UI suite, and a GitHub
  Actions workflow that runs it on every push, pull request and release tag.
- This changelog, an architecture document and a development guide, with the
  README rewritten for users.
- Fuzz targets for every reader of untrusted bytes (EXIF, HEIF, QuickTime,
  the PNG/gzip/ZIP/JPEG/PDF/media validators, the byte comparison and the
  daemon's own on-disk files), a `test/fuzz.sh` runner and a weekly `Fuzz`
  workflow; positive tests for the metadata readers, which had none.
- A beta-testing guide and a structured bug-report issue template.

### Fixed
- The Location dropdown of the search menu rendered folder names unescaped,
  so a folder named with markup could have run script in the administrator's
  session.
- Keep-one's survivor check accepted another tool's newer record of a
  rewritten file as a surviving duplicate; it now compares each member with
  its own group row, and every move names the tool whose results it acts on.
- Duplicates results persisted by an older build without content fingerprints
  are dropped at load instead of being moved without the prefix re-read.
- A destination inside a reference folder is refused, and an empty folder is
  confirmed empty again just before it moves.
- File Station moves are no longer given up after twelve hours; a move's
  lost reply is settled by the destination entry's identity; a generic
  failure after a partial directory move is reported instead of splitting
  the folder across two names; an all-failed batch no longer leaves an empty
  tool folder behind.
- Conflicting Files no longer convicts a copy on history alone: a file
  rewritten by a tool that preserved its timestamp was labelled Corrupted
  with "nothing edited this file". Framing-only checks no longer earn
  Intact, a file being saved mid-scan is not convicted, an I/O error is
  retried before it counts, unreadable members of very large families and
  flagged members of oversized sets are no longer dropped from the report,
  and the validators can be stopped mid-file and have no size caps.
- Stop during the final created-date lookup is written into the scan's
  issues instead of storing a silently incomplete result; a failed state save
  keeps the interruption marker; the unreadable-location list keeps 20,000
  entries instead of fifty.
- The empty-folder confirmation reads the directory natively as well as
  through File Station, so a hidden directory rejects the candidate whatever
  the listing shows.
- Switching to an unscanned tool while a page was loading could leave the
  grid masked "Loading…".
- The gateway accepts exactly one SynoToken, confines it to a token's
  alphabet and requires the auth helper to exit cleanly; the start scripts
  re-check the process before a forced kill; the fuzz runner fails when a
  target listing fails.
- The packaging test compared the stamped filename against the literal
  version `1.0.0`, so the test suite could not pass after the 1.0.1 release.
  It now asks `build.sh` for the version.

### Changed
- The Go module is organized by concern: the format readers and validators
  moved to `internal/media` and the pinned directory handle to
  `internal/dirhandle`; the former `main.go` and `scan.go` are split into
  files named for what they hold; and the scan orchestrator, the move
  handler and the UI's window and search-menu builders read as sequences of
  named steps instead of one long function each. No behaviour change.
- Source comments describe the current behaviour and its reasons; they no
  longer refer to internal build numbers or to earlier versions. No
  user-visible behaviour changed. The only code changes are cosmetic: two
  ZIP signature literals written as escape sequences, and two dev-only helper
  files moved behind the `dev` build tag.

## [1.0.1] - 2026-09-04

### Fixed
- The hint shown when a location cannot be read names the account to grant
  shared-folder access to (`sc-duplicatefinder` on the SynoCommunity build)
  instead of assuming the hand-built package's user.

## [1.0.0] - 2026-09-04

First public release.

- Duplicate files by BLAKE3 content hash with optional name, modified-date and
  created-date criteria; one copy of every group is always kept.
- Conflicting files: same size and modified time, different content, with
  per-copy Corrupted / Intact / Undetermined verdicts and the evidence behind
  them. Report only.
- Empty folders (including junk-only folders), empty files and temporary
  files.
- Read-only reference folders.
- Moves executed by File Station with the user's own session, never
  overwriting, with folder structure preserved into a new tool-named folder
  or flat, and optional read-back verification of every moved file.
- CSV export.
- Streaming scans with bounded memory, paged results with server-side search
  and sorting, results that survive restarts, and resumable interrupted
  duplicates scans.
- Packages for x86_64, armv8 and armv7; a SynoCommunity (spksrc) recipe.
