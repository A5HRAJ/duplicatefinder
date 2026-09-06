# Duplicate Finder for Synology DSM

[![CI](https://github.com/A5HRAJ/duplicatefinder/actions/workflows/ci.yml/badge.svg)](https://github.com/A5HRAJ/duplicatefinder/actions/workflows/ci.yml)

Duplicate Finder is a DSM 7 package that finds **duplicate files**, **empty
folders**, **empty files** and **temporary junk** on a Synology NAS, and
reports **conflicting files**: copies that claim to be the same file but hold
different content. It runs as a native app inside the DSM desktop and works
the way File Station does.

Files you select are **moved, never deleted**, into a folder you choose, and
**nothing is ever overwritten**.

## What it does

- **One Scan, every tool.** The sidebar holds the scan scope and a checkbox
  for each of the five tools, all ticked by default. Scan walks the scope once
  and runs every ticked tool over that one walk; each tool's results appear
  as its pass finishes.
- **Duplicate Files.** Files are grouped by size, then by a hash of their first
  64 KiB, then confirmed with a full BLAKE3 content hash, so only byte-identical
  files group. Every scan re-reads every candidate in full; a hash from an
  earlier scan is never trusted. Optional extra criteria (name, modified date,
  created date) narrow the groups. At least one copy of every group always
  stays where it is. Hard links to one file are labelled as such and count as
  one copy when reclaimable space is measured, because removing one name of a
  file frees nothing while another remains.
- **Conflicting Files.** Files with the same size and the same modified time
  but different content. Ordinary edits move the modified time, so a
  difference underneath an unchanged one points at bit rot, a bad sector or
  an interrupted copy; but tools that rewrite a file and put its timestamp
  back leave the same trace, so a copy is marked *Corrupted* only on positive
  evidence: a read that fails twice, a checksum or decode of its own that
  fails, a run of zeros where the other copy holds data, or a one-or-two-bit
  difference in a copy whose content changed under an unchanged timestamp.
  *Intact* is granted only when a check covered the copy's payload: PNG,
  gzip and ZIP-family checksums, a complete JPEG decode, or the checksums
  inside every compressed PDF stream. Everything else is left *Undetermined*
  with what was seen written beside it. This category is a report only: it
  has no checkboxes and no Move button, because choosing the copy to keep is
  your call.
- **Empty Folders.** Folders holding nothing you could miss: no entries at
  all, or nothing but disposable junk such as `Thumbs.db`, `.DS_Store` and
  Synology's `@eaDir` thumbnail cache. A folder whose only content is a
  hidden folder such as `.git` is never reported.
- **Empty Files** and **Temporary Files.** Zero-byte files, and cache, backup
  and editor droppings such as `*.tmp`, `*.bak` and `desktop.ini`.
- **Reference folders.** Folders you mark read-only are scanned but their
  files can never be selected or moved. Use them for the master copy of a
  photo library or a backup. The list is one setting shared by every tool
  and view, and a change takes effect at once, without a rescan.
- **Move.** Selected files go either into one new folder at the destination,
  named after the tool that found them and mirroring each file's original
  folder path, or straight into the destination. A name clash never
  overwrites anything; the file gets a ` (n)` suffix instead. An optional
  check reads every moved file back and compares it with the original.
- **Export.** Any result list can be saved as a CSV report.
- **Built for large volumes.** Scans stream, so memory does not grow with the
  size of the volume. Results are paged like a big folder in File Station,
  with search, filters and sorting applied across the whole result set.
  Finished scans survive a restart, and an interrupted duplicates scan can be
  resumed.

## Install

A [SynoCommunity](https://synocommunity.com/) package is in preparation.
Until it is listed there, build the package from source (below) and install it
by hand:

1. In **Package Center → Settings → General**, set the trust level to
   **Any publisher**. The package is not signed by Synology.
2. **Package Center → Manual Install**, and pick the `.spk` for your model:
   `x86_64` for Intel models, `armv8` for 64-bit ARM models, `armv7` for
   32-bit ARM models.
3. **Grant folder access.** The package runs as its own low-privilege user
   (`DuplicateFinder` for a hand-built package, `sc-duplicatefinder` for the
   SynoCommunity build), so give that user access to every shared folder you
   want to scan or move files into: *Control Panel → Shared Folder → select a
   folder → Edit → Permissions → switch the dropdown to **System internal
   user** → the package user → Read/Write*. DSM removes that user, and every
   grant made to it, when the package is uninstalled, so repeat this step
   after a reinstall. Without it a scan reports the folder as unreadable.
4. Open **Duplicate Finder** from the DSM main menu. Only administrators can
   use it.

### Build from source

Requires Go 1.20 or newer and Python 3.

```bash
./build.sh
```

This writes one package per architecture family into `dist/`:
`DuplicateFinder-<version>-{x86_64,armv8,armv7}.spk`.

## How a move is kept safe

Every move goes through the same checks, whether it comes from the app or
from a direct call to the daemon:

- Only files the tool's own scan listed can be moved, and only after the
  daemon has confirmed, through DSM's File Station, that each file still has
  the size, type and modified time that scan recorded. A file changed since
  the scan is refused with "rescan and try again". For duplicates the first
  64 KiB of content is re-read and compared as well, and an empty folder is
  checked again for content just before it moves.
- One copy of every duplicate group is held back, even if the request names
  them all, and the survivor stays protected until the next duplicates scan.
  Files inside reference folders are never moved, and nothing is moved into
  a reference folder either.
- The move itself is executed by File Station with your own DSM account, so
  it behaves exactly like a move made in File Station: permissions, metadata
  and the real creation date carry over, and moves within a btrfs volume use
  the instant copy-on-write path. The `overwrite` option is never used, so a
  name collision can only fail into the ` (n)` renaming, never replace a file.
- With **Verify file contents after moving** ticked (the default), every
  moved file is hashed just before the move and read back and hashed after
  it. A mismatch is reported as such, and is kept distinct from a destination
  the package user merely cannot read.
- A scan and a move never run at the same time, and only one move runs at a
  time.

Conflicting Files never moves anything, and the daemon refuses a move that
names it, not only the app.

## Good to know

- **Created Date** is File Station's own value. It is blank where File
  Station has no answer, and under created-date matching such files are not
  grouped.
- **Captured Date** is read from EXIF in JPEG and TIFF-family raw files, from
  the EXIF item in HEIF/HEIC/AVIF, and from the QuickTime creation date in
  MOV/MP4/M4V.
- **Very large result sets are capped**, and every cap says how much more was
  found: duplicates keep the groups with the most reclaimable space up to
  100,000 files; the other lists keep 20,000 rows. Narrowing the scan scope
  shows the rest.
- **Long moves do not time out.** A move across volumes or onto an external
  disk copies the data and takes as long as File Station needs, with no
  deadline; the app shows per-file progress meanwhile. If the connection
  drops, the NAS finishes the move and the app refreshes when it is done.
- **Hard links** (several names for one file, as backup tools that use
  `rsync --link-dest` create) are listed as duplicates but labelled, and the
  reclaimable figures count the file once. Moving one name of a hard-linked
  file frees no space while another name remains.
- **Every scan issue is listed.** The toast after a scan quotes the first
  location that could not be read; the summary bar links to the full list.
  If the results could not be saved to disk, the app says so: they are real
  on screen but will not survive a restart.
- **Where moved files land in preserve mode** is a new folder named
  `Duplicates`, `Empty Folders`, `Empty Files` or `Temporary Files` inside the
  folder you picked, or the next free ` (n)` variant of that name. The app
  reports the folder it actually created.
- **Scan roots must be inside a shared folder.** Symbolic links inside the
  scope are not followed. External USB and eSATA volumes can be scanned and
  used as destinations.
- **The interface is English only.**
- **Verification is a choice, not a guess.** A move within one shared folder
  only renames directory entries, so verification there re-reads the same
  blocks and is usually worth turning off. The app never decides that for
  you, because a path can look local and be a remote mount underneath.

## Tested on

| Environment | Status |
| --- | --- |
| DS916+ (x86_64), DSM 7.4, btrfs volume | the development NAS; every release is installed and exercised here |
| armv8 and armv7 builds | headless test suite under emulation only; no real ARM hardware yet |
| ext4 volumes, several volumes, DSM 7.0 to 7.3 | not yet tested |

If you run Duplicate Finder on anything in the last two rows, an issue
reporting how it went, good or bad, is welcome; the
[beta testing guide](docs/beta-testing.md) says what to try.

## Reporting a problem

Open an issue at <https://github.com/A5HRAJ/duplicatefinder/issues> with your
NAS model, DSM version, the package version shown in Package Center and what
you did. The daemon's log is `/var/packages/<package id>/var/dupfinder.log`
(readable by the package user and by root).

## Documentation

- [Architecture](docs/architecture.md): how the scanner, the results service,
  persistence and the move flow work, and the safety model in detail.
- [Development](docs/development.md): running the daemon off-NAS, the test
  suites, fuzzing, continuous integration and the release checklist.
- [Beta testing](docs/beta-testing.md): what to try on hardware and DSM
  versions the maintainer does not have, and how to report it.
- [Changelog](CHANGELOG.md).
- [SynoCommunity packaging](spksrc/README.md): the spksrc recipe and the state
  of the submission.

## License

MIT, see [LICENSE](LICENSE).
