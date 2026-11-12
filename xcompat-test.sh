#!/usr/bin/env bash
#
# xcompat-test.sh — conformance suite for brb's on-disc format
# =============================================================================
#
# brb writes disc sets with its Go implementation and reads them with either
# that or brb.sh, a deliberately small bash reader kept so a stranger holding a
# disc can see exactly what will happen to their bytes before running anything.
#
# The on-disc format is the contract between the two, and it is the only thing
# someone with a stack of discs and no brb at all has to work from. This suite
# proves that contract holds: that a set the Go build wrote is read identically
# by the bash reader, that both agree with the recipe printed on the disc
# itself, and that a damaged disc recovers the way the README promises.
#
# It also absorbed the reader half of the old e2e-test.sh, so the awkward-tree
# fidelity checks (a directory named core/, hardlinks, symlinks, fifos, spaces,
# unicode), the sidecar-rot path, --only, KEEP_IMAGES and doctor all live here.
# Writing discs — packing, mksquashfs, ISOs, burning, resume — is covered by
# go-e2e-test.sh, which is the only suite that exercises a writer.
#
# Where the two readers differ in a way that ought to be fixed, the check is
# written the way it ought to pass and marked XFAIL with the divergence named.
# An XFAIL that starts passing is reported as XPASS and counted as a FAILURE,
# because this file is then out of date. Nothing known-broken is quietly
# omitted.
#
# The ledger is not the same thing as the README's table of command-line
# differences, and section 19 says which belongs where: that table records
# differences nobody intends to close, this records the ones somebody does.
#
# Usage
# -----
#   ./xcompat-test.sh [path/to/brb.sh] [path/to/go/brb]
#
# Every external tool it needs is checked first. A missing one is named and the
# suite stops with exit 77 — automake's SKIP convention, and deliberately not 0:
# a caller that reads exit codes ("both suites are green") must never be told
# success by a run in which no assertion executed at all.
#
# The optional tools — script(1), flock(1), a working unshare -r and an
# en_US.UTF-8 locale — are the only ones that gate individual checks rather than
# the whole file; each such check prints its own SKIP naming what is missing.

set -uo pipefail

# Default to the brb.sh sitting beside this suite rather than an absolute path:
# the tree has moved once already, and a hardcoded path turns a relocation into
# "brb.sh at /old/path" in the prerequisites list — which reads like a missing
# tool rather than a stale default. build-dist.sh derives its REPO the same way.
HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
BRB_SH=${1:-$HERE/brb.sh}
BRB_GO=${2:-${BRB_GO_BIN:-$HERE/brb}}

# ---------------------------------------------------------------------------
# reporting
# ---------------------------------------------------------------------------
pass_n=0; fail_n=0; xfail_n=0; skip_n=0
declare -a FAILURES=()

_line() { printf '%-5s  %s\n' "$1" "$2"; }
_why()  { [[ -n "${1:-}" ]] && printf '       %s\n' "$1"; return 0; }

pass()  { pass_n=$((pass_n+1)); _line PASS "$1"; }
fail()  { fail_n=$((fail_n+1)); FAILURES+=("$1"); _line FAIL "$1"; _why "${2:-}"; }
xfail() { xfail_n=$((xfail_n+1)); _line XFAIL "$1"; _why "divergence: ${2:-}"; }
xpass() { fail_n=$((fail_n+1)); FAILURES+=("XPASS: $1")
          _line XPASS "$1"
          _why "this known divergence now PASSES — it looks fixed: ${2:-}"
          _why "update xcompat-test.sh: turn this xassert into a plain assert"; }
skip()  { skip_n=$((skip_n+1)); _line SKIP "$1"; _why "${2:-}"; }
head_s() { printf '\n== %s\n' "$1"; }

# Trim a command's output down to something that fits on a failure line.
_tail() { printf '%s' "$1" | tr '\n' '|' | sed -e 's/|\+/ | /g' -e 's/^ *| *//' | tail -c 300; }

# assert0 DESC CMD...   -- the check passes when CMD exits 0
#
# Both return 1 when the check fails, so that `if assert0 "fixture: ..."` can
# skip the cases that lean on a fixture which was never built. They used to
# return fail's status, which is _why's, which is always 0: the guard let every
# case through, and a "refuses" case then passed for refusing a disc that did
# not exist. Nothing else reads their status, and the file does not set -e.
assert0() {
  local d=$1; shift
  local out rc
  out=$("$@" 2>&1); rc=$?
  if ((rc == 0)); then pass "$d"; else fail "$d" "exit $rc: $(_tail "$out")"; return 1; fi
}
# assertN DESC CMD...   -- the check passes when CMD exits non-zero
assertN() {
  local d=$1; shift
  local out rc
  out=$("$@" 2>&1); rc=$?
  if ((rc != 0)); then pass "$d"; else fail "$d" "expected a non-zero exit, got 0: $(_tail "$out")"; return 1; fi
}
# xassert0 DESC WHY CMD... -- SHOULD exit 0; today it does not. Non-zero = XFAIL.
xassert0() {
  local d=$1 why=$2; shift 2
  local rc
  "$@" >/dev/null 2>&1; rc=$?   # output discarded: the reason is in WHY
  if ((rc == 0)); then xpass "$d" "$why"; else xfail "$d" "$why"; fi
}
# xassertN DESC WHY CMD... -- SHOULD exit non-zero; today it exits 0. Zero = XFAIL.
xassertN() {
  local d=$1 why=$2; shift 2
  local rc
  "$@" >/dev/null 2>&1; rc=$?   # output discarded: the reason is in WHY
  if ((rc != 0)); then xpass "$d" "$why"; else xfail "$d" "$why"; fi
}

# ---------------------------------------------------------------------------
# dependencies
# ---------------------------------------------------------------------------
have() { command -v "$1" >/dev/null 2>&1; }

# python3 used to be on this list and was never invoked: it was left over from
# the bash writer's bin packer, and brb.sh's restore path says in as many words
# that it never needs Python. Requiring it meant a restore-shaped machine — the
# exact audience this suite exists for — skipped every check. cmp stays because
# the sidecar-rot fixture below now uses it to prove its own sabotage landed.
missing=()
for t in sha512sum mksquashfs unsquashfs par2 age age-keygen xorriso gzip diff find awk sed cmp sort; do
  have "$t" || missing+=("$t")
done
[[ -r $BRB_SH   ]] || missing+=("brb.sh at $BRB_SH")
[[ -x $BRB_GO   ]] || missing+=("go brb at $BRB_GO")

printf 'brb cross-compatibility suite\n'
printf '  bash implementation : %s\n' "$BRB_SH"
printf '  go   implementation : %s\n' "$BRB_GO"

if ((${#missing[@]})); then
  head_s "prerequisites"
  skip "whole suite" "missing: ${missing[*]}"
  printf '\n%d passed, %d failed, %d xfail, %d skipped\n' 0 0 0 1
  # 77, not 0. Nothing ran, and a green exit code from a run with zero
  # assertions is the one result a release gate must never be handed.
  printf 'nothing was tested — install the missing tool(s) and re-run\n' >&2
  exit 77
fi

# The two optional tools. script(1) gives the pty that ingest's disc prompts and
# age's passphrase prompt both insist on; UTF8_LOCALE drives the locale
# round-trip. Both were computed and never read for a while — the residue of an
# earlier suite — and each now gates the checks named after it.
HAVE_SCRIPT=0; have script && HAVE_SCRIPT=1
# flock(1) stands in for a run in progress in the staging-lock checks. Optional
# for the same reason python3 was struck off the required list: a bare
# restore-shaped machine without util-linux must still get every other check in
# this file, not a whole-suite skip.
HAVE_FLOCK=0; have flock && HAVE_FLOCK=1
HAVE_UTF8_LOCALE=0
# Keep the spelling the system actually has (glibc reports "en_US.utf8", other
# systems "en_US.UTF-8") rather than guessing one back.
UTF8_LOCALE="$(locale -a 2>/dev/null | grep -iE '^en_US\.utf-?8$' | head -1)"
[[ -n "$UTF8_LOCALE" ]] && HAVE_UTF8_LOCALE=1

# script(1) is the only way to hand brb a terminal, and two reader paths insist
# on one: ingest swaps discs on /dev/tty, and age reads a passphrase from
# /dev/tty and never from a pipe — deliberately, so neither can be automated by
# accident. Both went untested for exactly that reason until this ran them under
# a pty.
#
# The command is assembled as one string because script -c takes one; every path
# passed through here comes from mktemp -d and contains no quotes.
run_pty() { # run_pty LOGFILE FEED COMMAND-STRING
  local lf=$1 feed=$2 cmd=$3 rc=0
  printf '%s' "$feed" | timeout 300 script -qec "$cmd" /dev/null > "$lf" 2>&1 || rc=$?
  # A pty terminates every line with CR. Strip it once here so the greps below
  # can anchor on $ without each of them knowing that.
  tr -d '\r' < "$lf" > "$lf.raw" && mv -f "$lf.raw" "$lf"
  return $rc
}

# ---------------------------------------------------------------------------
# fixture
# ---------------------------------------------------------------------------
T=$(mktemp -d "${TMPDIR:-/tmp}/brb-xcompat.XXXXXX") || { echo "mktemp failed" >&2; exit 1; }
cleanup() { if [[ ${BRB_XCOMPAT_KEEP:-0} = 1 ]]; then printf '\nkept: %s\n' "$T"; else rm -rf -- "$T"; fi; }
trap cleanup EXIT

LOG=$T/logs; mkdir -p "$LOG"
SRC=$T/src           # the tame-but-awkward tree used for the round-trips
IDXSRC=$T/idxsrc     # tab / newline / backslash names, for the index checks
ID=$T/cfg/identity.txt
RCP=$T/cfg/recipients.txt
mkdir -p "$T/cfg"

build_src() {
  mkdir -p "$SRC/project/core/src" "$SRC/lib" "$SRC/empty"
  head -c 500000 /dev/urandom > "$SRC/big.bin"
  ln "$SRC/big.bin" "$SRC/hard.bin"                    # hardlink pair
  head -c 600000 /dev/urandom > "$SRC/lib/a.c"
  head -c 300000 /dev/urandom > "$SRC/lib/file with spaces.txt"
  head -c 200000 /dev/urandom > "$SRC/lib/ünïcøde.txt"
  head -c 400000 /dev/urandom > "$SRC/project/core/src/main.c"
  printf 'int main;\n' > "$SRC/project/core/important.c"
  ln -s ../big.bin "$SRC/lib/sym.bin"                  # symlink
  # $SRC/empty stays empty on purpose
}

build_idxsrc() {
  mkdir -p "$IDXSRC"
  printf 'x' > "$IDXSRC/$(printf 'a\tb.txt')"
  printf 'y' > "$IDXSRC/$(printf 'new\nline.txt')"
  printf 'z' > "$IDXSRC/back\\slash.txt"
  printf 'w' > "$IDXSRC/plain.txt"
}
IDXSRC_FILES=4

mkcfg() { # mkcfg OUT STAGING SOURCE [extra ...]
  local out=$1 staging=$2 source=$3; shift 3
  { printf 'STAGING="%s"\n' "$staging"
    printf 'SOURCE_DIR="%s"\n' "$source"
    printf 'AGE_RECIPIENTS_FILE="%s"\n' "$RCP"
    printf 'AGE_IDENTITY="%s"\n' "$ID"
    printf 'ARCHIVE_NAME="xcompat"\n'
    printf 'DISC_CAPACITY_BYTES=60000000\n'
    printf 'RESERVE_BYTES=30000000\n'
    printf 'PAR2_BLOCKS=40\n'
    local e; for e in "$@"; do printf '%s\n' "$e"; done
  } > "$out"
}

# Every brb invocation goes through these, so the logs land somewhere a failure
# message can point at. Usage: run_sh LOGFILE CFG command args...
run_sh() { local lf=$1 cfg=$2 rc=0; shift 2
           bash "$BRB_SH" --yes -c "$cfg" "$@" > "$lf" 2>&1 || rc=$?
           ((rc)) && { printf 'see %s: ' "$lf" >&2; tail -3 "$lf" >&2; }; return $rc; }
run_go() { local lf=$1 cfg=$2 rc=0; shift 2
           "$BRB_GO" --yes --no-color -c "$cfg" "$@" > "$lf" 2>&1 || rc=$?
           ((rc)) && { printf 'see %s: ' "$lf" >&2; tail -3 "$lf" >&2; }; return $rc; }

# A fifo and a real core dump: the awkward-tree cases folded in from
# e2e-test.sh. A mask of "core" must never delete a directory called core/.
build_src_extra() {
  mkfifo "$SRC/lib/a-fifo" 2>/dev/null || true
  printf 'dump\n' > "$SRC/project/core.12345"
}

build_src
build_src_extra
build_idxsrc
age-keygen -o "$ID" >/dev/null 2>&1 || { echo "age-keygen failed" >&2; exit 1; }
grep -o 'age1[0-9a-z]*' "$ID" | head -1 > "$RCP"

mkdir -p "$T/stage-go"
mkcfg "$T/cfg/go" "$T/stage-go" "$SRC"
GOD=$T/stage-go/discs/disc01

# ---------------------------------------------------------------------------
head_s "0. the reference set (written by the Go build; brb.sh no longer writes)"
# ---------------------------------------------------------------------------
assert0 "go brb backup exits 0" run_go "$LOG/backup-go.log" "$T/cfg/go" backup
# -mindepth 1 is load-bearing: find reports the starting directory too, and the
# starting directory here is called "discs", which matches disc*. Every count
# taken this way was one too high, which is how the partial-restore sabotage in
# section 8 came to delete a disc number that does not exist.
n_go=$(find "$T/stage-go/discs" -mindepth 1 -maxdepth 1 -type d -name 'disc*' 2>/dev/null | wc -l)
[[ -d $GOD ]]; assert0 "the set has at least one disc directory ($n_go)" test -d "$GOD"

# brb.sh must refuse the writer commands by name, and say where they went: a
# bare "unknown command" would read as a broken install.
# assert0 declares `local out rc`, and bash scopes dynamically, so a helper that
# read $out would see assert0's empty copy rather than ours. Distinct names.
writer_guidance() { # writer_guidance COMMAND
  local o r=0
  o=$(bash "$BRB_SH" -c "$T/cfg/go" "$1" 2>&1) || r=$?
  (( r != 0 )) || { echo "exited 0" >&2; return 1; }
  grep -qi 'reads disc sets\|no longer writes' <<<"$o" || { echo "$o" >&2; return 1; }
}
for c in backup plan burn iso init-key; do
  assert0 "brb.sh '$c' explains that writing moved to the Go build" writer_guidance "$c"
done

# ---------------------------------------------------------------------------
head_s "1. both implementations restore the same set to the same bytes"
# ---------------------------------------------------------------------------
restore_with() { # restore_with sh|go DEST LOG [extra args...]
  local who=$1 dest=$2 lf=$3; shift 3
  rm -rf "$dest"; mkdir -p "$dest"
  case $who in
    sh) run_sh "$lf" "$T/cfg/go" restore "$dest" "$@" ;;
    go) run_go "$lf" "$T/cfg/go" restore "$dest" "$@" ;;
  esac
}
# core.12345 is a real core dump and the default masks drop it on purpose, so
# it is excluded here and asserted absent on its own further down. Everything
# else must match byte for byte.
tree_matches() { diff -r --no-dereference --exclude=core.12345 --exclude=a-fifo "$SRC" "$1" >&2; }

assert0 "go brb restores its own set"        restore_with go "$T/out-go" "$LOG/restore-go.log"
assert0 "  ... byte-identical to the source" tree_matches "$T/out-go"
assert0 "brb.sh restores the Go-written set" restore_with sh "$T/out-sh" "$LOG/restore-sh.log"
assert0 "  ... byte-identical to the source" tree_matches "$T/out-sh"

# The shapes that have broken this tool before. Asserted on the bash reader's
# output because that is the one that just shrank.
o=$T/out-sh
assert0 "a directory named core/ survives"        test -f "$o/project/core/src/main.c"
assert0 "files inside core/ survive"              test -f "$o/project/core/important.c"
assert0 "a filename with spaces restores"         test -f "$o/lib/file with spaces.txt"
assert0 "a unicode filename restores"             test -f "$o/lib/ünïcøde.txt"
assert0 "a symlink restores as a symlink"         test -L "$o/lib/sym.bin"
assert0 "a fifo restores as a fifo"               test -p "$o/lib/a-fifo"
assert0 "an empty directory restores"             test -d "$o/empty"
same_inode() { [[ "$(stat -c%i "$1")" == "$(stat -c%i "$2")" ]]; }
assert0 "a hardlink restores as a real link"      same_inode "$o/big.bin" "$o/hard.bin"

# Both readers must agree with each other, not merely with the source.
assert0 "the two restores agree with each other" diff -r --no-dereference --exclude=a-fifo "$T/out-go" "$T/out-sh"
assert0 "the excluded core dump is absent"        test ! -e "$T/out-sh/project/core.12345"

# ---------------------------------------------------------------------------
head_s "2. SHA512SUMS and .sha512 sidecars under GNU sha512sum"
# ---------------------------------------------------------------------------
sums_verify() { ( cd "$1" && sha512sum -c --quiet SHA512SUMS ) >&2; }
assert0 "GNU sha512sum -c accepts the disc's SHA512SUMS" sums_verify "$GOD"
sums_cover_data() { grep -q 'data/disc01.squashfs.age$' "$1/SHA512SUMS"; }
assert0 "SHA512SUMS covers the image itself" sums_cover_data "$GOD"
sidecar_format() { # two spaces, 128 hex digits, no CR
  local f=$1
  [[ "$(od -c <"$f" | grep -c '\\r')" == 0 ]] || return 1
  grep -qE '^[0-9a-f]{128}  [^ ]' "$f"
}
assert0 "a .sha512 sidecar is 128 hex digits, two spaces, no CR" \
  sidecar_format "$GOD/data/disc01.squashfs.age.sha512"
assert0 "brb.sh verify-disc accepts the disc" run_sh "$LOG/verify-sh.log" "$T/cfg/go" verify-disc 1 "$GOD"
assert0 "go brb verify-disc accepts the disc" run_go "$LOG/verify-go.log" "$T/cfg/go" verify-disc 1 "$GOD"

# A restorer has a reader-side config — STAGING and the identity, none of the
# writer's keys. verify-disc crashed on exactly that (unbound ARCHIVE_NAME under
# set -u) while every config this suite writes carried the writer keys, so the
# audience the command exists for was the one case it never saw.
reader_cfg_verify() {
  local cfg=$T/cfg/reader-only
  { printf 'STAGING="%s"\n' "$T/stage-go"
    printf 'AGE_IDENTITY="%s"\n' "$ID"
  } > "$cfg"
  bash "$BRB_SH" -c "$cfg" verify-disc 1 "$GOD" > "$LOG/verify-reader.log" 2>&1
}
assert0 "brb.sh verify-disc works with a reader-side config (no writer keys)" reader_cfg_verify

# A line inside SHA512SUMS that has itself rotted is only a WARNING to
# sha512sum -c, which then never hashes the file that line named at all.
# verify-disc must fail rather than report the disc clean off a checksum file
# that cannot vouch for it. The fixture is asserted first and the refusal
# checks run only when it was built, so a mangling that silently missed, or a
# disc that was never there to copy, cannot let them pass vacuously: both
# readers refuse a directory that does not exist.
mangle_sums_fixture() {
  rm -rf "$T/rotsums"; cp -a "$GOD" "$T/rotsums"
  printf 'X' | dd of="$T/rotsums/data/disc01.squashfs.age" bs=1 seek=99 count=1 conv=notrunc 2>/dev/null
  sed -i 's|^[0-9a-f]\{128\}\(  \./data/disc01\.squashfs\.age\)$|deadbeef\1|' "$T/rotsums/SHA512SUMS"
  grep -q '^deadbeef  ' "$T/rotsums/SHA512SUMS"
}
verify_rotsums() { # verify_rotsums sh|go
  case $1 in
    sh) bash "$BRB_SH" --yes -c "$T/cfg/go" verify-disc 1 "$T/rotsums" > "$LOG/rotsums-sh.log" 2>&1 ;;
    go) "$BRB_GO" --yes --no-color -c "$T/cfg/go" verify-disc 1 "$T/rotsums" > "$LOG/rotsums-go.log" 2>&1 ;;
  esac
}
if assert0 "fixture: a disc whose SHA512SUMS line for a corrupt file is itself mangled" mangle_sums_fixture; then
  assertN "brb.sh verify-disc rejects a disc whose SHA512SUMS has a corrupt line" verify_rotsums sh
  assertN "go brb verify-disc rejects a disc whose SHA512SUMS has a corrupt line" verify_rotsums go
fi

# ---------------------------------------------------------------------------
head_s "2a. SHA512SUMS parsing and verify-disc, in both readers"
# ---------------------------------------------------------------------------
# verify-disc is the one command whose whole job is the integrity claim, and
# section 2 only shows it accepting a good disc and refusing one rotted line.
# brb.sh used to hand SHA512SUMS to `sha512sum -c`, which hashes whatever each
# line names: a textual check in front of it stopped "<empty digest>  /dev/null"
# and nothing else, so a BSD-style line, a leading space, or a symlink on the
# disc still had a disc "verified" off /dev/null or off a host file. The Go
# reader, for its part, followed a symlinked data/, hung in open(2) on a FIFO,
# and called an empty SHA512SUMS clean. Both now parse the file with one grammar
# (ReadSumFile; brb.sh's SUM_AWK) under one byte cap, require the disc's image
# to be listed, and hold every listed name to one rule — no link anywhere on the
# way, nothing climbing out, a regular file at the end (agecrypt.hashInRoot;
# brb.sh's verify_disc_sums). This section is that rule, case by case, on both.
#
# Every fixture is a copy of the reference disc with one thing done to it, used
# as a directory where a mount point goes, and removed — with anything its
# builder put beside it — once both readers have seen it. Each run is under a
# timeout, and a run the timeout ended counts as a FAILURE, never as a refusal:
# several of these discs used to hang one reader or the other, and a hang must
# not read as "refused". Each fixture builder checks that its own sabotage
# landed, so a builder that silently missed cannot let a refusal pass
# vacuously — and where a case appends one bad line to an intact SHA512SUMS
# rather than replacing it, nothing but that line can be what is refused.
#
# Some discs here would be refused by more than one guard: the rule that the
# image be listed refuses nearly anything, and where a reader refuses a disc
# outright or calls it unrecognised, the walk over the listed names would
# still have refused it a step later, as a mismatch. A refusal alone then goes
# on passing with the guard a case is about deleted, so such a case also names
# an ERE its refusal has to print — one for both readers, or one for each where
# they word it differently.
SV=$T/sv
SV_EMPTY=$(sha512sum < /dev/null | cut -c1-128)   # the digest of the empty file
sv_disc() { # sv_disc NAME -- a fresh copy of the reference disc to sabotage
  rm -rf -- "${SV:?}/${1:?}"; mkdir -p "$SV"; cp -a "$GOD" "$SV/$1" && test -f "$SV/$1/SHA512SUMS"
}
sv_corrupt() { # sv_corrupt NAME -- one flipped byte in the image, proven to have landed
  printf 'X' | dd of="$SV/$1/data/disc01.squashfs.age" bs=1 seek=99 count=1 conv=notrunc 2>/dev/null
  ! cmp -s "$GOD/data/disc01.squashfs.age" "$SV/$1/data/disc01.squashfs.age"
}
sv_keep() { # sv_keep NAME ERE -- keep only the SHA512SUMS lines matching ERE
  grep -E "$2" "$SV/$1/SHA512SUMS" > "$SV/$1/SHA512SUMS.new"
  mv -f -- "$SV/$1/SHA512SUMS.new" "$SV/$1/SHA512SUMS"
}
sv_drop() { # sv_drop NAME ERE -- drop the SHA512SUMS lines matching ERE
  grep -vE "$2" "$SV/$1/SHA512SUMS" > "$SV/$1/SHA512SUMS.new"
  mv -f -- "$SV/$1/SHA512SUMS.new" "$SV/$1/SHA512SUMS"
}
sv_sum_of() { sha512sum < "$1" | cut -c1-128; }
sv_run() { # sv_run sh|go NAME -- verify-disc 1 over the fixture, under SV_LOCALE if set; prints the exit status
  local r=0
  # Always run inside $( ), so the export goes no further than this one run.
  [[ -z "${SV_LOCALE:-}" ]] || export LC_ALL="$SV_LOCALE"
  case $1 in
    sh) timeout -k 5 60 bash "$BRB_SH" --yes -c "$T/cfg/go" verify-disc 1 "$SV/$2" > "$LOG/sv-$2-sh.log" 2>&1 || r=$? ;;
    go) timeout -k 5 60 "$BRB_GO" --yes --no-color -c "$T/cfg/go" verify-disc 1 "$SV/$2" > "$LOG/sv-$2-go.log" 2>&1 || r=$? ;;
  esac
  printf '%s' "$r"
}
sv_refuses() { # sv_refuses sh|go NAME [ERE] -- refused, and saying ERE if one is given
  local r; r=$(sv_run "$1" "$2")
  case $r in
    0)       echo "called it verified (see $LOG/sv-$2-$1.log)" >&2; return 1 ;;
    124|137) echo "hung until the timeout killed it (see $LOG/sv-$2-$1.log)" >&2; return 1 ;;
  esac
  [[ -z "${3:-}" ]] || grep -Eq -- "$3" "$LOG/sv-$2-$1.log" || {
    echo "refused, but not saying /$3/ — some other guard refused it (see $LOG/sv-$2-$1.log)" >&2
    tail -3 "$LOG/sv-$2-$1.log" >&2; return 1; }
}
sv_accepts() { # sv_accepts sh|go NAME
  local r; r=$(sv_run "$1" "$2")
  (( r == 0 )) || { echo "exit $r (see $LOG/sv-$2-$1.log)" >&2; tail -3 "$LOG/sv-$2-$1.log" >&2; return 1; }
}
sv_case() { # sv_case refuses|accepts NAME BUILDER DESCRIPTION [ERE [GO_ERE]]
  local how=$1 nm=$2 build=$3 desc=$4 sh_re=${5:-} go_re=${6:-${5:-}}
  if assert0 "fixture: $desc" "$build" "$nm"; then
    assert0 "  ... brb.sh verify-disc $how it" "sv_$how" sh "$nm" ${sh_re:+"$sh_re"}
    assert0 "  ... go brb verify-disc $how it" "sv_$how" go "$nm" ${go_re:+"$go_re"}
  fi
  rm -rf -- "${SV:?}/${nm:?}" "${SV:?}/host-$nm" "${SV:?}/out-$nm"
}

# The control every refusal below leans on: an untouched copy verifies.
sv_plain() { sv_disc "$1"; }
sv_case accepts plain sv_plain "an untouched copy of the reference disc"

# The shape refuse_escaping_sums existed for, and the two spellings it missed.
# GNU sha512sum -c --strict takes all three, so the old brb.sh hashed /dev/null
# and called the disc verified; the Go parser refuses the last two outright.
# The first is refused by brb.sh as a checksum file brb did not write and
# counted by Go as a mismatch (README's table of reader differences), and the
# ERE pins that: the walk from the disc root would also make it a mismatch in
# brb.sh, which would hide the loss of the refusal.
sv_gnu_null()  { sv_disc "$1" && printf '%s  /dev/null\n' "$SV_EMPTY" >> "$SV/$1/SHA512SUMS"; }
sv_bsd_null()  { sv_disc "$1" && printf 'SHA512 (/dev/null) = %s\n' "$SV_EMPTY" >> "$SV/$1/SHA512SUMS"; }
sv_lead_null() { sv_disc "$1" && printf '  %s  /dev/null\n' "$SV_EMPTY" >> "$SV/$1/SHA512SUMS"; }
sv_case refuses gnunull  sv_gnu_null  "an intact SHA512SUMS plus a line for /dev/null" \
  'which is not a file on the disc' 'mismatch: /dev/null'
sv_case refuses bsdnull  sv_bsd_null  "an intact SHA512SUMS plus a BSD-style line for /dev/null"
sv_case refuses leadnull sv_lead_null "an intact SHA512SUMS plus a /dev/null line with leading whitespace"

# Links. A disc brb wrote carries none at the names it lists, and following one
# reads a file that is not on the disc: the image linked to /dev/null beside the
# empty file's digest, or a listed link to a host file carrying that file's real
# digest, verified under the old brb.sh. A link that stays inside the disc is
# refused too — the rule is "no link", not "no link out", in both readers.
sv_null_image() {
  sv_disc "$1" || return 1
  mv -f -- "$SV/$1/data/disc01.squashfs.age" "$SV/$1/data/disc01.squashfs.age.was"
  ln -s /dev/null "$SV/$1/data/disc01.squashfs.age"
  printf '%s  ./data/disc01.squashfs.age\n' "$SV_EMPTY" > "$SV/$1/SHA512SUMS"
  test -L "$SV/$1/data/disc01.squashfs.age"
}
sv_host_link() {
  sv_disc "$1" || return 1
  printf 'a file on the host, not on the disc\n' > "$SV/host-$1"
  ln -s "$SV/host-$1" "$SV/$1/hostlink"
  printf '%s  ./hostlink\n' "$(sv_sum_of "$SV/host-$1")" >> "$SV/$1/SHA512SUMS"
  test -L "$SV/$1/hostlink"
}
sv_inner_link() {
  sv_disc "$1" || return 1
  ln -s README.md "$SV/$1/innerlink"
  printf '%s  ./innerlink\n' "$(sv_sum_of "$SV/$1/README.md")" >> "$SV/$1/SHA512SUMS"
  test -L "$SV/$1/innerlink"
}
# A link at the image's own name is no image at all to either reader's identity
# check (brb.sh's find -type f, Go's dataFiles), so that disc is unrecognised
# before a hash is read; the ERE keeps it from passing as a mere mismatch.
sv_case refuses nullimage sv_null_image "the image a symlink to /dev/null, listed with the empty file's digest" \
  'unrecognised disc'
sv_case refuses hostlink  sv_host_link  "a listed symlink to a host file, carrying that file's real digest"
sv_case refuses innerlink sv_inner_link "a listed symlink to a file inside the disc, carrying its real digest"

# data/ as a link off the disc, with a SHA512SUMS that does not mention the
# image at all: the Go reader's os.ReadDir followed the link, found an image
# there, and verified the disc on the strength of MANIFEST.txt alone. Now that
# the image has to be listed, that alone refuses this disc too, so the case
# needs the refusal to name the link.
sv_data_link() {
  sv_disc "$1" || return 1
  mkdir -p "$SV/out-$1"
  mv -- "$SV/$1/data" "$SV/out-$1/data"
  ln -s "$SV/out-$1/data" "$SV/$1/data"
  sv_keep "$1" '  \./MANIFEST\.txt$'
  test -L "$SV/$1/data" && [[ "$(wc -l < "$SV/$1/SHA512SUMS")" == 1 ]]
}
sv_case refuses datalink sv_data_link "data/ a symlink to a directory off the disc, SHA512SUMS listing only MANIFEST.txt" \
  'symbolic link'

# SHA512SUMS itself a link to an intact copy off the disc. Both readers followed
# it and called the disc verified on hashes the operator's machine supplied,
# since brb.sh's -f and Go's os.Stat both follow a link; ingest already refused
# it (Go) or warned and ignored it (brb.sh). The ERE pins the refusal to the
# link, since the copy's hashes match the disc and nothing else would refuse.
sv_sums_link() {
  sv_disc "$1" || return 1
  mkdir -p "$SV/out-$1"
  mv -- "$SV/$1/SHA512SUMS" "$SV/out-$1/SHA512SUMS"
  ln -s "$SV/out-$1/SHA512SUMS" "$SV/$1/SHA512SUMS"
  test -L "$SV/$1/SHA512SUMS"
}
sv_case refuses sumslink sv_sums_link "SHA512SUMS a symlink to an intact copy off the disc" \
  'symbolic link'

# What a SHA512SUMS has to say before it can vouch for anything. An empty one
# beside a corrupt image was clean to the Go reader; one that simply leaves the
# image out was clean to both, since each checked only what the file listed.
# The empty one fails the image rule as well, hence its ERE. The .. name is
# refused by brb.sh outright and counted by Go as a mismatch, as /dev/null is
# above, and its EREs pin that for the same reason.
sv_empty_sums() { sv_disc "$1" && sv_corrupt "$1" && : > "$SV/$1/SHA512SUMS"; }
sv_no_image() {
  sv_disc "$1" && sv_corrupt "$1" || return 1
  sv_drop "$1" 'data/disc01\.squashfs\.age$'
  ! grep -q 'disc01\.squashfs\.age$' "$SV/$1/SHA512SUMS" && test -s "$SV/$1/SHA512SUMS"
}
sv_dotdot() {
  sv_disc "$1" && printf '%s  ./data/../README.md\n' "$(sv_sum_of "$SV/$1/README.md")" >> "$SV/$1/SHA512SUMS"
}
sv_case refuses emptysums sv_empty_sums "a corrupt image beside an empty SHA512SUMS" \
  'lists no files'
sv_case refuses noimage   sv_no_image   "a corrupt image beside a SHA512SUMS that lists every file but it"
sv_case refuses dotdot    sv_dotdot     "a listed name that climbs out with .. and back in, carrying the real digest" \
  'which is not a file on the disc' 'mismatch: \./data/\.\./README\.md'

# A NUL in a listed name. bash's read drops it, so without SUM_ZS brb.sh's
# parser would look up README.md for "./READ<NUL>ME.md" and verify the disc,
# where Go's open refuses the name; and the image's own line spelled with a NUL
# in it would satisfy brb.sh's rule that the image be listed and not Go's. Both
# carry the real digest of the file the name reads as without its NUL. The
# second needs its ERE: brb.sh would also count that line a mismatch, which
# still refuses the disc, so only the message shows whether the line was taken
# for the image's.
sv_nul_name() {
  sv_disc "$1" || return 1
  printf '%s  ./READ\0ME.md\n' "$(sv_sum_of "$SV/$1/README.md")" >> "$SV/$1/SHA512SUMS"
  grep -qa 'READ.ME\.md$' "$SV/$1/SHA512SUMS"
}
sv_nul_image() {
  sv_disc "$1" || return 1
  sv_drop "$1" 'data/disc01\.squashfs\.age$'
  printf '%s  ./data/disc01.squash\0fs.age\n' "$(sv_sum_of "$SV/$1/data/disc01.squashfs.age")" >> "$SV/$1/SHA512SUMS"
  grep -qa 'squash.fs\.age$' "$SV/$1/SHA512SUMS" && ! grep -qa 'disc01\.squashfs\.age$' "$SV/$1/SHA512SUMS"
}
sv_case refuses nulname  sv_nul_name  "a listed name with a NUL in it, carrying the digest of the name without it"
sv_case refuses nulimage sv_nul_image "the image's own line with a NUL in its name, carrying the image's digest" \
  'records no hash for data/disc01\.squashfs\.age'

# A megabyte of name is allowed on a line, and an escaped one can be nothing
# but newlines. brb.sh prints a name it cannot match, and printing one of these
# through ${1//$'\n'/\\n} is quadratic in bash: most of a minute per name in a
# UTF-8 locale, where the Go reader takes milliseconds, so four of them would
# take brb.sh past the timeout. esc_str's awk join takes two or three seconds
# over all four, and this case is what keeps the substitution from coming back.
# It runs under the suite's UTF-8 locale, as CI does, and is skipped without
# one: under C the quadratic cost is about twenty times smaller, small enough
# for four names to fit in the timeout, and the case would pin nothing.
sv_nl_names() {
  sv_disc "$1" || return 1
  awk -v d="$SV_EMPTY" 'BEGIN {
    s = "\\n"; while (length(s) < 1040000) s = s s; s = substr(s, 1, 1040000)
    for (i = 1; i <= 4; i++) printf "\\%s  ./%d%s\n", d, i, s
  }' >> "$SV/$1/SHA512SUMS"
  [[ "$(grep -c '^[\]' "$SV/$1/SHA512SUMS")" == 4 ]]
}
if (( HAVE_UTF8_LOCALE )); then
  SV_LOCALE=$UTF8_LOCALE
  sv_case refuses nlnames sv_nl_names "four escaped names of about a megabyte each, nothing but newlines after ./N"
  unset SV_LOCALE
else
  skip "four escaped names of about a megabyte each, nothing but newlines after ./N (both readers)" \
       "no en_US.UTF-8 locale on this machine, and under C the case would pin nothing"
fi

# FIFOs. open(2) on one blocks until a writer appears; the Go reader hung there
# on SHA512SUMS and on MANIFEST.txt (through the first Ctrl-C), and both readers
# hung on a listed one. A FIFO MANIFEST.txt is not refused, only stepped past,
# as brb.sh's [[ -f ]] always did — it carries advice, not a hash.
sv_fifo_entry() { sv_disc "$1" && mkfifo "$SV/$1/fifo" && printf '%s  ./fifo\n' "$SV_EMPTY" >> "$SV/$1/SHA512SUMS"; }
sv_fifo_sums() {
  sv_disc "$1" || return 1
  mv -f -- "$SV/$1/SHA512SUMS" "$SV/$1/SHA512SUMS.was"
  mkfifo "$SV/$1/SHA512SUMS"
}
sv_fifo_manifest() {
  sv_disc "$1" || return 1
  mv -f -- "$SV/$1/MANIFEST.txt" "$SV/$1/MANIFEST.was"
  mkfifo "$SV/$1/MANIFEST.txt"
  sv_drop "$1" '  \./MANIFEST\.(txt|was)$'
  test -p "$SV/$1/MANIFEST.txt"
}
sv_case refuses fifoentry sv_fifo_entry    "a listed FIFO inside the disc"
sv_case refuses fifosums  sv_fifo_sums     "SHA512SUMS itself a FIFO"
sv_case accepts fifoman   sv_fifo_manifest "MANIFEST.txt a FIFO, unlisted, with an archive name configured"

# The byte cap, at its edge, in both readers: agecrypt.maxSumBytes and brb.sh's
# MAX_SUM_BYTES are the same number counted the same way. The padding is comment
# lines, which store nothing and are skipped by GNU sha512sum too, so before the
# cap both readers verified the file one byte over, and the case exactly at the
# cap shows the cap is not refusing everything near it. A SHA512SUMS linked to
# /proc/self/pagemap is the case a stat-based cap misses: a "regular file" of
# size 0 that reads far past the cap. Both readers now refuse the link itself
# before reading (sumslink above), so this now pins that no link here
# reaches the reader, rather than the cap.
SV_CAP=16777216
sv_pad_to() { # sv_pad_to NAME SIZE
  local f=$SV/$1/SHA512SUMS have
  have=$(stat -c %s "$f")
  { yes "#$(printf '%0998d' 0)" | head -c "$(( $2 - have ))"; } >> "$f"
  [[ "$(stat -c %s "$f")" == "$2" ]]
}
sv_at_cap()   { sv_disc "$1" && sv_pad_to "$1" "$SV_CAP"; }
sv_over_cap() { sv_disc "$1" && sv_pad_to "$1" "$(( SV_CAP + 1 ))"; }
sv_pagemap() {
  sv_disc "$1" || return 1
  mv -f -- "$SV/$1/SHA512SUMS" "$SV/$1/SHA512SUMS.was"
  ln -s /proc/self/pagemap "$SV/$1/SHA512SUMS"
}
sv_case accepts atcap   sv_at_cap   "a SHA512SUMS padded with comments to exactly $SV_CAP bytes"
sv_case refuses overcap sv_over_cap "a SHA512SUMS padded with comments to one byte more"
if [[ -r /proc/self/pagemap ]]; then
  sv_case refuses pagemap sv_pagemap "SHA512SUMS a symlink to /proc/self/pagemap"
else
  skip "a SHA512SUMS linked to /proc/self/pagemap" "no readable /proc/self/pagemap here"
fi

# The per-line cap, where bufio.Scanner puts it: a line of 1048576 bytes or more
# (a trailing CR counted, the newline not) refuses the file, even as a comment.
# brb.sh's SUM_AWK reproduces that edge by hand, so both sides of it are pinned.
sv_comment_of() { # sv_comment_of NAME LENGTH
  sv_disc "$1" || return 1
  { printf '#'; head -c "$(( $2 - 1 ))" /dev/zero | tr '\0' c; printf '\n'; } >> "$SV/$1/SHA512SUMS"
}
sv_long_ok()  { sv_comment_of "$1" 1048575; }
sv_long_bad() { sv_comment_of "$1" 1048576; }
sv_case accepts longok  sv_long_ok  "a comment line of 1048575 bytes"
sv_case refuses longbad sv_long_bad "a comment line of 1048576 bytes"

# The parser's own control. Names holding a newline or a backslash are written
# escaped (a leading backslash on the line), and brb.sh now unescapes them
# itself, as unescapeName does, instead of leaving that to sha512sum: a disc
# carrying such files, with a SHA512SUMS GNU sha512sum wrote over all of it,
# must still verify in both readers.
sv_escaped() {
  sv_disc "$1" || return 1
  printf 'x\n' > "$SV/$1/$(printf 'new\nline')"
  printf 'y\n' > "$SV/$1/back\\slash"
  ( cd "$SV/$1" && find . -type f ! -name SHA512SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha512sum > ../escaped.sums ) || return 1
  mv -f -- "$SV/escaped.sums" "$SV/$1/SHA512SUMS"
  # A leading backslash is GNU's mark of an escaped name; without one the case
  # would prove nothing about unescaping.
  grep -q '^[\]' "$SV/$1/SHA512SUMS" && grep -q 'data/disc01\.squashfs\.age$' "$SV/$1/SHA512SUMS"
}
sv_case accepts escaped sv_escaped "files whose names hold a newline and a backslash, summed by GNU sha512sum"

# Ingest reads the same file and is held to the same cap: one over it is not
# read at all, in either reader, and the copies go across unchecked with a
# warning naming the cap — where before, both read it, and brb.sh went on
# checking against it. The image must still be staged, byte for byte.
#
# And it reads it with the same grammar. brb.sh's ingest used to split each
# line with `read -r`, so a SHA512SUMS with CRLF line endings kept the CR on
# every name: not one copy was checked against it, and nothing said so, where
# the Go reader trims the CR and checks them all. The image's own sidecar is
# the copy to watch: the image is checked against that sidecar whatever
# SHA512SUMS says, but the sidecar has nothing else to vouch for it.
sv_ingest() { # sv_ingest sh|go NAME -- ingest the fixture into a STAGING of its own
  local st=$T/sv-ing-$2-$1
  rm -rf -- "$st"; mkdir -p "$st/enc"
  mkcfg "$T/cfg/sv-ing-$2-$1" "$st" "$SRC"
  case $1 in
    sh) run_pty "$LOG/sv-ing-$2-sh.log" $'\nq\n' "bash '$BRB_SH' -c '$T/cfg/sv-ing-$2-sh' ingest '$SV/$2'" || true ;;
    go) run_pty "$LOG/sv-ing-$2-go.log" $'\n\n'  "'$BRB_GO' --no-color -c '$T/cfg/sv-ing-$2-go' ingest '$SV/$2'" || true ;;
  esac
}
sv_crlf() { sv_disc "$1" && sed -i 's/$/\r/' "$SV/$1/SHA512SUMS" && grep -q $'data/disc01\\.squashfs\\.age\r$' "$SV/$1/SHA512SUMS"; }
if (( HAVE_SCRIPT )); then
  if assert0 "fixture: a disc whose SHA512SUMS is padded with comments to one byte over the cap" sv_over_cap ingcap; then
    for who in sh go; do
      case $who in sh) name="brb.sh" ;; go) name="go brb" ;; esac
      sv_ingest "$who" ingcap
      assert0 "$name ingest does not read a SHA512SUMS one byte over the cap, and says why" \
        grep -q "larger than $SV_CAP bytes" "$LOG/sv-ing-ingcap-$who.log"
      assert0 "  ... and still stages the image, byte for byte" \
        cmp -s "$GOD/data/disc01.squashfs.age" "$T/sv-ing-ingcap-$who/enc/disc01.squashfs.age"
    done
  fi
  if assert0 "fixture: a disc whose SHA512SUMS has CRLF line endings" sv_crlf ingcrlf; then
    for who in sh go; do
      case $who in sh) name="brb.sh" ;; go) name="go brb" ;; esac
      sv_ingest "$who" ingcrlf
      assert0 "$name ingest checks the image's sidecar against a CRLF SHA512SUMS" \
        grep -q 'disc01\.squashfs\.age\.sha512 copied and verified' "$LOG/sv-ing-ingcrlf-$who.log"
    done
  fi
  unset name
  rm -rf -- "${SV:?}/ingcap" "${SV:?}/ingcrlf" "${T:?}"/sv-ing-*
else
  skip "ingest of a disc whose SHA512SUMS is over the cap, or has CRLF line endings (both readers)" \
       "script(1) is not installed; ingest reads its disc prompts from /dev/tty"
fi

# ---------------------------------------------------------------------------
head_s "3. the encrypted index"
# ---------------------------------------------------------------------------
idx_plain() { age -d -i "$ID" "$1/enc/index.tsv.gz.age" 2>/dev/null | gzip -dc 2>/dev/null; }
src_files=$(find "$SRC" -type f ! -name 'core.12345' | wc -l)

index_grammar_ok() { # one row per file, exactly two tab-separated fields
  local n; n=$(idx_plain "$T/stage-go" | wc -l)
  [[ "$n" == "$1" ]] || { echo "index has $n row(s), want $1" >&2; return 1; }
  idx_plain "$T/stage-go" | awk -F'\t' 'NF!=2 { print "row with "NF" field(s): "$0 > "/dev/stderr"; b=1 } END { exit b+0 }'
}
assert0 "the index has one row per file, two fields each" index_grammar_ok "$src_files"

index_discs_in_range() {
  idx_plain "$T/stage-go" | awk -F'\t' -v m="$1" '$1 !~ /^[0-9]+$/ || $1+0<1 || $1+0>m { print "names disc "$1 > "/dev/stderr"; b=1 } END { exit b+0 }'
}
assert0 "the index names only discs that exist" index_discs_in_range "$n_go"

# Both readers must print the index identically, and identically to what the
# on-disc README's awk recipe would see.
#
# Captured to files rather than compared through process substitution: diff
# never sees a process substitution's exit status, so two readers that both DIE
# produce two empty streams that compare equal and the check passes with both
# implementations broken. Demonstrated against a staging directory with no
# images in it. Each side must therefore exit 0, print something, and mention a
# file the fixture is known to hold, before the diff is worth running.
readers_agree_on_index() {
  local a=$T/index-sh b=$T/index-go
  bash "$BRB_SH" -c "$T/cfg/go" index >"$a" 2>/dev/null \
    || { echo "brb.sh index exited non-zero" >&2; return 1; }
  "$BRB_GO" --no-color -c "$T/cfg/go" index >"$b" 2>/dev/null \
    || { echo "go brb index exited non-zero" >&2; return 1; }
  [[ -s $a ]] || { echo "brb.sh printed an empty index" >&2; return 1; }
  [[ -s $b ]] || { echo "go brb printed an empty index" >&2; return 1; }
  grep -q 'big\.bin' "$a" || { echo "brb.sh's index does not mention big.bin" >&2; return 1; }
  grep -q 'big\.bin' "$b" || { echo "go brb's index does not mention big.bin" >&2; return 1; }
  diff "$a" "$b" >&2
}
assert0 "brb.sh and go brb print the same index" readers_agree_on_index
readme_recipe_works() {
  local n; n=$(idx_plain "$T/stage-go" | awk -F'\t' '$1==1' | wc -l)
  (( n > 0 ))
}
assert0 "the README's awk -F'\\t' recipe returns rows" readme_recipe_works

# Awkward names get their own set, because the index is where they used to break.
mkdir -p "$T/stage-idx"; mkcfg "$T/cfg/idx" "$T/stage-idx" "$IDXSRC"
assert0 "go brb backs up tab/newline/backslash names" run_go "$LOG/backup-idx.log" "$T/cfg/idx" backup
idx_escaped_rows() {
  local n; n=$(age -d -i "$ID" "$T/stage-idx/enc/index.tsv.gz.age" 2>/dev/null | gzip -dc | wc -l)
  [[ "$n" == "$IDXSRC_FILES" ]] || { echo "$n row(s) for $IDXSRC_FILES file(s)" >&2; return 1; }
  age -d -i "$ID" "$T/stage-idx/enc/index.tsv.gz.age" 2>/dev/null | gzip -dc \
    | awk -F'\t' 'NF!=2 { b=1 } END { exit b+0 }'
}
assert0 "a tab or newline in a filename still yields one row of two fields" idx_escaped_rows
# Captured to files and compared with the decrypted index itself, for the
# reasons readers_agree_on_index gives and one more. Piped through awk, an
# empty index passed (awk exits 0 on no input), and so did one that dropped
# every escaped row, or one read without `read -r` that stripped every
# backslash while keeping two fields a row. Against each other, two readers
# sharing that last bug would still agree; against what was written, neither
# does. Both readers print the index unchanged into a pipe, which is the
# contract this holds them to. The field check runs before the count, so a row
# split by an unescaped newline is reported as that rather than as a count.
readers_print_escaped_index() {
  local a=$T/index-idx-sh b=$T/index-idx-go p=$T/index-idx-plain n
  idx_plain "$T/stage-idx" >"$p" || { echo "could not decrypt the escaped index" >&2; return 1; }
  bash "$BRB_SH" -c "$T/cfg/idx" index >"$a" 2>/dev/null || { echo "brb.sh index exited non-zero" >&2; return 1; }
  "$BRB_GO" --no-color -c "$T/cfg/idx" index >"$b" 2>/dev/null || { echo "go brb index exited non-zero" >&2; return 1; }
  awk -F'\t' 'NF!=2 { b=1 } END { exit b+0 }' "$a" || { echo "brb.sh split a row" >&2; return 1; }
  n=$(wc -l < "$a")
  (( n == IDXSRC_FILES )) || { echo "brb.sh printed $n row(s) for $IDXSRC_FILES file(s)" >&2; return 1; }
  cmp -s "$a" "$p" || { echo "brb.sh does not print the escaped index as written" >&2; return 1; }
  cmp -s "$b" "$p" || { echo "go brb does not print the escaped index as written" >&2; return 1; }
}
assert0 "both readers print the escaped index as written, one row per file" readers_print_escaped_index

# ---------------------------------------------------------------------------
head_s "4. par2 parity: verify, damage, repair"
# ---------------------------------------------------------------------------
par2_verify_in() { ( cd "$1" && par2 verify -q -- "$2" ) >&2; }
assert0 "the image's par2 set verifies from data/"    par2_verify_in "$GOD/data" disc01.squashfs.age.par2
assert0 "the sidecars' par2 set verifies from data/"  par2_verify_in "$GOD/data" sidecars.par2

# A rotted .sha512 must not condemn an image par2 proves is whole. This is the
# case that used to make the Go reader abandon a good multi-GB image.
rotted_sidecar_restore() { # rotted_sidecar_restore sh|go
  local who=$1
  local st=$T/rot-$who dest=$T/out-rot-$who cfg=$T/cfg/rot-$who
  rm -rf "$st" "$dest"; mkdir -p "$dest"
  cp -a "$T/stage-go" "$st"
  rm -rf "$st/restore"          # a cached plaintext would skip the check entirely
  mkcfg "$cfg" "$st" "$SRC"
  # 'x', not '0'. Writing '0' over the first hex digit is a no-op whenever the
  # digest already starts with '0' — 1 run in 16, and both subtests copy the
  # same stage so they share the digit and flake together. The run then went
  # down the happy path: "restores through a rotted sidecar" passed having
  # tested nothing, and both "names the sidecar" checks failed because no
  # sidecar was ever mentioned. A non-hex byte cannot collide with the digest
  # it overwrites, and it also makes the line malformed, so sha512sum -c fails
  # with "no properly formatted checksum lines" rather than a mismatch —
  # either way the reader must fall through to par2 and blame the sidecar.
  printf 'x' | dd of="$st/enc/disc01.squashfs.age.sha512" bs=1 count=1 conv=notrunc 2>/dev/null
  cmp -s "$T/stage-go/enc/disc01.squashfs.age.sha512" "$st/enc/disc01.squashfs.age.sha512" \
    && { echo "sabotage was a no-op: the sidecar is byte-identical to the pristine one" >&2; return 1; }
  case $who in
    sh) run_sh "$LOG/rot-sh.log" "$cfg" restore "$dest" ;;
    go) run_go "$LOG/rot-go.log" "$cfg" restore "$dest" ;;
  esac || return 1
  diff -r --no-dereference --exclude=core.12345 --exclude=a-fifo "$SRC" "$dest" >&2
}
assert0 "brb.sh restores through a rotted sidecar par2 says is the corrupt party" rotted_sidecar_restore sh
assert0 "go brb restores through a rotted sidecar par2 says is the corrupt party" rotted_sidecar_restore go
named_the_sidecar() { grep -qi 'sidecar' "$LOG/rot-$1.log"; }
assert0 "  ... and brb.sh names the sidecar as the corrupt party" named_the_sidecar sh
assert0 "  ... and go brb names the sidecar as the corrupt party" named_the_sidecar go

# A genuinely destroyed image must still be refused RATHER THAN DECRYPTED, and
# both halves of that sentence are asserted here. An assertN over the restore's
# exit status alone is satisfied by a reader that decrypts the image, starts
# unsquashfs, discovers the damage part-way through and exits non-zero — leaving
# a half-extracted tree over a destination that may be a live directory and a
# plaintext image in staging. Same three-part shape as only_prefix_refused and
# escape_refused: refused, and nothing written, either side of the guard. The
# fixture proves its own sabotage first, the way rotted_sidecar_restore does:
# a dd that silently wrote nothing would send this down the happy path.
corrupt_image_refused() { # corrupt_image_refused sh|go
  local who=$1 rc=0 n
  local st=$T/bad-$who dest=$T/out-bad-$who cfg=$T/cfg/bad-$who
  rm -rf "$st" "$dest"; mkdir -p "$dest"
  cp -a "$T/stage-go" "$st"; rm -rf "$st/restore"
  mkcfg "$cfg" "$st" "$SRC"
  dd if=/dev/urandom of="$st/enc/disc01.squashfs.age" bs=1 seek=2000 count=900000 conv=notrunc 2>/dev/null
  cmp -s "$T/stage-go/enc/disc01.squashfs.age" "$st/enc/disc01.squashfs.age" \
    && { echo "sabotage was a no-op: the image is byte-identical to the pristine one" >&2; return 1; }
  case $who in
    sh) run_sh "$LOG/bad-sh.log" "$cfg" restore "$dest" || rc=$? ;;
    go) run_go "$LOG/bad-go.log" "$cfg" restore "$dest" || rc=$? ;;
  esac
  (( rc != 0 )) || { echo "exited 0 over an image par2 cannot repair" >&2; return 1; }
  n=$(find "$dest" -type f | wc -l)
  (( n == 0 )) || { echo "extracted $n file(s) from an image it could not verify" >&2; return 1; }
  n=$(find "$st/restore" -maxdepth 1 -name '*.squashfs' 2>/dev/null | wc -l)
  (( n == 0 )) || { echo "left $n decrypted image(s) behind in staging" >&2; return 1; }
}
assert0 "brb.sh refuses an image par2 cannot repair, and writes nothing" corrupt_image_refused sh
assert0 "go brb refuses an image par2 cannot repair, and writes nothing"  corrupt_image_refused go

# ---------------------------------------------------------------------------
head_s "5. age interchange and the manual restore recipe"
# ---------------------------------------------------------------------------
# The recipe the on-disc README prints, run with neither implementation.
manual_restore() {
  local w=$T/manual; rm -rf "$w"; mkdir -p "$w"
  cp "$GOD/data/disc01.squashfs.age" "$GOD/data/disc01.squashfs.sha512" "$w/" || return 1
  ( cd "$w" && age -d -i "$ID" -o disc01.squashfs disc01.squashfs.age ) || return 1
  ( cd "$w" && sha512sum -c --quiet disc01.squashfs.sha512 ) >&2
}
assert0 "the README's manual recipe decrypts and verifies with no brb at all" manual_restore
# shellcheck disable=SC2016  # $0 is sh -c's own positional parameter, so the
# single quotes are the point: it must be expanded by the inner shell, not here.
assert0 "unsquashfs lists the manually decrypted image" \
  sh -c 'unsquashfs -l "$0"/disc01.squashfs >/dev/null 2>&1' "$T/manual"

# ---------------------------------------------------------------------------
head_s "6. the on-disc format inventory (a new artifact fails here)"
# ---------------------------------------------------------------------------
inventory() { ( cd "$1" && find . -type f -printf '%P\n' ) | sed -e 's/\.vol[0-9]*+[0-9]*\.par2$/.vol+N.par2/' | LC_ALL=C sort; }
data_inventory() { inventory "$1" | grep '^data/'; }
DATA_EXPECTED='data/disc01.squashfs.age
data/disc01.squashfs.age.par2
data/disc01.squashfs.age.sha512
data/disc01.squashfs.age.vol+N.par2
data/disc01.squashfs.sha512
data/index.tsv.gz.age
data/index.tsv.gz.age.sha512
data/sidecars.par2
data/sidecars.vol+N.par2'
inv_is() { diff <(printf '%s\n' "$2") <(data_inventory "$1") >&2; }
assert0 "a disc's data/ holds exactly the expected artifacts" inv_is "$GOD" "$DATA_EXPECTED"
root_has() { test -f "$GOD/$1"; }
for f in README.md MANIFEST.txt SHA512SUMS; do
  assert0 "a disc's root carries $f" root_has "$f"
done
# The root, too, is part of the frozen format, and "carries these three" is
# not "carries nothing else": --public-archive added identity.txt at the root
# without this section noticing, which is exactly the event it exists to catch.
# The tool payload (brb.sh, the two static binaries, the source tarball) is
# optional and whitelisted rather than expected: a set built without a dist
# directory legitimately carries only the copy of the running binary.
ROOT_EXPECTED='MANIFEST.txt
README.md
SHA512SUMS'
root_inventory() { inventory "$1" | grep -v '^data/' | grep -vE '^(brb\.sh|brb-linux-amd64|brb-linux-aarch64|brb-src\.tar\.gz)$'; }
root_is() { diff <(printf '%s\n' "$2") <(root_inventory "$1") >&2; }
assert0 "a disc's root holds exactly README.md, MANIFEST.txt and SHA512SUMS beside the payload (a new root artifact fails here)" root_is "$GOD" "$ROOT_EXPECTED"
readme_documents_escaping() { grep -q 'never span two rows' "$GOD/README.md"; }
assert0 "the on-disc README states the index escaping contract" readme_documents_escaping
readme_no_udf() { ! grep -qi 'UDF' "$GOD/README.md" || grep -qi 'no UDF\|not UDF' "$GOD/README.md"; }
assert0 "the on-disc README does not claim a UDF filesystem" readme_no_udf

# ---------------------------------------------------------------------------
head_s "7. reader behaviours: --only, KEEP_IMAGES, list, mount, doctor"
# ---------------------------------------------------------------------------
# The --only checks that expect an extraction compare the destination's whole
# inventory, not just the presence of the requested path, because presence is
# also what restoring the ENTIRE image gives. Every --only refusal (the index
# lookup, the listing pre-check, the prefix rule) runs before extraction, so a
# reader that lost the extraction operand would pass every one of them, then
# write the whole disc over a destination the operator had confirmed for one
# path, and a presence check would call that a pass. The inventory lists every
# non-directory, so an extra symlink or fifo fails it as surely as a file.
dest_inventory() { ( cd "$1" && find . ! -type d -printf '%P\n' ) | LC_ALL=C sort; }
only_extracts() { # only_extracts sh|go
  local who=$1
  local dest=$T/only-$who got
  rm -rf "$dest"; mkdir -p "$dest"
  case $who in
    sh) run_sh "$LOG/only-$who.log" "$T/cfg/go" restore "$dest" --only 'project/core/src/main.c' ;;
    go) run_go "$LOG/only-$who.log" "$T/cfg/go" restore "$dest" --only 'project/core/src/main.c' ;;
  esac || return 1
  test -s "$dest/project/core/src/main.c" || { echo "the requested path did not come back" >&2; return 1; }
  got=$(dest_inventory "$dest")
  [[ "$got" == 'project/core/src/main.c' ]] \
    || { echo "--only extracted more than that path: $(_tail "$got")" >&2; return 1; }
}
assert0 "brb.sh --only extracts just that path" only_extracts sh
assert0 "go brb --only extracts just that path" only_extracts go
only_missing_fails() {
  local dest=$T/only-none-$1; rm -rf "$dest"; mkdir -p "$dest"
  case $1 in
    sh) run_sh "$LOG/onlynone-sh.log" "$T/cfg/go" restore "$dest" --only 'not/here.txt' ;;
    go) run_go "$LOG/onlynone-go.log" "$T/cfg/go" restore "$dest" --only 'not/here.txt' ;;
  esac
}
assertN "brb.sh --only exits non-zero when nothing matched" only_missing_fails sh
assertN "go brb --only exits non-zero when nothing matched" only_missing_fails go

# --only with a PREFIX of a real path names no whole path component:
# 'project/cor' against an archive holding project/core. A substring match here
# used to select the disc, extract nothing (the extraction filter is exact),
# and print "restore complete" over an empty destination — the user discards
# the source believing the restore worked. Both readers must refuse AND write
# nothing; either half alone is not enough, so this is one compound assert0
# rather than an assertN that a crash could satisfy.
only_prefix_refused() { # only_prefix_refused sh|go
  local who=$1
  local dest=$T/onlypre-$who rc=0 n
  rm -rf "$dest"; mkdir -p "$dest"
  case $who in
    sh) bash "$BRB_SH" --yes -c "$T/cfg/go" restore "$dest" --only 'project/cor' > "$LOG/onlypre-$who.log" 2>&1 || rc=$? ;;
    go) "$BRB_GO" --yes --no-color -c "$T/cfg/go" restore "$dest" --only 'project/cor' > "$LOG/onlypre-$who.log" 2>&1 || rc=$? ;;
  esac
  (( rc != 0 )) || { echo "exited 0 over a prefix that names nothing" >&2; return 1; }
  n=$(find "$dest" -type f | wc -l)
  (( n == 0 )) || { echo "extracted $n file(s) for a path that names nothing" >&2; return 1; }
}
assert0 "brb.sh --only <prefix-of-a-real-path> refuses and writes nothing" only_prefix_refused sh
assert0 "go brb --only <prefix-of-a-real-path> refuses and writes nothing" only_prefix_refused go
# ...and a whole-component directory match must still work, with or without a
# trailing slash — the refusal above must not have overtightened into exact-file-only.
# The expected inventory is the whole of project/core as build_src makes it:
# a file added under project/core there has to be added here too.
only_dir_extracts() { # only_dir_extracts sh|go SPELLING
  local who=$1 spelling=$2
  local dest=$T/onlydir-$who got
  rm -rf "$dest"; mkdir -p "$dest"
  case $who in
    sh) run_sh "$LOG/onlydir-$who.log" "$T/cfg/go" restore "$dest" --only "$spelling" ;;
    go) run_go "$LOG/onlydir-$who.log" "$T/cfg/go" restore "$dest" --only "$spelling" ;;
  esac || return 1
  test -s "$dest/project/core/src/main.c" || { echo "the requested directory did not come back" >&2; return 1; }
  got=$(dest_inventory "$dest")
  [[ "$got" == $'project/core/important.c\nproject/core/src/main.c' ]] \
    || { echo "--only extracted other than exactly that directory: $(_tail "$got")" >&2; return 1; }
}
assert0 "brb.sh --only project/core extracts the directory"      only_dir_extracts sh 'project/core'
assert0 "go brb --only project/core extracts the directory"      only_dir_extracts go 'project/core'
# Both readers, both spellings: three quarters of that grid was asserted, and
# the missing cell was the Go one — in the file whose whole premise is that the
# two agree. A trailing slash has produced a Go-only divergence in this suite
# before (the destination-symlink case in section 12). Both readers now fold the
# spelling once, before the index is consulted — onlyPath in Restore on the Go
# build, the --only arm in brb.sh — and hand unsquashfs the folded operand;
# until they did, the Go build stripped slashes only for its pre-checks and
# passed the operand on unchanged. Section 8a pins the other spellings. If the
# two ever stopped matching, the result would be a run that selects the disc,
# extracts nothing and reports success — the exact silent-empty-restore this
# section exists for.
assert0 "brb.sh --only project/core/ (trailing slash) works too" only_dir_extracts sh 'project/core/'
assert0 "go brb --only project/core/ (trailing slash) works too" only_dir_extracts go 'project/core/'

keep_images() { # keep_images sh|go WANT   (WANT=1 keep, 0 remove)
  local who=$1 want=$2
  local st=$T/keep-$who-$want dest=$T/outkeep-$who-$want cfg=$T/cfg/keep-$who-$want
  rm -rf "$st" "$dest"; mkdir -p "$dest"
  cp -a "$T/stage-go" "$st"; rm -rf "$st/restore"
  # The behaviour is what has to match, not the spelling, so the two are driven
  # by different ones: brb.sh by the config key, the Go build by its
  # --keep-images flag. Both readers take the key from the file too, and
  # go_reads_keep_images_from_config further down covers the Go build's half of
  # that; this check keeps the flag covered as well.
  local -a extra=()
  case $who in
    sh) mkcfg "$cfg" "$st" "$SRC" "KEEP_IMAGES=$want"
        run_sh "$LOG/keep-$who-$want.log" "$cfg" restore "$dest" ;;
    go) mkcfg "$cfg" "$st" "$SRC"
        (( want )) && extra=( --keep-images )
        run_go "$LOG/keep-$who-$want.log" "$cfg" restore "$dest" ${extra[@]+"${extra[@]}"} ;;
  esac || return 1
  local left; left=$(find "$st/restore" -maxdepth 1 -name '*.squashfs' 2>/dev/null | wc -l)
  if (( want )); then (( left > 0 )); else (( left == 0 )); fi
}
assert0 "brb.sh KEEP_IMAGES=0 removes the decrypted image" keep_images sh 0
assert0 "brb.sh KEEP_IMAGES=1 keeps it"                    keep_images sh 1
assert0 "go brb KEEP_IMAGES=0 removes the decrypted image" keep_images go 0
assert0 "go brb KEEP_IMAGES=1 keeps it"                    keep_images go 1

# Same shape as readers_agree_on_index, and this one matters more: list has no
# other assertion in either suite, so a regression that made BOTH list commands
# crash used to go green on two empty streams comparing equal.
list_agrees() {
  local a=$T/list-sh b=$T/list-go
  bash "$BRB_SH" -c "$T/cfg/go" list 1 >"$a.raw" 2>/dev/null \
    || { echo "brb.sh list 1 exited non-zero" >&2; return 1; }
  "$BRB_GO" --no-color -c "$T/cfg/go" list 1 >"$b.raw" 2>/dev/null \
    || { echo "go brb list 1 exited non-zero" >&2; return 1; }
  [[ -s $a.raw ]] || { echo "brb.sh listed nothing" >&2; return 1; }
  [[ -s $b.raw ]] || { echo "go brb listed nothing" >&2; return 1; }
  grep -q 'big\.bin' "$a.raw" || { echo "brb.sh's listing does not mention big.bin" >&2; return 1; }
  grep -q 'big\.bin' "$b.raw" || { echo "go brb's listing does not mention big.bin" >&2; return 1; }
  # Both readers hand `unsquashfs -ll` straight to a pipe (cmd_list in brb.sh,
  # restore.List in Go), so the two listings must match byte for byte: order,
  # and the whitespace inside names, included. This used to collapse runs of
  # spaces and then sort both sides, citing padding that differed; it never
  # did, and the sort discarded the very order it said carried information.
  diff "$a.raw" "$b.raw" >&2
}
assert0 "brb.sh and go brb list a disc's contents identically" list_agrees

assert0 "brb.sh doctor exits 0 with every restore tool present" run_sh "$LOG/doctor-sh.log" "$T/cfg/go" doctor
assert0 "go brb doctor exits 0"                                 run_go "$LOG/doctor-go.log" "$T/cfg/go" doctor
# The positive control the index and list checks above both carry, for the same
# reason: a bare negative grep is satisfied by an empty log, so a doctor that
# printed nothing at all — or printed somewhere run_sh does not capture — would
# report "no longer checks writer-only tools" having reported nothing whatever.
# Anchored on par2 and unsquashfs because doctor really does name both, and
# because 'unsquashfs' does not collide with the 'mksquashfs' being excluded.
doctor_is_reader_only() {
  # Written as an if rather than A && B || C: with the && chain, a failure of
  # the SECOND grep runs the || arm too, which happens to be right here but is
  # the shape SC2015 exists to catch, and the next person to add a third
  # positive control would inherit the trap.
  if ! grep -qi 'unsquashfs' "$LOG/doctor-sh.log" || ! grep -qi 'par2' "$LOG/doctor-sh.log"; then
    echo "doctor printed no tool report to grep" >&2
    return 1
  fi
  ! grep -qi 'mksquashfs\|xorriso' "$LOG/doctor-sh.log"
}
assert0 "brb.sh doctor reports the reader tools and no writer-only ones" doctor_is_reader_only

# The staging lock, which is a reader-parity property like any other. Both
# implementations take an exclusive flock on <staging>/.brb.lock — fsx.LockStaging
# on the Go side, lock_staging in brb.sh — and both must refuse while another run
# holds it. The harm they prevent is the quiet kind: two runs writing one image
# path produce a body that still looks like a readable squashfs, and the
# encrypt-and-hash pass, par2 and SHA512SUMS then all agree with each other about
# the mix; the disc says so for the first time at restore, years later. Neither
# shell suite exercised the lock at all until this, so a reader that stopped
# taking it would have gone unnoticed.
#
# Held from flock(1) rather than from a second brb: a real run would have to be
# caught in a window, and the lock is the thing under test either way. The fd is
# held open by a background subshell and released by deleting a file, so nothing
# has to be killed — a killed flock(1) can leave its child holding the fd, and a
# staging tree left locked would fail every check after this one.
if (( HAVE_FLOCK )); then
  hold_staging_lock() { # hold_staging_lock STAGING
    printf 'held\n' > "$T/lock-held"
    # stdout closed too: assert0 reads its command's output through a pipe, and
    # a background child holding that pipe open would hang the whole suite.
    ( flock -x 9 || exit 1
      while [[ -e "$T/lock-held" ]]; do sleep 0.05; done ) 9>"$1/.brb.lock" >/dev/null 2>&1 &
    LOCK_HOLDER=$!
    for _ in $(seq 1 200); do
      flock -n -x "$1/.brb.lock" -c true 2>/dev/null || return 0   # refused: it is held
      sleep 0.05
    done
    echo "the stand-in never took the lock; the check would prove nothing" >&2
    return 1
  }
  drop_staging_lock() { rm -f "$T/lock-held"; wait "$LOCK_HOLDER" 2>/dev/null; return 0; }

  busy_staging_refused() { # busy_staging_refused sh|go
    local who=$1 rc=0 n
    local dest=$T/out-busy-$who lf=$LOG/busy-$who.log
    rm -rf "$dest"; mkdir -p "$dest"
    hold_staging_lock "$T/stage-go" || { drop_staging_lock; return 1; }
    case $who in
      sh) bash "$BRB_SH" --yes -c "$T/cfg/go" restore "$dest" > "$lf" 2>&1 || rc=$? ;;
      go) "$BRB_GO" --yes --no-color -c "$T/cfg/go" restore "$dest" > "$lf" 2>&1 || rc=$? ;;
    esac
    drop_staging_lock
    (( rc != 0 )) || { echo "restored from a staging directory another brb holds" >&2; return 1; }
    grep -qi 'another brb is using' "$lf" \
      || { echo "refused, but not for the lock: $(_tail "$(cat "$lf")")" >&2; return 1; }
    n=$(find "$dest" -type f | wc -l)
    (( n == 0 )) || { echo "wrote $n file(s) despite refusing" >&2; return 1; }
  }
  assert0 "brb.sh refuses a staging directory another brb is using" busy_staging_refused sh
  assert0 "go brb refuses a staging directory another brb is using" busy_staging_refused go

  # A symlink left at the lock's name. bash has no O_NOFOLLOW to give a
  # redirection, so `exec {fd}>` followed the link and truncated whatever it
  # pointed at, with the run's privileges — under the sudo restore the README
  # recommends, any file on the machine. fsx.LockStaging opens the same path
  # O_NOFOLLOW and has always refused it; brb.sh now tests the name first.
  #
  # The assertion that matters is that the TARGET SURVIVES, not that the run
  # failed. A reader that truncated the file and then failed for some later
  # reason would satisfy an exit code with the operator's file already gone.
  # Its CONTENT is compared, not its size: brb.sh opens the lock without
  # truncating now and writes its pid once it holds it, so a link it followed
  # would leave the target holding a pid — not empty, and still destroyed.
  symlinked_lock_refused() { # symlinked_lock_refused sh|go
    local who=$1 rc=0
    local st=$T/lock-link-$who victim=$T/lock-victim-$who lf=$LOG/locklink-$who.log
    local cfg=$T/cfg/locklink-$who
    rm -rf "$st"; mkdir -p "$st"; chmod 700 "$st"
    printf 'the operator file, which is none of this run\047s business\n' > "$victim"
    ln -s "$victim" "$st/.brb.lock"
    mkcfg "$cfg" "$st" "$SRC"
    case $who in
      sh) bash "$BRB_SH" --yes -c "$cfg" index > "$lf" 2>&1 || rc=$? ;;
      go) "$BRB_GO" --yes --no-color -c "$cfg" index > "$lf" 2>&1 || rc=$? ;;
    esac
    (( rc != 0 )) || { echo "accepted a symlinked staging lock" >&2; return 1; }
    grep -qi 'symlink' "$lf" \
      || { echo "refused, but not for the link: $(_tail "$(cat "$lf")")" >&2; return 1; }
    grep -Fqx "the operator file, which is none of this run's business" "$victim" \
      || { echo "the link target was rewritten: '$(_tail "$(cat "$victim")")'" >&2; return 1; }
  }
  assert0 "brb.sh refuses a symlinked staging lock, and leaves its target whole" symlinked_lock_refused sh
  assert0 "go brb refuses a symlinked staging lock, and leaves its target whole" symlinked_lock_refused go
else
  # brb.sh's lock is opportunistic for the same reason this check is gated:
  # flock(1) is util-linux, and a machine without it still has to be able to
  # restore. Both readers then run unguarded, so there is nothing to assert.
  skip "the staging lock (both readers)" "no flock(1) to stand in for a run in progress"
fi

# ---------------------------------------------------------------------------
head_s "7a. brb mount (both readers): the mount point, and what mount(8) is handed"
# ---------------------------------------------------------------------------
# mount was the one reader command with no check in this file at all, because
# a real mount needs root. Two things about it can be held to account without
# one, and both are reader parity in the ordinary sense: given the same
# arguments, the two must refuse the same paths, for the same reason, and hand
# mount(8) the same list.
#
# First, a mount point that is itself a symlink. mount(8) run as root resolves
# the path and mounts on the link's target, so a link left where root is about
# to mount puts the image over whatever it names. Both readers refuse it, and
# do so BEFORE their root check, which is what lets this run unprivileged. Every
# spelling below is one the kernel resolves THROUGH the link unless the reader
# strips it first; "link/." and "link/./" are the two that a strip of trailing
# slashes alone lets past, so brb.sh would have mounted over the target where
# Go refused. The staging tree is empty on purpose: a reader that lost the
# guard and was run as root would stop at "no image for disc 1" rather than
# mount anything, and the check would still fail on the missing refusal.
mkdir -p "$T/mntlink/victim"
ln -s victim "$T/mntlink/browse"
mkdir -p "$T/mntlink-st"; chmod 700 "$T/mntlink-st"
mkcfg "$T/cfg/mntlink" "$T/mntlink-st" "$SRC"
symlinked_mount_point_refused() { # symlinked_mount_point_refused sh|go SPELLING
  local who=$1 mp=$T/mntlink/browse$2 rc=0 n
  local lf=$LOG/mntlink-$who.log
  case $who in
    sh) bash "$BRB_SH" --yes -c "$T/cfg/mntlink" mount 1 "$mp" > "$lf" 2>&1 || rc=$? ;;
    go) "$BRB_GO" --yes --no-color -c "$T/cfg/mntlink" mount 1 "$mp" > "$lf" 2>&1 || rc=$? ;;
  esac
  (( rc != 0 )) || { echo "accepted a symlinked mount point" >&2; return 1; }
  grep -qi 'is a symlink' "$lf" \
    || { echo "refused, but not for the link: $(_tail "$(cat "$lf")")" >&2; return 1; }
  n=$(find "$T/mntlink/victim" -mindepth 1 | wc -l)
  (( n == 0 )) || { echo "wrote $n entr(ies) into the link's target" >&2; return 1; }
}
for sp in '' '/' '//' '/.' '/./'; do
  assert0 "brb.sh refuses a symlinked mount point spelled 'browse$sp'" symlinked_mount_point_refused sh "$sp"
  assert0 "go brb refuses a symlinked mount point spelled 'browse$sp'" symlinked_mount_point_refused go "$sp"
done

# The order of the refusals is parity too. cmd_mount checks the mount point,
# then root, and only then touches staging; Go's Mount used to take the
# staging lock first, which creates and secures STAGING. So under a STAGING
# that was itself a link Go named the staging link and brb.sh the mount point,
# and a STAGING that did not exist yet was created by Go for a run it went on
# to refuse. Both must name the mount point under either, and leave no
# staging tree behind.
mkdir -p "$T/mntorder-real"; chmod 700 "$T/mntorder-real"
ln -s mntorder-real "$T/mntorder-link"
mkcfg "$T/cfg/mntorder-link" "$T/mntorder-link" "$SRC"
mkcfg "$T/cfg/mntorder-fresh" "$T/mntorder-fresh" "$SRC"
mount_point_refused_first() { # mount_point_refused_first sh|go
  local who=$1 rc st n
  local lf=$LOG/mntorder-$who.log
  rm -rf -- "$T/mntorder-fresh"
  for st in link fresh; do
    rc=0
    case $who in
      sh) bash "$BRB_SH" --yes -c "$T/cfg/mntorder-$st" mount 1 "$T/mntlink/browse" > "$lf" 2>&1 || rc=$? ;;
      go) "$BRB_GO" --yes --no-color -c "$T/cfg/mntorder-$st" mount 1 "$T/mntlink/browse" > "$lf" 2>&1 || rc=$? ;;
    esac
    (( rc != 0 )) || { echo "accepted a symlinked mount point (STAGING: $st)" >&2; return 1; }
    grep -qiE 'mount point .* is a symlink' "$lf" \
      || { echo "STAGING $st: refused, but not for the mount point: $(_tail "$(cat "$lf")")" >&2; return 1; }
  done
  [[ ! -e "$T/mntorder-fresh" ]] || { echo "created STAGING for a run it refused" >&2; return 1; }
  n=$(find "$T/mntorder-real" -mindepth 1 | wc -l)
  (( n == 0 )) || { echo "wrote $n entr(ies) through the staging link" >&2; return 1; }
}
assert0 "brb.sh refuses a symlinked mount point before it touches STAGING" mount_point_refused_first sh
assert0 "go brb refuses a symlinked mount point before it touches STAGING" mount_point_refused_first go

# A trailing newline is part of a mount point's name, and both readers keep
# it. brb.sh's mp="$(dir_path ...)" dropped it, as $(...) drops every trailing
# newline: "browse<newline>", which does not exist, was tested as "browse", the
# link beside it, and refused where Go went on; and "browse/<newline>" came
# back as "browse/", which [[ -L ]] answers through the link, so as root it was
# mounted over the link's target. The first is the one that shows without
# root, and both readers have to get past the symlink check with it and stop
# at root — or, run as root, at the empty staging tree.
newline_mount_point_kept() { # newline_mount_point_kept sh|go
  local who=$1 mp=$T/mntlink/browse$'\n' rc=0 n
  local lf=$LOG/mntnl-$who.log
  case $who in
    sh) bash "$BRB_SH" --yes -c "$T/cfg/mntlink" mount 1 "$mp" > "$lf" 2>&1 || rc=$? ;;
    go) "$BRB_GO" --yes --no-color -c "$T/cfg/mntlink" mount 1 "$mp" > "$lf" 2>&1 || rc=$? ;;
  esac
  (( rc != 0 )) || { echo "exited 0 with no image to mount" >&2; return 1; }
  if grep -qi 'is a symlink' "$lf"; then
    echo "took 'browse<newline>' for the link 'browse': $(_tail "$(cat "$lf")")" >&2; return 1
  fi
  grep -qiE 'requires root|no image for disc 1' "$lf" \
    || { echo "refused, but not at root or the missing image: $(_tail "$(cat "$lf")")" >&2; return 1; }
  n=$(find "$T/mntlink/victim" -mindepth 1 | wc -l)
  (( n == 0 )) || { echo "wrote $n entr(ies) into the link's target" >&2; return 1; }
}
assert0 "brb.sh keeps a mount point's trailing newline" newline_mount_point_kept sh
assert0 "go brb keeps a mount point's trailing newline" newline_mount_point_kept go

# Second, the mount itself, run for real as far as mount(8) and no further: a
# stand-in mount first on PATH records its arguments and exits 0, and both
# readers are run as uid 0 of a user namespace, which gets them past their root
# checks without being root anywhere that matters (and a real mount(8) could
# not mount on the host from in there anyway). The list must be exactly
#
#   -o loop,ro,nosuid,nodev -- <decrypted image> <mount point>
#
# nosuid,nodev because ro implies neither, and the format keeps setuid bits and
# device nodes: a setuid-root binary from a backup of / acted on the host for
# any account that could reach a mount made with loop,ro alone. "--" because
# brb.sh used to leave it out and read a mount point with a leading dash as
# options. The mount point is the one given with its trailing slashes gone,
# byte for byte the same from both — Go's dirPath is brb.sh's dir_path, not
# filepath.Clean.
#
# And the directories made on the way must be 0700 from both. brb.sh's mkdir -p
# runs under the umask 077 its secure_staging sets; the Go build used to make
# them 0755, which on a shared machine decides whether another account can
# reach the decrypted tree at all. Run under umask 022, where the two modes
# differ, or the check proves nothing.
#
# The decrypted image is compared by realpath: Go joins the staging path with
# filepath.Join and brb.sh with a plain "/", so a TMPDIR with a trailing slash
# spells the same file two ways, and that is not what is under test here.
#
# HAVE_USERNS is an optional gate like HAVE_SCRIPT and the rest: without root
# or a user namespace this check skips and the suite still exits 0. So CI
# checks for a working unshare -r by name before it runs this file, and turns
# off the AppArmor policy that refuses it on Ubuntu's runners.
HAVE_USERNS=0
if (( EUID == 0 )) || unshare -r true >/dev/null 2>&1; then HAVE_USERNS=1; fi
if (( HAVE_USERNS )); then
  mkdir -p "$T/mntstub"
  # Each argument on its own line, and nothing else: none of the arguments in
  # this check holds a newline.
  # shellcheck disable=SC2016  # the $-expressions are for the stand-in to expand
  printf '%s\n' '#!/bin/sh' 'printf "%s\n" "$@" > "$BRB_MOUNT_ARGV"' > "$T/mntstub/mount"
  chmod 755 "$T/mntstub/mount"
  as_uid0() { # as_uid0 CMD...   -- uid 0, in a user namespace unless already root
    if (( EUID == 0 )); then "$@"; else unshare -r "$@"; fi
  }
  mount_args_match() { # mount_args_match sh|go
    local who=$1 rc=0 line d m
    local st=$T/mntrun-st-$who base=$T/mntrun-$who lf=$LOG/mntrun-$who.log
    local argv=$T/mntrun-argv-$who cfg=$T/cfg/mntrun-$who
    local -a got=()
    rm -rf "$st" "$base" "$argv"; mkdir -p "$st" "$base"; chmod 700 "$st"
    cp -a "$T/stage-go/enc" "$st/enc"
    mkcfg "$cfg" "$st" "$SRC"
    case $who in
      sh) ( umask 022; as_uid0 env PATH="$T/mntstub:$PATH" BRB_MOUNT_ARGV="$argv" \
              bash "$BRB_SH" --yes -c "$cfg" mount 1 "$base/new/leaf//" ) > "$lf" 2>&1 || rc=$? ;;
      go) ( umask 022; as_uid0 env PATH="$T/mntstub:$PATH" BRB_MOUNT_ARGV="$argv" \
              "$BRB_GO" --yes --no-color -c "$cfg" mount 1 "$base/new/leaf//" ) > "$lf" 2>&1 || rc=$? ;;
    esac
    (( rc == 0 )) || { echo "mount exited $rc: $(_tail "$(cat "$lf")")" >&2; return 1; }
    [[ -f "$argv" ]] || { echo "mount(8) was never run" >&2; return 1; }
    while IFS= read -r line; do got+=("$line"); done < "$argv"
    (( ${#got[@]} == 5 )) \
      || { echo "mount(8) got ${#got[@]} argument(s): $(tr '\n' ' ' < "$argv")" >&2; return 1; }
    [[ "${got[0]}" == -o && "${got[1]}" == loop,ro,nosuid,nodev && "${got[2]}" == -- ]] \
      || { echo "mount(8) was given ${got[0]} ${got[1]} ${got[2]}, want -o loop,ro,nosuid,nodev --" >&2; return 1; }
    [[ "$(realpath -- "${got[3]}")" == "$(realpath -- "$st/restore/disc01.squashfs")" ]] \
      || { echo "mount(8) was handed ${got[3]} as the image" >&2; return 1; }
    [[ "${got[4]}" == "$base/new/leaf" ]] \
      || { echo "mount(8) was handed ${got[4]} as the mount point, want $base/new/leaf" >&2; return 1; }
    for d in "$base/new" "$base/new/leaf"; do
      m=$(stat -c %a -- "$d") || return 1
      [[ "$m" == 700 ]] || { echo "$d was created $m, want 700" >&2; return 1; }
    done
  }
  assert0 "brb.sh hands mount(8) -o loop,ro,nosuid,nodev -- IMAGE MOUNTPOINT, and makes it 0700" mount_args_match sh
  assert0 "go brb hands mount(8) -o loop,ro,nosuid,nodev -- IMAGE MOUNTPOINT, and makes it 0700" mount_args_match go
else
  skip "what mount(8) is handed (both readers)" "not root, and unshare -r cannot make a user namespace here"
  # Some hosts refuse unprivileged user namespaces (an AppArmor policy is the
  # usual reason; CI turns it off, and fails rather than arrive here), and the
  # options are the half of this that matters most, so they are at least read
  # out of the script rather than left unchecked. This is weaker than running
  # it — it pins the spelling of one line — and the Go half is pinned by
  # TestMountHandsMountTheHardenedArguments either way.
  mount_line_static() {
    # shellcheck disable=SC2016  # $img and $mp are the script's text, matched literally
    sed -n '/^cmd_mount() {/,/^}/p' "$BRB_SH" \
      | grep -Eq '^[[:space:]]*mount -o loop,ro,nosuid,nodev -- "\$img" "\$mp"'
  }
  assert0 "brb.sh's cmd_mount passes -o loop,ro,nosuid,nodev -- (read from the script, not run)" mount_line_static
fi

# ---------------------------------------------------------------------------
head_s "7b. configuration loading, the same on both readers"
# ---------------------------------------------------------------------------
# Section 7 asserts what the two readers do once they are running; this
# asserts what they start FROM. They read one config file and one environment,
# and every check here is a case where they used to come away with different
# settings — the quiet kind of difference, because each reader on its own
# looks perfectly sane. Each was found by running both on the same input.
#
# The refusal checks export STAGING into an empty scratch directory, so a
# reader that regressed into running on its defaults fails there rather than
# going on to touch the machine's own /var/tmp/brb. The list checks cannot:
# finding the reference set's staging tree through the config or the
# environment under test is their point, so they take STAGING from there, and
# a reader that fell back onto its defaults in one of them runs `list 1`
# against /var/tmp/brb — creating enc/, restore/ and the lock there if they
# are missing — before it fails for want of an image. The defaults check runs
# only doctor, which writes nothing to staging. Runs start in an empty working
# directory, so nothing relative can be found by accident, and under a
# timeout, because one of these used to be a hang.
CL=$T/cfgload
mkdir -p "$CL/cwd" "$CL/home/.config/brb" "$CL/stage" "$CL/cfgdir"
cp "$ID" "$CL/home/.config/brb/identity.txt"
mkfifo "$CL/fifo"
ln -s "$T/cfg/go" "$CL/link-to-cfg"
cfgload_n=0
# The readers are run from $CL/cwd, so a relative path given on the command
# line has to be made absolute first.
cfgload_abs() { case $1 in /*) printf '%s' "$1" ;; *) printf '%s/%s' "$PWD" "$1" ;; esac; }
CL_SH=$(cfgload_abs "$BRB_SH")
CL_GO=$(cfgload_abs "$BRB_GO")

# cfgload_run sh|go LOGFILE [VAR=value ...] -- ARGS...
cfgload_run() {
  local who=$1 lf=$2 rc=0; shift 2
  local -a envs=()
  while (( $# )) && [[ $1 != -- ]]; do envs+=("$1"); shift; done
  shift
  case $who in
    sh) ( cd "$CL/cwd" && env ${envs[@]+"${envs[@]}"} timeout -k 5 60 bash "$CL_SH" --yes "$@" ) > "$lf" 2>&1 || rc=$? ;;
    go) ( cd "$CL/cwd" && env ${envs[@]+"${envs[@]}"} timeout -k 5 60 "$CL_GO" --yes --no-color "$@" ) > "$lf" 2>&1 || rc=$? ;;
  esac
  return $rc
}
cfgload_name() { case $1 in sh) printf 'brb.sh' ;; go) printf 'go brb' ;; esac; }

# cfgload_refused sh|go WANT [VAR=value ...] -- ARGS...  — the reader refuses,
# and says WANT. The words are checked, not just the exit: a reader that ran
# on its defaults and then failed for some other reason exits non-zero too.
cfgload_refused() {
  local who=$1 want=$2 lf; shift 2
  cfgload_n=$((cfgload_n+1)); lf=$LOG/cfgload-$cfgload_n-$who.log
  if cfgload_run "$who" "$lf" STAGING="$CL/stage" "$@"; then
    echo "exited 0: $(_tail "$(cat "$lf")")" >&2; return 1
  fi
  grep -qF -- "$want" "$lf" \
    || { echo "refused, but not with '$want': $(_tail "$(cat "$lf")")" >&2; return 1; }
}

# cfgload_lists sh|go [VAR=value ...] -- ARGS...  — the reader lists disc 1 of
# the reference set, which only works if it found that set's staging tree
# and its identity.
cfgload_lists() {
  local who=$1 lf; shift
  cfgload_n=$((cfgload_n+1)); lf=$LOG/cfgload-$cfgload_n-$who.log
  cfgload_run "$who" "$lf" "$@" \
    || { echo "exit non-zero: $(_tail "$(cat "$lf")")" >&2; return 1; }
  grep -q 'big\.bin' "$lf" \
    || { echo "the listing does not mention big.bin: $(_tail "$(cat "$lf")")" >&2; return 1; }
}

# cfgload_defaults_shown sh|go — doctor, on a config whose STAGING= and BURNER=
# lines are empty, reports the built-in defaults for both. Exit status is not
# asserted: doctor's verdict depends on what else this machine has.
cfgload_defaults_shown() {
  local who=$1 lf
  cfgload_n=$((cfgload_n+1)); lf=$LOG/cfgload-$cfgload_n-$who.log
  cfgload_run "$who" "$lf" HOME="$CL/home" -- -c "$CL/empty.cfg" doctor || true
  grep -qE 'staging +/var/tmp/brb$' "$lf" \
    || { echo "staging is not the default: $(grep -m1 'staging  ' "$lf")" >&2; return 1; }
  grep -qE 'burner +/dev/sr0( |$)' "$lf" \
    || { echo "burner is not the default: $(grep -m1 'burner  ' "$lf")" >&2; return 1; }
}
printf 'STAGING=\nBURNER=\nAGE_RECIPIENTS_FILE=\nAGE_IDENTITY=\n' > "$CL/empty.cfg"
printf 'STAGING="%s"\nAGE_RECIPIENTS_FILE=\nAGE_IDENTITY=\n' "$T/stage-go" > "$CL/empty-id.cfg"

for who in sh go; do
  name=$(cfgload_name "$who")
  # A config that is not a regular file. brb.sh's load_config tested -f, so it
  # said "not found" under -c and "no config file" under BRB_CONFIG — and ran
  # on its defaults — while the Go build read /dev/null, a device or a pipe as
  # a config, and blocked in read(2) on a FIFO, past the first Ctrl-C. Both
  # now refuse, in the same words.
  assert0 "$name refuses -c /dev/null as not a regular file" \
    cfgload_refused "$who" "is not a regular file" -- -c /dev/null doctor
  assert0 "$name refuses a FIFO named by -c at once, without reading it" \
    cfgload_refused "$who" "is not a regular file" -- -c "$CL/fifo" doctor
  assert0 "$name refuses BRB_CONFIG=/dev/null rather than using its defaults" \
    cfgload_refused "$who" "is not a regular file" BRB_CONFIG=/dev/null -- doctor
  assert0 "$name refuses a directory named by BRB_CONFIG" \
    cfgload_refused "$who" "is a directory" BRB_CONFIG="$CL/cfgdir" -- doctor
  # The companion that keeps the refusals honest: a symlink to a real config
  # is a config to both, since -f and os.Stat both follow it.
  assert0 "$name reads a config through a symlink to it" \
    cfgload_lists "$who" -- -c "$CL/link-to-cfg" list 1

  # KEY= with nothing after it means "not set" to the Go build, which keeps the
  # default; brb.sh applied its defaults before sourcing the file, so the same
  # line overwrote them with nothing — an empty staging directory, no burner,
  # and an identity looked for in the working directory instead of beside
  # ~/.config/brb/recipients.txt. load_config applies them again afterwards.
  assert0 "$name keeps the default STAGING and BURNER for empty STAGING= and BURNER= lines" \
    cfgload_defaults_shown "$who"
  assert0 "$name finds the identity beside the default recipients file when the config empties both keys" \
    cfgload_lists "$who" HOME="$CL/home" -- -c "$CL/empty-id.cfg" list 1

  # A literal ~ or $HOME in an exported value — a systemd Environment= line,
  # an env file — was expanded by the Go build and taken as written by brb.sh,
  # so STAGING='~/stage-go' was two different directories. HOME is pointed at
  # $T so that the expansion lands on the reference set's staging tree, and
  # the working directory is empty, so the literal reading finds nothing.
  # shellcheck disable=SC2016,SC2088  # the markers are meant literally: that is the case under test
  for spelling in '$HOME/stage-go' '${HOME}/stage-go' '~/stage-go'; do
    assert0 "$name expands an exported STAGING=$spelling written literally" \
      cfgload_lists "$who" HOME="$T" STAGING="$spelling" -- -c "$T/cfg/go" list 1
  done
done

if (( HAVE_FLOCK )); then
  # The staging lock's pid. LockStaging writes its pid into .brb.lock and a
  # refused Go run reads it back to name the holder. brb.sh opened the file
  # with `>`, which truncates BEFORE flock says whether the lock is free, so a
  # brb.sh that was about to be refused had already erased a running Go
  # backup's pid. It opens with `<>` now and writes its own pid only once the
  # lock is its own. The holder here is flock(1) over a file that already
  # names pid 424242, opened without truncating it.
  cfgload_pid_kept() {
    local st=$CL/lock cfg=$CL/lock.cfg lsh=$LOG/cfgload-lock-sh.log lgo=$LOG/cfgload-lock-go.log
    local holder held ran=0
    mkdir -p "$st"; chmod 700 "$st"
    mkcfg "$cfg" "$st" "$SRC"
    printf '424242\n' > "$st/.brb.lock"
    printf 'held\n' > "$CL/lock-held"
    # stdout closed for the reason section 7's stand-in gives.
    ( flock -x 9 || exit 1
      while [[ -e "$CL/lock-held" ]]; do sleep 0.05; done ) 9<>"$st/.brb.lock" >/dev/null 2>&1 &
    holder=$!
    for _ in $(seq 1 200); do
      flock -n -x "$st/.brb.lock" -c true 2>/dev/null || break
      sleep 0.05
    done
    if flock -n -x "$st/.brb.lock" -c true 2>/dev/null; then
      rm -f "$CL/lock-held"; wait "$holder" 2>/dev/null
      echo "the stand-in never took the lock; the check would prove nothing" >&2; return 1
    fi
    bash "$BRB_SH" --yes -c "$cfg" index > "$lsh" 2>&1 && ran=1
    held=$(cat "$st/.brb.lock")
    "$BRB_GO" --yes --no-color -c "$cfg" index > "$lgo" 2>&1 && ran=1
    rm -f "$CL/lock-held"; wait "$holder" 2>/dev/null
    (( ran == 0 )) || { echo "a reader ran in a staging directory another brb holds" >&2; return 1; }
    grep -qi 'another brb is using' "$lsh" \
      || { echo "brb.sh refused, but not for the lock: $(_tail "$(cat "$lsh")")" >&2; return 1; }
    [[ $held == 424242 ]] \
      || { echo "brb.sh's refused attempt left the lock holding '$held', not the holder's pid" >&2; return 1; }
    if ! grep -qF 'another brb is using' "$lgo" || ! grep -qF '(pid 424242)' "$lgo"; then
      echo "go brb's refusal does not name the holder: $(_tail "$(cat "$lgo")")" >&2; return 1
    fi
  }
  assert0 "a refused brb.sh leaves the holder's pid in the lock, and go brb names it" cfgload_pid_kept

  # And once brb.sh does hold the lock, a pid left by a run that has ended is
  # replaced with its own, as LockStaging does, so a Go contender names the
  # run that is really there. index fails afterwards — there is no index in
  # this staging tree — but the lock is taken before that.
  cfgload_pid_replaced() {
    local st=$CL/lock2 cfg=$CL/lock2.cfg lf=$LOG/cfgload-lock2-sh.log v
    mkdir -p "$st"; chmod 700 "$st"
    mkcfg "$cfg" "$st" "$SRC"
    printf '424242\n' > "$st/.brb.lock"
    bash "$BRB_SH" --yes -c "$cfg" index > "$lf" 2>&1
    if grep -qi 'another brb is using' "$lf"; then
      echo "refused a lock nobody holds: $(_tail "$(cat "$lf")")" >&2; return 1
    fi
    v=$(cat "$st/.brb.lock")
    [[ $v =~ ^[1-9][0-9]*$ && $v != 424242 ]] \
      || { echo "the lock holds '$v' after brb.sh held it; want that run's pid" >&2; return 1; }
  }
  assert0 "brb.sh replaces a stale pid in the lock with its own once it holds it" cfgload_pid_replaced
else
  skip "the staging lock's pid (both readers)" "no flock(1) to stand in for a run in progress"
fi

# ---------------------------------------------------------------------------
head_s "8. multi-disc sets and partial restores"
# ---------------------------------------------------------------------------
mkdir -p "$T/m-src"
for i in $(seq -w 1 12); do head -c 3000000 /dev/urandom > "$T/m-src/blob-$i.bin"; done
mkdir -p "$T/m-go"; mkcfg "$T/cfg/m" "$T/m-go" "$T/m-src" 'DISC_CAPACITY_BYTES=40000000' 'RESERVE_BYTES=12000000'
assert0 "go brb builds a multi-disc set" run_go "$LOG/backup-m.log" "$T/cfg/m" backup
n_m=$(find "$T/m-go/discs" -mindepth 1 -maxdepth 1 -type d -name 'disc*' 2>/dev/null | wc -l)
multi_enough() { (( n_m >= 2 )); }
assert0 "the multi-disc set spans $n_m discs" multi_enough

if (( n_m >= 2 )); then
  # shellcheck disable=SC2016  # as above: $0..$3 belong to the inner sh -c.
  assert0 "brb.sh restores the whole multi-disc set" \
    sh -c 'rm -rf "$1"; mkdir -p "$1"; bash "$0" --yes -c "$2" restore "$1" >"$3" 2>&1' \
    "$BRB_SH" "$T/out-m-sh" "$T/cfg/m" "$LOG/restore-m-sh.log"
  assert0 "  ... byte-identical to the source" diff -r --no-dereference "$T/m-src" "$T/out-m-sh"

  # Hide the last disc's image: both readers must say the set is incomplete.
  partial_announced() { # partial_announced sh|go
    local who=$1
    local st=$T/part-$who dest=$T/out-part-$who cfg=$T/cfg/part-$who out img
    rm -rf "$st" "$dest"; mkdir -p "$dest"
    cp -a "$T/m-go" "$st"; rm -rf "$st/restore"
    # Asserted on both sides of the rm, because "the file is not there" is what
    # a sabotage that never ran looks like too. That is not hypothetical: n_m
    # used to count one disc too many, so this deleted disc03 of a two-disc set,
    # the set stayed complete, and the check below passed on the word "present"
    # in the SUCCESS message.
    img="$st/enc/$(printf 'disc%02d' "$n_m").squashfs.age"
    [[ -e $img ]] || { echo "fixture: $(basename "$img") is not in staging to begin with" >&2; return 1; }
    rm -f "$st/enc/$(printf 'disc%02d' "$n_m")".*
    [[ ! -e $img ]] || { echo "sabotage missed: $(basename "$img") is still in staging" >&2; return 1; }
    mkcfg "$cfg" "$st" "$T/m-src"
    case $who in
      sh) out=$(bash "$BRB_SH" --yes -c "$cfg" restore "$dest" 2>&1) ;;
      go) out=$("$BRB_GO" --yes --no-color -c "$cfg" restore "$dest" 2>&1) ;;
    esac
    printf '%s\n' "$out" > "$LOG/part-$who.log"
    # 'present' used to be one of the alternatives, which is the vocabulary of
    # SUCCESS: brb.sh says "all N disc image(s) present" on a COMPLETE set, so
    # the pattern matched whether or not the set was short a disc. Only the
    # failure words count. Both readers print "MISSING:".
    grep -qiE 'missing|incomplete' <<<"$out"
  }
  assert0 "brb.sh announces that it is restoring an incomplete set" partial_announced sh
  assert0 "go brb announces that it is restoring an incomplete set" partial_announced go
fi

# ---------------------------------------------------------------------------
head_s "8a. brb.sh restore, index and --only: the paths the rescue reader takes alone"
# ---------------------------------------------------------------------------
# Every case here is one where brb.sh did something the Go reader does not —
# bar the --only spellings, where each reader was wrong about a different one —
# on a path the sections above walk past: which staged images a whole-set
# restore picks and counts, and what it reads as a disc number, what the last
# line of a partial restore says, how --only is spelled, what happens when the
# index is absent, rotted, contradicted by its own hash or holding rows that do
# not parse, what a listing that is long or that fails does to --only, what a
# long non-fatal unsquashfs log does to the run, what --disc does alongside
# --only, which image --only narrows to, and what 'index' takes its pattern to
# mean. Each is asserted on both readers wherever the Go reader has the same
# command, because the claim being tested is that they agree. The fixtures live
# under $T/rsc, and every name this section defines starts rsc_ or RSC_, so
# nothing here can collide with the sections around it.
#
# Three cases put a wrapper in front of a real tool on PATH, because the
# conditions they test — a listing that fails, an extraction that finishes with
# a long non-fatal log, and a hash that cannot be taken — are not ones a
# well-formed set produces on demand. The wrappers change nothing else: every
# call they do not target is handed straight to the real tool.
RSC=$T/rsc
mkdir -p "$RSC"
RSC_REAL_UNSQ=$(command -v unsquashfs)

# rsc_run sh|go LOGFILE CFG args... — one reader, everything it prints in
# LOGFILE, its exit status left for the caller to judge. A caller that puts a
# wrapper in front of unsquashfs says so with a local RSC_PATH, which this hands
# to the reader alone, so the suite's own PATH is never touched.
rsc_run() {
  local who=$1 lf=$2 cfg=$3; shift 3
  case $who in
    sh) env PATH="${RSC_PATH:-$PATH}" bash "$BRB_SH" --yes -c "$cfg" "$@" > "$lf" 2>&1 ;;
    go) env PATH="${RSC_PATH:-$PATH}" "$BRB_GO" --yes --no-color -c "$cfg" "$@" > "$lf" 2>&1 ;;
  esac
}
rsc_name() { case $1 in sh) echo "brb.sh" ;; go) echo "go brb" ;; esac; }
# rsc_copy FROM NAME SOURCE [extra config...] — a private staging tree holding
# FROM's encrypted images and manifest, which is all a restore reads, and a
# config for it at $RSC/NAME.cfg. Nothing else is copied: the disc directories
# a backup leaves beside them would double the cost of every copy here.
rsc_copy() {
  local from=$1 name=$2 src=$3; shift 3
  local st=$RSC/$name
  rm -rf "$st"; mkdir -p "$st" || return 1
  cp -a "$from/enc" "$st/enc" || return 1
  if [[ -f "$from/MANIFEST.txt" ]]; then cp -a "$from/MANIFEST.txt" "$st/" || return 1; fi
  mkcfg "$RSC/$name.cfg" "$st" "$src" "$@"
}
# rsc_rot FILE — zero the last 16 bytes, which are the authentication tag of the
# last age chunk, so the file still looks like age and no longer decrypts. The
# chance that the tag was all zeros already is not worth a thought.
rsc_rot() {
  local f=$1 sz
  sz=$(wc -c < "$f")
  { head -c $(( sz - 16 )) "$f"; head -c 16 /dev/zero; } > "$f.rot" && mv -f "$f.rot" "$f"
}

# ---- which images a whole-set restore picks, and how many it counts --------
# A whole-set restore used to take every name matching disc*.squashfs.age. A
# forged image named discx.squashfs.age — no disc number in it at all — was
# therefore picked, its index cross-check "degraded" because the name did not
# say which disc it was, and its tree extracted into the destination with
# nothing asked: a forger holding one disc needed only that name, the set's
# public key and the genuine index copied across. The Go reader picks only
# names with a disc number in them, and so now does brb.sh. The foreign image
# here is the reference set's disc 1, which decrypts with the same identity, so
# nothing but its name can keep it out.
rsc_unnumbered_fixture() {
  rsc_copy "$T/m-go" unnumbered "$T/m-src" 'DISC_CAPACITY_BYTES=40000000' 'RESERVE_BYTES=12000000' || return 1
  cp -a "$T/stage-go/enc/disc01.squashfs.age" "$RSC/unnumbered/enc/discx.squashfs.age"
}
rsc_unnumbered_ignored() { # rsc_unnumbered_ignored sh|go
  local who=$1 dest=$RSC/out-unnumbered-$1 rc=0
  rm -rf "$dest" "$RSC/unnumbered/restore"; mkdir -p "$dest"
  rsc_run "$who" "$LOG/rsc-unnumbered-$who.log" "$RSC/unnumbered.cfg" restore "$dest" || rc=$?
  (( rc == 0 )) || { echo "exited $rc: $(_tail "$(cat "$LOG/rsc-unnumbered-$who.log")")" >&2; return 1; }
  if [[ -e "$dest/big.bin" || -e "$dest/project" ]]; then
    echo "the image staged as discx.squashfs.age was extracted into the destination" >&2; return 1
  fi
  diff -r --no-dereference "$T/m-src" "$dest" >&2
}

# The completeness count had the same hole from the other side: it counted
# names, so disc001 beside disc01, a disc03 in a two-disc set, a discx, or even
# a directory with an image's name made a set missing its disc 2 read "all 2
# disc image(s) present" — and the warning, and the prompt before a partial
# restore, never came. The Go reader counts distinct disc numbers in range.
# Asserted through brb.sh's doctor, as section 18 asserts the manifest's own
# number: the Go build's doctor is a writer's, and its count is pinned by
# TestCheckComplete. A trailing slash on a name here stages a directory.
#
# Every case also requires that doctor printed none of bash's own arithmetic
# errors, which all carry "error token": that is what a name or a manifest
# field that slipped past a digit test used to produce, while the count came
# out right only because the arithmetic failing happened to mean "skip it".
rsc_doctor_says() { # rsc_doctor_says CLAIMED EXPECTED-RX NAME...
  local claimed=$1 rx=$2 st=$RSC/doc out f; shift 2
  rm -rf "$st"; mkdir -p "$st/enc"
  for f in "$@"; do
    case $f in
      */) mkdir -p "$st/enc/${f%/}" ;;
      *)  : > "$st/enc/$f" ;;
    esac
  done
  printf 'discs           : %s\n' "$claimed" > "$st/MANIFEST.txt"
  mkcfg "$T/cfg/rsc-doc" "$st" "$SRC"
  out=$(LC_ALL=${RSC_DOC_LC:-${LC_ALL:-}} bash "$BRB_SH" --yes -c "$T/cfg/rsc-doc" doctor 2>&1) || true
  grep -qE "$rx" <<<"$out" || { grep -E 'MANIFEST|present' <<<"$out" >&2; return 1; }
  ! grep -q 'error token' <<<"$out" || { grep 'error token' <<<"$out" >&2; return 1; }
}
# rsc_doctor_says_in LOCALE CLAIMED EXPECTED-RX NAME... — the same, with doctor
# run under LOCALE.
rsc_doctor_says_in() { local RSC_DOC_LC=$1; shift; rsc_doctor_says "$@"; }

if (( n_m >= 2 )); then
  assert0 "fixture: the multi-disc set with another set's image staged beside it as discx.squashfs.age" \
    rsc_unnumbered_fixture
  for who in sh go; do
    assert0 "$(rsc_name "$who") restores the set and never picks the image whose name carries no disc number" \
      rsc_unnumbered_ignored "$who"
  done
fi
assert0 "brb.sh counts disc001 beside disc01 as one disc, and names disc 2 missing" \
  rsc_doctor_says 2 '2 present\. MISSING: 2$' disc01.squashfs.age disc001.squashfs.age
assert0 "  ... does not count a disc03 towards a two-disc set" \
  rsc_doctor_says 2 '2 present\. MISSING: 2$' disc01.squashfs.age disc03.squashfs.age
assert0 "  ... does not count a name with no disc number at all" \
  rsc_doctor_says 2 '1 present\. MISSING: 2$' disc01.squashfs.age discx.squashfs.age
assert0 "  ... does not count a directory that has an image's name" \
  rsc_doctor_says 2 '1 present\. MISSING: 2$' disc01.squashfs.age disc02.squashfs.age/
assert0 "  ... and counts nothing at all when nothing staged is numbered" \
  rsc_doctor_says 2 '0 present\. MISSING: 1 2$' discx.squashfs.age
assert0 "  ... while a disc staged as disc002 still counts as disc 2" \
  rsc_doctor_says 2 'all 2 disc image\(s\) present' disc01.squashfs.age disc002.squashfs.age
# A disc number is read the way strconv.Atoi reads it, never through bash
# arithmetic, which wraps at 2^64: disc18446744073709551618 used to be disc 2.
assert0 "  ... and does not take disc18446744073709551618 for disc 2, as arithmetic wrapping at 2^64 did" \
  rsc_doctor_says 2 '1 present\. MISSING: 2$' disc01.squashfs.age disc18446744073709551618.squashfs.age
# And as ASCII digits only. Under a UTF-8 locale bash's [0-9] also takes other
# scripts' digits (see disc_number_of in brb.sh), so a fullwidth 2 in a name
# reached arithmetic as a "number", and an Arabic-Indic 3 as the manifest's
# disc count ended doctor — and a restore — on a bash syntax error, where Atoi
# refuses both and the Go reader carries on.
if (( HAVE_UTF8_LOCALE )); then
  assert0 "  ... and under LC_ALL=$UTF8_LOCALE does not read a fullwidth 2 in a name as a disc number" \
    rsc_doctor_says_in "$UTF8_LOCALE" 2 '1 present\. MISSING: 2$' \
      disc01.squashfs.age "$(printf 'disc\357\274\222.squashfs.age')"
  assert0 "  ... nor a manifest's disc count written in Arabic-Indic digits, which it cannot tell" \
    rsc_doctor_says_in "$UTF8_LOCALE" "$(printf '\331\243')" 'cannot tell how many discs' disc01.squashfs.age
else
  skip "brb.sh's disc numbers under a UTF-8 locale" "no en_US.UTF-8 locale on this machine"
fi

# ---- the last line of a partial restore -------------------------------------
# Every disc carries the whole directory skeleton, so a partial restore leaves a
# tree with the right shape and files silently absent, and the MISSING warning
# it opened with has scrolled away by the end of a multi-disc run. brb.sh ended
# a partial restore on the same "ok restore complete" a complete one ends on;
# the Go reader closes it with three PARTIAL warnings and "partial restore
# complete". Section 8 already ran both readers over a set missing its last
# disc, so its logs are read here, beside the complete restore's.
rsc_partial_closing() { # rsc_partial_closing LOGFILE
  grep -q 'this restore was PARTIAL' "$1" || { echo "no PARTIAL warning at the end" >&2; return 1; }
  grep -q 'partial restore complete: ' "$1" || { echo "no 'partial restore complete' line" >&2; return 1; }
  ! grep -q 'ok restore complete: ' "$1" || { echo "ended on the line a complete restore ends on" >&2; return 1; }
}
rsc_complete_closing() { # rsc_complete_closing LOGFILE
  grep -q 'ok restore complete: ' "$1" || { echo "no 'restore complete' line" >&2; return 1; }
  ! grep -q 'PARTIAL' "$1" || { echo "a complete restore was called PARTIAL" >&2; return 1; }
}
# And the two holes together: a set missing its last disc, with disc 1 staged a
# second time as disc001. The name count saw a complete set; the number count
# sees disc $n_m missing, and the last line has to say so.
rsc_dup_fixture() {
  rsc_copy "$T/m-go" dup "$T/m-src" 'DISC_CAPACITY_BYTES=40000000' 'RESERVE_BYTES=12000000' || return 1
  local last; last=$(printf 'disc%02d' "$n_m")
  [[ -e "$RSC/dup/enc/$last.squashfs.age" ]] || { echo "fixture: $last is not in staging to remove" >&2; return 1; }
  rm -f "$RSC/dup/enc/$last".*
  cp -a "$RSC/dup/enc/disc01.squashfs.age" "$RSC/dup/enc/disc001.squashfs.age"
}
rsc_dup_announced() { # rsc_dup_announced sh|go
  local who=$1 dest=$RSC/out-dup-$1 lf=$LOG/rsc-dup-$1.log rc=0
  rm -rf "$dest" "$RSC/dup/restore"; mkdir -p "$dest"
  rsc_run "$who" "$lf" "$RSC/dup.cfg" restore "$dest" || rc=$?
  (( rc == 0 )) || { echo "exited $rc: $(_tail "$(cat "$lf")")" >&2; return 1; }
  grep -q "MISSING: $n_m\$" "$lf" || { echo "disc $n_m was not named missing: $(grep -E 'MANIFEST|present' "$lf")" >&2; return 1; }
  rsc_partial_closing "$lf"
}
if (( n_m >= 2 )); then
  for who in sh go; do
    assert0 "$(rsc_name "$who") ends a partial restore on 'partial restore complete', not the clean line" \
      rsc_partial_closing "$LOG/part-$who.log"
  done
  assert0 "brb.sh still ends a complete restore on the clean line" rsc_complete_closing "$LOG/restore-m-sh.log"
  assert0 "fixture: the multi-disc set missing disc $n_m, with disc 1 staged twice" rsc_dup_fixture
  for who in sh go; do
    assert0 "$(rsc_name "$who") counts disc 1 staged twice as one disc, names disc $n_m missing, and ends PARTIAL" \
      rsc_dup_announced "$who"
  done
fi

# ---- how --only is spelled --------------------------------------------------
# Both readers drop every leading '/' and './' and every trailing '/'. They did
# not agree before: brb.sh stripped one trailing slash and one leading './' and
# never a leading slash, the Go reader trimmed every slash and never './' — so
# './x' restored on one and failed on the other, and '/x' and 'x//' did the
# reverse. A spelling with nothing left names the whole tree and is refused by
# both, where brb.sh used to send it to the index to be "not found".
rsc_spelling_restores() { # rsc_spelling_restores sh|go SPELLING
  local who=$1 sp=$2 dest=$RSC/out-spell-$1 lf=$LOG/rsc-spell-$1.log rc=0
  rm -rf "$dest" "$T/stage-go/restore"; mkdir -p "$dest"
  rsc_run "$who" "$lf" "$T/cfg/go" restore "$dest" --only "$sp" || rc=$?
  (( rc == 0 )) || { echo "exited $rc: $(_tail "$(cat "$lf")")" >&2; return 1; }
  test -s "$dest/project/core/src/main.c" || { echo "project/core/src/main.c was not restored" >&2; return 1; }
  [[ ! -e "$dest/big.bin" ]] || { echo "big.bin was restored too" >&2; return 1; }
}
rsc_root_refused() { # rsc_root_refused sh|go SPELLING
  local who=$1 sp=$2 dest=$RSC/out-root-$1 lf=$LOG/rsc-root-$1.log rc=0 n
  rm -rf "$dest"; mkdir -p "$dest"
  rsc_run "$who" "$lf" "$T/cfg/go" restore "$dest" --only "$sp" || rc=$?
  (( rc != 0 )) || { echo "exited 0" >&2; return 1; }
  grep -q 'whole tree' "$lf" || { echo "refused, but not as the whole tree: $(_tail "$(cat "$lf")")" >&2; return 1; }
  n=$(find "$dest" -type f | wc -l)
  (( n == 0 )) || { echo "wrote $n file(s)" >&2; return 1; }
}
for who in sh go; do
  for sp in './project/core/src/main.c' '/project/core/src/main.c' 'project/core//' './project/core/'; do
    assert0 "$(rsc_name "$who") --only '$sp' restores project/core/src/main.c" rsc_spelling_restores "$who" "$sp"
  done
  for sp in '.' './' '/'; do
    assert0 "$(rsc_name "$who") --only '$sp' is refused as the whole tree" rsc_root_refused "$who" "$sp"
  done
done
unset sp

# ---- the index: absent, rotted, or contradicted by its own hash -------------
# The Go reader checks index.tsv.gz.age against index.tsv.gz.age.sha512 before
# reading it; brb.sh never looked at that sidecar. And where the Go reader
# warns and searches every image when the index is absent or will not read,
# brb.sh refused: "--only needs the encrypted index", or — when one was there
# and would not decrypt — "is not in the index", for a path the index had never
# been able to answer for. 'index' turned the same unreadable index into "no
# match". Three private copies of the reference set: one whose sidecar records
# the wrong hash, one with no index at all, and one whose index has rotted and
# whose sidecar is gone, so that age is the one to notice.
rsc_index_fixtures() {
  local f
  rsc_copy "$T/stage-go" badsum "$SRC" || return 1
  printf '%s  index.tsv.gz.age\n' "$(sha512sum < /dev/null | cut -d' ' -f1)" > "$RSC/badsum/enc/index.tsv.gz.age.sha512"
  rsc_copy "$T/stage-go" noidx "$SRC" || return 1
  rm -f "$RSC/noidx/enc/index.tsv.gz.age" "$RSC/noidx/enc/index.tsv.gz.age.sha512"
  rsc_copy "$T/stage-go" rotidx "$SRC" || return 1
  f=$RSC/rotidx/enc/index.tsv.gz.age
  rm -f "$f.sha512"
  rsc_rot "$f" || return 1
  ! age -d -i "$ID" "$f" > /dev/null 2>&1 || { echo "fixture: the rotted index still decrypts" >&2; return 1; }
}
rsc_index_refused() { # rsc_index_refused sh|go NAME EXPECTED-RX
  local who=$1 name=$2 rx=$3 lf=$LOG/rsc-index-$2-$1.log rc=0
  rsc_run "$who" "$lf" "$RSC/$name.cfg" index lib || rc=$?
  (( rc != 0 )) || { echo "printed the index as though nothing were wrong" >&2; return 1; }
  ! grep -q 'no match' "$lf" || { echo "called an index it could not trust a search with no match" >&2; return 1; }
  grep -qE "$rx" "$lf" || { echo "refused, but not for the index: $(_tail "$(cat "$lf")")" >&2; return 1; }
}
rsc_only_falls_back() { # rsc_only_falls_back sh|go NAME EXPECTED-RX
  local who=$1 name=$2 rx=$3 dest=$RSC/out-$2-$1 lf=$LOG/rsc-only-$2-$1.log rc=0
  rm -rf "$dest" "$RSC/$name/restore"; mkdir -p "$dest"
  rsc_run "$who" "$lf" "$RSC/$name.cfg" restore "$dest" --only lib/a.c || rc=$?
  (( rc == 0 )) || { echo "exited $rc: $(_tail "$(cat "$lf")")" >&2; return 1; }
  cmp -s "$SRC/lib/a.c" "$dest/lib/a.c" || { echo "lib/a.c was not restored" >&2; return 1; }
  grep -qE "$rx" "$lf" || { echo "searched every image without saying why" >&2; return 1; }
}
assert0 "fixture: the reference set with a wrong index hash, with no index, and with a rotted one" rsc_index_fixtures
for who in sh go; do
  name=$(rsc_name "$who")
  assert0 "$name index refuses an index that does not match its recorded hash" \
    rsc_index_refused "$who" badsum 'index\.tsv\.gz\.age does not match the hash in index\.tsv\.gz\.age\.sha512'
  assert0 "$name restore --only searches every image when the index fails its hash, and restores" \
    rsc_only_falls_back "$who" badsum 'could not be read'
  assert0 "$name restore --only searches every image when there is no index, and restores" \
    rsc_only_falls_back "$who" noidx 'no index\.tsv\.gz\.age in '
  assert0 "$name restore --only searches every image when the index will not decrypt, and restores" \
    rsc_only_falls_back "$who" rotidx 'could not be read'
  assert0 "$name index says a rotted index could not be read, never 'no match'" \
    rsc_index_refused "$who" rotidx 'repair it from parity'
done
unset name

# A whole-set restore takes an index that fails its hash the same way, as an
# index that will not read: each image is extracted with the cross-check
# DEGRADED, and says so, rather than compared against rows nothing vouches for.
# That is the Go reader's crossCheckRows, and its price is in README's
# Limitations — ingest does not pin index.tsv.gz.age.sha512, so a later disc can
# put a junk one in its place and switch the check off for the whole set. The
# case holds the two readers to one answer, so that closing that gap, or opening
# another, cannot happen on one side alone.
rsc_badsum_degrades() { # rsc_badsum_degrades sh|go
  local who=$1 dest=$RSC/out-badsum-all-$1 lf=$LOG/rsc-badsum-all-$1.log rc=0
  rm -rf "$dest" "$RSC/badsum/restore"; mkdir -p "$dest"
  rsc_run "$who" "$lf" "$RSC/badsum.cfg" restore "$dest" || rc=$?
  (( rc == 0 )) || { echo "exited $rc: $(_tail "$(cat "$lf")")" >&2; return 1; }
  grep -qE 'DEGRADED|NOT checked against the index' "$lf" \
    || { echo "compared the image against an index that failed its own hash" >&2; return 1; }
  cmp -s "$SRC/lib/a.c" "$dest/lib/a.c" || { echo "lib/a.c was not restored" >&2; return 1; }
}

# Index rows that do not parse are counted and reported by both readers, and a
# disc number written with leading zeros is still that disc. brb.sh said
# nothing of the first and lost the second outright: it put the raw field
# through `sort -un`, which keeps the first of two numerically equal lines, so
# a row reading 00000000001 came out in place of the row reading 1, was then
# dropped as too long to be a disc number, and the path was "not in the index".
# The index here is the reference set's, encrypted again to the same key with
# three rows put in front of it: one that Atoi reads as disc 1, and two that
# name no disc at all.
rsc_oddrows_fixture() {
  local f=$RSC/oddrows/enc/index.tsv.gz.age n
  rsc_copy "$T/stage-go" oddrows "$SRC" || return 1
  { printf '00000000001\tlib/a.c\nx\tlib/a.c\n0\tlib/a.c\n'; idx_plain "$T/stage-go"; } \
    | gzip -c | age -R "$RCP" > "$f.new" || return 1
  mv -f "$f.new" "$f"
  printf '%s  index.tsv.gz.age\n' "$(sha512sum < "$f" | cut -d' ' -f1)" > "$f.sha512"
  n=$(idx_plain "$RSC/oddrows" | grep -c 'lib/a\.c$')
  (( n == 4 )) || { echo "fixture: the new index has $n row(s) for lib/a.c, want the three added and the set's own" >&2; return 1; }
}
rsc_oddrows_only() { # rsc_oddrows_only sh|go
  local who=$1 dest=$RSC/out-oddrows-$1 lf=$LOG/rsc-oddrows-$1.log rc=0
  rm -rf "$dest" "$RSC/oddrows/restore"; mkdir -p "$dest"
  rsc_run "$who" "$lf" "$RSC/oddrows.cfg" restore "$dest" --only lib/a.c || rc=$?
  (( rc == 0 )) || { echo "exited $rc: $(_tail "$(cat "$lf")")" >&2; return 1; }
  cmp -s "$SRC/lib/a.c" "$dest/lib/a.c" || { echo "lib/a.c was not restored" >&2; return 1; }
  grep -q '2 record(s) in index\.tsv\.gz\.age do not parse and were ignored' "$lf" \
    || { echo "did not report the two rows that do not parse" >&2; return 1; }
}
assert0 "fixture: the reference set with three odd rows put in front of its index" rsc_oddrows_fixture
for who in sh go; do
  name=$(rsc_name "$who")
  assert0 "$name restore takes an index that fails its hash as unreadable, and extracts with the cross-check DEGRADED" \
    rsc_badsum_degrades "$who"
  assert0 "$name restore --only reports the index rows that do not parse, and reads 00000000001 as disc 1" \
    rsc_oddrows_only "$who"
done
unset name

# brb.sh hashes the index with sha512sum before reading it, where the Go build
# hashes in-process, and a hash that cannot be taken is not a mismatch. It used
# to come back empty, fail the comparison and report the index rotted, sending
# the operator to par2 over a file nothing had read. The wrapper fails every
# call; a restore hashes each image with it long before the index matters, so
# 'index' is where this shows.
mkdir -p "$RSC/bin-shafail"
cat > "$RSC/bin-shafail/sha512sum" <<'EOF'
#!/bin/sh
# xcompat wrapper: a sha512sum that cannot read its input.
echo "sha512sum: the test wrapper cannot read its input" >&2
exit 1
EOF
chmod +x "$RSC/bin-shafail/sha512sum"
rsc_index_unhashable() {
  local lf=$LOG/rsc-index-shafail-sh.log rc=0
  local RSC_PATH="$RSC/bin-shafail:$PATH"
  rsc_run sh "$lf" "$T/cfg/go" index lib || rc=$?
  (( rc != 0 )) || { echo "printed the index without its hash having been checked" >&2; return 1; }
  ! grep -q 'has rotted' "$lf" || { echo "called an index it could not hash rotted" >&2; return 1; }
  grep -q 'could not hash index\.tsv\.gz\.age' "$lf" \
    || { echo "refused, but not for the hash: $(_tail "$(cat "$lf")")" >&2; return 1; }
}
assert0 "brb.sh index says when it could not hash the index, never that the index has rotted" rsc_index_unhashable

# ---- what 'index' takes its pattern to mean ---------------------------------
# A literal, case-insensitive substring, on both readers. brb.sh handed the
# pattern to grep as a basic regular expression, so a bracketed name found
# nothing, an unbalanced bracket was an error, and both came out as "no match"
# — from the reader whose own refusals say "check 'brb.sh index ...'". A dot or
# a star matched more than was typed. And GNU grep in a UTF-8 locale took a row
# whose name is not valid UTF-8 for binary data and printed nothing for it. A
# newline in the pattern became two patterns. Output is compared byte for byte,
# with the exit status, and for a pattern that matches nothing both readers
# must also have said "no match", so that two readers failing alike for some
# other reason cannot pass for agreement.
RSC_IDXSRC=$RSC/idxsrc
mkdir -p "$RSC_IDXSRC/src"
for f in 'notes[1].txt' 'a[1.txt' 'report.pdf' 'reportXpdf' 'src/main.go' "$(printf 'caf\351.txt')"; do
  printf 'x' > "$RSC_IDXSRC/$f"
done
unset f
mkdir -p "$RSC/stage-idx"; mkcfg "$T/cfg/rsc-idx" "$RSC/stage-idx" "$RSC_IDXSRC"
assert0 "fixture: go brb backs up names holding brackets, a dot, and a byte that is not UTF-8" \
  run_go "$LOG/rsc-backup-idx.log" "$T/cfg/rsc-idx" backup
rsc_index_agrees() { # rsc_index_agrees LOCALE PATTERN ROWS
  local loc=$1 pat=$2 want=$3 a=$RSC/index-sh b=$RSC/index-go ra=0 rb=0
  LC_ALL=$loc bash "$BRB_SH" -c "$T/cfg/rsc-idx" index "$pat" > "$a" 2> "$a.err" || ra=$?
  LC_ALL=$loc "$BRB_GO" --no-color -c "$T/cfg/rsc-idx" index "$pat" > "$b" 2> "$b.err" || rb=$?
  (( $(wc -l < "$a") == want )) || { echo "brb.sh printed $(wc -l < "$a") row(s), want $want" >&2; return 1; }
  (( $(wc -l < "$b") == want )) || { echo "go brb printed $(wc -l < "$b") row(s), want $want" >&2; return 1; }
  (( ra == rb )) || { echo "brb.sh exited $ra, go brb $rb" >&2; return 1; }
  if (( want == 0 )); then
    grep -q 'no match' "$a.err" || { echo "brb.sh printed nothing without saying 'no match': $(_tail "$(cat "$a.err")")" >&2; return 1; }
    grep -q 'no match' "$b.err" || { echo "go brb printed nothing without saying 'no match': $(_tail "$(cat "$b.err")")" >&2; return 1; }
  fi
  cmp -s "$a" "$b" || { diff "$a" "$b" >&2; return 1; }
}
rsc_locales=( C )
if (( HAVE_UTF8_LOCALE )); then rsc_locales+=( "$UTF8_LOCALE" ); fi
for loc in "${rsc_locales[@]}"; do
  assert0 "both readers' index finds 'notes[1]' literally (LC_ALL=$loc)"        rsc_index_agrees "$loc" 'notes[1]' 1
  assert0 "  ... and 'a[1', which is not a valid regular expression"            rsc_index_agrees "$loc" 'a[1' 1
  assert0 "  ... and 'report.pdf' without reportXpdf"                           rsc_index_agrees "$loc" 'report.pdf' 1
  assert0 "  ... and nothing for 'main.*go'"                                    rsc_index_agrees "$loc" 'main.*go' 0
  assert0 "  ... and the row whose name is not UTF-8, for 'caf'"                rsc_index_agrees "$loc" 'caf' 1
  assert0 "  ... and nothing for a pattern holding a newline"                   rsc_index_agrees "$loc" "$(printf 'notes\nreport')" 0
done
if (( ! HAVE_UTF8_LOCALE )); then
  skip "the index pattern under a UTF-8 locale (both readers)" "no en_US.UTF-8 locale on this machine"
fi
unset loc rsc_locales

# ---- a long listing after the match, and a listing that fails ---------------
# brb.sh asked each image whether it held the --only path with an awk that
# exited at the first match. unsquashfs neither ignores nor handles SIGPIPE, so
# it died on its next write, and pipefail turned that 141 into "not on this
# image" whenever more than a few kilobytes of listing followed the match — a
# path near the top of any real disc was reported absent, and a directory was
# skipped on the disc holding it. The listing here puts the path third, and
# twelve thousand entries (about 400 KiB) after it: far past anything a pipe
# buffers, which is what makes the old early exit fail every time rather than
# only sometimes. The reference set's few dozen entries never got near it.
RSC_BIGSRC=$RSC/bigsrc
mkdir -p "$RSC_BIGSRC/a" "$RSC_BIGSRC/b"
printf 'the one file that was asked for\n' > "$RSC_BIGSRC/a/target.txt"
( cd "$RSC_BIGSRC/b" && for i in $(seq -w 1 12000); do : > "filler-$i.txt"; done )
mkdir -p "$RSC/stage-big"; mkcfg "$T/cfg/rsc-big" "$RSC/stage-big" "$RSC_BIGSRC"
assert0 "fixture: go brb backs up a tree whose listing runs on for 12000 entries after a/target.txt" \
  run_go "$LOG/rsc-backup-big.log" "$T/cfg/rsc-big" backup
rsc_big_only() { # rsc_big_only sh|go PATH WANT-FILES
  local who=$1 p=$2 want=$3 dest=$RSC/out-big-$1 lf=$LOG/rsc-big-$1.log rc=0 n
  rm -rf "$dest" "$RSC/stage-big/restore"; mkdir -p "$dest"
  rsc_run "$who" "$lf" "$T/cfg/rsc-big" restore "$dest" --only "$p" || rc=$?
  (( rc == 0 )) || { echo "exited $rc: $(_tail "$(cat "$lf")")" >&2; return 1; }
  n=$(find "$dest" -type f | wc -l)
  (( n == want )) || { echo "restored $n file(s), want $want" >&2; return 1; }
}
for who in sh go; do
  assert0 "$(rsc_name "$who") --only a/target.txt finds it with 12000 entries listed after it" \
    rsc_big_only "$who" a/target.txt 1
  assert0 "$(rsc_name "$who") --only b restores all 12000 files of a directory listed early" \
    rsc_big_only "$who" b 12000
done

# And a listing that fails outright — an unsquashfs built without the image's
# compression is the likely cause — used to say the same "not on this image",
# with unsquashfs's own error sent to /dev/null, and on a multi-disc set the run
# then carried on and called a partial restore complete. Both readers now stop
# on it, naming the listing.
mkdir -p "$RSC/bin-lfail"
cat > "$RSC/bin-lfail/unsquashfs" <<EOF
#!/bin/sh
# xcompat wrapper: refuse the bare listing (-l) as a broken unsquashfs would,
# and hand every other call to the real tool.
if [ "\$1" = "-l" ]; then echo "FATAL ERROR: the test wrapper refuses to list this image" >&2; exit 1; fi
exec "$RSC_REAL_UNSQ" "\$@"
EOF
chmod +x "$RSC/bin-lfail/unsquashfs"
rsc_listing_failure_stops() { # rsc_listing_failure_stops sh|go
  local who=$1 dest=$RSC/out-lfail-$1 lf=$LOG/rsc-lfail-$1.log rc=0 n
  local RSC_PATH="$RSC/bin-lfail:$PATH"
  rm -rf "$dest" "$RSC/lfail/restore"; mkdir -p "$dest"
  rsc_run "$who" "$lf" "$RSC/lfail.cfg" restore "$dest" --only lib/a.c || rc=$?
  (( rc != 0 )) || { echo "exited 0 although the listing failed" >&2; return 1; }
  ! grep -q 'is not on' "$lf" || { echo "took the failed listing for 'not on this image'" >&2; return 1; }
  grep -qE 'could not list|listing ' "$lf" || { echo "stopped, but not for the listing: $(_tail "$(cat "$lf")")" >&2; return 1; }
  n=$(find "$dest" -type f | wc -l)
  (( n == 0 )) || { echo "wrote $n file(s)" >&2; return 1; }
}
assert0 "fixture: a private copy of the reference set for the failing listing" rsc_copy "$T/stage-go" lfail "$SRC"
for who in sh go; do
  assert0 "$(rsc_name "$who") --only stops on a listing that fails, rather than calling the path absent" \
    rsc_listing_failure_stops "$who"
done

# ---- a long non-fatal log from unsquashfs -----------------------------------
# Exit 2 from unsquashfs means it extracted everything and could not set some
# attribute, and both readers promise to warn, keep the log and go on to the
# next disc. brb.sh printed the log's first three lines with `grep | head -3`,
# and under pipefail and errexit grep's SIGPIPE, once head had its three lines,
# ended the whole restore with status 141 and no message, on a log of more than
# a few kilobytes — before the next disc, every time. A destination that will
# not take xattrs writes such a log; the wrapper writes 3000 lines.
mkdir -p "$RSC/bin-x2"
cat > "$RSC/bin-x2/unsquashfs" <<EOF
#!/bin/sh
# xcompat wrapper: an extraction (the one call that starts with -d) runs the
# real tool, then reports 3000 non-fatal errors and exits 2, as unsquashfs does
# when it extracted everything and could not set some attribute.
if [ "\$1" = "-d" ]; then
  "$RSC_REAL_UNSQ" "\$@" || exit \$?
  i=0
  while [ \$i -lt 3000 ]; do echo "write_xattr: the test wrapper could not set an xattr on file \$i"; i=\$((i+1)); done
  exit 2
fi
exec "$RSC_REAL_UNSQ" "\$@"
EOF
chmod +x "$RSC/bin-x2/unsquashfs"
rsc_long_log_continues() { # rsc_long_log_continues sh|go
  local who=$1 dest=$RSC/out-x2-$1 lf=$LOG/rsc-x2-$1.log rc=0
  local RSC_PATH="$RSC/bin-x2:$PATH"
  rm -rf "$dest" "$RSC/x2/restore"; mkdir -p "$dest"
  rsc_run "$who" "$lf" "$RSC/x2.cfg" restore "$dest" || rc=$?
  (( rc == 0 )) || { echo "exited $rc: $(_tail "$(cat "$lf")")" >&2; return 1; }
  grep -q 'non-fatal' "$lf" || { echo "the wrapper's exit 2 was never reported" >&2; return 1; }
  diff -r --no-dereference "$T/m-src" "$dest" >&2
}
if (( n_m >= 2 )); then
  assert0 "fixture: a private copy of the multi-disc set for the long log" \
    rsc_copy "$T/m-go" x2 "$T/m-src" 'DISC_CAPACITY_BYTES=40000000' 'RESERVE_BYTES=12000000'
  for who in sh go; do
    assert0 "$(rsc_name "$who") goes on to every disc after a long non-fatal unsquashfs log" \
      rsc_long_log_continues "$who"
  done
fi

# ---- --only alongside --disc ------------------------------------------------
# The Go reader asks the index even when --disc names the disc, and refuses up
# front when the index puts the path only on other discs. brb.sh skipped the
# index whenever --disc was given: for a file it then hashed and decrypted the
# whole image to find the same thing out, and for a directory whose files are
# all on another disc the skeleton copy of it on the named disc satisfied the
# pre-check — so the run restored one empty directory and said "restore
# complete". The set is built so that one directory, rsca, sits on a single disc:
# its three files are the largest in the tree and the packer is first-fit
# decreasing, so they fill the first disc before anything else does. That is
# checked against the index rather than assumed.
RSC_DIRSRC=$RSC/dirsrc
mkdir -p "$RSC_DIRSRC/rsca" "$RSC_DIRSRC/rscb"
for i in 1 2 3; do head -c 4000000 /dev/urandom > "$RSC_DIRSRC/rsca/big-$i.bin"; done
for i in $(seq -w 1 10); do head -c 2000000 /dev/urandom > "$RSC_DIRSRC/rscb/small-$i.bin"; done
unset i
mkdir -p "$RSC/stage-dir"
mkcfg "$T/cfg/rsc-dir" "$RSC/stage-dir" "$RSC_DIRSRC" 'DISC_CAPACITY_BYTES=40000000' 'RESERVE_BYTES=12000000'
assert0 "fixture: go brb builds a multi-disc set with one directory on one disc" \
  run_go "$LOG/rsc-backup-dir.log" "$T/cfg/rsc-dir" backup
# Which disc rsca is on, and one it is not on — read here, in this shell, so the
# checks below can use them.
RSC_HOME=$(idx_plain "$RSC/stage-dir" | awk -F'\t' 'index($2, "rsca/") == 1 { print $1 + 0 }' | sort -un | tr '\n' ' ')
RSC_HOME=${RSC_HOME% }
RSC_AWAY=""
for d in $(idx_plain "$RSC/stage-dir" | awk -F'\t' '{ print $1 + 0 }' | sort -un); do
  if [[ " $RSC_HOME " != *" $d "* ]]; then RSC_AWAY=$d; break; fi
done
unset d
rsc_layout_ok() {
  [[ "$RSC_HOME" =~ ^[0-9]+$ ]] || { echo "rsca is on disc(s) '$RSC_HOME', not on exactly one" >&2; return 1; }
  [[ -n "$RSC_AWAY" ]] || { echo "no disc of the set lacks rsca's files" >&2; return 1; }
}
rsc_disc_refused() { # rsc_disc_refused sh|go LOGNAME PATH DISC
  local who=$1 lf=$LOG/rsc-$2-$1.log p=$3 d=$4 dest=$RSC/out-$2-$1 rc=0 n
  rm -rf "$dest" "$RSC/stage-dir/restore"; mkdir -p "$dest"
  rsc_run "$who" "$lf" "$T/cfg/rsc-dir" restore "$dest" --only "$p" --disc "$d" || rc=$?
  (( rc != 0 )) || { echo "exited 0 for a path the index puts on disc $RSC_HOME only" >&2; return 1; }
  ! grep -q 'preparing disc' "$lf" || { echo "hashed and decrypted an image before refusing" >&2; return 1; }
  grep -q 'the index puts' "$lf" || { echo "refused, but not on the index's word: $(_tail "$(cat "$lf")")" >&2; return 1; }
  n=$(find "$dest" -type f | wc -l)
  (( n == 0 )) || { echo "wrote $n file(s)" >&2; return 1; }
}
rsc_disc_restores() { # rsc_disc_restores sh|go PATH DISC WANT-FILES
  local who=$1 p=$2 d=$3 want=$4 dest=$RSC/out-discok-$1 lf=$LOG/rsc-discok-$1.log rc=0 n
  rm -rf "$dest" "$RSC/stage-dir/restore"; mkdir -p "$dest"
  rsc_run "$who" "$lf" "$T/cfg/rsc-dir" restore "$dest" --only "$p" --disc "$d" || rc=$?
  (( rc == 0 )) || { echo "exited $rc: $(_tail "$(cat "$lf")")" >&2; return 1; }
  n=$(find "$dest" -type f | wc -l)
  (( n == want )) || { echo "restored $n file(s), want $want" >&2; return 1; }
}
assert0 "fixture: the index puts rsca on one disc ($RSC_HOME) of a set with another (${RSC_AWAY:-none})" rsc_layout_ok
if rsc_layout_ok 2>/dev/null; then
  for who in sh go; do
    name=$(rsc_name "$who")
    assert0 "$name --only <file> --disc <a disc it is not on> refuses before preparing anything" \
      rsc_disc_refused "$who" disc-file rsca/big-1.bin "$RSC_AWAY"
    assert0 "$name --only <directory> --disc <a disc holding only its skeleton> refuses too" \
      rsc_disc_refused "$who" disc-dir rsca "$RSC_AWAY"
    assert0 "$name --only <directory> --disc <the disc it is on> still restores it" \
      rsc_disc_restores "$who" rsca "$RSC_HOME" 3
  done
  unset name
fi

# ---- which image --only narrows to ------------------------------------------
# The index names discs by number, and both readers look the image for each
# one up among the numbered images staged, the same set a whole-set restore
# works through — so rsca's disc staged under a longer spelling of its number
# still serves --only rsca. brb.sh built the name disc%02d from the number
# instead, found no such file, warned that the disc had not been ingested and
# refused a path the Go reader restores from that very image. The copy renames
# the image and its sidecars, and rewrites the name each hash sidecar records,
# so that both readers verify the image as they would under its own name.
rsc_renumbered_fixture() {
  local old new f
  old=$(printf 'disc%02d' "$RSC_HOME"); new=$(printf 'disc0%02d' "$RSC_HOME")
  rsc_copy "$RSC/stage-dir" renum "$RSC_DIRSRC" || return 1
  for f in "$RSC/renum/enc/$old".*; do
    [[ -e "$f" ]] || { echo "fixture: nothing named $old.* in staging" >&2; return 1; }
    mv -- "$f" "$RSC/renum/enc/$new.${f##*/"$old".}"
  done
  for f in "$RSC/renum/enc/$new.squashfs.sha512" "$RSC/renum/enc/$new.squashfs.age.sha512"; do
    sed "s/$old\.squashfs/$new.squashfs/" "$f" > "$f.new" || return 1
    mv -f -- "$f.new" "$f"
  done
}
rsc_renumbered_narrows() { # rsc_renumbered_narrows sh|go
  local who=$1 dest=$RSC/out-renum-$1 lf=$LOG/rsc-renum-$1.log rc=0 n
  rm -rf "$dest" "$RSC/renum/restore"; mkdir -p "$dest"
  rsc_run "$who" "$lf" "$RSC/renum.cfg" restore "$dest" --only rsca || rc=$?
  (( rc == 0 )) || { echo "exited $rc: $(_tail "$(cat "$lf")")" >&2; return 1; }
  n=$(find "$dest" -type f | wc -l)
  (( n == 3 )) || { echo "restored $n file(s), want rsca's 3" >&2; return 1; }
}
if rsc_layout_ok 2>/dev/null; then
  assert0 "fixture: the one-directory set with disc $RSC_HOME staged as $(printf 'disc0%02d' "$RSC_HOME")" \
    rsc_renumbered_fixture
  for who in sh go; do
    assert0 "$(rsc_name "$who") --only rsca finds disc $RSC_HOME's image under that longer name" \
      rsc_renumbered_narrows "$who"
  done
fi
unset who

# ---------------------------------------------------------------------------
head_s "9. ingest, driven on a real terminal"
# ---------------------------------------------------------------------------
# ingest was the one reader command no suite could reach: it swaps discs on the
# terminal, so a piped stdin cannot drive it, and everything behind that prompt
# — the .part promotion, the hash check against the disc, "already have a
# verified", the MANIFEST copy — went unexercised. A disc DIRECTORY stands in
# for a mounted disc, which is exactly what the mount-point argument is for, and
# is also the only shape available here: no optical drive, not root.
if (( HAVE_SCRIPT )); then
  ing_prepare() { # ing_prepare sh|go — a staging area of its own, asserted empty
    local who=$1
    local st=$T/ing-$who
    rm -rf "$st"; mkdir -p "$st/enc"
    mkcfg "$T/cfg/ing-$who" "$st" "$SRC"
    (( $(find "$st/enc" -type f | wc -l) == 0 ))
  }
  ingest_pty() { # ingest_pty sh|go LOGFILE
    case $1 in
      # brb.sh asks twice per disc and takes 'q' to stop; the Go build asks for
      # Enter and then confirms "Another disc?", whose default is no.
      sh) run_pty "$2" $'\nq\n' "bash '$BRB_SH' -c '$T/cfg/ing-sh' ingest '$GOD'" ;;
      go) run_pty "$2" $'\n\n'  "'$BRB_GO' --no-color -c '$T/cfg/ing-go' ingest '$GOD'" ;;
    esac
  }
  ingest_once() { ingest_pty "$1" "$LOG/ingest-$1.log"; }

  # A second pass over the same disc must recognise what it already holds and
  # leave it alone. This is the check that catches a partial copy going sticky:
  # "already have" has to mean "already have a file proven good".
  ingest_is_idempotent() { # ingest_is_idempotent sh|go
    local who=$1
    local st=$T/ing-$who before after
    before=$(sha512sum < "$st/enc/disc01.squashfs.age")
    ingest_pty "$who" "$LOG/ingest2-$who.log" || return 1
    grep -qi 'already have' "$LOG/ingest2-$who.log" \
      || { echo "the second pass did not recognise the staged copy" >&2; return 1; }
    after=$(sha512sum < "$st/enc/disc01.squashfs.age")
    [[ "$before" == "$after" ]] || { echo "the staged image changed on a re-read" >&2; return 1; }
  }

  # The point of ingest is that what comes off the discs restores. Copying the
  # right file names proves nothing on its own.
  restore_from_ingest() { # restore_from_ingest sh|go
    local who=$1
    local dest=$T/out-ing-$who
    rm -rf "$dest"; mkdir -p "$dest"
    case $who in
      sh) run_sh "$LOG/restore-ing-sh.log" "$T/cfg/ing-sh" restore "$dest" ;;
      go) run_go "$LOG/restore-ing-go.log" "$T/cfg/ing-go" restore "$dest" ;;
    esac || return 1
    diff -r --no-dereference --exclude=core.12345 --exclude=a-fifo "$SRC" "$dest" >&2
  }

  for who in sh go; do
    name=$([[ $who == sh ]] && echo 'brb.sh' || echo 'go brb')
    assert0 "fixture: $name starts from a staging area holding nothing" ing_prepare "$who"
    assert0 "$name ingest reads a disc off a mounted disc directory" ingest_once "$who"
    assert0 "  ... the image landed under its real name" test -f "$T/ing-$who/enc/disc01.squashfs.age"
    assert0 "  ... byte-identical to the copy on the disc" \
      cmp -s "$GOD/data/disc01.squashfs.age" "$T/ing-$who/enc/disc01.squashfs.age"
    assert0 "  ... the encrypted index came across too" test -f "$T/ing-$who/enc/index.tsv.gz.age"
    # Nothing else pins this. Each disc carries a sidecars.par2 of its own, so
    # staging renames the set per disc or the N sets collide in one flat
    # directory; staged_name and stagedSidecarName have to spell that name the
    # same way for either reader's staging to be the other's, and the two are
    # separate implementations with nothing but a comment holding them together.
    # Each reader ingests into a staging area of its own above, so no test can
    # catch the drift by having one read what the other wrote — only the name.
    assert0 "  ... and the disc's sidecar parity was staged under its per-disc name" \
      test -f "$T/ing-$who/enc/sidecars-disc01.par2"
    assert0 "  ... and MANIFEST.txt was copied into staging" test -f "$T/ing-$who/MANIFEST.txt"
    assert0 "  ... a second pass keeps the copy it already has" ingest_is_idempotent "$who"
    assert0 "  ... and the ingested set restores byte-identical to the source" restore_from_ingest "$who"
  done
  unset name
else
  skip "ingest on a real terminal (both readers)" \
       "script(1) is not installed; ingest reads its disc prompts from /dev/tty and cannot be driven from a pipe"
fi

# ---------------------------------------------------------------------------
head_s "10. the rescue key and the identity search order"
# ---------------------------------------------------------------------------
# The rescue key is a second recipient whose identity is protected by a
# passphrase — the key for the day the plaintext identity is gone. Two
# properties hold the design up and neither had a test: it must never be chosen
# while a cheaper key is available (otherwise every command asks for a
# passphrase that guards nothing), and one command must ask at most once however
# many things it decrypts.
if (( HAVE_SCRIPT )); then
  RESCUE_PASS='correct horse battery staple'
  RKEY=$T/cfg/rescue
  RCFG=$T/cfg/rescue.conf
  RSTAGE=$T/rescue-stage

  build_rescue_key() {
    rm -rf "$RKEY" "$RSTAGE"; mkdir -p "$RKEY"
    cp "$ID" "$RKEY/identity.txt"
    cp "$RCP" "$RKEY/recipients.txt"
    cp -a "$T/stage-go" "$RSTAGE"; rm -rf "$RSTAGE/restore"
    # No AGE_IDENTITY: with it set, both readers try that path and its .age
    # sibling and never reach the rescue key, so the search order under test
    # would be the wrong one.
    { printf 'STAGING="%s"\n' "$RSTAGE"
      printf 'SOURCE_DIR="%s"\n' "$SRC"
      printf 'AGE_RECIPIENTS_FILE="%s"\n' "$RKEY/recipients.txt"
      printf 'ARCHIVE_NAME="xcompat"\n'
      printf 'DISC_CAPACITY_BYTES=60000000\n'
      printf 'RESERVE_BYTES=30000000\n'
      printf 'PAR2_BLOCKS=40\n'
    } > "$RCFG"
    run_pty "$LOG/rescue-build.log" "$RESCUE_PASS"$'\n'"$RESCUE_PASS"$'\n' \
      "age -p -o '$RKEY/rescue-identity.txt.age' '$ID'" || return 1
    # Asserted on its own, because a "rescue key" that is not really a
    # passphrase-protected container would make every check below pass for the
    # wrong reason.
    head -1 "$RKEY/rescue-identity.txt.age" | grep -q '^age-encryption.org/v1' \
      || { echo "the rescue key is not an age container" >&2; return 1; }
    run_pty "$LOG/rescue-open.log" "$RESCUE_PASS"$'\n' \
      "age -d -o '$RKEY/opened.txt' '$RKEY/rescue-identity.txt.age'" || return 1
    grep -q 'AGE-SECRET-KEY-' "$RKEY/opened.txt"
  }
  assert0 "fixture: a real passphrase-protected rescue identity beside recipients.txt" build_rescue_key

  idx_rows() { grep -E "^[0-9]+$(printf '\t')" "$1" | LC_ALL=C sort; }
  brb_pty() { # brb_pty sh|go LOGFILE FEED ARGS-AS-ONE-STRING
    case $1 in
      sh) run_pty "$2" "$3" "bash '$BRB_SH' -c '$RCFG' $4" ;;
      go) run_pty "$2" "$3" "'$BRB_GO' --no-color -c '$RCFG' $4" ;;
    esac
  }

  # The cheaper key wins: a plaintext identity sitting beside the rescue key
  # must be the one used, and nothing may ask for a passphrase.
  rescue_is_last_resort() { # rescue_is_last_resort sh|go
    local who=$1
    local lf=$LOG/rescue-order-$who.log
    [[ -f "$RKEY/identity.txt" ]] || { echo "fixture lost the plaintext identity" >&2; return 1; }
    brb_pty "$who" "$lf" '' index || return 1
    ! grep -qi 'passphrase' "$lf" \
      || { echo "asked for a passphrase while a plaintext identity was available" >&2; return 1; }
    grep -qF "$RKEY/identity.txt" "$lf" \
      || { echo "did not name the plaintext identity" >&2; return 1; }
    # Kept for rescue_unlocks to diff against: the rescue key must decrypt to
    # the same index, not merely to something.
    idx_rows "$lf" > "$T/rows-order-$who"
    [[ -s "$T/rows-order-$who" ]] || { echo "printed no index rows" >&2; return 1; }
  }

  # ...and when it is gone, the rescue key is picked up, unlocked, and decrypts
  # the very same index.
  rescue_unlocks() { # rescue_unlocks sh|go
    local who=$1
    local lf=$LOG/rescue-use-$who.log rc=0
    mv -f "$RKEY/identity.txt" "$RKEY/identity.hidden" || return 1
    brb_pty "$who" "$lf" "$RESCUE_PASS"$'\n' index || rc=$?
    mv -f "$RKEY/identity.hidden" "$RKEY/identity.txt"
    (( rc == 0 )) || { echo "exit $rc with only the rescue key present" >&2; return 1; }
    grep -q 'rescue-identity.txt.age' "$lf" || { echo "never named the rescue key" >&2; return 1; }
    grep -qi 'passphrase' "$lf" || { echo "never said the key was passphrase-protected" >&2; return 1; }
    idx_rows "$lf" > "$T/rows-use-$who"
    [[ -s "$T/rows-use-$who" ]] || { echo "printed no index rows" >&2; return 1; }
    diff "$T/rows-order-$who" "$T/rows-use-$who" >&2
  }

  # One command, one prompt. --only decrypts the index and then the image, so a
  # reader that asked per decryption would ask twice here.
  rescue_asks_once() { # rescue_asks_once sh|go
    local who=$1
    local lf=$LOG/rescue-once-$who.log dest=$T/out-rescue-$who rc=0 n
    rm -rf "$dest" "$RSTAGE/restore"; mkdir -p "$dest"
    mv -f "$RKEY/identity.txt" "$RKEY/identity.hidden" || return 1
    # No --yes on either side: the Go build refuses to prompt under it, which is
    # the divergence pinned below, so the two runs have to be the same shape.
    brb_pty "$who" "$lf" "$RESCUE_PASS"$'\n' "restore '$dest' --only 'lib/a.c'" || rc=$?
    mv -f "$RKEY/identity.hidden" "$RKEY/identity.txt"
    (( rc == 0 )) || { echo "exit $rc" >&2; return 1; }
    test -s "$dest/lib/a.c" || { echo "the restore extracted nothing" >&2; return 1; }
    cmp -s "$SRC/lib/a.c" "$dest/lib/a.c" || { echo "what came out is not the source file" >&2; return 1; }
    n=$(grep -ciE 'enter the passphrase|enter passphrase' "$lf")
    (( n == 1 )) || { echo "$n passphrase prompt(s) for a command that decrypts twice" >&2; return 1; }
  }

  # A wrong passphrase must stop the command, not fall through to "no identity"
  # and a confusing failure three steps later.
  rescue_refuses_a_wrong_passphrase() { # sh|go
    local who=$1
    local lf=$LOG/rescue-bad-$who.log rc=0
    mv -f "$RKEY/identity.txt" "$RKEY/identity.hidden" || return 1
    brb_pty "$who" "$lf" $'not the passphrase\n' index || rc=$?
    mv -f "$RKEY/identity.hidden" "$RKEY/identity.txt"
    (( rc != 0 )) || { echo "exited 0 on a wrong passphrase" >&2; return 1; }
    grep -qi 'could not unlock' "$lf"
  }

  for who in sh go; do
    name=$([[ $who == sh ]] && echo 'brb.sh' || echo 'go brb')
    assert0 "$name prefers the plaintext identity over the rescue key" rescue_is_last_resort "$who"
    assert0 "$name falls back to the rescue key and decrypts the same index" rescue_unlocks "$who"
    assert0 "$name asks for the passphrase once per command, not per decryption" rescue_asks_once "$who"
    assert0 "$name refuses a wrong passphrase by name" rescue_refuses_a_wrong_passphrase "$who"
  done
  unset name

  # A deliberate divergence, pinned rather than assumed. Under --yes the Go
  # build REFUSES to ask for a passphrase — nobody is there to type one, and the
  # alternative is a script that blocks forever — while brb.sh's unlock_identity
  # asks anyway, because --yes there answers confirmations and prompt_media and
  # unlock_identity is neither. Both are defensible and both are load-bearing,
  # so neither is written as an XFAIL against the other.
  go_refuses_under_yes() {
    local lf=$LOG/rescue-yes-go.log rc=0
    mv -f "$RKEY/identity.txt" "$RKEY/identity.hidden" || return 1
    run_pty "$lf" '' "'$BRB_GO' --yes --no-color -c '$RCFG' index" || rc=$?
    mv -f "$RKEY/identity.hidden" "$RKEY/identity.txt"
    (( rc != 0 )) || { echo "exited 0: it asked, or it went on without the key" >&2; return 1; }
    grep -qi -- '--yes' "$lf"
  }
  sh_still_asks_under_yes() {
    local lf=$LOG/rescue-yes-sh.log rc=0
    mv -f "$RKEY/identity.txt" "$RKEY/identity.hidden" || return 1
    run_pty "$lf" "$RESCUE_PASS"$'\n' "bash '$BRB_SH' --yes -c '$RCFG' index" || rc=$?
    mv -f "$RKEY/identity.hidden" "$RKEY/identity.txt"
    (( rc == 0 )) || { echo "exit $rc" >&2; return 1; }
    grep -qi 'identity unlocked' "$lf"
  }
  assert0 "go brb refuses to ask for a passphrase under --yes, naming the flag" go_refuses_under_yes
  assert0 "brb.sh still asks under --yes (--yes answers confirmations, not prompts)" sh_still_asks_under_yes
else
  skip "the rescue key on a real terminal (both readers)" \
       "script(1) is not installed; age reads passphrases from /dev/tty and never from a pipe"
fi

# ---------------------------------------------------------------------------
head_s "11. locale independence"
# ---------------------------------------------------------------------------
# An old XFAIL claimed locale-dependent behaviour somewhere around SHA512SUMS.
# It could not be reproduced against the current readers, and the variable that
# used to gate the check had been left computed and unread — so the claim was
# neither proven nor retired. A round trip under both locales settles it: the
# fixture already carries a unicode filename, and a collation or byte-vs-
# character difference in either reader would show up as two different trees.
if (( HAVE_UTF8_LOCALE )); then
  restore_under_locale() { # restore_under_locale sh|go LOCALE DEST
    local who=$1 loc=$2 dest=$3
    rm -rf "$dest" "$T/stage-go/restore"; mkdir -p "$dest"
    # Exported inside a subshell: `LC_ALL=x run_sh ...` would leave LC_ALL set
    # in this shell for every check after it, bash not being POSIX-strict here.
    ( export LC_ALL="$loc" LANG="$loc"
      case $who in
        sh) run_sh "$LOG/loc-$who.log" "$T/cfg/go" restore "$dest" ;;
        go) run_go "$LOG/loc-$who.log" "$T/cfg/go" restore "$dest" ;;
      esac )
  }
  for who in sh go; do
    name=$([[ $who == sh ]] && echo 'brb.sh' || echo 'go brb')
    assert0 "$name restores under LC_ALL=C"           restore_under_locale "$who" C            "$T/loc-$who-c"
    assert0 "$name restores under LC_ALL=$UTF8_LOCALE" restore_under_locale "$who" "$UTF8_LOCALE" "$T/loc-$who-u"
    assert0 "  ... the C restore matches the source"  tree_matches "$T/loc-$who-c"
    assert0 "  ... and the two locales agree exactly" \
      diff -r --no-dereference --exclude=a-fifo "$T/loc-$who-c" "$T/loc-$who-u"
  done
  unset name
else
  skip "locale independence (both readers)" "no en_US.UTF-8 locale on this machine"
fi

# ---------------------------------------------------------------------------
head_s "12. symlinks already in the DESTINATION"
# ---------------------------------------------------------------------------
# Every symlink check before this one put the link in the SOURCE and asked
# whether it came back. Nothing ever asked the opposite question: what happens
# when the link is already sitting in the place the archive is about to be
# written to. unsquashfs is run with -f, and -f follows a symlink that resolves
# to a directory — at any depth — so the archive's files land wherever that
# link points, outside the destination, with the restore's privileges (which
# the README recommends be root's). Both readers refuse; that refusal had no
# test at all, in either suite.
#
# The victim directory lives OUTSIDE the destination and is checked after every
# single run: "the command failed" is not the property, "nothing escaped" is.
VICTIM=$T/victim
fresh_victim() {
  rm -rf -- "$VICTIM"; mkdir -p "$VICTIM/keep"
  printf 'canary\n' > "$VICTIM/canary.txt"
  # Two entries, one of each kind: a write that only creates directories and a
  # write that only creates files both show up as a changed count.
  [[ -d "$VICTIM/keep" && -f "$VICTIM/canary.txt" ]] || { echo "could not build the victim directory" >&2; return 1; }
  (( $(find "$VICTIM" -mindepth 1 | wc -l) == 2 ))
}
victim_untouched() {
  local n; n=$(find "$VICTIM" -mindepth 1 | wc -l)
  (( n == 2 )) || { printf 'the victim directory now holds %d entry(ies): %s\n' \
                      "$n" "$(find "$VICTIM" -mindepth 1 | tr '\n' ' ')" >&2; return 1; }
  [[ "$(cat "$VICTIM/canary.txt" 2>/dev/null)" == canary ]] \
    || { echo "the canary file was overwritten" >&2; return 1; }
}
links_to_victim() {
  [[ -L "$1" ]] || { echo "$1 is not a symlink" >&2; return 1; }
  # -d follows the link: true only when it resolves to a DIRECTORY, which is
  # the whole hazard. A plant that quietly became a dangling link would make
  # every refusal below pass for the wrong reason.
  [[ -d "$1" ]] || { echo "$1 does not resolve to a directory" >&2; return 1; }
  [[ "$(readlink -f -- "$1")" == "$(readlink -f -- "$VICTIM")" ]] \
    || { echo "$1 resolves to $(readlink -f -- "$1"), not to the victim directory" >&2; return 1; }
}

# plant KIND DEST — build one destination, and prove the plant is what it claims
# to be. Asserted on its own before the refusal it is supposed to provoke.
plant() {
  local kind=$1 d=$2
  fresh_victim || return 1
  rm -rf -- "$d"
  # Every plant is named after a directory the ARCHIVE actually contains — lib/
  # and project/core/src/ both hold files — so the extraction genuinely
  # traverses the link and the "victim untouched" half of each check is
  # load-bearing. A link called something the archive has never heard of would
  # be refused just the same, but nothing could ever escape through it, and the
  # check would be measuring only the refusal.
  case $kind in
    depth1)   mkdir -p "$d";              ln -s "$VICTIM" "$d/lib"
              links_to_victim "$d/lib" ;;
    depth3)   mkdir -p "$d/project/core"; ln -s "$VICTIM" "$d/project/core/src"
              links_to_victim "$d/project/core/src" ;;
    # A RELATIVE target, because a guard written with readlink -f on the target
    # string alone reads this one as a path that does not exist. $d is always a
    # direct child of $T, so ../victim is $VICTIM.
    relative) mkdir -p "$d";              ln -s ../victim "$d/lib"
              links_to_victim "$d/lib" ;;
    # A link to a link: the resolution has to be followed all the way, not one hop.
    chained)  mkdir -p "$d" "$T/hop"; rm -f -- "$T/hop/to-victim"
              ln -s "$VICTIM" "$T/hop/to-victim"; ln -s "$T/hop/to-victim" "$d/lib"
              links_to_victim "$d/lib" ;;
    # The destination ITSELF. mkdir -p on it succeeds and ls -A lists the
    # victim's contents, so nothing upstream of the guard notices.
    itself)   ln -s "$VICTIM" "$d";  links_to_victim "$d" ;;
    # The legitimate cases, which must NOT be refused: a symlink to a FILE
    # (unsquashfs unlinks and replaces it as an entry, so nothing escapes),
    # named after a real archive file so it is genuinely in the way...
    filelink) mkdir -p "$d";     ln -s "$VICTIM/canary.txt" "$d/big.bin"
              [[ -L "$d/big.bin" && -f "$d/big.bin" && ! -d "$d/big.bin" ]] \
                || { echo "the plant is not a symlink to a file" >&2; return 1; } ;;
    # ...and a dangling link, which resolves to nothing at all. Named after
    # big.bin — a file the archive really holds — for the reason at the top of
    # this function: it was called "dangle", a name no archive entry matches,
    # so unsquashfs never had occasion to touch it and "did not create what it
    # pointed at" was true before the restore started and could not be made
    # false by deleting either guard from either reader.
    dangling) mkdir -p "$d";     ln -s "$T/no-such-thing" "$d/big.bin"
              [[ -L "$d/big.bin" && ! -e "$d/big.bin" ]] \
                || { echo "the plant is not a dangling symlink" >&2; return 1; } ;;
    *) echo "unknown plant kind: $kind" >&2; return 1 ;;
  esac
}

sym_restore() { # sym_restore sh|go LOGNAME DEST [restore args...]
  local who=$1 tag=$2 dest=$3; shift 3
  case $who in
    sh) bash "$BRB_SH" --yes -c "$T/cfg/go" restore "$dest" "$@" > "$LOG/symdest-$tag.log" 2>&1 ;;
    go) "$BRB_GO" --yes --no-color -c "$T/cfg/go" restore "$dest" "$@" > "$LOG/symdest-$tag.log" 2>&1 ;;
  esac
}

# One compound assertion, deliberately: a run that died for an unrelated reason
# satisfies "exited non-zero" on its own, and a run that never started
# satisfies "the victim is untouched" on its own. All three have to hold —
# refused, refused FOR THIS, and nothing written outside the destination.
# --yes is passed every time: this refusal is not a confirmation and --yes must
# not be able to answer it.
escape_refused() { # escape_refused sh|go LOGNAME DEST [restore args...]
  local who=$1 tag=$2 rc=0
  sym_restore "$@" || rc=$?
  (( rc != 0 )) || { echo "exited 0 — the destination symlink was followed" >&2; return 1; }
  grep -q 'symlink(s) to directories' "$LOG/symdest-$tag.log" \
    || { echo "refused, but not for the symlink: $(_tail "$(cat "$LOG/symdest-$tag.log")")" >&2; return 1; }
  victim_untouched
}
# ...and the mirror property for the cases that must still work: the restore
# succeeds, and the file the link pointed at is still the file it was.
escape_not_refused() { # escape_not_refused sh|go LOGNAME DEST
  local who=$1 tag=$2 rc=0
  sym_restore "$@" || rc=$?
  (( rc == 0 )) || { echo "exit $rc: $(_tail "$(cat "$LOG/symdest-$tag.log")")" >&2; return 1; }
  victim_untouched
}

for who in sh go; do
  name=$([[ $who == sh ]] && echo 'brb.sh' || echo 'go brb')
  D=$T/symdest-$who

  assert0 "fixture: $name — the destination's lib/ is a symlink resolving to a directory outside it" plant depth1 "$D"
  assert0 "$name refuses a destination holding a symlinked directory"                escape_refused "$who" "d1-$who" "$D"

  assert0 "fixture: the symlink is project/core/src, three levels down"              plant depth3 "$D"
  assert0 "  ... at any depth, not just the top level"                               escape_refused "$who" "d3-$who" "$D"

  assert0 "fixture: the symlink's target is written as a relative path"              plant relative "$D"
  assert0 "  ... with a relative target"                                             escape_refused "$who" "rel-$who" "$D"

  assert0 "fixture: the symlink points at another symlink, which points at the victim" plant chained "$D"
  assert0 "  ... through a chain of symlinks"                                        escape_refused "$who" "ch-$who" "$D"

  assert0 "fixture: the destination is ITSELF a symlink to a directory"              plant itself "$D"
  assert0 "  ... and when the destination itself is the symlink"                     escape_refused "$who" "self-$who" "$D"

  assert0 "fixture: the lib/ link again, for the --only path"                        plant depth1 "$D"
  assert0 "  ... on --only, which extracts one path through that link"               escape_refused "$who" "only-$who" "$D" --only 'lib/a.c'

  assert0 "fixture: the lib/ link again, for the --disc path"                        plant depth1 "$D"
  assert0 "  ... and on --disc, which skips the index entirely"                      escape_refused "$who" "disc-$who" "$D" --disc 1

  # The refusal must not have overtightened. A symlink to a FILE is safe, and a
  # destination holding one is the ordinary case of restoring over a tree that
  # already has some of the archive's own links in it.
  assert0 "fixture: the destination holds a symlink to a FILE outside it"            plant filelink "$D"
  assert0 "$name still restores over a symlink to a file"                            escape_not_refused "$who" "file-$who" "$D"
  assert0 "  ... replacing the link with the archive's real file"                    cmp -s "$SRC/big.bin" "$D/big.bin"
  assert0 "  ... without writing through it"                                         test "$(cat "$VICTIM/canary.txt")" = canary

  assert0 "fixture: the destination holds a DANGLING symlink, at an archive path"    plant dangling "$D"
  assert0 "$name still restores over a dangling symlink"                             escape_not_refused "$who" "dang-$who" "$D"
  assert0 "  ... replacing it with the archive's real file"                          cmp -s "$SRC/big.bin" "$D/big.bin"
  assert0 "  ... and did not create what it pointed at"                              test ! -e "$T/no-such-thing"
done
unset name D

# A destination that is itself a symlink, SPELLED WITH A TRAILING SLASH. This is
# not a design divergence: it is the same escape as the "itself" case above, and
# brb.sh refuses it because refuse_symlinked_dirs strips the trailing slash
# before handing the path to find -P. The Go reader passes the path to
# filepath.WalkDir as given, and a trailing slash makes the kernel resolve the
# link before lstat ever sees it — so WalkDir starts INSIDE the victim, finds no
# symlink, and the whole archive is written there. Written the way it ought to
# pass, so the day it is fixed this reports XPASS and this comment gets deleted.
selflink_slash_refused() { # selflink_slash_refused sh|go
  local who=$1
  local d=$T/symslash-$who
  plant itself "$d" || return 1
  escape_refused "$who" "selfslash-$who" "$d/"
}
assert0 "brb.sh refuses a destination that is a symlink, spelled with a trailing slash" \
  selflink_slash_refused sh
# Was an XFAIL: filepath.WalkDir(dest) with a trailing slash resolved the link
# before lstat, so refuseSymlinkedDirs walked the TARGET and saw nothing to
# refuse. Restore cleans the destination path first now, as brb.sh always did.
assert0 "go brb refuses a destination that is a symlink, spelled with a trailing slash" \
  selflink_slash_refused go

# Every plant above resolves to a DIRECTORY, so every one of them is answered by
# refuse_symlinked_dirs / refuseSymlinkedDirs — the guard against unsquashfs -f
# traversing a link and writing the archive's files outside the destination.
# There is a SECOND, per-image guard, refuse_symlinks_at_dirs /
# auditImage (formerly refuseSymlinksAtImageDirs), for the case that guard cannot see: a link
# resolving to a FILE, at a path the IMAGE holds as a directory. Nothing
# escapes through that one — unsquashfs finds the path taken and carries on —
# but when it finishes the directory it applies the archive's mode, owner and
# mtime to the path, and the kernel follows the link and applies them to the
# target instead. A planted "Documents -> /etc/shadow" under a root restore is
# the case both readers cite in their comments.
#
# No check in either shell suite ever fired that refusal until this one: both
# readers build their directory list by parsing `unsquashfs -ll` text, so one
# format change is all it would take for the guard to find nothing and return
# happily, and a suite that only ever exercised the traversal guard would stay
# green through it. Each check below therefore insists on the words only this
# guard uses — "symlink(s) where", which the traversal guard's "symlink(s) to
# directories" does not contain — so the other guard cannot answer for it.
#
# empty/ is the plant site because the reference image holds it as a directory
# with NO children (drwxr-xr-x … squashfs-root/empty, read out of the image's
# own listing rather than assumed). Where the archive directory has children
# unsquashfs aborts on its own; an empty one is the case where this guard is
# the only thing between the restore and the target file's metadata.
HOSTAGE=$T/hostage.txt
fresh_hostage() {
  printf 'hostage\n' > "$HOSTAGE"
  chmod 600 "$HOSTAGE"
  [[ -f $HOSTAGE ]] || { echo "could not build the hostage file" >&2; return 1; }
  [[ "$(stat -c%a "$HOSTAGE")" == 600 ]]
}
# Mode as much as contents: nothing is ever written into this file: the hazard
# is the archive directory's 0755, owner and mtime landing on a 0600 file that
# the restore was never pointed at.
hostage_untouched() {
  [[ -f $HOSTAGE && ! -L $HOSTAGE ]] \
    || { echo "the hostage file is no longer a plain file" >&2; return 1; }
  [[ "$(cat "$HOSTAGE" 2>/dev/null)" == hostage ]] \
    || { echo "the hostage file's contents were rewritten" >&2; return 1; }
  local m; m=$(stat -c%a "$HOSTAGE")
  [[ $m == 600 ]] \
    || { printf "the hostage file is now mode %s, not 600 — the archive directory's mode was written THROUGH the link\n" \
           "$m" >&2; return 1; }
}

# plant_at DEST RELPATH — a symlink to the hostage FILE at RELPATH, with the
# proof that it is one. A plant that quietly resolved to a directory, or to
# nothing, would be answered by a different guard, or by none, and the refusal
# below would be measuring something else.
plant_at() {
  local d=$1 rel=$2 parent=$1
  fresh_hostage || return 1
  rm -rf -- "$d"
  if [[ $rel == */* ]]; then parent=$d/${rel%/*}; fi
  mkdir -p -- "$parent"
  ln -s "$HOSTAGE" "$d/$rel"
  [[ -L "$d/$rel" ]] || { echo "$d/$rel is not a symlink" >&2; return 1; }
  [[ -f "$d/$rel" && ! -d "$d/$rel" ]] \
    || { echo "$d/$rel does not resolve to a plain file" >&2; return 1; }
}

# The same three-part shape as escape_refused: refused, refused BY THIS GUARD,
# and the target of the link untouched.
imagedir_refused() { # imagedir_refused sh|go LOGNAME DEST [restore args...]
  local who=$1 tag=$2 rc=0
  sym_restore "$@" || rc=$?
  (( rc != 0 )) || { echo "exited 0 — the link at an archive directory path was extracted onto" >&2; return 1; }
  grep -q 'symlink(s) where' "$LOG/symdest-$tag.log" \
    || { echo "refused, but not by the per-image directory guard: $(_tail "$(cat "$LOG/symdest-$tag.log")")" >&2; return 1; }
  hostage_untouched
}
# ...and the mirror, for the plants that must still restore.
imagedir_not_refused() { # imagedir_not_refused sh|go LOGNAME DEST
  local who=$1 tag=$2 rc=0
  sym_restore "$@" || rc=$?
  (( rc == 0 )) || { echo "exit $rc: $(_tail "$(cat "$LOG/symdest-$tag.log")")" >&2; return 1; }
  hostage_untouched
}

for who in sh go; do
  name=$([[ $who == sh ]] && echo 'brb.sh' || echo 'go brb')
  D=$T/symatdir-$who

  assert0 "fixture: $name — the destination holds a link to a FILE at empty/, which the image holds as a directory" \
    plant_at "$D" empty
  assert0 "$name refuses a symlink where the IMAGE holds a directory"                imagedir_refused "$who" "atdir-$who" "$D"
  assert0 "  ... leaving the link's target file at mode 600 with its own contents"   hostage_untouched

  # Two negative controls, because "refuses" is only worth having next to
  # "still restores". A guard that refused every symlink it found in the
  # destination would pass the check above and break every ordinary restore
  # over a tree that already holds some of the archive's own entries: a link
  # where the archive has a FILE, and a link where the archive has a SYMLINK of
  # its own — which is exactly what disc 1's extracted links look like to disc
  # 2 — must both restore, and be replaced rather than written through.
  assert0 "fixture: the same link at big.bin instead, which the image holds as a file" \
    plant_at "$D" big.bin
  assert0 "$name still restores over a link at an archive FILE path"                 imagedir_not_refused "$who" "atfile-$who" "$D"
  assert0 "  ... replacing it with the archive's real file"                          cmp -s "$SRC/big.bin" "$D/big.bin"

  assert0 "fixture: the same link at lib/sym.bin, which the image holds as a symlink" \
    plant_at "$D" lib/sym.bin
  assert0 "$name still restores over a link at an archive SYMLINK path"              imagedir_not_refused "$who" "atsym-$who" "$D"
  assert0 "  ... replacing it with the archive's own link"                           test "$(readlink -- "$D/lib/sym.bin")" = ../big.bin
done
unset name D

# ---------------------------------------------------------------------------
head_s "12a. an AGE_IDENTITY that is not a file; more than five planted links"
# ---------------------------------------------------------------------------
# Two reader-parity checks from one round of fixes to the Go reader, each of
# which failed against it before.
#
# AGE_IDENTITY is the operator's own setting, and a slip there — a directory, a
# FIFO — used to end the Go reader's identity search on the spot: "is a
# directory", or a hang inside open(2) on the FIFO that the first SIGTERM does
# not end. brb.sh's find_identity takes only `-f && -r` and goes on to
# <AGE_IDENTITY>.age and then the rescue key, so the same staging area
# decrypted under one reader and not the other. The plaintext identity is put
# at <AGE_IDENTITY>.age: a file that is not an age container is used as it is
# by both readers whatever it is called, so this needs no passphrase and no
# terminal. timeout -k, because the FIFO hung the Go reader past SIGTERM.
G8_ODD=$T/g8-odd-identity
G8_IDSTAGE=$T/g8-id-stage
G8_IDCFG=$T/cfg/g8-id
g8_identity_fixture() { # g8_identity_fixture dir|fifo
  rm -rf -- "$G8_IDSTAGE" "$G8_ODD" "$G8_ODD.age"
  cp -a "$T/stage-go" "$G8_IDSTAGE" || return 1
  rm -rf -- "$G8_IDSTAGE/restore"
  case $1 in
    dir)  mkdir -- "$G8_ODD" ;;
    fifo) mkfifo -- "$G8_ODD" ;;
  esac
  cp -- "$ID" "$G8_ODD.age" || return 1
  { printf 'STAGING="%s"\n' "$G8_IDSTAGE"
    printf 'SOURCE_DIR="%s"\n' "$SRC"
    printf 'AGE_RECIPIENTS_FILE="%s"\n' "$RCP"
    printf 'AGE_IDENTITY="%s"\n' "$G8_ODD"
    printf 'ARCHIVE_NAME="xcompat"\n'
  } > "$G8_IDCFG"
  # The plant has to be what it says, or the fallback below proves nothing.
  [[ -e "$G8_ODD" && ! -f "$G8_ODD" ]] || { echo "$G8_ODD is missing or a regular file" >&2; return 1; }
  [[ -f "$G8_ODD.age" ]] && ! head -1 "$G8_ODD.age" | grep -q '^age-encryption.org/v1'
}
g8_identity_falls_back() { # g8_identity_falls_back sh|go LOGTAG
  local who=$1 lf=$LOG/g8-id-$2.log rc=0
  case $who in
    sh) timeout -k 5 60 bash "$BRB_SH" --yes -c "$G8_IDCFG" index > "$lf" 2>&1 || rc=$? ;;
    go) timeout -k 5 60 "$BRB_GO" --yes --no-color -c "$G8_IDCFG" index > "$lf" 2>&1 || rc=$? ;;
  esac
  (( rc == 0 )) || { echo "exit $rc: $(_tail "$(cat "$lf")")" >&2; return 1; }
  grep -qF "using identity $G8_ODD.age" "$lf" \
    || { echo "did not say it fell back to $G8_ODD.age: $(_tail "$(cat "$lf")")" >&2; return 1; }
  grep -qE "^[0-9]+$(printf '\t')" "$lf" || { echo "printed no index rows" >&2; return 1; }
}
for kind in dir fifo; do
  assert0 "fixture: AGE_IDENTITY names a $kind, with the identity at <AGE_IDENTITY>.age" g8_identity_fixture "$kind"
  for who in sh go; do
    name=$([[ $who == sh ]] && echo 'brb.sh' || echo 'go brb')
    assert0 "$name skips an AGE_IDENTITY that is a $kind and decrypts with <AGE_IDENTITY>.age" \
      g8_identity_falls_back "$who" "$kind-$who"
  done
done
unset kind name

# The per-image guard of section 12 refuses a destination holding symlinks where
# the image has directories, and names five of them: brb.sh's
# refuse_symlinks_at_dirs stops at five and its comment says the sentence is
# the Go reader's, word for word. With the set's index readable the Go reader's
# listing scan runs on for the cross-check, and its list of offenders used to
# run on with it, so more than five such links drew five names from brb.sh and
# every one of them from the Go build. The reference set has room for three
# such links at most (a link at project/ hides project/core/ from the guard),
# so this backs up a set of its own: seven directories of one file each, and a
# link to a plain file at every one.
G8_SRC=$T/g8-src
G8_SET=$T/g8-set
G8_TARGET=$T/g8-target.txt
G8_LINKS=7
g8_link_set() {
  rm -rf -- "$G8_SRC" "$G8_SET"; mkdir -p "$G8_SRC" "$G8_SET"
  local i
  for (( i = 1; i <= G8_LINKS; i++ )); do
    mkdir -p "$G8_SRC/d$i"; printf 'file %d\n' "$i" > "$G8_SRC/d$i/f.txt"
  done
  mkcfg "$T/cfg/g8-links" "$G8_SET" "$G8_SRC"
  run_go "$LOG/g8-links-backup.log" "$T/cfg/g8-links" backup || return 1
  # Without a staged index the Go reader stops at five on its own, and the
  # check below would pass against the code it exists to catch.
  [[ -f "$G8_SET/enc/index.tsv.gz.age" ]] || { echo "the set has no staged index" >&2; return 1; }
}
g8_plant_links() { # g8_plant_links DEST
  local d=$1 i
  rm -rf -- "$d"; mkdir -p "$d"
  printf 'target\n' > "$G8_TARGET"; chmod 600 "$G8_TARGET"
  for (( i = 1; i <= G8_LINKS; i++ )); do
    ln -s "$G8_TARGET" "$d/d$i"
    # A link to a plain FILE: one resolving to a directory is the traversal
    # guard's, which would refuse first and say something else.
    [[ -L "$d/d$i" && -f "$d/d$i" ]] || { echo "$d/d$i is not a link to a plain file" >&2; return 1; }
  done
}
# One destination path for both readers, planted afresh for each: the path is
# part of the sentence the two refusals are compared on.
g8_names_five() { # g8_names_five sh|go
  local who=$1 d=$T/g8-dest lf=$LOG/g8-links-$1.log rc=0 line n
  g8_plant_links "$d" || return 1
  rm -rf -- "$G8_SET/restore"
  case $who in
    sh) bash "$BRB_SH" --yes -c "$T/cfg/g8-links" restore "$d" > "$lf" 2>&1 || rc=$? ;;
    go) "$BRB_GO" --yes --no-color -c "$T/cfg/g8-links" restore "$d" > "$lf" 2>&1 || rc=$? ;;
  esac
  (( rc != 0 )) || { echo "exited 0 with $G8_LINKS links where the image has directories" >&2; return 1; }
  # Each reader's own prefix off: "fail restore: " from the Go build, "fail "
  # from brb.sh. What is left is the sentence the two are meant to share.
  line=$(grep -m1 'symlink(s) where' "$lf" | sed -e 's/^fail //' -e 's/^restore: //')
  [[ -n $line ]] || { echo "refused, but not by the per-image guard: $(_tail "$(cat "$lf")")" >&2; return 1; }
  n=$(grep -o ' -> ' <<<"$line" | wc -l)
  (( n == 5 )) || { echo "the refusal names $n link(s), want 5: $(_tail "$line")" >&2; return 1; }
  [[ "$(stat -c%a "$G8_TARGET")" == 600 && "$(cat "$G8_TARGET")" == target ]] \
    || { echo "the links' target was touched" >&2; return 1; }
  printf '%s\n' "$line" > "$T/g8-refusal-$who"
}
assert0 "fixture: a set of $G8_LINKS directories, backed up with its index staged" g8_link_set
assert0 "brb.sh names five of $G8_LINKS links at the image's directories" g8_names_five sh
assert0 "go brb names five of them too, with the index read for the cross-check" g8_names_five go
assert0 "  ... and the two refusals are the same sentence" diff "$T/g8-refusal-sh" "$T/g8-refusal-go"

# ---------------------------------------------------------------------------
head_s "13. a staging path full of glob metacharacters"
# ---------------------------------------------------------------------------
# No suite ever put a metacharacter in a path, and that is exactly where the
# last par2 bug hid: the alternate-copy lookup globbed the whole staging path as
# a pattern, so a staging directory containing [ ] * or ? matched nothing, the
# second pressing was never named on par2's command line, and a set that could
# have been repaired was declared unrepairable.
#
# The scenario is the archive's last line of defence: an image damaged past its
# own redundancy, plus a second pressing damaged somewhere ELSE. Neither holds
# enough blocks alone; together they hold every one.
GSRC=$T/g-src
GNAME='st [a]ge*?q x'          # square brackets, a star, a question mark, a space
GREF="$T/g-ref/$GNAME"
mkdir -p "$GSRC" "$T/g-ref"
head -c 2000000 /dev/urandom > "$GSRC/blob.bin"
printf 'hello\n' > "$GSRC/note.txt"
mkdir -p "$GREF"
mkcfg "$T/cfg/g-ref" "$GREF" "$GSRC"

stage_name_has_metachars() {
  local s=$1
  [[ $s == *'['* ]] || { echo "no [" >&2; return 1; }
  [[ $s == *']'* ]] || { echo "no ]" >&2; return 1; }
  [[ $s == *'*'* ]] || { echo "no *" >&2; return 1; }
  [[ $s == *'?'* ]] || { echo "no ?" >&2; return 1; }
  [[ $s == *' '* ]] || { echo "no space" >&2; return 1; }
}
assert0 "fixture: the staging directory name holds [, ], *, ? and a space" \
  stage_name_has_metachars "$GNAME"
assert0 "go brb writes a set into a staging path full of glob metacharacters" \
  run_go "$LOG/backup-glob.log" "$T/cfg/g-ref" backup
assert0 "  ... and the image really landed there" test -f "$GREF/enc/disc01.squashfs.age"

# damage_pair STAGEDIR — one image damaged past its own parity, plus a second
# pressing damaged somewhere else. PAR2_BLOCKS=40 at the default 10% redundancy
# is 4 recovery blocks, so 12 damaged blocks is comfortably past what either
# copy can fix on its own, and the two damaged ranges do not overlap.
damage_pair() {
  local st=$1 img="$1/enc/disc01.squashfs.age" alt sz blk
  alt="$1/enc/disc01.squashfs.age.copy1700000000"   # the name ingest gives a second pressing
  sz=$(stat -c%s "$img") || return 1
  blk=$(( sz / 40 + 1 ))
  cp -- "$img" "$alt" || return 1
  dd if=/dev/urandom of="$img" bs="$blk" seek=1  count=12 conv=notrunc 2>/dev/null || return 1
  dd if=/dev/urandom of="$alt" bs="$blk" seek=20 count=12 conv=notrunc 2>/dev/null || return 1
  # Both dd's landing in the same place, or neither landing at all, would leave
  # a fixture that proves nothing about combining.
  cmp -s -- "$img" "$alt" && { echo "the two copies are byte-identical: the damage did not land" >&2; return 1; }
  return 0
}
# Prove each copy is beyond its own parity, by running par2 on it ALONE.
alone_is_unrepairable() { # alone_is_unrepairable STAGEDIR FILE
  local st=$1 f=$2 w=$T/solo
  rm -rf "$w"; mkdir -p "$w"
  cp -- "$f" "$w/disc01.squashfs.age" || return 1
  cp -- "$st/enc/disc01.squashfs.age.par2" "$w/" || return 1
  # The directory part quoted, the pattern part not: the same shape prepare_image
  # now uses, and the reason this suite can be written at all.
  cp -- "$st/enc/"disc01.squashfs.age.vol*.par2 "$w/" || return 1
  ( cd "$w" && par2 repair -q -- disc01.squashfs.age.par2 ) >/dev/null 2>&1 \
    && { echo "par2 repaired it from its own parity alone — not damaged past its redundancy" >&2; return 1; }
  return 0
}
glob_stage() { printf '%s/%s' "$T/g-$1" "$GNAME"; }
glob_build() { # glob_build sh|go
  local who=$1 st; st="$(glob_stage "$who")"
  rm -rf -- "$T/g-$who" "$T/g-out-$who"; mkdir -p "$T/g-$who" "$T/g-out-$who"
  cp -a "$GREF" "$st" || return 1
  rm -rf -- "$st/restore"
  mkcfg "$T/cfg/g-$who" "$st" "$GSRC"
  damage_pair "$st"
}
glob_restores() { # glob_restores sh|go
  local who=$1 st; st="$(glob_stage "$who")"
  case $who in
    sh) run_sh "$LOG/glob-sh.log" "$T/cfg/g-$who" restore "$T/g-out-$who" ;;
    go) run_go "$LOG/glob-go.log" "$T/cfg/g-$who" restore "$T/g-out-$who" ;;
  esac || return 1
  diff -r --no-dereference "$GSRC" "$T/g-out-$who" >&2
}
# The negative control that stops the check above passing vacuously: the same
# damage, the same metacharacter staging, the alternate copy taken away. If this
# restored too, the repair above would not have been a combination of anything.
#
# One compound assert0, in corrupt_image_refused's shape, and not an assertN.
# Under assertN every `return 1` of the fixture's own (a cp -a that failed, a
# damage_pair whose sabotage did not land, an alternate copy that would not go)
# counted as the refusal being tested, and so did a reader that died for an
# unrelated reason or half-extracted before failing. So the fixture's failures
# fail the check, and the reader's exit status is captured on its own and must
# be non-zero for the right reason, with nothing written either side of the
# guard: no file in the destination and no decrypted image in staging.
glob_without_the_copy_fails() { # glob_without_the_copy_fails sh|go
  local who=$1 rc=0 n
  local st="$T/g-nc-$who/$GNAME" dest=$T/g-nc-out-$who
  rm -rf -- "$T/g-nc-$who" "$dest"; mkdir -p "$T/g-nc-$who" "$dest"
  cp -a "$GREF" "$st" || return 1
  rm -rf -- "$st/restore"
  mkcfg "$T/cfg/g-nc-$who" "$st" "$GSRC"
  damage_pair "$st" || return 1
  rm -f -- "$st/enc/disc01.squashfs.age.copy1700000000"
  [[ -e "$st/enc/disc01.squashfs.age.copy1700000000" ]] \
    && { echo "the alternate copy is still there" >&2; return 1; }
  case $who in
    sh) bash "$BRB_SH" --yes -c "$T/cfg/g-nc-$who" restore "$dest" > "$LOG/glob-nc-sh.log" 2>&1 || rc=$? ;;
    go) "$BRB_GO" --yes --no-color -c "$T/cfg/g-nc-$who" restore "$dest" > "$LOG/glob-nc-go.log" 2>&1 || rc=$? ;;
  esac
  (( rc != 0 )) || { echo "restored from the damaged image alone, with no second copy to combine" >&2; return 1; }
  n=$(find "$dest" -type f | wc -l)
  (( n == 0 )) || { echo "extracted $n file(s) from an image it could not repair" >&2; return 1; }
  # The path is quoted, so the metacharacters in $st are taken literally here.
  n=$(find "$st/restore" -maxdepth 1 -name '*.squashfs' 2>/dev/null | wc -l)
  (( n == 0 )) || { echo "left $n decrypted image(s) behind in staging" >&2; return 1; }
  # Both readers say this, in these words, when par2 gives up on an image.
  grep -q 'par2 could not repair' "$LOG/glob-nc-$who.log" \
    || { echo "refused, but not because par2 could not repair it: $(_tail "$(cat "$LOG/glob-nc-$who.log")")" >&2; return 1; }
}

for who in sh go; do
  name=$([[ $who == sh ]] && echo 'brb.sh' || echo 'go brb')
  assert0 "fixture: $name — two differently damaged copies in the metacharacter staging" glob_build "$who"
  assert0 "  ... the image alone is past its own par2 redundancy" \
    alone_is_unrepairable "$(glob_stage "$who")" "$(glob_stage "$who")/enc/disc01.squashfs.age"
  assert0 "  ... and so is the second pressing alone" \
    alone_is_unrepairable "$(glob_stage "$who")" "$(glob_stage "$who")/enc/disc01.squashfs.age.copy1700000000"
  assert0 "  ... so neither copy alone can restore the set"                glob_without_the_copy_fails "$who"
  assert0 "$name combines the two copies and restores byte-identical"      glob_restores "$who"
done
unset name

# ---------------------------------------------------------------------------
head_s "14. control bytes in a filename, printed to a real terminal"
# ---------------------------------------------------------------------------
# The names in an index come out of the archive, so they are chosen by whoever
# could put one file in the backed-up tree. Printed raw to a terminal, an
# ESC ] 0 ; ... BEL in a filename retitles the operator's window, and worse
# where OSC 52 is enabled. Both readers escape control bytes when — and only
# when — stdout is a terminal, and NEITHER suite could see it: every existing
# check pipes both readers into a file, and down a pipe the output is
# deliberately left byte-faithful, so the two agreed byte for byte either way.
if (( HAVE_SCRIPT )); then
  ESCSRC=$T/escsrc
  ESC_EVIL=$(printf 'esc\033]0;PWNED\007evil.txt')
  ESC_DEL=$(printf 'del\177bell\007.txt')
  # C1 in both of its spellings, and the case that must NOT be escaped. A
  # terminal decoding UTF-8 acts on U+009B as CSI; one that is not acts on the
  # bare 0x9b byte the same way. The third name is U+4E9B, which encodes as
  # E4 B8 9B: its last byte is a continuation, not a control, and a reader that
  # escaped it would mangle every CJK name in a listing.
  ESC_C1U8=$(printf 'csi\302\233u8.txt')
  ESC_C1RAW=$(printf 'csi\233raw.txt')
  ESC_KEEP=$(printf 'keep\344\270\233me.txt')
  mkdir -p "$ESCSRC/dir"
  printf 'x' > "$ESCSRC/$ESC_EVIL"
  printf 'y' > "$ESCSRC/dir/$ESC_DEL"
  printf 'z' > "$ESCSRC/plain.txt"
  printf 'a' > "$ESCSRC/$ESC_C1U8"
  printf 'b' > "$ESCSRC/$ESC_C1RAW"
  printf 'c' > "$ESCSRC/$ESC_KEEP"
  ESCSRC_FILES=6
  mkdir -p "$T/stage-esc"; mkcfg "$T/cfg/esc" "$T/stage-esc" "$ESCSRC"

  # Count C0 controls and DEL, sparing tab, newline and carriage return: run_pty
  # already strips the CR every pty line ends with, and a tab is what separates
  # the index's two fields.
  raw_ctl_bytes() { LC_ALL=C tr -dc '\001-\010\013\014\016-\037\177' < "$1" | wc -c; }

  esc_fixture_is_hostile() {
    # Asserted against the filesystem, not against the string this script built:
    # a shell that mangled the name on the way to open(2) would otherwise make
    # every check below pass over a tame tree.
    find "$ESCSRC" -type f -printf '%P\n' | LC_ALL=C grep -q $'\033]0;PWNED\007' \
      || { echo "no ESC ] 0 ; PWNED BEL in any filename on disk" >&2; return 1; }
    find "$ESCSRC" -type f -printf '%P\n' | LC_ALL=C grep -q $'\177' \
      || { echo "no DEL in any filename on disk" >&2; return 1; }
    find "$ESCSRC" -type f -printf '%P\n' | LC_ALL=C grep -q $'\302\233' \
      || { echo "no UTF-8-spelled C1 in any filename on disk" >&2; return 1; }
    find "$ESCSRC" -type f -printf '%P\n' | LC_ALL=C grep -q $'csi\233raw' \
      || { echo "no raw C1 byte in any filename on disk" >&2; return 1; }
    find "$ESCSRC" -type f -printf '%P\n' | LC_ALL=C grep -q $'\344\270\233' \
      || { echo "no rune with a C1-range tail byte in any filename on disk" >&2; return 1; }
  }
  assert0 "fixture: the source tree holds filenames with ESC, BEL and DEL in them" esc_fixture_is_hostile
  assert0 "go brb backs up a tree with control bytes in its filenames" \
    run_go "$LOG/backup-esc.log" "$T/cfg/esc" backup

  # The deliberate divergence from a terminal, pinned first: down a PIPE both
  # readers pass the bytes through untouched, which is what keeps the README's
  # awk recipes working and what every byte-for-byte check in this file relies
  # on. If this ever stops holding, the terminal checks below are measuring
  # nothing.
  piped_index_stays_raw() {
    local a=$T/esc-pipe-sh b=$T/esc-pipe-go
    bash "$BRB_SH" -c "$T/cfg/esc" index >"$a" 2>/dev/null || { echo "brb.sh index exited non-zero" >&2; return 1; }
    "$BRB_GO" --no-color -c "$T/cfg/esc" index >"$b" 2>/dev/null || { echo "go brb index exited non-zero" >&2; return 1; }
    (( $(raw_ctl_bytes "$a") > 0 )) || { echo "brb.sh escaped down a pipe" >&2; return 1; }
    (( $(raw_ctl_bytes "$b") > 0 )) || { echo "go brb escaped down a pipe" >&2; return 1; }
    cmp -s "$a" "$b" || { echo "the two piped indexes are not byte-identical" >&2; return 1; }
  }
  assert0 "piped, both readers leave the control bytes raw and byte-identical" piped_index_stays_raw

  # stderr goes to /dev/null INSIDE the pty so that what is measured is the
  # reader's own output stream: the Go build draws a progress bar out of real
  # CSI sequences and brb.sh colours its log lines, and neither is a filename.
  esc_pty() { # esc_pty sh|go LOGFILE ARGS-AS-ONE-STRING
    case $1 in
      sh) run_pty "$2" '' "bash '$BRB_SH' -c '$T/cfg/esc' $3 2>/dev/null" ;;
      go) run_pty "$2" '' "'$BRB_GO' --no-color -c '$T/cfg/esc' $3 2>/dev/null" ;;
    esac
  }
  esc_rows() { grep -E "^[0-9]+$(printf '\t')" "$1" | LC_ALL=C sort; }

  terminal_index_is_escaped() { # terminal_index_is_escaped sh|go
    local who=$1
    local lf=$LOG/esc-idx-$who.log n
    esc_pty "$who" "$lf" index || { echo "index exited non-zero on a terminal" >&2; return 1; }
    n=$(raw_ctl_bytes "$lf")
    (( n == 0 )) || { echo "$n raw control byte(s) reached the terminal" >&2; return 1; }
    # Absence of raw bytes is also what dropping the row entirely looks like.
    grep -qF '\x1b]0;PWNED\x07' "$lf" || { echo "the hostile name was not printed in escaped form" >&2; return 1; }
    grep -qF '\x7fbell\x07' "$lf"     || { echo "DEL and BEL were not escaped" >&2; return 1; }
    # C1 in both spellings. Escaping one and not the other leaves the attack
    # working on whichever terminals decode the other way.
    grep -qF 'csi\xc2\x9bu8.txt' "$lf" \
      || { echo "the UTF-8-spelled C1 was not escaped" >&2; return 1; }
    grep -qF 'csi\x9braw.txt' "$lf" \
      || { echo "the raw C1 byte was not escaped" >&2; return 1; }
    # ... and the byte that only looks like one is still there, unescaped.
    LC_ALL=C grep -q $'keep\344\270\233me.txt' "$lf" \
      || { echo "a rune whose tail byte is in the C1 range was mangled" >&2; return 1; }
    n=$(esc_rows "$lf" | wc -l)
    (( n == ESCSRC_FILES )) || { echo "$n index row(s) for $ESCSRC_FILES file(s)" >&2; return 1; }
    esc_rows "$lf" | awk -F'\t' 'NF!=2 { b=1 } END { exit b+0 }' \
      || { echo "escaping split a row" >&2; return 1; }
  }
  terminal_list_is_escaped() { # terminal_list_is_escaped sh|go
    local who=$1
    local lf=$LOG/esc-list-$who.log n
    rm -rf -- "$T/stage-esc/restore"
    esc_pty "$who" "$lf" 'list 1' || { echo "list exited non-zero on a terminal" >&2; return 1; }
    n=$(raw_ctl_bytes "$lf")
    (( n == 0 )) || { echo "$n raw control byte(s) reached the terminal" >&2; return 1; }
    grep -qF '\x1b]0;PWNED\x07' "$lf" || { echo "the hostile name was not printed in escaped form" >&2; return 1; }
  }
  for who in sh go; do
    name=$([[ $who == sh ]] && echo 'brb.sh' || echo 'go brb')
    assert0 "$name index escapes ESC/BEL/DEL on a terminal" terminal_index_is_escaped "$who"
    assert0 "$name list escapes ESC/BEL/DEL on a terminal"  terminal_list_is_escaped  "$who"
  done
  unset name
  # Reader parity, on the one stream no other check in this file can compare:
  # the escaping is a behaviour both have, so both must render the same name the
  # same way, not merely render something safe.
  assert0 "the two readers escape a terminal index identically" \
    diff <(esc_rows "$LOG/esc-idx-sh.log") <(esc_rows "$LOG/esc-idx-go.log")
  # Only the rows naming the hostile file are compared: this check is about how
  # each reader escapes that name on a terminal. Collapsing runs of spaces is
  # slack, not a known difference. Both escapers (esc_controls in brb.sh, the
  # escapingWriter in Go) pass unsquashfs's own column padding through, so none
  # is expected, and the columns are section 7's business: list_agrees holds
  # the two piped listings to byte-for-byte agreement, order included.
  esc_list_rows() { sed 's/  */ /g' "$1" | grep -F 'evil.txt' | LC_ALL=C sort; }
  assert0 "the two readers escape a terminal listing identically" \
    diff <(esc_list_rows "$LOG/esc-list-sh.log") <(esc_list_rows "$LOG/esc-list-go.log")
else
  skip "terminal escaping of control bytes in filenames (both readers)" \
       "script(1) is not installed; both readers escape only when stdout is a terminal, and a pipe cannot provide one"
fi

go_reads_keep_images_from_config() {
  local st=$T/kcfg dest=$T/outkcfg cfg=$T/cfg/kcfg
  rm -rf "$st" "$dest"; mkdir -p "$dest"
  cp -a "$T/stage-go" "$st"; rm -rf "$st/restore"
  mkcfg "$cfg" "$st" "$SRC" "KEEP_IMAGES=1"
  run_go "$LOG/keep-cfg-go.log" "$cfg" restore "$dest" || return 1
  (( $(find "$st/restore" -maxdepth 1 -name '*.squashfs' 2>/dev/null | wc -l) > 0 ))
}
assert0 "go brb honours KEEP_IMAGES from the config file" go_reads_keep_images_from_config

# The other reader-side key, and the one with teeth: ASSUME_YES answers the
# confirmation that stands between a restore and a destination it overwrites.
# Both readers take it from the config, so a file that silences one silences
# both — the alternative, a key that means "do not ask" to brb.sh and nothing at
# all to the Go build, is the worse failure of the two.
sh_and_go_take_assume_yes_from_config() { # ..._from_config sh|go
  # Two locals, not one: a variable assigned in a 'local' is not yet visible to
  # the assignments beside it, so the paths below would all have been built from
  # an empty $who and both readers would have shared one staging directory.
  local who=$1
  local st=$T/ycfg-$who dest=$T/outycfg-$who cfg=$T/cfg/ycfg-$who
  rm -rf "$st" "$dest"; mkdir -p "$dest"
  cp -a "$T/stage-go" "$st"; rm -rf "$st/restore"
  mkcfg "$cfg" "$st" "$SRC" "ASSUME_YES=1"
  # No pty and no --yes: without ASSUME_YES from the file this cannot confirm
  # and must fail, so a pass here is the config key and nothing else.
  case $who in
    sh) bash "$BRB_SH" -c "$cfg" restore "$dest" >"$LOG/yes-$who.log" 2>&1 ;;
    go) "$BRB_GO" --no-color -c "$cfg" restore "$dest" >"$LOG/yes-$who.log" 2>&1 ;;
  esac || return 1
  [[ -e "$dest/lib/a.c" ]]
}
for who in sh go; do
  name=$([[ $who == sh ]] && echo 'brb.sh' || echo 'go brb')
  assert0 "$name restores non-interactively on ASSUME_YES=1 from the config" \
    sh_and_go_take_assume_yes_from_config "$who"
done
unset name

# ---------------------------------------------------------------------------
head_s "15. a public archive: the set that keeps no secret"
# ---------------------------------------------------------------------------
# --public-archive mints a keypair for the set, encrypts to it as usual, and
# writes the secret key onto every disc as identity.txt at the root, beside
# README.md; the writer keeps the same key at <staging>/enc/identity.txt for
# the life of the set. Both readers pick the key up: ingest copies the disc's
# identity.txt into staging, and it is used as an identity in addition to any
# configured one. Nothing else about the format changes — same ciphertext,
# same par2, same SHA512SUMS — and that is what this section pins, from both
# sides. The recipients path below points into an EMPTY directory on purpose:
# a public set must need no key material on the machine, and a stray
# identity.txt beside the recipients file would have masked a reader that
# only worked by accident.
PUB=$T/stage-pub
mkdir -p "$PUB" "$T/cfg/pubkeys"
{ printf 'STAGING="%s"\nSOURCE_DIR="%s"\nARCHIVE_NAME="xcompat-public"\n' "$PUB" "$SRC"
  printf 'DISC_CAPACITY_BYTES=60000000\nRESERVE_BYTES=30000000\nPAR2_BLOCKS=40\n'
  printf 'AGE_RECIPIENTS_FILE="%s"\nPUBLIC_ARCHIVE=1\n' "$T/cfg/pubkeys/no-such-recipients.txt"
} > "$T/cfg/pub"
assert0 "go brb backup with PUBLIC_ARCHIVE=1 exits 0 with no key material on the machine" \
  run_go "$LOG/backup-pub.log" "$T/cfg/pub" backup
PUBD=$PUB/discs/disc01
pub_key0() { grep -o 'AGE-SECRET-KEY-1[A-Z0-9]*' "$PUB/enc/identity.txt" 2>/dev/null | head -1; }
pub_staging_has_key() { [[ -n "$(pub_key0)" ]]; }
assert0 "the writer left the set's key at <staging>/enc/identity.txt" pub_staging_has_key
pub_key_everywhere() {
  local k0 d k; k0=$(pub_key0); [[ -n $k0 ]] || return 1
  for d in "$PUB"/discs/disc*; do
    k=$(grep -o 'AGE-SECRET-KEY-1[A-Z0-9]*' "$d/identity.txt" 2>/dev/null | head -1)
    [[ $k == "$k0" ]] || return 1
    [[ $(stat -c %a "$d/identity.txt") == 644 ]] || return 1
    grep -q "$k0" "$d/MANIFEST.txt" && grep -q "$k0" "$d/README.md" || return 1
  done
}
assert0 "every disc carries that key as identity.txt (0644), and again in MANIFEST.txt and README.md" pub_key_everywhere
pub_sums_cover_key() { grep -qE '\./identity\.txt$' "$PUBD/SHA512SUMS" && ( cd "$PUBD" && sha512sum -c --quiet SHA512SUMS ) >/dev/null 2>&1; }
assert0 "SHA512SUMS lists ./identity.txt and the disc still verifies" pub_sums_cover_key
assert0 "a public disc's data/ is byte-for-byte the ordinary layout" inv_is "$PUBD" "$DATA_EXPECTED"
assert0 "  ... and its root is the ordinary root plus identity.txt, nothing else" \
  root_is "$PUBD" "$ROOT_EXPECTED"$'\n'"identity.txt"
pub_readme_truthful() {
  grep -q 'deliberately NOT confidential' "$PUBD/README.md" \
    && ! grep -q 'never will be' "$PUBD/README.md" \
    && grep -q 'never will be' "$GOD/README.md"
}
assert0 "the public README says so at the top, and only the public one drops the key-is-not-here sentence" pub_readme_truthful

# The manual recipe, using nothing but the disc.
pub_manual() {
  local w=$T/pub-manual; rm -rf "$w"; mkdir -p "$w"
  cp "$PUBD"/data/disc01.squashfs.age* "$w"/ || return 1
  ( cd "$w" && sha512sum -c --quiet disc01.squashfs.age.sha512 \
      && age -d -i "$PUBD/identity.txt" -o disc01.squashfs disc01.squashfs.age \
      && sha512sum -c --quiet "$PUBD/data/disc01.squashfs.sha512" ) >/dev/null 2>&1
}
assert0 "the on-disc recipe opens the image with the disc's own key and nothing else" pub_manual

# Both readers, from the writer's own staging, with no identity configured
# anywhere: the key must be found at <staging>/enc/identity.txt.
pub_restore_from_staging() { # pub_restore_from_staging sh|go
  local who=$1
  local dest=$T/pub-out-$who
  rm -rf "$dest"; mkdir -p "$dest"
  case $who in
    sh) run_sh "$LOG/pub-restore-sh.log" "$T/cfg/pub" restore "$dest" ;;
    go) run_go "$LOG/pub-restore-go.log" "$T/cfg/pub" restore "$dest" ;;
  esac || return 1
  # tree_matches: the same comparison the reference set uses, which knows the
  # planted core dump is excluded by the default masks and that diff will not
  # compare the fifo.
  tree_matches "$dest"
}
assert0 "brb.sh restores a public set from staging with no identity configured, byte-identical" pub_restore_from_staging sh
assert0 "go brb restores a public set from staging with no identity configured, byte-identical" pub_restore_from_staging go
assert0 "  ... and the two readers' trees agree exactly" diff -r --no-dereference --exclude=a-fifo "$T/pub-out-sh" "$T/pub-out-go"

# And from the discs: ingest must carry the key into a fresh staging area,
# refuse to mix two different public sets into one, and restore from it.
if (( HAVE_SCRIPT )); then
  pub_ing_prepare() { # pub_ing_prepare sh|go
    local who=$1
    local st=$T/ing-pub-$who
    rm -rf "$st"; mkdir -p "$st/enc"
    { printf 'STAGING="%s"\nSOURCE_DIR="%s"\nARCHIVE_NAME="xcompat-public"\n' "$st" "$SRC"
      printf 'AGE_RECIPIENTS_FILE="%s"\n' "$T/cfg/pubkeys/no-such-recipients.txt"
    } > "$T/cfg/ing-pub-$who"
    (( $(find "$st/enc" -type f | wc -l) == 0 ))
  }
  pub_ingest() { # pub_ingest sh|go LOGFILE
    case $1 in
      sh) run_pty "$2" $'\nq\n' "bash '$BRB_SH' -c '$T/cfg/ing-pub-sh' ingest '$PUBD'" ;;
      go) run_pty "$2" $'\n\n'  "'$BRB_GO' --no-color -c '$T/cfg/ing-pub-go' ingest '$PUBD'" ;;
    esac
  }
  pub_ingest_carries_key() { # pub_ingest_carries_key sh|go
    local who=$1
    local st=$T/ing-pub-$who
    pub_ingest "$who" "$LOG/ingest-pub-$who.log" || return 1
    cmp -s "$st/enc/identity.txt" "$PUBD/identity.txt"
  }
  pub_ingest_refuses_mix() { # pub_ingest_refuses_mix sh|go — a different key already staged
    # Exits 0 only when the reader REFUSED the disc and left the staged key
    # alone. Written positively so that a broken fixture (age-keygen missing,
    # cp failing) fails the assertion instead of passing it by accident.
    local who=$1
    local st=$T/ing-pub-$who
    local other; other=$T/cfg/pubkeys/other-identity.txt
    [[ -f $other ]] || age-keygen -o "$other" >/dev/null 2>&1 || return 1
    cp -f "$other" "$st/enc/identity.txt" && chmod 644 "$st/enc/identity.txt" || return 1
    if pub_ingest "$who" "$LOG/ingest-pub-mix-$who.log"; then return 1; fi   # accepted a foreign key
    cmp -s "$st/enc/identity.txt" "$other"                                    # and did not touch it
  }
  pub_restore_from_ingest() { # pub_restore_from_ingest sh|go
    local who=$1
    local st=$T/ing-pub-$who
    local dest=$T/pub-ing-out-$who
    # put the right key back after the mix test, the way a fresh ingest would
    cp -f "$PUBD/identity.txt" "$st/enc/identity.txt" || return 1
    rm -rf "$dest"; mkdir -p "$dest"
    case $who in
      sh) run_sh "$LOG/pub-ing-restore-sh.log" "$T/cfg/ing-pub-$who" restore "$dest" --disc 1 ;;
      go) run_go "$LOG/pub-ing-restore-go.log" "$T/cfg/ing-pub-$who" restore "$dest" --disc 1 ;;
    esac || return 1
    (( $(find "$dest" -type f | wc -l) > 0 ))
  }
  for who in sh go; do
    case $who in sh) name="brb.sh" ;; go) name="go brb" ;; esac
    assert0 "fixture: $name starts from an empty staging area for the public set" pub_ing_prepare "$who"
    assert0 "$name ingest carries the disc's identity.txt into staging, byte-identical" pub_ingest_carries_key "$who"
    assert0 "$name ingest refuses a disc whose key differs from the one already staged, and leaves it alone" pub_ingest_refuses_mix "$who"
    assert0 "  ... and a restore from that staging opens disc 1 with the ingested key" pub_restore_from_ingest "$who"
  done
else
  skip "public-archive ingest (both readers)" "no script(1) for a pty"
fi

# ---------------------------------------------------------------------------
head_s "16. a disc that does not belong to this set"
# ---------------------------------------------------------------------------
# age encrypts to a PUBLIC key, and MANIFEST.txt on every disc prints the
# recipients the set was encrypted to. So anyone who gets hold of ONE disc can
# read that key, pack a squashfs image of their own choosing, encrypt it to
# that key, compute its .sha512 sidecars and its par2 volumes, write a
# SHA512SUMS over the lot, and hand back a disc that decrypts, verifies clean,
# passes par2 and is extracted by 'unsquashfs -f' straight into the operator's
# destination — with the restore's privileges, which the README recommends be
# root's. None of that needs the private key. Nothing on the read path used to
# ask whether a disc belonged to the operator's SET at all.
#
# The mitigation is in two halves, and neither one is any use alone:
#
#   HALF 1 — THE INDEX IS PINNED AT INGEST. Every disc of one set carries the
#     same index.tsv.gz.age, because the writer copies one file onto all of
#     them; a disc whose index differs from the one already staged is refused.
#     The attacker cannot forge an index — it is encrypted to a key they do not
#     hold — so this leaves them one move: ship the genuine index, byte for
#     byte, which needs no key at all.
#
#   HALF 2 — EACH IMAGE IS CROSS-CHECKED AT RESTORE. The regular files an image
#     holds must be exactly the paths the index gives that disc. Having been
#     forced to ship the genuine index, the attacker cannot make their image
#     agree with it: they cannot read it to find out what it says.
#
# What this does NOT do is pinned here too, deliberately, as passing checks,
# because a guard oversold is worse than no guard at all:
#
#   * It detects that the discs of a set DISAGREE. It cannot say which disc is
#     lying — ingest the forged one first and its index becomes the pinned one
#     and the genuine discs are the ones refused. The operator is told the set
#     contradicts itself and has to decide.
#   * It is no protection whatever against a SELF-CONSISTENT forgery: one disc,
#     a forged image, and index rows that agree with it. The attacker controls
#     both halves and there is nothing left to compare. "a forgery the index
#     agrees with is extracted" below is that limit written down as a test, so
#     that nobody reads this section as more than it is.
#   * It is not a signature. Nothing in this format authenticates the sender;
#     there is no signing key to check.
#
# The two readers are written separately and their wordings will never be
# identical, so every message this section reads is matched through one of the
# three patterns named here — one place for the integrator to retune when a
# wording lands differently. Each pattern is asserted BOTH ways: present on the
# run that must refuse or degrade, and absent from the genuine restores that
# must not. A pattern too loose fails the absent half, one too tight fails the
# present half, so neither half can quietly pass on a pattern that is matching
# the wrong line.
XCHK_RX='index.*(disagree|differ|does not|do not|not list|unexpected)|(disagree|differ|does not|do not|not list|unexpected).*index'
PIN_RX='index.*differ|differ.*index'
DEGRADE_RX='newline|cross-?check'

# The set forged below is the multi-disc one from section 8, and the disc
# forged is disc 2 rather than disc 1: the refusal has to hold on a disc
# reached after a genuine one has already been read and extracted, which is
# the shape of the attack — one disc of a set that is otherwise the operator's
# own.
XCHK_SET=$T/m-go
XCHK_SRC=$T/m-src
XCHK_N=2
FORGE=$T/forge; mkdir -p "$FORGE"

# index_rows_for STAGE DISC — the index's paths for one disc, spelled the way
# the index spells them (escaped: \t, \n, \\).
index_rows_for() { idx_plain "$1" | awk -F'\t' -v d="$2" '$1+0==d { print $2 }'; }

# restore_from CFG sh|go DEST LOGFILE [args...] — one restore, whichever
# reader, with the exit status left for the caller to judge.
restore_from() {
  local cfg=$1 who=$2 dest=$3 lf=$4; shift 4
  case $who in
    sh) bash "$BRB_SH" --yes -c "$cfg" restore "$dest" "$@" > "$lf" 2>&1 ;;
    go) "$BRB_GO" --yes --no-color -c "$cfg" restore "$dest" "$@" > "$lf" 2>&1 ;;
  esac
}

# ---- the regression guard: a genuine set must still restore ---------------
# This is the most important check in the section. A cross-check that refuses
# a legitimate disc has taken the whole tool away from its operator, which is
# a worse outcome than the attack it guards against — so the genuine
# multi-disc set is restored by both readers, byte for byte, and their logs
# are required to say nothing about a disagreement and nothing about a
# degraded check.
genuine_restores() { # genuine_restores sh|go
  local who=$1; local dest=$T/out-xchk-genuine-$who
  rm -rf "$dest" "$XCHK_SET/restore"; mkdir -p "$dest"
  restore_from "$T/cfg/m" "$who" "$dest" "$LOG/xchk-genuine-$who.log" || return 1
  diff -r --no-dereference "$XCHK_SRC" "$dest" >&2
}
if (( n_m >= 2 )); then
  for who in sh go; do
    case $who in sh) name="brb.sh" ;; go) name="go brb" ;; esac
    assert0 "$name restores the genuine $n_m-disc set, byte-identical to the source" genuine_restores "$who"
    assertN "  ... without claiming any image disagrees with the index" \
      grep -qiE "$XCHK_RX" "$LOG/xchk-genuine-$who.log"
    assertN "  ... and without degrading the cross-check on ordinary names" \
      grep -qiE "$DEGRADE_RX" "$LOG/xchk-genuine-$who.log"
  done
  unset name
fi

# ---- the forged disc -------------------------------------------------------
# forge_image OUT NAME... — a squashfs image whose root holds exactly the named
# regular files, with contents that were never in anybody's backup. mksquashfs
# without -keep-as-directory puts the directory's CONTENTS at the image root,
# which is the shape the writer produces: 'unsquashfs -ll' prints them as
# squashfs-root/<name>, the same as a real disc image.
forge_image() {
  local out=$1; shift
  local tree=$out.tree n
  rm -rf "$tree" "$out"; mkdir -p "$tree" || return 1
  for n in "$@"; do
    printf 'planted by whoever read the recipients out of MANIFEST.txt\n' > "$tree/$n" || return 1
  done
  mksquashfs "$tree" "$out" -noappend -no-progress -quiet >/dev/null 2>&1 || return 1
  [[ -s $out ]]
}

# forge_into_staging STAGE DISC IMAGE — everything the attacker does, and
# nothing they cannot: IMAGE replaces disc DISC's encrypted image in STAGE, and
# every artifact a reader checks before extracting is regenerated over it — the
# ciphertext sidecar, the plaintext sidecar, and the par2 set. The encryption
# uses the set's own recipients file, which is nothing but the public key
# printed in MANIFEST.txt on every disc.
#
# The forgery is then verified the way a reader will, and this fails if any of
# it comes back damaged: a refusal below that could be explained by a corrupt
# image would prove nothing about the cross-check. The plaintext side is
# checked too, by decrypting — a wrong plaintext sidecar is refused a step
# earlier than the guard under test, and would send every check here down the
# right path for the wrong reason.
forge_into_staging() {
  local st=$1 n=$2 img=$3
  local base; base=$(printf 'disc%02d.squashfs' "$n")
  local enc=$st/enc tmp=$st/.forge
  rm -rf "$tmp"; mkdir -p "$tmp" || return 1
  [[ -f "$enc/$base.age" ]] || { echo "fixture: $base.age is not in staging to forge over" >&2; return 1; }
  cp -f "$img" "$tmp/$base" || return 1
  age -e -R "$RCP" -o "$enc/$base.age" "$tmp/$base" || return 1
  ( cd "$enc" && sha512sum "$base.age" ) > "$enc/$base.age.sha512" || return 1
  ( cd "$tmp" && sha512sum "$base" ) > "$enc/$base.sha512" || return 1
  rm -f "$enc/$base".age*.par2 "$enc/$base.age".copy*
  ( cd "$enc" && par2 create -q -q -b40 -- "$base.age.par2" "$base.age" ) >/dev/null 2>&1 || return 1
  rm -rf "$st/restore"
  ( cd "$enc" && sha512sum -c --quiet "$base.age.sha512" ) >&2 || return 1
  ( cd "$enc" && par2 verify -q -- "$base.age.par2" ) >/dev/null 2>&1 || return 1
  rm -f "$tmp/$base"
  age -d -i "$ID" -o "$tmp/$base" "$enc/$base.age" || return 1
  ( cd "$tmp" && sha512sum -c --quiet "$enc/$base.sha512" ) >&2 || return 1
  rm -rf "$tmp"
}

# forged_copy KIND — a private copy of the multi-disc staging with disc 2's
# image replaced. Both readers share the copy, one after the other: a refused
# restore reads staging and writes only its own restore/ directory, which every
# run below clears first.
#
#   alien     — an image holding files the index never heard of. This is the
#               attack: the genuine index came off a genuine disc, and a forged
#               image cannot agree with an index its author cannot read.
#   agreeing  — an image holding exactly the paths the index gives disc 2, with
#               different contents. This is the forgery the cross-check cannot
#               catch, and it is asserted to restore rather than to be refused.
forged_copy() {
  local kind=$1; local st=$T/xchk-$kind
  rm -rf "$st"
  cp -a "$XCHK_SET" "$st" || return 1
  rm -rf "$st/restore"
  mkcfg "$T/cfg/xchk-$kind" "$st" "$XCHK_SRC" 'DISC_CAPACITY_BYTES=40000000' 'RESERVE_BYTES=12000000'
  local rows
  case $kind in
    alien)
      # Two files nobody backed up, and — deliberately — ONE path the index
      # really does give this disc. The two strangers are what the cross-check
      # must catch. The third is what makes the --only check below mean
      # something: both readers ask an image whether it holds the requested
      # path before they extract it, and an image holding none of it is skipped
      # without ever being compared with anything. An attacker who wants their
      # bytes handed to an operator who typed --only names their file after a
      # file the index says is there, so that is the disc this fixture builds.
      rows=$(index_rows_for "$XCHK_SET" "$XCHK_N")
      [[ -n "$rows" ]] || { echo "the index gives disc $XCHK_N no rows at all" >&2; return 1; }
      # Disjoint from the index in the part that has to be, and asserted rather
      # than assumed: a stranger that happened to be a name the index gives
      # disc 2 would weaken the very check this fixture exists for.
      if grep -qxE 'passwd|authorized_keys' <<<"$rows"; then
        echo "the index really does give disc $XCHK_N one of the forged names" >&2; return 1
      fi
      local one; one=$(head -1 <<<"$rows")
      if [[ "$one" == *[\\/]* ]]; then
        echo "disc $XCHK_N's first index row is not the flat unescaped name this fixture assumes" >&2; return 1
      fi
      forge_image "$FORGE/img-alien" 'passwd' 'authorized_keys' "$one" || return 1
      forge_into_staging "$st" "$XCHK_N" "$FORGE/img-alien" || return 1
      ;;
    agreeing)
      local -a rowv=()
      mapfile -t rowv < <(index_rows_for "$XCHK_SET" "$XCHK_N")
      (( ${#rowv[@]} >= 2 )) || { echo "the index gives disc $XCHK_N ${#rowv[@]} row(s)" >&2; return 1; }
      # The rows become literal file names here, so this fixture only holds
      # where they need no unescaping and name no directories — true of the
      # blob set, and asserted so that changing that set cannot silently forge
      # an image full of backslashes.
      if printf '%s\n' "${rowv[@]}" | grep -q '[\\/]'; then
        echo "disc $XCHK_N's index rows are not the flat unescaped names this fixture assumes" >&2; return 1
      fi
      forge_image "$FORGE/img-agreeing" "${rowv[@]}" || return 1
      forge_into_staging "$st" "$XCHK_N" "$FORGE/img-agreeing" || return 1
      ;;
  esac
}

# forged_staging_ready KIND — a fixture that failed to build must not let the
# checks below pass by accident. A restore that dies on a missing config file
# also exits non-zero and also writes nothing, which is indistinguishable from
# a guard refusing the disc; the first version of this section proved it, by
# reporting a clean PASS for a refusal that had never happened.
forged_staging_ready() {
  local kind=$1; local st=$T/xchk-$kind
  local img; img=$(printf 'disc%02d.squashfs.age' "$XCHK_N")
  [[ -f "$T/cfg/xchk-$kind" && -s "$st/enc/index.tsv.gz.age" && -s "$st/enc/$img" ]] \
    || { echo "fixture: the $kind staging was never built" >&2; return 1; }
  # ...and disc $XCHK_N's image really was replaced: the genuine ciphertext is
  # still there in the set this was copied from, to compare against.
  ! cmp -s "$XCHK_SET/enc/$img" "$st/enc/$img" \
    || { echo "fixture: disc $XCHK_N's image in the $kind staging is the genuine one" >&2; return 1; }
}

# alien_refused sh|go LOGNAME [restore args...] — the forged disc is refused,
# and nothing the attacker chose is written.
#
# The destination is NOT empty afterwards and must not be asserted to be: disc
# 1 is genuine and is extracted before the forged disc is reached. What is
# asserted is that none of the forged image's own files came out, which is the
# property an operator is harmed by losing.
alien_refused() {
  local who=$1 lf=$LOG/$2; shift 2
  local dest=$T/out-xchk-alien-$who rc=0 n
  forged_staging_ready alien || return 1
  rm -rf "$dest" "$T/xchk-alien/restore"; mkdir -p "$dest"
  restore_from "$T/cfg/xchk-alien" "$who" "$dest" "$lf" "$@" || rc=$?
  (( rc != 0 )) || { echo "extracted a disc whose image holds nothing the index gives it" >&2; return 1; }
  n=$(find "$dest" \( -name passwd -o -name authorized_keys \) | wc -l)
  (( n == 0 )) || { echo "$n file(s) the forged image chose landed in the destination" >&2; return 1; }
}

# alien_refused_only sh|go — --only narrows the EXTRACTION, not the disc. The
# path asked for is one the index gives the forged disc AND one the forged
# image holds, so nothing narrows this disc away: both readers ask the image
# whether it holds the requested path, this one says yes, and without the
# cross-check the run ends 0 having written the attacker's version of a file
# the operator asked for by name. That is the whole attack in miniature, and it
# is why this check does not settle for a non-zero exit: it requires that the
# requested path was not written either.
alien_refused_only() {
  local who=$1; local lf=$LOG/xchk-alien-only-$who.log one
  one=$(index_rows_for "$XCHK_SET" "$XCHK_N" | head -1)
  [[ -n "$one" ]] || { echo "the index gives disc $XCHK_N no rows to ask for" >&2; return 1; }
  alien_refused "$who" "xchk-alien-only-$who.log" --only "$one" || return 1
  local dest=$T/out-xchk-alien-$who
  [[ ! -e "$dest/$one" ]] \
    || { echo "refused, but the forged $one was written to the destination anyway" >&2; return 1; }
}

# agreeing_extracts sh|go — the documented limit, asserted as a pass. The
# forged image is extracted, and what lands is the forgery's content and not
# the backup's: a check that merely exited 0 could be a restore that did
# nothing at all.
agreeing_extracts() {
  local who=$1; local dest=$T/out-xchk-agreeing-$who one
  forged_staging_ready agreeing || return 1
  rm -rf "$dest" "$T/xchk-agreeing/restore"; mkdir -p "$dest"
  restore_from "$T/cfg/xchk-agreeing" "$who" "$dest" "$LOG/xchk-agreeing-$who.log" || return 1
  one=$(index_rows_for "$XCHK_SET" "$XCHK_N" | head -1)
  [[ -n "$one" && -s "$dest/$one" ]] || { echo "${one:-the first row} was not extracted at all" >&2; return 1; }
  if cmp -s "$XCHK_SRC/$one" "$dest/$one"; then
    echo "fixture: $one came back identical to the source, so nothing was forged" >&2; return 1
  fi
}

if (( n_m >= 2 )); then
  assert0 "fixture: disc $XCHK_N's image replaced by one holding files the index never names, with sidecars and par2 regenerated over it" \
    forged_copy alien
  for who in sh go; do
    case $who in sh) name="brb.sh" ;; go) name="go brb" ;; esac
    assert0 "$name refuses disc $XCHK_N when its image is not what the index says that disc holds" \
      alien_refused "$who" "xchk-alien-$who.log"
    assert0 "  ... and says the image and the index disagree" \
      grep -qiE "$XCHK_RX" "$LOG/xchk-alien-$who.log"
    assert0 "$name refuses it under --only too, which narrows the extraction and not the disc" \
      alien_refused_only "$who"
  done
  unset name

  assert0 "fixture: disc $XCHK_N's image replaced by one holding exactly the paths the index gives that disc" \
    forged_copy agreeing
  for who in sh go; do
    case $who in sh) name="brb.sh" ;; go) name="go brb" ;; esac
    # Not a bug and not an oversight: with one disc forged in both halves there
    # is nothing left to compare it against. Written down so the section above
    # cannot be read as claiming more than it does.
    assert0 "$name extracts a forgery whose files agree with the index — the limit this guard does not cover" \
      agreeing_extracts "$who"
  done
  unset name
else
  skip "the forged-image cross-check (both readers)" \
       "the multi-disc set in section 8 has fewer than two discs, so no disc can be forged behind a genuine one"
fi

# ---- a name with a newline degrades the check, and never refuses -----------
# 'unsquashfs -ll' is line-based, so a file called "new<newline>line.txt" is
# listed as two half-lines and no line-based listing can carry it faithfully —
# which is why the index has an escaping contract at all. The cross-check has
# to notice that and say so, and must NOT refuse: a backup tool that cannot
# restore a legitimate set is worse than one that misses an exotic attack. The
# set built in section 3 carries exactly that name.
nl_restores() { # nl_restores sh|go
  local who=$1; local st=$T/nl-$who dest=$T/out-nl-$who cfg=$T/cfg/nl-$who
  rm -rf "$st" "$dest"; mkdir -p "$dest"
  cp -a "$T/stage-idx" "$st" || return 1
  rm -rf "$st/restore"
  mkcfg "$cfg" "$st" "$IDXSRC"
  restore_from "$cfg" "$who" "$dest" "$LOG/nl-$who.log" || return 1
  diff -r --no-dereference "$IDXSRC" "$dest" >&2
}
for who in sh go; do
  case $who in sh) name="brb.sh" ;; go) name="go brb" ;; esac
  assert0 "$name still restores a set holding a filename with a newline, byte-identical" nl_restores "$who"
  assert0 "  ... having said the cross-check could not be made exact for that disc" \
    grep -qiE "$DEGRADE_RX" "$LOG/nl-$who.log"
done
unset name

# ---- the index is pinned at ingest ----------------------------------------
if (( HAVE_SCRIPT )); then
  ingest_disc() { # ingest_disc sh|go CFG MOUNTPOINT LOGFILE
    case $1 in
      # As in section 9: brb.sh asks twice per disc and takes 'q' to stop, the
      # Go build asks for Enter and then confirms "Another disc?".
      sh) run_pty "$4" $'\nq\n' "bash '$BRB_SH' -c '$2' ingest '$3'" ;;
      go) run_pty "$4" $'\n\n'  "'$BRB_GO' --no-color -c '$2' ingest '$3'" ;;
    esac
  }

  # The false-refusal side of the pin, written first because it is the one that
  # matters most: every disc of one set carries the identical index, so
  # ingesting a whole set disc by disc into one staging area must be completely
  # untouched by this. An implementation that pinned too eagerly would refuse
  # its own disc 2 here, and a set nobody can ingest is a worse outcome than
  # the forgery the pin exists for.
  ingest_whole_set() { # ingest_whole_set sh|go
    local who=$1; local st=$T/ing-m-$who cfg=$T/cfg/ing-m-$who d n=0
    rm -rf "$st"; mkdir -p "$st/enc"
    mkcfg "$cfg" "$st" "$XCHK_SRC" 'DISC_CAPACITY_BYTES=40000000' 'RESERVE_BYTES=12000000'
    for d in "$XCHK_SET"/discs/disc*; do
      n=$((n+1))
      ingest_disc "$who" "$cfg" "$d" "$LOG/ing-m-$who-$(basename "$d").log" \
        || { echo "$(basename "$d") of a single set was refused" >&2; return 1; }
    done
    (( n == n_m )) || { echo "walked $n disc(s) of a $n_m-disc set" >&2; return 1; }
    (( $(find "$st/enc" -maxdepth 1 -name 'disc*.squashfs.age' | wc -l) == n_m )) \
      || { echo "staging holds fewer than $n_m image(s) after ingesting them all" >&2; return 1; }
    cmp -s "$st/enc/index.tsv.gz.age" "$XCHK_SET/enc/index.tsv.gz.age"
  }
  restore_ingested_set() { # restore_ingested_set sh|go
    local who=$1; local dest=$T/out-ing-m-$who
    rm -rf "$dest" "$T/ing-m-$who/restore"; mkdir -p "$dest"
    restore_from "$T/cfg/ing-m-$who" "$who" "$dest" "$LOG/restore-ing-m-$who.log" || return 1
    diff -r --no-dereference "$XCHK_SRC" "$dest" >&2
  }

  # And the refusal. Two sets, each internally consistent — every hash, every
  # sidecar and every par2 set on both discs is genuine — ingested into one
  # staging area. Their indexes differ, and that is the only thing wrong with
  # the pair: it is either two sets being mixed or one of the discs is not what
  # it claims, and neither is something to warn about and carry on from.
  #
  # Note which staged copy has to survive. The disc's SHA512SUMS records a hash
  # for its own index, so the reconciliation that ran before this guard existed
  # took the second disc's index for "this disc's verified copy" and REPLACED
  # the pinned one with it. "Refused" therefore has to mean the staged bytes did
  # not move, not merely that something was printed.
  ingest_foreign_index_refused() { # ingest_foreign_index_refused sh|go
    local who=$1; local st=$T/ing-mix-$who cfg=$T/cfg/ing-mix-$who
    rm -rf "$st"; mkdir -p "$st/enc"
    mkcfg "$cfg" "$st" "$SRC"
    ingest_disc "$who" "$cfg" "$GOD" "$LOG/ing-mix-first-$who.log" \
      || { echo "the first disc, of an ordinary set, was refused" >&2; return 1; }
    cmp -s "$st/enc/index.tsv.gz.age" "$T/stage-go/enc/index.tsv.gz.age" \
      || { echo "fixture: the first disc's index is not what landed in staging" >&2; return 1; }
    if ingest_disc "$who" "$cfg" "$XCHK_SET/discs/disc01" "$LOG/ing-mix-$who.log"; then
      echo "a disc carrying another set's index was accepted" >&2; return 1
    fi
    cmp -s "$st/enc/index.tsv.gz.age" "$T/stage-go/enc/index.tsv.gz.age" \
      || { echo "the pinned index was overwritten by the other disc's" >&2; return 1; }
  }

  indexes_really_differ() { ! cmp -s "$T/stage-go/enc/index.tsv.gz.age" "$XCHK_SET/enc/index.tsv.gz.age"; }
  assert0 "fixture: the reference set and the multi-disc set carry different encrypted indexes" indexes_really_differ

  for who in sh go; do
    case $who in sh) name="brb.sh" ;; go) name="go brb" ;; esac
    assert0 "$name ingests all $n_m discs of one set into one staging area — the same index on each is not a conflict" \
      ingest_whole_set "$who"
    assert0 "  ... and the set that came off those discs restores byte-identical to the source" \
      restore_ingested_set "$who"
    assert0 "$name refuses a disc whose index differs from the one already staged, and leaves the staged index alone" \
      ingest_foreign_index_refused "$who"
    assert0 "  ... naming the index as the reason" grep -qiE "$PIN_RX" "$LOG/ing-mix-$who.log"
  done
  unset name
else
  skip "the pinned index at ingest (both readers)" \
       "script(1) is not installed; ingest reads its disc prompts from /dev/tty and cannot be driven from a pipe"
fi

# ---------------------------------------------------------------------------
head_s "17. a disc that carries symbolic links at its own names"
# ---------------------------------------------------------------------------
# A disc brb wrote holds data/ as a real directory and its three root files as
# real files. A hostile or hand-mastered one can hold a link at any of those
# names, and following it reads a file that is not on the disc: data -> a
# directory of the operator's stages its contents as if they were images, and
# MANIFEST.txt -> /dev/zero streams until the filesystem fills, because the hash
# that would judge the file is taken as the bytes go past and /dev/zero does not
# end.
#
# Neither reader has to mount anything to be shown this: both take a directory
# where a mount point goes, which is the whole reason a dir-as-disc is what the
# hostile cases in this file are built from.
#
# The assertion is not merely "it refuses". It is that NOTHING was staged: a
# reader that copied the linked-to bytes and then reported a hash mismatch would
# satisfy an exit code while leaving the operator's own files sitting in enc/
# under an image's name.
hostile_disc() { # hostile_disc DIR
  local d=$1
  mkdir -p "$d/disc" "$d/outside"
  printf 'not from any disc\n' > "$d/outside/secret.txt"
  ln -s "$d/outside"  "$d/disc/data"
  ln -s /dev/zero     "$d/disc/MANIFEST.txt"
}

# staged_names DIR -- every file under a staging tree, bar the lock, one per line
staged_names() { find "$1" -type f ! -name '.brb.lock' 2>/dev/null | sort; }

HOS=$T/hostile
hostile_disc "$HOS"
for who in sh go; do
  case $who in sh) name="brb.sh" ;; go) name="go brb" ;; esac
  st=$T/hostile-stage-$who
  mkdir -p "$st"
  mkcfg "$T/cfg/hostile-$who" "$st" "$SRC"
  if [[ $who = go ]]; then
    run_go "$LOG/hostile-$who.log" "$T/cfg/hostile-$who" ingest "$HOS/disc" || true
  elif ((HAVE_SCRIPT)); then
    run_pty "$LOG/hostile-$who.log" $'\nq\n' \
      "bash $BRB_SH --yes -c $T/cfg/hostile-$who ingest $HOS/disc" || true
  else
    skip "$name refuses a disc whose data/ is a symlink" "script(1) is not installed"
    continue
  fi
  assert0 "$name stages nothing from a disc whose data/ is a symlink and whose MANIFEST.txt is a device" \
    test -z "$(staged_names "$st")"
  assert0 "  ... and says the link is why, rather than calling the disc empty" \
    grep -qi 'symbolic link' "$LOG/hostile-$who.log"
done
unset name st

# ---------------------------------------------------------------------------
head_s "17a. ingest, both readers: what a disc can make either one stage"
# ---------------------------------------------------------------------------
# Sections 9, 15 and 16 drive ingest over discs the Go build wrote. This one
# drives it mostly over discs built by hand, the way section 17 builds its
# hostile one — only its first check uses the reference and public discs — to
# pin what each reader STAGES from them, which has to be the same answer from
# both, since a staging area filled by one is read by the other. Every check
# runs both readers into staging areas of their own, and every check but the
# last was written to fail against the code before it, in at least one reader:
#
#   * a disc refused for carrying another set's index left files behind: the
#     Go reader a public disc's key and everything that sorts before the index
#     — its image and sidecars, renamed over the staged ones when the names
#     matched, since its own sums vouched for them — and brb.sh that key, under
#     a message saying nothing had been staged. Where the staged set's own key
#     had not come across, the stray key then had the set's next genuine disc
#     refused as another public archive's;
#   * ingest staged any regular file in data/ under its own name, so a disc
#     could plant names staging uses for itself — a .mapfile the Go reader
#     then resumed a salvage from, INTO the staged image;
#   * brb.sh staged files nested under data/, the Go reader did not; and with
#     no file at the top of data/, brb.sh reported success having staged
#     nothing, where the Go reader fails the disc;
#   * the Go reader never consulted a file's own .sha512 sidecar when the
#     disc's SHA512SUMS said nothing about it, so the readers disagreed about
#     the same disc; and brb.sh took a damaged sidecar's first word for a
#     digest, read a linked one through the link, and judged a linked index
#     against the staged one, which its copy loop never reads;
#   * a second pressing's copy took the name <image>.copy<unix seconds>
#     whatever was already there, and two reads in one second destroyed the
#     first;
#   * each re-read of a disc that returns the same wrong bytes kept one more
#     byte-identical .copy. The last check pins how far the cure for that
#     reaches: a new copy is compared with complete copies, never a salvage.
#
# No mksquashfs or par2 is involved: ingest copies and hashes, and decrypts
# nothing, so the hand-built discs only have to look like discs to it.
if (( HAVE_SCRIPT )); then
  G3=$T/g3; mkdir -p "$G3"

  # g3_stage NAME — a fresh staging area of its own, and its config. Prints the
  # staging path.
  g3_stage() {
    local st=$G3/stage-$1
    rm -rf "$st"; mkdir -p "$st/enc"
    mkcfg "$T/cfg/g3-$1" "$st" "$SRC"
    printf '%s' "$st"
  }
  # g3_ingest sh|go NAME DISC LOGFILE — one pass of one reader over one disc
  # directory, into the staging area g3_stage NAME made.
  g3_ingest() {
    case $1 in
      sh) run_pty "$4" $'\nq\n' "bash '$BRB_SH' -c '$T/cfg/g3-$2' ingest '$3'" ;;
      go) run_pty "$4" $'\n\n'  "'$BRB_GO' --no-color -c '$T/cfg/g3-$2' ingest '$3'" ;;
    esac
  }
  # g3_snapshot DIR — every file under DIR with its digest, for a before and
  # after that must be identical.
  g3_snapshot() { ( cd "$1" && find . -type f -exec sha512sum {} + | LC_ALL=C sort -k2 ); }
  # g3_sidecar FILE — FILE.sha512 beside it, in the format the writer uses.
  g3_sidecar() { ( cd "$(dirname "$1")" && sha512sum "$(basename "$1")" ) > "$1.sha512"; }

  # ---- a disc refused for its index leaves nothing behind ------------------
  # The finding behind this check was a public disc refused by brb.sh for its
  # index after its key had already been staged as enc/identity.txt — live,
  # because the identity search then offered it as the archive's own key — and
  # a Go reader that refused the same disc only once the copy loop reached the
  # index, having staged everything that sorted before it. The two discs used
  # are real: disc 1 of the reference set, and disc 1 of the public set from
  # section 15, whose index is another set's and whose image has the same name.
  # A refusal leaves staging alone whatever caused it, so the fixture is checked
  # first and the refusal has to name the index: a missing disc would
  # otherwise pass for a refused one.
  g3_foreign_left_nothing() { # g3_foreign_left_nothing sh|go
    local who=$1 st before after rc=0
    [[ -f $GOD/data/index.tsv.gz.age && -f $PUBD/data/index.tsv.gz.age && -f $PUBD/identity.txt ]] \
      || { echo "fixture: the reference or the public disc is not there to ingest" >&2; return 1; }
    cmp -s "$GOD/data/index.tsv.gz.age" "$PUBD/data/index.tsv.gz.age" || rc=$?
    (( rc == 1 )) || { echo "fixture: cmp exited $rc, not 1 — the two sets' indexes are not two differing files" >&2; return 1; }
    rc=0
    st=$(g3_stage "foreign-$who")
    g3_ingest "$who" "foreign-$who" "$GOD" "$LOG/g3-foreign-first-$who.log" \
      || { echo "fixture: the first disc, of an ordinary set, was refused" >&2; return 1; }
    [[ -f $st/enc/index.tsv.gz.age && ! -e $st/enc/identity.txt ]] \
      || { echo "fixture: the first disc did not pin an index, or left a key" >&2; return 1; }
    before=$(g3_snapshot "$st/enc")
    g3_ingest "$who" "foreign-$who" "$PUBD" "$LOG/g3-foreign-$who.log" || rc=$?
    (( rc != 0 )) || { echo "the public disc carrying another set's index was accepted" >&2; return 1; }
    grep -qiE "$PIN_RX" "$LOG/g3-foreign-$who.log" \
      || { echo "the public disc was refused, but not for its index" >&2; return 1; }
    after=$(g3_snapshot "$st/enc")
    [[ "$before" == "$after" ]] && return 0
    { echo "the refused disc changed staging:"; diff <(printf '%s\n' "$before") <(printf '%s\n' "$after"); } >&2
    return 1
  }

  # ---- a refused disc's key cannot shut out the staged set ------------------
  # Set P's own key has not come across — its first disc's copy fails the hash
  # that disc records — when a disc of public set Q is offered and refused for
  # its index. The key that refusal used to leave behind then had P's next
  # genuine disc refused as "two different public archives", with a fresh
  # STAGING the only remedy offered.
  g3_sums() { ( cd "$1" && find . -type f ! -name SHA512SUMS | LC_ALL=C sort | while IFS= read -r f; do sha512sum "$f"; done ) > "$1/SHA512SUMS"; }
  g3_keyset_discs() {
    local d
    for d in key-p1 key-q key-p2; do rm -rf "${G3:?}/$d"; mkdir -p "$G3/$d/data" || return 1; done
    printf 'set P index\n' > "$G3/key-p1/data/index.tsv.gz.age"
    printf 'set P index\n' > "$G3/key-p2/data/index.tsv.gz.age"
    printf 'set Q index\n' > "$G3/key-q/data/index.tsv.gz.age"
    head -c 4096 /dev/urandom > "$G3/key-p1/data/disc01.squashfs.age"
    head -c 4096 /dev/urandom > "$G3/key-q/data/disc01.squashfs.age"
    head -c 4096 /dev/urandom > "$G3/key-p2/data/disc02.squashfs.age"
    age-keygen -o "$G3/key-p1/identity.txt" >/dev/null 2>&1 || return 1
    cp "$G3/key-p1/identity.txt" "$G3/key-p2/identity.txt" || return 1
    age-keygen -o "$G3/key-q/identity.txt" >/dev/null 2>&1 || return 1
    for d in key-p1 key-q key-p2; do g3_sums "$G3/$d"; done
    printf '# a line added after the sums were taken\n' >> "$G3/key-p1/identity.txt"
  }
  g3_key_not_left_by_refusal() { # ... sh|go
    local who=$1 st rc=0
    st=$(g3_stage "key-$who")
    g3_ingest "$who" "key-$who" "$G3/key-p1" "$LOG/g3-key-p1-$who.log"
    [[ -f $st/enc/index.tsv.gz.age && ! -e $st/enc/identity.txt ]] \
      || { echo "fixture: set P's first disc did not pin its index, or its damaged key came across" >&2; return 1; }
    g3_ingest "$who" "key-$who" "$G3/key-q" "$LOG/g3-key-q-$who.log" || rc=$?
    (( rc != 0 )) || { echo "set Q's disc was accepted into set P's staging" >&2; return 1; }
    [[ ! -e $st/enc/identity.txt ]] || { echo "the refused disc left its key in staging" >&2; return 1; }
    g3_ingest "$who" "key-$who" "$G3/key-p2" "$LOG/g3-key-p2-$who.log" \
      || { echo "set P's second disc was refused" >&2; return 1; }
    cmp -s "$G3/key-p2/identity.txt" "$st/enc/identity.txt" \
      || { echo "set P's key did not come across from its second disc" >&2; return 1; }
  }

  # ---- names staging keeps for itself ---------------------------------------
  # Each of these means something in enc/ beyond "a file this disc carries",
  # and no disc brb wrote has one in data/: a ddrescue map (which the Go reader
  # resumed a salvage from, writing the disc's bytes into the staged image in
  # place), a copy still being written, another pressing's copy, the published
  # key's staged name, and a disc's renamed sidecar parity. The disc carries no
  # SHA512SUMS, so nothing would have checked what a salvage wrote.
  G3_OWN='disc01.squashfs.age.mapfile disc01.squashfs.age.part disc01.squashfs.age.copy1700000000 identity.txt sidecars-disc02.par2'
  g3_own_disc() {
    local d=$G3/own-disc n
    rm -rf "$d"; mkdir -p "$d/data" || return 1
    printf 'ATTACKER-CHOSEN-BYTES!!!' > "$d/data/disc01.squashfs.age"
    printf '0x00000000  0x00000018  -\n' > "$d/data/disc01.squashfs.age.mapfile"
    for n in disc01.squashfs.age.part disc01.squashfs.age.copy1700000000 sidecars-disc02.par2; do
      printf 'planted\n' > "$d/data/$n"
    done
    age-keygen -o "$d/data/identity.txt" >/dev/null 2>&1
    [[ -s $d/data/identity.txt ]]
  }
  g3_own_not_staged() { # g3_own_not_staged sh|go
    local who=$1 st n pass
    st=$(g3_stage "own-$who")
    printf 'GENUINE-CIPHERTEXT-BYTES' > "$st/enc/disc01.squashfs.age"
    for pass in 1 2; do
      g3_ingest "$who" "own-$who" "$G3/own-disc" "$LOG/g3-own-$who-$pass.log"
    done
    for n in $G3_OWN; do
      [[ ! -e $st/enc/$n ]] || { echo "$n was staged off the disc" >&2; return 1; }
    done
    [[ "$(cat "$st/enc/disc01.squashfs.age")" == GENUINE-CIPHERTEXT-BYTES ]] \
      || { echo "the staged image was rewritten" >&2; return 1; }
    # With no ddrescue installed the planted map still did harm: the Go reader
    # called the good staged copy "an incomplete salvage" and counted it bad.
    ! grep -qi 'incomplete salvage' "$LOG/g3-own-$who-2.log" \
      || { echo "a map file off the disc was taken for a salvage of the staged image" >&2; return 1; }
  }
  g3_own_named() { # g3_own_named sh|go — each skipped name is named in the first pass's log
    local n
    for n in $G3_OWN; do
      grep -q "$n on this disc has a name staging keeps" "$LOG/g3-own-$1-1.log" \
        || { echo "nothing said $n was skipped" >&2; return 1; }
    done
  }

  # ---- one level of data/ ---------------------------------------------------
  g3_nested_disc() {
    local d=$G3/nested-disc
    rm -rf "$d"; mkdir -p "$d/data/sub" || return 1
    head -c 4096 /dev/urandom > "$d/data/sub/disc01.squashfs.age"
    printf 'an index\n' > "$d/data/index.tsv.gz.age"
  }
  g3_nested_ignored() { # g3_nested_ignored sh|go
    local who=$1 st
    st=$(g3_stage "nested-$who")
    g3_ingest "$who" "nested-$who" "$G3/nested-disc" "$LOG/g3-nested-$who.log"
    [[ -f $st/enc/index.tsv.gz.age ]] \
      || { echo "the top-level index was not staged, so nothing was read at all" >&2; return 1; }
    [[ ! -e $st/enc/disc01.squashfs.age ]] \
      || { echo "data/sub/disc01.squashfs.age was staged as the disc's image" >&2; return 1; }
  }
  # With nothing but the subdirectory, there is no file at the top of data/ at
  # all: a disc neither reader can take anything from, which both fail.
  g3_flat_empty_disc() {
    local d=$G3/only-nested-disc
    rm -rf "$d"; mkdir -p "$d/data/sub" || return 1
    head -c 4096 /dev/urandom > "$d/data/sub/disc01.squashfs.age"
  }
  g3_flat_empty_refused() { # ... sh|go
    local who=$1 st rc=0
    st=$(g3_stage "flat-empty-$who")
    g3_ingest "$who" "flat-empty-$who" "$G3/only-nested-disc" "$LOG/g3-flat-empty-$who.log" || rc=$?
    (( rc != 0 )) || { echo "a disc with no file at the top of data/ was ingested as a success" >&2; return 1; }
    [[ -z "$(find "$st/enc" -type f)" ]] || { echo "something was staged off it" >&2; return 1; }
  }

  # ---- the per-file sidecar as the second witness ---------------------------
  # No SHA512SUMS on either disc, so the file's own .sha512 is the only witness
  # left. "mismatch": the image no longer matches its sidecar, and both readers
  # must say so. "rot": the disc's index copy has rotted while its sidecar
  # still vouches for the index already staged — damage on this disc, which
  # both must ingest past rather than refuse as another set's.
  g3_sidecar_discs() {
    local m=$G3/sc-mismatch r=$G3/sc-rot
    rm -rf "$m" "$r"; mkdir -p "$m/data" "$r/data" || return 1
    head -c 4096 /dev/urandom > "$m/data/disc01.squashfs.age"
    g3_sidecar "$m/data/disc01.squashfs.age"
    printf 'x' >> "$m/data/disc01.squashfs.age"
    head -c 4096 /dev/urandom > "$r/data/disc01.squashfs.age"
    printf 'the set index\n' > "$r/data/index.tsv.gz.age"
    g3_sidecar "$r/data/index.tsv.gz.age"
    printf 'the set indeX\n' > "$r/data/index.tsv.gz.age"
    printf 'parity\n' > "$r/data/sidecars.par2"
  }
  g3_sidecar_mismatch_reported() { # ... sh|go
    local who=$1
    g3_stage "scm-$who" >/dev/null
    g3_ingest "$who" "scm-$who" "$G3/sc-mismatch" "$LOG/g3-scm-$who.log"
    grep -q 'does not match the hash this disc records' "$LOG/g3-scm-$who.log"
  }
  g3_sidecar_rot_ingested() { # ... sh|go
    local who=$1 st
    st=$(g3_stage "scr-$who")
    printf 'the set index\n' > "$st/enc/index.tsv.gz.age"
    # The exit status, not $PIN_RX: brb.sh's rot warning says the index
    # "differs from the staged one" on its way to accepting the disc.
    g3_ingest "$who" "scr-$who" "$G3/sc-rot" "$LOG/g3-scr-$who.log" \
      || { echo "a rotted index its own sidecar vouches against was refused as another set's" >&2; return 1; }
    [[ "$(cat "$st/enc/index.tsv.gz.age")" == 'the set index' ]] \
      || { echo "the pinned index was replaced" >&2; return 1; }
    [[ -f $st/enc/sidecars-disc01.par2 ]] \
      || { echo "the disc stopped at its index and never staged its sidecar parity" >&2; return 1; }
  }

  # ---- a sidecar that is no witness, and an index that is a link ------------
  # A sidecar that does not parse, or that is a link, says nothing about its
  # image: both readers say so and copy the image unchecked, as restore treats
  # the same file once it is staged. brb.sh used to take the damaged one's
  # first word for a digest and read the linked one through its link, and
  # count the image bad either way. An index that is a link is a name neither
  # copy loop reads, so it is not judged against the staged one either; brb.sh
  # used to follow it there and refuse the disc as another set's.
  g3_nowitness_discs() {
    local s=$G3/sc-damaged l=$G3/sc-linked i=$G3/idx-linked o=$G3/outside
    rm -rf "$s" "$l" "$i" "$o"; mkdir -p "$s/data" "$l/data" "$i/data" "$o" || return 1
    head -c 4096 /dev/urandom > "$s/data/disc01.squashfs.age"
    printf 'not a checksum\n' > "$s/data/disc01.squashfs.age.sha512"
    head -c 4096 /dev/urandom > "$l/data/disc01.squashfs.age"
    printf '%s  disc01.squashfs.age\n' "$(printf 'some other image' | sha512sum | awk '{print $1}')" \
      > "$o/disc01.squashfs.age.sha512"
    ln -s "$o/disc01.squashfs.age.sha512" "$l/data/disc01.squashfs.age.sha512"
    head -c 4096 /dev/urandom > "$i/data/disc01.squashfs.age"
    printf "another set's index\n" > "$o/index.tsv.gz.age"
    ln -s "$o/index.tsv.gz.age" "$i/data/index.tsv.gz.age"
  }
  g3_nowitness_said() { # g3_nowitness_said sh|go damaged|linked TEXT
    local who=$1 kind=$2 text=$3 st lf=$LOG/g3-sc-$2-$1.log
    st=$(g3_stage "sc-$kind-$who")
    g3_ingest "$who" "sc-$kind-$who" "$G3/sc-$kind" "$lf" \
      || { echo "the disc was not ingested cleanly" >&2; return 1; }
    [[ -f $st/enc/disc01.squashfs.age ]] || { echo "the image was not staged" >&2; return 1; }
    grep -qF "$text" "$lf" || { echo "nothing said the sidecar is no witness ('$text')" >&2; return 1; }
    ! grep -q 'does not match the hash this disc records' "$lf" \
      || { echo "the image was judged against a sidecar that is no witness" >&2; return 1; }
  }
  g3_linked_index_not_judged() { # ... sh|go
    local who=$1 st
    st=$(g3_stage "idx-linked-$who")
    printf 'the set index\n' > "$st/enc/index.tsv.gz.age"
    g3_ingest "$who" "idx-linked-$who" "$G3/idx-linked" "$LOG/g3-idx-linked-$who.log" \
      || { echo "the disc was refused over an index that is a link" >&2; return 1; }
    [[ "$(cat "$st/enc/index.tsv.gz.age")" == 'the set index' ]] \
      || { echo "the pinned index changed" >&2; return 1; }
    [[ -f $st/enc/disc01.squashfs.age ]] || { echo "the disc's image was not staged" >&2; return 1; }
  }

  # ---- a second pressing never lands on a name already taken ----------------
  # Sentinels from a second ago to a few seconds ahead, so the clock ticking
  # over mid-check changes nothing; the disc's copy differs from the staged one
  # and has no recorded hash, so it is kept as a second pressing.
  g3_alt_disc() {
    local d=$G3/alt-disc
    rm -rf "$d"; mkdir -p "$d/data" || return 1
    printf 'this pressing, damaged its own way\n' > "$d/data/disc01.squashfs.age"
  }
  g3_alt_sentinels_survive() { # ... sh|go
    local who=$1 st now s
    st=$(g3_stage "alt-$who")
    printf 'the staged pressing\n' > "$st/enc/disc01.squashfs.age"
    now=$(date +%s)
    for (( s = now - 1; s <= now + 5; s++ )); do
      printf 'an earlier pressing, %s\n' "$s" > "$st/enc/disc01.squashfs.age.copy$s"
    done
    g3_ingest "$who" "alt-$who" "$G3/alt-disc" "$LOG/g3-alt-$who.log"
    for (( s = now - 1; s <= now + 5; s++ )); do
      [[ "$(cat "$st/enc/disc01.squashfs.age.copy$s" 2>/dev/null)" == "an earlier pressing, $s" ]] \
        || { echo "disc01.squashfs.age.copy$s was overwritten or removed" >&2; return 1; }
    done
    grep -lx 'this pressing, damaged its own way' "$st"/enc/disc01.squashfs.age.copy* >/dev/null \
      || { echo "this disc's copy was not kept under a name of its own" >&2; return 1; }
  }

  # ---- the same wrong bytes are not kept twice ------------------------------
  # Both discs record a hash neither image matches. Pressing A read three
  # times: the first read is kept as the damaged primary, the next two are the
  # same bytes and are dropped. Pressing B then read twice: kept once.
  g3_dup_discs() {
    local a=$G3/dup-a b=$G3/dup-b want
    rm -rf "$a" "$b"; mkdir -p "$a/data" "$b/data" || return 1
    want=$(printf 'what the image should have been' | sha512sum | awk '{print $1}')
    printf 'pressing A, wrong the same way every time\n' > "$a/data/disc01.squashfs.age"
    printf 'pressing B, wrong another way\n' > "$b/data/disc01.squashfs.age"
    printf '%s  ./data/disc01.squashfs.age\n' "$want" > "$a/SHA512SUMS"
    cp "$a/SHA512SUMS" "$b/SHA512SUMS"
  }
  g3_dup_not_kept() { # ... sh|go
    local who=$1 st i
    st=$(g3_stage "dup-$who")
    for i in 1 2 3; do g3_ingest "$who" "dup-$who" "$G3/dup-a" "$LOG/g3-dup-$who-a$i.log"; done
    (( $(find "$st/enc" -name 'disc01.squashfs.age.copy*' | wc -l) == 0 )) \
      || { echo "re-reading the staged pressing kept byte-identical copies of it" >&2; return 1; }
    for i in 1 2; do g3_ingest "$who" "dup-$who" "$G3/dup-b" "$LOG/g3-dup-$who-b$i.log"; done
    (( $(find "$st/enc" -name 'disc01.squashfs.age.copy*' | wc -l) == 1 )) \
      || { echo "a second pressing read twice was not kept exactly once" >&2; return 1; }
  }
  # Only complete copies are compared. An alternate with a ddrescue map beside
  # it is a salvage, its unread regions zeros the drive never returned, so a
  # complete read is kept beside it even when its bytes happen to be the
  # salvage's. A planted copy and map stand in for the salvage: there is no
  # ddrescue here to make one.
  g3_salvage_not_compared() { # ... sh|go
    local who=$1 st
    st=$(g3_stage "salv-$who")
    printf 'pressing A, wrong the same way every time\n' > "$st/enc/disc01.squashfs.age"
    printf 'pressing B, wrong another way\n' > "$st/enc/disc01.squashfs.age.copy1700000000"
    printf '0x00000000  0x0000001e  -\n' > "$st/enc/disc01.squashfs.age.copy1700000000.mapfile"
    g3_ingest "$who" "salv-$who" "$G3/dup-b" "$LOG/g3-salv-$who.log"
    (( $(find "$st/enc" -name 'disc01.squashfs.age.copy*' ! -name '*.mapfile' | wc -l) == 2 )) \
      || { echo "a complete read was dropped as a copy of an incomplete salvage" >&2; return 1; }
  }

  assert0 "fixture: two discs of public set P, the first with a damaged key, and one of set Q" g3_keyset_discs
  assert0 "fixture: a disc carrying names staging keeps for itself" g3_own_disc
  assert0 "fixture: a disc with a file nested under data/" g3_nested_disc
  assert0 "fixture: a disc with nothing but a subdirectory in data/" g3_flat_empty_disc
  assert0 "fixture: discs with no SHA512SUMS whose sidecars are the witness" g3_sidecar_discs
  assert0 "fixture: discs whose sidecar or index is no witness" g3_nowitness_discs
  assert0 "fixture: a differing pressing with no recorded hash" g3_alt_disc
  assert0 "fixture: two pressings that each fail the hash their disc records" g3_dup_discs
  for who in sh go; do
    case $who in sh) name="brb.sh" ;; go) name="go brb" ;; esac
    assert0 "$name refuses a public disc carrying another set's index, and staging is exactly as it was — no key, no image" \
      g3_foreign_left_nothing "$who"
    assert0 "$name leaves no key behind from a refused public disc, so the staged set's next disc still goes in" \
      g3_key_not_left_by_refusal "$who"
    assert0 "$name stages none of the names staging keeps for itself, and the staged image is untouched" \
      g3_own_not_staged "$who"
    assert0 "  ... and says which it skipped" g3_own_named "$who"
    assert0 "$name stages nothing nested under data/" g3_nested_ignored "$who"
    assert0 "$name fails a disc with no file at the top of data/, staging nothing" \
      g3_flat_empty_refused "$who"
    assert0 "$name checks a copy against its own .sha512 when SHA512SUMS says nothing" \
      g3_sidecar_mismatch_reported "$who"
    assert0 "$name ingests past an index its sidecar says has rotted, rather than refusing the disc" \
      g3_sidecar_rot_ingested "$who"
    assert0 "$name copies an image unchecked past a damaged .sha512, and says the sidecar is damaged" \
      g3_nowitness_said "$who" damaged "disc01.squashfs.age.sha512 on this disc is damaged"
    assert0 "$name copies an image unchecked past a linked .sha512, and says it is a link" \
      g3_nowitness_said "$who" linked "symbolic link"
    assert0 "$name does not judge a disc by an index that is a link" \
      g3_linked_index_not_judged "$who"
    assert0 "$name never puts a second pressing on a .copy name already taken" \
      g3_alt_sentinels_survive "$who"
    assert0 "$name keeps no byte-identical copy of a pressing it already holds" \
      g3_dup_not_kept "$who"
    assert0 "$name compares a new copy with complete copies only, not with a salvage" \
      g3_salvage_not_compared "$who"
  done
  unset name
else
  skip "ingest of hand-built discs (both readers)" \
       "script(1) is not installed; ingest reads its disc prompts from /dev/tty and cannot be driven from a pipe"
fi

# ---------------------------------------------------------------------------
head_s "18. numbers that reach the shell's arithmetic"
# ---------------------------------------------------------------------------
# check_complete is the one thing standing between a partial set and a restore
# that looks complete: every disc carries the full skeleton, so a missing disc
# shows up as files silently absent rather than as an error. MANIFEST.txt is
# read off a disc, so the number it claims is an attacker's to choose, and
# brb.sh put it through (( )) after checking only that it was digits.
#
# In bash arithmetic a leading zero is OCTAL, so "010" was 8 and eight staged
# images satisfied a ten-disc set; and anything past 2^64 wraps, so
# "18446744073709551616" was 0 and any number of images satisfied any set. The
# Go reader reads the same field with strconv.Atoi -- decimal, and an error
# past int64 -- which TestExpectedDiscs pins on that side.
#
# Asserted through brb.sh's doctor, which is where the bash reader runs the
# check without needing a whole set staged first.
manifest_count_says() { # manifest_count_says CLAIMED EXPECTED-RX
  local claimed=$1 rx=$2 st=$T/mcount out
  rm -rf "$st"; mkdir -p "$st/enc"
  local i; for i in 01 02 03 04 05 06 07 08; do : > "$st/enc/disc$i.squashfs.age"; done
  printf 'discs           : %s
' "$claimed" > "$st/MANIFEST.txt"
  mkcfg "$T/cfg/mcount" "$st" "$SRC"
  out=$(bash "$BRB_SH" --yes -c "$T/cfg/mcount" doctor 2>&1) || true
  grep -qE "$rx" <<<"$out" || { printf '%s\n' "$out" | tail -3 >&2; return 1; }
}
assert0 "brb.sh reads a manifest's 010 as ten discs, not as octal eight" \
  manifest_count_says 010 'MANIFEST says 10 discs; 8 present'
assert0 "  ... and still names the plain spelling the same way" \
  manifest_count_says 10 'MANIFEST says 10 discs; 8 present'
assert0 "brb.sh refuses a disc count larger than the arithmetic can hold" \
  manifest_count_says 18446744073709551616 'cannot tell how many discs'
assert0 "  ... and one that is all zeros" \
  manifest_count_says 000 'cannot tell how many discs'
# The companion that stops the two above from passing by refusing everything:
# a set whose manifest matches what is staged must still read as complete.
manifest_complete() {
  local st=$T/mcount2 out
  rm -rf "$st"; mkdir -p "$st/enc"
  local i; for i in 01 02; do : > "$st/enc/disc$i.squashfs.age"; done
  printf 'discs           : 2
' > "$st/MANIFEST.txt"
  mkcfg "$T/cfg/mcount2" "$st" "$SRC"
  out=$(bash "$BRB_SH" --yes -c "$T/cfg/mcount2" doctor 2>&1) || true
  grep -q 'all 2 disc image(s) present' <<<"$out" || { printf '%s\n' "$out" | tail -3 >&2; return 1; }
}
assert0 "brb.sh still reports a genuinely complete set as complete" manifest_complete

# "N present" counts disc numbers, not names. disc001 beside disc01 is one disc
# to both readers, and brb.sh printed "2 discs; 2 present. MISSING: 2" for it.
# Go's side is TestCheckComplete; its doctor does not run the check.
manifest_present_counts_discs() {
  local st=$T/mcount3 out
  rm -rf "$st"; mkdir -p "$st/enc"
  : > "$st/enc/disc01.squashfs.age"; : > "$st/enc/disc001.squashfs.age"
  printf 'discs           : 2\n' > "$st/MANIFEST.txt"
  mkcfg "$T/cfg/mcount3" "$st" "$SRC"
  out=$(bash "$BRB_SH" --yes -c "$T/cfg/mcount3" doctor 2>&1) || true
  grep -q 'MANIFEST says 2 discs; 1 present. MISSING: 2' <<<"$out" || { printf '%s\n' "$out" | tail -3 >&2; return 1; }
}
assert0 "brb.sh counts disc01 and disc001 as one disc present, not two" manifest_present_counts_discs

# The same hazard one argument along. num_arg forces base 10 but did not bound
# the value, and bash arithmetic wraps at 2^64 silently: "18446744073709551618"
# arrived as 2, so `restore DEST --disc 18446744073709551618` extracted disc 2
# over the destination with unsquashfs -f and exited 0, for a disc the operator
# never asked for. The Go reader refuses the same argument as a usage error.
disc_arg_refused() { # disc_arg_refused sh|go NUMBER
  local who=$1 n=$2 rc=0 out
  case $who in
    sh) out=$(bash "$BRB_SH" --yes -c "$T/cfg/go" list "$n" 2>&1) || rc=$? ;;
    go) out=$("$BRB_GO" --yes --no-color -c "$T/cfg/go" list "$n" 2>&1) || rc=$? ;;
  esac
  (( rc != 0 )) || { echo "accepted the number" >&2; return 1; }
  # It must be refused AS A NUMBER, not merely fail later for some other reason:
  # a reader that took it and then tripped over a missing image would satisfy
  # the exit code while having chosen a disc nobody named.
  grep -qiE 'not a disc number|must be a number|is not a number' <<<"$out" \
    || { printf '%s\n' "$out" | tail -2 >&2; return 1; }
}
for who in sh go; do
  case $who in sh) name="brb.sh" ;; go) name="go brb" ;; esac
  assert0 "$name refuses a disc number too large for its own arithmetic" \
    disc_arg_refused "$who" 18446744073709551618
  assert0 "  ... and one just past the wrap, which came through as 1" \
    disc_arg_refused "$who" 18446744073709551617
done
unset name

# ---------------------------------------------------------------------------
head_s "19. the divergence ledger"
# ---------------------------------------------------------------------------
# Every section above asserts a property both readers already share. This one
# is the other half of the promise: where they genuinely differ, the check is
# written the way it OUGHT to pass and marked XFAIL with the divergence named,
# so that fixing it turns the check into an XPASS — counted as a failure — and
# forces it to be promoted to a real assertion instead of sitting here forever.
#
# Nothing used xassert0/xassertN until this section existed. The suite reported
# "0 known divergence(s) remain" with the machinery never once executed, which
# read as "the two readers agree on everything" when what it meant was "nothing
# has been written down here". A ledger nobody files in is not an empty ledger.
#
# What belongs here is a DEFECT, not a design difference. The README's table of
# command-line differences records several deliberate ones — `version` is a
# command in one reader and not the other, doctor answers a writer's question
# and a reader's — and those will never converge, so writing them the way they
# "ought to pass" would be writing a test for something nobody intends to do.
# The entry below is the other kind: the Go parser stops at a bare "--" and
# takes what follows as data, cli.go's own comment calls the bash behaviour
# "the bug this parser exists to avoid", and TestIndexPatternIsNotAFlag pins
# the Go half. brb.sh ought to do the same and does not.
sh_honours_end_of_flags() {
  # `list -- -y` asks for the disc numbered "-y". Both readers refuse it, and
  # WHICH argument the refusal names is the divergence: the Go reader names -y,
  # brb.sh names -- because main() ate the -y as a flag on the way past — and
  # set CLI_ASSUME_YES while it was there, which is the half with teeth, since
  # that is the answer to the prompt before a restore overwrites its
  # destination. Only the visible half is asserted; the two travel together.
  local out
  out=$(bash "$BRB_SH" -c "$T/cfg/go" list -- -y 2>&1) || true
  grep -qF "got '-y'" <<<"$out"
}
xassert0 "brb.sh takes everything after a bare -- as data, as the Go reader does" \
  "brb.sh main() strips -y and -c from any position including out of user data, so the disc number becomes '--' and ASSUME_YES is silently turned on (README's command-line table, 'flags after --')" \
  sh_honours_end_of_flags

printf '\n%d passed, %d failed, %d xfail (known divergences), %d skipped\n' \
  "$pass_n" "$fail_n" "$xfail_n" "$skip_n"
if ((fail_n)); then printf '\nfailures:\n'; printf '  - %s\n' "${FAILURES[@]}"; exit 1; fi
printf 'the format contract holds; %d known divergence(s) remain.\n' "$xfail_n"
