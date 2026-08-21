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
# Where the two readers genuinely differ, the check is written the way it ought
# to pass and marked XFAIL with the divergence named. An XFAIL that starts
# passing is reported as XPASS and counted as a FAILURE, because this file is
# then out of date. Nothing known-broken is quietly omitted.
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
# The two optional tools, script(1) and an en_US.UTF-8 locale, are the only ones
# that gate individual checks rather than the whole file; each such check prints
# its own SKIP naming what is missing.

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
assert0() {
  local d=$1; shift
  local out rc
  out=$("$@" 2>&1); rc=$?
  if ((rc == 0)); then pass "$d"; else fail "$d" "exit $rc: $(_tail "$out")"; fi
}
# assertN DESC CMD...   -- the check passes when CMD exits non-zero
assertN() {
  local d=$1; shift
  local out rc
  out=$("$@" 2>&1); rc=$?
  if ((rc != 0)); then pass "$d"; else fail "$d" "expected a non-zero exit, got 0: $(_tail "$out")"; fi
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
# that cannot vouch for it. The fixture is asserted separately so a mangling
# that silently missed cannot let the refusal checks pass vacuously.
mangle_sums_fixture() {
  rm -rf "$T/rotsums"; cp -a "$GOD" "$T/rotsums"
  printf 'X' | dd of="$T/rotsums/data/disc01.squashfs.age" bs=1 seek=99 count=1 conv=notrunc 2>/dev/null
  sed -i 's|^[0-9a-f]\{128\}\(  \./data/disc01\.squashfs\.age\)$|deadbeef\1|' "$T/rotsums/SHA512SUMS"
  grep -q '^deadbeef  ' "$T/rotsums/SHA512SUMS"
}
assert0 "fixture: a disc whose SHA512SUMS line for a corrupt file is itself mangled" mangle_sums_fixture
verify_rotsums() { # verify_rotsums sh|go
  case $1 in
    sh) bash "$BRB_SH" --yes -c "$T/cfg/go" verify-disc 1 "$T/rotsums" > "$LOG/rotsums-sh.log" 2>&1 ;;
    go) "$BRB_GO" --yes --no-color -c "$T/cfg/go" verify-disc 1 "$T/rotsums" > "$LOG/rotsums-go.log" 2>&1 ;;
  esac
}
assertN "brb.sh verify-disc rejects a disc whose SHA512SUMS has a corrupt line" verify_rotsums sh
assertN "go brb verify-disc rejects a disc whose SHA512SUMS has a corrupt line" verify_rotsums go

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
sh_reads_escaped() { bash "$BRB_SH" -c "$T/cfg/idx" index 2>/dev/null | awk -F'\t' 'NF!=2 { b=1 } END { exit b+0 }'; }
assert0 "brb.sh reads the escaped index without splitting a row" sh_reads_escaped

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
only_extracts() { # only_extracts sh|go
  local who=$1
  local dest=$T/only-$who
  rm -rf "$dest"; mkdir -p "$dest"
  case $who in
    sh) run_sh "$LOG/only-$who.log" "$T/cfg/go" restore "$dest" --only 'project/core/src/main.c' ;;
    go) run_go "$LOG/only-$who.log" "$T/cfg/go" restore "$dest" --only 'project/core/src/main.c' ;;
  esac || return 1
  test -s "$dest/project/core/src/main.c"
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
only_dir_extracts() { # only_dir_extracts sh|go SPELLING
  local who=$1 spelling=$2
  local dest=$T/onlydir-$who
  rm -rf "$dest"; mkdir -p "$dest"
  case $who in
    sh) run_sh "$LOG/onlydir-$who.log" "$T/cfg/go" restore "$dest" --only "$spelling" ;;
    go) run_go "$LOG/onlydir-$who.log" "$T/cfg/go" restore "$dest" --only "$spelling" ;;
  esac || return 1
  test -s "$dest/project/core/src/main.c"
}
assert0 "brb.sh --only project/core extracts the directory"      only_dir_extracts sh 'project/core'
assert0 "go brb --only project/core extracts the directory"      only_dir_extracts go 'project/core'
# Both readers, both spellings: three quarters of that grid was asserted, and
# the missing cell was the Go one — in the file whose whole premise is that the
# two agree. A trailing slash has produced a Go-only divergence in this suite
# before (the destination-symlink case in section 12), and the two implementations
# reach unsquashfs by different routes: brb.sh strips the slash at parse time,
# while the Go build's covers() strips it only for the index pre-check and hands
# the operand on unchanged. If that ever stopped matching, the result would be a
# run that selects the disc, extracts nothing and reports success — the exact
# silent-empty-restore this section exists for.
assert0 "brb.sh --only project/core/ (trailing slash) works too" only_dir_extracts sh 'project/core/'
assert0 "go brb --only project/core/ (trailing slash) works too" only_dir_extracts go 'project/core/'

keep_images() { # keep_images sh|go WANT   (WANT=1 keep, 0 remove)
  local who=$1 want=$2
  local st=$T/keep-$who-$want dest=$T/outkeep-$who-$want cfg=$T/cfg/keep-$who-$want
  rm -rf "$st" "$dest"; mkdir -p "$dest"
  cp -a "$T/stage-go" "$st"; rm -rf "$st/restore"
  # Each implementation is driven the way it actually exposes the option: the
  # behaviour is what has to match, not the spelling. The Go build has no
  # KEEP_IMAGES config key and REJECTS the file outright if it sees one, so its
  # config must not carry it — that divergence is recorded on its own below.
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
  # The two tools' column padding differs and carries no information; the names
  # and the order do, so only whitespace runs are normalised.
  sed 's/  */ /g' "$a.raw" | sort > "$a"
  sed 's/  */ /g' "$b.raw" | sort > "$b"
  diff "$a" "$b" >&2
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
else
  # brb.sh's lock is opportunistic for the same reason this check is gated:
  # flock(1) is util-linux, and a machine without it still has to be able to
  # restore. Both readers then run unguarded, so there is nothing to assert.
  skip "the staging lock (both readers)" "no flock(1) to stand in for a run in progress"
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
# refuseSymlinksAtImageDirs, for the case that guard cannot see: a link
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
glob_without_the_copy_fails() { # glob_without_the_copy_fails sh|go
  local who=$1
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
    sh) bash "$BRB_SH" --yes -c "$T/cfg/g-nc-$who" restore "$dest" > "$LOG/glob-nc-sh.log" 2>&1 ;;
    go) "$BRB_GO" --yes --no-color -c "$T/cfg/g-nc-$who" restore "$dest" > "$LOG/glob-nc-go.log" 2>&1 ;;
  esac
}

for who in sh go; do
  name=$([[ $who == sh ]] && echo 'brb.sh' || echo 'go brb')
  assert0 "fixture: $name — two differently damaged copies in the metacharacter staging" glob_build "$who"
  assert0 "  ... the image alone is past its own par2 redundancy" \
    alone_is_unrepairable "$(glob_stage "$who")" "$(glob_stage "$who")/enc/disc01.squashfs.age"
  assert0 "  ... and so is the second pressing alone" \
    alone_is_unrepairable "$(glob_stage "$who")" "$(glob_stage "$who")/enc/disc01.squashfs.age.copy1700000000"
  assertN "  ... so neither copy alone can restore the set"                glob_without_the_copy_fails "$who"
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
  mkdir -p "$ESCSRC/dir"
  printf 'x' > "$ESCSRC/$ESC_EVIL"
  printf 'y' > "$ESCSRC/dir/$ESC_DEL"
  printf 'z' > "$ESCSRC/plain.txt"
  ESCSRC_FILES=3
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
  # list's column padding differs between the two and carries no information,
  # the same normalisation section 7 uses.
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
xassert0 "go brb honours KEEP_IMAGES from the config file" \
  "brb.sh takes KEEP_IMAGES from the config; the Go build exposes only a --keep-images flag, so one config file cannot drive both readers the same way" \
  go_reads_keep_images_from_config

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

printf '\n%d passed, %d failed, %d xfail (known divergences), %d skipped\n' \
  "$pass_n" "$fail_n" "$xfail_n" "$skip_n"
if ((fail_n)); then printf '\nfailures:\n'; printf '  - %s\n' "${FAILURES[@]}"; exit 1; fi
printf 'the format contract holds; %d known divergence(s) remain.\n' "$xfail_n"
