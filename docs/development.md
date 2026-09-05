# Development

## Layout

```
server/        Go daemon, CGI proxy and scanners (dev.go is compiled only with -tags dev)
spk/ui/        The DSM desktop app (DuplicateFinder.js), its ui/config and api.cgi
spk/scripts/   Package lifecycle scripts (start-stop-status and no-op hooks)
spk/conf/      privilege: run as the package user
spksrc/        SynoCommunity packaging recipe (see spksrc/README.md)
test/          Release gate, headless test suites and the ARM runner
tools/         Icon generator (pure-Python PNG writer)
build.sh       Builds the binaries, icons and one .spk per architecture
docs/          This guide and the architecture document
```

## Requirements

Go 1.20 or newer, Node 18 or newer, Python 3, and Docker for the ARM runner.
The first test run downloads ExtJS 3.4 and jsdom into `test/` (both
gitignored).

## Running the daemon off-NAS

A `dev` build adds environment hooks that a release binary does not have:

```bash
cd server && go build -tags dev -o ../build/dupfinder-dev .
DUPFINDER_ROOTS=/path/to/fake/volume \
DUPFINDER_UI=../spk/ui \
DUPFINDER_DSM_URL=http://localhost:9807 \
  ../build/dupfinder-dev -mode daemon
# then open http://localhost:9807
```

| Variable | Effect (dev builds only) |
| --- | --- |
| `DUPFINDER_ROOTS` | colon-separated directories that stand in for `/volume*` |
| `DUPFINDER_UI` | serves the packaged UI files alongside the API |
| `DUPFINDER_DSM_URL` | points File Station calls at the daemon's own mock Web API (`server/dev.go`); required, since every scan and mutation needs a session |
| `DUPFINDER_STATE` | directory for persistence (results, hash store, scan marker); unset means nothing survives a restart |
| `DUPFINDER_BIND_HOST` | widens the loopback-only bind, for the containerized ARM runner |

A release binary reads exactly one variable of its own, `DUPFINDER_VAR`, which
`api.cgi` exports so the CGI process can find the daemon's token. The port is
fixed at 9807 in the daemon, the start script and the CGI gateway alike; there
is deliberately no override, because the gateway is executed by DSM's web
server with an environment the package does not control.

## Tests

### The release gate

```bash
test/check.sh
```

This is what CI runs on every push, pull request and release tag
(`.github/workflows/ci.yml`), and what a release must pass first. It runs, in
order: `gofmt`; `go vet` for the host and for linux/amd64, linux/arm64 and
linux/arm, in release and dev builds; `staticcheck` (pinned to the last
release that supports the module's Go version) for the same targets; the unit
tests under the race detector; and the headless suite below.

### The headless suite

```bash
test/run.sh
```

Self-contained, in five phases. It first stages a package payload and checks
the UI cache stamp (`test/stamp.sh`, driving `build.sh`'s real `--stage-ui`
code; this is the only check that the assembled payload is coherent). It then
builds the dev daemon, creates a disposable fixture volume (duplicate pairs, a
same-size/different-content trap, junk-only and unreadable folders, symlink
escapes and aliases, conflicting-file sets and enough duplicate groups to
need a second page), and drives the real `DuplicateFinder.js` headlessly in
jsdom, with actual ExtJS 3.4 plus stubs for the `SYNO.*` desktop classes
(`test/harness.html`), through scan, grouping, selection rules,
reference-folder protection, the search menu, the move flow and export
(`test/smoke.js`). Finally it restarts the daemon to confirm stored results
and the interruption marker survive, and exercises the resume rules against
the hash store's generation.

jsdom does no layout, so anything that depends on rendered geometry (clipping,
column widths, z-order) has to be checked on a real DSM; the suite says so at
each such point.

### The ARM runner

```bash
test/run-arm.sh
```

Runs the same smoke suite against the daemon cross-compiled for linux/arm64
and linux/arm/v7, each executing in a Docker container of that platform
(QEMU for armv7 on Apple Silicon), with persistence enabled so the binary
state layouts are exercised on 32-bit. The node harness stays on the host with
the fixture bind-mounted at an identical path. This is a local, pre-release
check rather than part of CI: emulation is slow, and the run has shown an
intermittent failure in the junk-folder move step on armv7 that is not yet
understood. Treat a failure there as something to investigate, not to rerun
until green.

### Unit tests alone

```bash
cd server && go test -race ./...
```

### Fuzzing

```bash
FUZZTIME=1m test/fuzz.sh
```

Every reader of bytes the daemon does not control has a fuzz target in
`server/fuzz_test.go`: the EXIF, HEIF and QuickTime metadata readers, the
container validators behind Conflicting Files, the byte-level comparison, and
the daemon's own spill, hash-store and results files. Each target is seeded
with a valid file built by `server/parsers_test.go`, and asserts that the
reader neither panics nor runs away and that its output is well-formed.
`go test` replays the seeds and any saved crashers on every run; the script
above runs the targets for real, for `FUZZTIME` each (default 30 seconds). A
crasher is written to `server/testdata/fuzz/<Target>/` and fails `go test`
from then on, so commit it together with the fix. The `Fuzz` workflow runs the
same script weekly and on demand from the Actions tab.

## Conventions

- Go code is `gofmt`-formatted and `staticcheck`-clean; the gate enforces
  both. Exemptions are made inline with `//lint:ignore` and a reason.
- The UI is ES5 (`var`, no arrow functions) because it runs inside DSM's
  ExtJS 3.4 desktop page; it is loaded by jsdom in the tests, so a syntax
  error fails the suite.
- Comments explain why a decision was made and what it protects, in the
  present tense. They do not narrate what earlier versions did.
- Everything File Station can answer is asked of File Station rather than the
  filesystem; the exceptions and their reasons are listed in the
  [architecture document](architecture.md).
- DSM's own theme paints the UI. Any visual difference from File Station is
  treated as a defect, and the suite guards against restyling what DSM
  already draws.

## Releasing

1. Run `test/check.sh`, `test/run-arm.sh` and `FUZZTIME=2m test/fuzz.sh`; all
   three must pass on the commit you intend to release. CI must be green for
   that commit too.
2. Bump `VERSION` in `build.sh`, and in `spksrc/` bump `PKG_VERS`
   (`cross/duplicatefinder/Makefile`), `SPK_VERS`, `SPK_REV` and the
   `CHANGELOG` line (`spk/duplicatefinder/Makefile`). Add the release to
   `CHANGELOG.md`.
3. Commit, tag `vX.Y.Z` (annotated) and push the tag. CI runs the gate on
   the tag.
4. In `spksrc/cross/duplicatefinder`, regenerate `digests` for the new tag's
   archive (`make digests`), commit and push.
5. Build the SynoCommunity packages through the spksrc toolchain for x64,
   aarch64 and armv7 (the recipe is in `spksrc/README.md`), and `./build.sh`
   for the hand-built packages.
6. Install on a test NAS and verify: a fresh install needs an explicit start,
   an upgrade starts by itself; the app must load the newly stamped script,
   persisted results must survive the upgrade, and a scan must reproduce the
   known baseline.
7. Open or update the pull request against SynoCommunity/spksrc.
