#!/usr/bin/env bash
#
# build-dist.sh — build the payload that brb writes onto every disc.
#
# Produces, in the output directory:
#
#   brb-linux-amd64     static Go binary  (uname -m: x86_64)
#   brb-linux-aarch64   static Go binary  (uname -m: aarch64)
#   brb-src.tar.gz      complete Go source, dependencies vendored
#   SHA512SUMS          hashes of the three above
#
# The binaries are static (CGO_ENABLED=0), so they run on any Linux of the right
# architecture with no libc, no Go toolchain and no shared libraries. The source
# tarball vendors every dependency, so it rebuilds offline:
#
#   tar xzf brb-src.tar.gz && cd brb-*/go && go build -mod=vendor ./cmd/brb
#
# Output goes outside the working tree by default, per this machine's convention
# that generated files live under ~/zx/dev/artifacts. Point brb at the result
# with BRB_DIST_DIR, or symlink it to ./dist.
#
# Copyright (c) 2026 Jonathan Zeppettini. MIT licensed — see the LICENSE file,
# which is the authoritative copy of the licence text for everything here.
#
set -Eeuo pipefail

REPO="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
OUT="${1:-/home/jz/zx/dev/artifacts/brb-dist}"

# The version the tarball and the say() lines are named after.
VERSION="$(sed -n 's/^VERSION="\([^"]*\)".*/\1/p' "$REPO/brb.sh" | head -1)"
[[ -n "$VERSION" ]] || { echo "could not read VERSION out of brb.sh" >&2; exit 1; }

# The two implementations carry their own version constants and there is no way
# to stamp one from here: this script used to pass
# -X github.com/jzbz/brb/internal/cli.Version, a symbol that does not exist —
# cli's version is a *const* aliasing backup.Version, and -X can only set a
# package-level string *var*, so the linker dropped the flag without a word and
# the "keep this in step" note was decoration. Bumping brb.sh's VERSION alone
# would have shipped a brb-1.0.1 tarball whose binaries, MANIFEST.txt and
# on-disc READMEs all still said 1.0.0.
#
# So the source stays the source of truth and this script's job is to NOTICE
# skew rather than paper over it: the version is read back out of the Go source
# and out of the binary that was just built, and either disagreeing with brb.sh
# stops the release. check_version_agrees is called after the binaries exist.
GO_VERSION_FILE="$REPO/go/internal/backup/backup.go"

check_version_agrees() { # check_version_agrees DIR-HOLDING-THE-BINARIES
  local dir=$1 src_version built native=""
  src_version="$(sed -n 's/^const Version = "\([^"]*\)".*/\1/p' "$GO_VERSION_FILE" | head -1)"
  [[ -n "$src_version" ]] \
    || { echo "could not read 'const Version' out of $GO_VERSION_FILE" >&2; return 1; }
  [[ "$src_version" == "$VERSION" ]] || {
    echo "version skew: brb.sh says $VERSION, $GO_VERSION_FILE says $src_version" >&2
    echo "  the tarball name, both binaries, MANIFEST.txt and every on-disc README would disagree" >&2
    return 1; }

  # Reading the const proves what the source says; running the binary proves
  # what was actually linked. Only the native architecture can be executed here,
  # and both binaries come out of the same tree, so one run settles both.
  case "$(uname -m)" in
    x86_64)        native=amd64 ;;
    aarch64|arm64) native=aarch64 ;;
  esac
  [[ -n "$native" && -x "$dir/brb-linux-$native" ]] || {
    say "no native binary to run ($(uname -m)); version checked against the source only"
    return 0; }
  built="$("$dir/brb-linux-$native" version 2>/dev/null | awk '{print $2}')"
  [[ "$built" == "$VERSION" ]] || {
    echo "version skew: brb.sh says $VERSION, the built brb-linux-$native reports '${built:-nothing}'" >&2
    return 1; }
  say "version $VERSION confirmed by the built brb-linux-$native"
}

# Go names the 64-bit ARM target "arm64"; Linux calls it "aarch64". The disc
# carries the uname -m spelling, because that is what a restorer will type.
#   GOARCH  ->  on-disc filename suffix
ARCHES=( "amd64:amd64" "arm64:aarch64" )

say() { printf '==> %s\n' "$*" >&2; }

command -v go >/dev/null 2>&1 || { echo "go toolchain not found" >&2; exit 1; }
say "go $(go version | awk '{print $3}')  building brb $VERSION"

# Everything is built into a sibling temp directory and moved into $OUT only
# once every artifact exists. set -Eeuo pipefail aborts on the first failure but
# nothing here was atomic, so a toolchain error, an OOM or a Ctrl-C part-way
# through left $OUT holding a NEW binary for one architecture beside the STALE
# one for the other, under a stale SHA512SUMS that described neither. Nothing
# downstream would have caught it: the Go writer's writePayload only stats each
# payload name and copies it, so the mixed pair would be burned onto every disc
# of a set, and each disc's own SHA512SUMS — generated over the mixed files —
# would certify the result as sound.
#
# A sibling of $OUT keeps the final moves on one filesystem, where each is a
# rename(2) that cannot half-happen.
OUT_PARENT="$(dirname -- "$OUT")"
mkdir -p "$OUT_PARENT"
STAGE_OUT="$(mktemp -d "$OUT_PARENT/.brb-dist.XXXXXX")"
stage=""                      # the source-tarball staging dir, made further down
trap 'rm -rf -- "$STAGE_OUT" ${stage:+"$stage"}' EXIT

# --- vendor, so the source tarball builds with no network ------------------
say "vendoring dependencies"
( cd "$REPO/go" && go mod tidy && go mod vendor )

# --- binaries --------------------------------------------------------------
for spec in "${ARCHES[@]}"; do
  goarch="${spec%%:*}"; suffix="${spec##*:}"
  out="$STAGE_OUT/brb-linux-$suffix"
  say "building brb-linux-$suffix"
  ( cd "$REPO/go" && CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" \
      go build -mod=vendor -trimpath -buildvcs=false \
        -ldflags "-s -w" \
        -o "$out" ./cmd/brb )
  chmod 755 "$out"
done

check_version_agrees "$STAGE_OUT"

# --- source tarball --------------------------------------------------------
# Deterministic: fixed ownership, sorted entries, zeroed timestamps. Two runs of
# this script over unchanged source produce byte-identical tarballs.
say "packing source"
stage="$(mktemp -d)"
top="brb-$VERSION"
mkdir -p "$stage/$top"

cp -a "$REPO/go" "$stage/$top/go"
cp -a "$REPO/brb.sh" "$REPO/LICENSE" "$REPO/README.md" "$REPO/build-dist.sh" "$stage/$top/"
find "$stage/$top" -name '.DS_Store' -delete 2>/dev/null || true

# -z (gzip), not zstd: this tarball is unpacked by a person, years from now,
# possibly on a rescue system, and `tar xzf` works everywhere. `tar --zstd` needs
# both tar support and the zstd binary. At a couple of megabytes the ratio is
# irrelevant; availability is not. Same reasoning as the encrypted index.
tar --sort=name --mtime='UTC 1970-01-01' --owner=0 --group=0 --numeric-owner \
    -czf "$STAGE_OUT/brb-src.tar.gz" -C "$stage" "$top"
chmod 644 "$STAGE_OUT/brb-src.tar.gz"

# --- the bash script, as a payload artifact --------------------------------
# brb.sh copies itself onto every disc, but the Go build has no shell script to
# copy. Shipping it here means a disc carries the readable implementation no
# matter which one produced it.
install -m 755 "$REPO/brb.sh" "$STAGE_OUT/brb.sh"

# --- checksums -------------------------------------------------------------
( cd "$STAGE_OUT" && sha512sum brb.sh brb-linux-amd64 brb-linux-aarch64 brb-src.tar.gz > SHA512SUMS )

# --- publish ---------------------------------------------------------------
# Every build step is behind us, so from here only rename(2) remains. SHA512SUMS
# is removed first and put back last: at no instant does $OUT hold a checksum
# file describing artifacts other than the ones beside it, and a machine killed
# mid-swap leaves a directory with no SHA512SUMS at all — visibly unfinished
# rather than plausibly complete. $OUT itself is never removed, so a symlink or
# a bind mount someone pointed BRB_DIST_DIR at survives.
say "publishing to $OUT"
mkdir -p "$OUT"
rm -f -- "$OUT/SHA512SUMS"
for f in brb.sh brb-linux-amd64 brb-linux-aarch64 brb-src.tar.gz; do
  mv -f -- "$STAGE_OUT/$f" "$OUT/$f"
done
mv -f -- "$STAGE_OUT/SHA512SUMS" "$OUT/SHA512SUMS"

say "done"
( cd "$OUT" && ls -l brb.sh brb-linux-amd64 brb-linux-aarch64 brb-src.tar.gz SHA512SUMS ) >&2
printf '\n  Point brb at it:  export BRB_DIST_DIR=%s\n  or:               ln -sfn %s %s/dist\n\n' \
  "$OUT" "$OUT" "$REPO" >&2
