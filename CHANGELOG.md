# Changelog

Notable changes to Duplicate Finder. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions are the
package versions shown in Package Center.

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
