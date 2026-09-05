# Changelog

Notable changes to Duplicate Finder. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions are the
package versions shown in Package Center.

## [Unreleased]

### Added
- A release gate, `test/check.sh`, that runs formatting, vet, staticcheck,
  the race-detector unit tests and the headless UI suite, and a GitHub
  Actions workflow that runs it on every push, pull request and release tag.
- This changelog, an architecture document and a development guide, with the
  README rewritten for users.

### Fixed
- The packaging test compared the stamped filename against the literal
  version `1.0.0`, so the test suite could not pass after the 1.0.1 release.
  It now asks `build.sh` for the version.

### Changed
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
