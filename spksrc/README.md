# SynoCommunity (spksrc) packaging draft

This directory mirrors the two folders a SynoCommunity submission adds to
[spksrc](https://github.com/SynoCommunity/spksrc): copy `spk/duplicatefinder`
and `cross/duplicatefinder` into a spksrc checkout to build with
`make -C spk/duplicatefinder arch-x64-7.1` (or any DSM 7 arch).

It builds: on 2026-09-04 the recipe went through the spksrc toolchain for
x64, aarch64 and armv7 on the DSM 7.1 toolchains, first try on each, with
no warnings (see "Building it" below). It is still NOT what `build.sh`
produces: `build.sh` keeps shipping the hand-built `DuplicateFinder`
package, which is the one installed on the DS916+, and the two cannot be
installed side by side (same port, same `dsmappname`).

## Building it

The spksrc image is amd64 and builds fine under emulation on Apple Silicon,
but on Docker Desktop for macOS the checkout must live in a Docker named
volume, not a bind mount: extracting Synology's toolchain applies read-only
directory modes before filling the directories, and on a bind mount the
container's root does not get root's bypass, so `tar` fails thousands of
times. Nothing is wrong with the package when that happens.

```bash
docker pull --platform linux/amd64 ghcr.io/synocommunity/spksrc:latest
git clone --depth 1 https://github.com/SynoCommunity/spksrc.git spksrc-src
cp -R spksrc/spk/duplicatefinder spksrc-src/spk/
cp -R spksrc/cross/duplicatefinder spksrc-src/cross/
docker volume create spksrc-work
docker run --rm --platform linux/amd64 -v spksrc-work:/spksrc -v "$PWD/spksrc-src":/src:ro   ghcr.io/synocommunity/spksrc:latest cp -a /src/. /spksrc/
docker run --rm --platform linux/amd64 -v spksrc-work:/spksrc -w /spksrc   ghcr.io/synocommunity/spksrc:latest /bin/bash -c   'make setup && make -C spk/duplicatefinder arch-x64-7.1 arch-aarch64-7.1 arch-armv7-7.1'
docker run --rm -v spksrc-work:/spksrc -v "$PWD/dist":/out alpine cp /spksrc/packages/duplicatefinder_*.spk /out/
```

`make setup` only writes `local.mk`. The generic arch names on DSM 7 are
`x64`, `aarch64` and `armv7`. Each arch takes about two minutes after the
one-off image pull: toolchain download and extraction, native Go, then the
package. The volume keeps the toolchains and Go for later builds.

What was checked on the results, per arch: INFO fields (`package`,
`version`, `arch` family, `os_min_ver`, `dsmuidir`, `dsmappname`, the
description with its parentheses intact), the checksum against
`package.tgz`, a static stripped `bin/dupfinder` of the right ELF
architecture, `ui/api.cgi` and the stamped script byte-identical to the
sources, `ui/config` naming exactly the shipped script, seven icons, the
`SSS_SCRIPT` copy shipped unchanged, every generated script passing
`sh -n`, and `conf/privilege` carrying `run-as: package`. The x64 daemon was
also started inside the build container: it wrote its token and ready file,
refused a token-less request with 401, and reported version `1.0.0`.

## The package id changes: `DuplicateFinder` -> `duplicatefinder`

SynoCommunity package ids are lowercase (every one of the 173 packages in
spksrc is), and spksrc writes `SPK_NAME` verbatim into INFO's `package=`.
DSM keys everything on that id, and until 0136 four places in the source
tree hardcoded the old one. They now discover the id at runtime, so one
source tree serves both builds and neither id appears in code:

| File | How the id is found now |
| --- | --- |
| `spk/ui/DuplicateFinder.js` | DSM loads the app through a script tag whose src is `/webman/3rdparty/<id>/DuplicateFinder-<stamp>.js`; `API_BASE` is that path with the filename replaced by `api.cgi`. `document.currentScript` first, then a scan of `document.scripts` for the `DuplicateFinder*.js` stem. No tag at all throws at load. Under the dev daemon and the jsdom harness the script is served from the root, giving `/api.cgi`, which dev.go routes. |
| `spk/ui/api.cgi` | `readlink -f "$0"` resolves to `/volumeN/@appstore/<id>/ui/api.cgi` whichever symlink the web server came through; `SCRIPT_FILENAME` and `SCRIPT_NAME` are the fallbacks. The id must match `[A-Za-z0-9._-]+` and `/var/packages/<id>/target/bin/dupfinder` must exist, or the request is refused with a 500 rather than guessed. `/debug` reports `pkgId` and `pkgIdSource`. |
| `server/main.go` | `DUPFINDER_VAR` from the shim as before; the fallback is now `argv[0]/../../var`, which is `/var/packages/<id>/var` for any id (argv[0], not `os.Executable`, because the resolved path lives under `@appstore`). |
| `server/dev.go` | strips any `/webman/3rdparty/<id>` prefix instead of the literal one. |

`spk/scripts/start-stop-status` keeps its literal for the hand-built package;
the spksrc copy in `spk/duplicatefinder/src/` reads `SYNOPKG_PKGNAME`
instead. The spk Makefile's last step still greps the staged UI for the old
literal paths so they cannot come back.

Consequence for the DS916+: DSM treats `duplicatefinder` as a different
package from `DuplicateFinder`. The first SynoCommunity build will install
alongside the hand-built one rather than upgrade it, and the old one has to
be uninstalled by hand (its var dir, including the hash cache and interrupted
scan state, does not carry over).

## What is kept from the hand-built package, and how

- **Start script.** spksrc's generic start-stop-status backgrounds the
  command, records its pid and returns without waiting. The hand-built
  script waits for the daemon's ready file so a daemon that dies during
  startup fails the package start visibly (the "Package Center says
  running, UI says not running" bug). spksrc allows a custom script through
  `SSS_SCRIPT`, so that script ships unchanged apart from the id.
- **UI cache stamp.** `build.sh` renames `DuplicateFinder.js` to carry its
  content hash and rewrites `ui/config` to match. The spk Makefile's
  `duplicatefinder_extra_install` does the same with `md5sum` and `jq`,
  with the same two postconditions (config names a file that exists, the
  unstamped script is gone).
- **Icons.** `tools/make_icons.py` runs inside the cross package and its
  `icon_*.png` land in `ui/images/`, so `ui/config`'s `images/icon_{0}.png`
  keeps resolving. `PACKAGE_ICON*.PNG` come from `SPK_ICON`
  (`src/duplicatefinder.png`, the same 256 px render).
- **Privilege.** `run-as: package` is exactly what spksrc generates for
  DSM 7, so `spk/conf/privilege` is not carried over.
- **No `SERVICE_PORT`.** The daemon listens on 127.0.0.1:9807 and is only
  reachable through `api.cgi`. Declaring the port would add a pointless
  firewall rule and make spksrc overwrite `ui/config` with its own.

## State of the submission

- The public repository is https://github.com/A5HRAJ/duplicatefinder, and
  `MAINTAINER`, `HOMEPAGE` and `PKG_DIST_SITE` name it. Tag `v1.0.1` marks
  the source this recipe builds (1.0.1-2; `v1.0.0` was 1.0.0-1); `digests`
  holds the SHA1, SHA256 and MD5 of the archive GitHub serves for that tag
  (`archive/refs/tags/v1.0.1.tar.gz`, 327,634 bytes, downloaded twice and
  byte-identical on 2026-09-04, top directory `duplicatefinder-1.0.1`). A
  new release means a new tag, a new `PKG_VERS`/`SPK_VERS`, `SPK_REV` one
  higher, a `CHANGELOG` line, and `make digests` in `cross/duplicatefinder`.
- The tagged archive itself still carries the zeroed `digests` file. That is
  unavoidable, since the hashes depend on the archive's bytes, and harmless:
  spksrc reads `digests` from its own tree, never from the archive.
- `LICENSE = MIT` matches the repository's LICENSE file.
- The daemon's `/api/info` will report `1.0.0`, not `1.0.0-1`: the cross
  package cannot see `SPK_REV`.
- Built through the spksrc toolchain on 2026-09-04 for x64, aarch64 and
  armv7 (`duplicatefinder_<arch>-7.1_1.0.0-1.spk`). Note `os_min_ver` comes
  out as 7.1 from those toolchains, against the hand-built package's 7.0.
- Installed on the DS916+ on 2026-09-04 after the hand-built package was
  uninstalled (the two cannot coexist: same port 9807, same `dsmappname`).
  Verified: package running (a fresh install needs an explicit start; DSM
  only auto-starts on upgrade or when the install call asks for it), the
  CGI resolves the id `duplicatefinder` from its own path and runs as
  `sc-duplicatefinder`, the desktop loads the stamped script from the new
  package path, the app's requests go to
  `/webman/3rdparty/duplicatefinder/api.cgi`. The service user is new, so
  the shared-folder grants (README step 3) have to be made again for
  `sc-duplicatefinder` before a scan can read anything; with them in
  place the scan reproduced the known baseline exactly.
- Upgraded in place to 1.0.1-2 the same day through the same route. An
  upgrade auto-starts (a fresh install does not), the persisted results
  survived, and the unreadable-location hint now names
  `sc-duplicatefinder`.
- The pull request to SynoCommunity/spksrc itself has not been opened.
