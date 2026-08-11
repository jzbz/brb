#!/usr/bin/env bash
#
# go-e2e-test.sh — end-to-end regression suite for the Go implementation.
#
# Counterpart to xcompat-test.sh, which owns the format contract between the
# two implementations. This one's focus is the things only a real run can
# prove: that an interrupted multi-disc backup resumes correctly, and that the
# resumed set restores byte-identical to its source.
#
# Usage: ./go-e2e-test.sh [path-to-go-brb]
# Exit 0 when every assertion passed.
#
# Needs: mksquashfs, unsquashfs, age, age-keygen, par2, xorriso, and a built
# Go binary. No optical drive and no root.
#
# MIT licensed — see the LICENSE file.
#
# This suite asserts by running a condition and handing its exit status to ck:
#
#     (( imgs_before >= 1 )); ck "  ... with $imgs_before image(s)" $?
#
# which is what SC2319 and SC2181 exist to question, because a $? read too late
# reports something other than the thing being tested. Here the condition and
# the ck call are always one line with a single ; between them, so nothing can
# get in between to overwrite the status. Disabled for the file rather than at
# seventeen separate sites; keep the two halves on one line and it stays true.
# shellcheck disable=SC2319,SC2181
set -uo pipefail

# The binary under test: first argument, else $BRB_GO_BIN, else one built
# beside this script. No absolute path baked in — the tree has moved once and
# this is a public repo, so a hardcoded home directory is just a stale default
# that reads like a missing tool.
HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
BRB="${1:-${BRB_GO_BIN:-$HERE/brb}}"
[[ -x "$BRB" ]] || { echo "no executable Go brb at: $BRB" >&2; exit 2; }

W="$(mktemp -d)"
trap 'rm -rf "$W"' EXIT

pass=0 fail=0 xfail=0
ck() { if (( $2 == 0 )); then printf '  PASS  %s\n' "$1"; pass=$((pass+1));
       else printf '  FAIL  %s\n' "$1"; fail=$((fail+1)); fi; }
# xck DESC RC WHY — an assertion written the way it OUGHT to pass, against a
# defect that is real, reproduced, and not yet fixed. Same convention
# xcompat-test.sh uses: a non-zero rc is XFAIL and does not fail the run, and a
# zero rc is XPASS and DOES, because this file is then out of date and the
# check should become a plain ck. Nothing known-broken is quietly omitted.
xck() { if (( $2 != 0 )); then printf '  XFAIL %s\n        %s\n' "$1" "$3"; xfail=$((xfail+1));
        else printf '  XPASS %s\n        this now passes — turn the xck into a ck\n' "$1"; fail=$((fail+1)); fi; }
sect() { printf '\n== %s\n' "$1"; }

for t in mksquashfs unsquashfs age age-keygen par2 xorriso; do
  command -v "$t" >/dev/null 2>&1 || { echo "required tool missing: $t" >&2; exit 2; }
done

STATE=""   # set once we know the staging directory

mkdir -p "$W"/{src,cfg,out}
# Enough incompressible data to need several discs AND to take long enough that
# the run can actually be caught mid-set. Too small and the backup finishes
# before the watcher sees the first completed disc, which proves nothing.
for i in $(seq -w 1 40); do head -c 3000000 /dev/urandom > "$W/src/blob-$i.bin"; done
# One awkward name, so the resume is also carrying escaped index rows across.
printf 'x' > "$W/src/tab	and space.txt"
# One real subdirectory. The destination-symlink check further down plants its
# link under this name: a link the archive has no directory for is refused just
# the same, but nothing could ever escape through it, and the "wrote nothing
# outside the destination" half of that check would be measuring nothing.
mkdir -p "$W/src/sub"
head -c 200000 /dev/urandom > "$W/src/sub/deep.bin"

age-keygen -o "$W/cfg/identity.txt" 2>/dev/null
grep -o 'age1[0-9a-z]*' "$W/cfg/identity.txt" | tail -1 > "$W/cfg/recipients.txt"
cat > "$W/cfg/config" <<EOF
SOURCE_DIR="$W/src"
STAGING="$W/stage"
AGE_RECIPIENTS_FILE="$W/cfg/recipients.txt"
AGE_IDENTITY="$W/cfg/identity.txt"
DISC_CAPACITY_BYTES=40000000
RESERVE_BYTES=12000000
PAR2_BLOCKS=40
ARCHIVE_NAME="go-resume"
EOF
# RESERVE_BYTES has to clear the on-disc tool copy, and this implementation
# ships itself as a multi-megabyte static binary where brb.sh ships a ~114 KB
# script. A 2 MB reserve is ample for e2e-test.sh and is refused outright here —
# which is the HL-4 preflight check doing its job, not a test failure.
export BRB_CONFIG="$W/cfg/config"
STATE="$W/stage/state.json"

discs_done() { # read discs_done out of state.json without needing jq
  [[ -f "$STATE" ]] || return 1
  sed -n 's/.*"discs_done"[[:space:]]*:[[:space:]]*\([0-9]\+\).*/\1/p' "$STATE" | head -1
}

sect "kill a multi-disc backup mid-set"
# Own process group: a single kill has to take the whole tree down, or a
# surviving child still writing into staging would make the resume prove nothing.
set -m
"$BRB" --yes backup </dev/null > "$W/run1.log" 2>&1 &
bpid=$!
set +m

killed_at=""
for _ in $(seq 1 4000); do
  n="$(discs_done 2>/dev/null || true)"
  if [[ -n "$n" ]] && (( n >= 1 )); then
    killed_at="$n"
    kill -9 -- "-$bpid" 2>/dev/null
    break
  fi
  kill -0 "$bpid" 2>/dev/null || break
  sleep 0.05
done
wait "$bpid" 2>/dev/null

[[ -n "$killed_at" ]] && (( killed_at >= 1 ))
ck "backup was killed after disc ${killed_at:-?} completed" $?

if [[ -z "$killed_at" ]]; then
  printf '\ncould not interrupt the run; nothing further can be proven\n'
  printf '%d passed, %d failed\n' "$pass" "$((fail+1))"
  exit 1
fi

# The set is genuinely unfinished.
imgs_before=$(find "$W/stage/enc" -maxdepth 1 -name 'disc*.squashfs.age' 2>/dev/null | wc -l)
(( imgs_before >= 1 )); ck "  ... with $imgs_before disc image(s) already protected" $?
before="$(sha512sum "$W"/stage/enc/disc*.squashfs.age 2>/dev/null | LC_ALL=C sort)"
[[ -n "$before" ]]; ck "  ... whose ciphertext could be fingerprinted" $?

sect "a plain backup must not silently discard the interrupted set"
"$BRB" --yes backup </dev/null > "$W/noresume.log" 2>&1
(( $? != 0 )); ck "plain backup refuses to start over an interrupted set" $?
after_refuse="$(sha512sum "$W"/stage/enc/disc*.squashfs.age 2>/dev/null | LC_ALL=C sort)"
[[ "$before" == "$after_refuse" ]]; ck "  ... and left the finished discs untouched" $?

sect "--resume guards against resuming into a different set"
ARCHIVE_NAME=something-else "$BRB" --yes backup --resume </dev/null > "$W/mismatch.log" 2>&1
(( $? != 0 )); ck "--resume refuses when ARCHIVE_NAME disagrees with the state" $?

sect "--resume finishes the set"
"$BRB" --yes backup --resume </dev/null > "$W/run2.log" 2>&1
ck "--resume exits 0" $?
grep -qiE 'resum' "$W/run2.log"; ck "  ... and says it resumed" $?
# DIVERGENCE, deliberate on both sides, asserted rather than assumed:
# brb.sh reuses the original scan so a set can never be half-old and half-new.
# This implementation re-scans and filters out what is already on a disc
# (resumeFilter), which picks up files added since the run started and warns
# when the tree's measured size has changed. The property that must hold either
# way is that the resume re-seeds from what was already written rather than
# starting over, which the byte-identical check below proves.
# No '|resum' alternative here: with it this pattern was a strict superset of
# line 117's, so it could only fail when that one had already failed and it
# certified nothing of its own. resumeFilter prints "resume: N file(s) already
# on disc, M still to write" unconditionally, so the tightened pattern passes
# today and can genuinely fail tomorrow.
grep -qiE 'already (on disc|written)' "$W/run2.log"
ck "  ... re-seeding from the discs already written" $?
[[ ! -f "$STATE" ]]; ck "  ... and removed the state file on success" $?

sect "the discs built before the kill were not rebuilt"
names=$(printf '%s\n' "$before" | awk '{print $2}')
# shellcheck disable=SC2086  # $names is deliberately split into one argument
# per disc image. The names are brb's own (discNN.squashfs.age), so they carry
# no whitespace, and the [[ -n "$after" ]] below refuses to let a split that
# went wrong pass as agreement.
after="$(sha512sum $names 2>/dev/null | LC_ALL=C sort)"
[[ -n "$after" && "$before" == "$after" ]]
ck "already-protected discs are byte-identical after the resume" $?

sect "the finished set is complete and restores"
# -mindepth 1: find reports the starting directory too, and it is called
# "discs", which matches disc* — without this the set always looked one disc
# bigger than it is.
n_disc=$(find "$W/stage/discs" -mindepth 1 -maxdepth 1 -type d -name 'disc*' 2>/dev/null | wc -l)
(( n_disc >= 2 )); ck "the set spans $n_disc discs" $?
allok=0
for d in "$W"/stage/discs/disc*; do
  ( cd "$d" && sha512sum -c --quiet SHA512SUMS ) >/dev/null 2>&1 || allok=1
  ( cd "$d/data" && par2 verify -q -- disc*.squashfs.age.par2 ) >/dev/null 2>&1 || allok=1
  ( cd "$d/data" && par2 verify -q -- sidecars.par2 ) >/dev/null 2>&1 || allok=1
done
ck "every disc verifies (SHA512SUMS, image par2, sidecars par2)" "$allok"

"$BRB" --yes restore "$W/out" > "$W/restore.log" 2>&1
ck "the resumed set restores" $?
diff -r --no-dereference "$W/src" "$W/out" >/dev/null 2>&1
ck "  ... byte-identical to the source" $?
[[ -f "$W/out/tab	and space.txt" ]]
ck "  ... including the file whose name holds a tab" $?

sect "the index survived the interruption intact"
idx=$("$BRB" index 2>/dev/null | wc -l)
src=$(find "$W/src" -type f | wc -l)
(( idx == src )); ck "index has one row per file ($idx rows, $src files)" $?
"$BRB" index 2>/dev/null | awk -F'\t' 'NF!=2 { exit 1 }'
ck "  ... every row exactly two tab-separated fields" $?
"$BRB" index 2>/dev/null | awk -F'\t' -v m="$n_disc" '$1 !~ /^[0-9]+$/ || $1+0<1 || $1+0>m { exit 1 }'
ck "  ... naming only discs that exist" $?

sect "ISOs are built from the set, and hold it"
# xorriso is a required tool for this suite and, until now, nothing here ever
# ran it: ISO_MODE defaults to ondemand, so a plain backup builds no image, and
# the iso command was invoked by no shell suite at all. The unit tests cover
# range parsing and labels; what only a real run proves is that the ISO of a
# finished disc carries that disc's tree.
n_iso_before=$(find "$W/stage/iso" -maxdepth 1 -name 'disc*.iso' 2>/dev/null | wc -l)
(( n_iso_before == 0 )); ck "the default ISO_MODE=ondemand backup built no ISO" $?

"$BRB" --yes iso all > "$W/iso.log" 2>&1
ck "brb iso all exits 0" $?
n_iso=$(find "$W/stage/iso" -maxdepth 1 -name 'disc*.iso' 2>/dev/null | wc -l)
(( n_iso == n_disc )); ck "  ... one ISO per disc ($n_iso of $n_disc)" $?

# The ISO 9660 primary volume descriptor's volume identifier is 32 bytes at
# offset 0x8028. Read straight out of the file, so the assertion does not depend
# on the tool that wrote it — and the "OF nn" half proves the total came from
# the set rather than from whatever staging happened to hold.
iso_label() { dd if="$1" bs=1 skip=32808 count=32 2>/dev/null | tr -d '\0' | sed 's/ *$//'; }
want_label="$(printf 'BACKUP_01_OF_%02d' "$n_disc")"
got_label="$(iso_label "$W/stage/iso/disc01.iso")"
[[ "$got_label" == "$want_label" ]]
ck "  ... disc01.iso is labelled $want_label (got '${got_label:-nothing}')" $?

rm -rf "$W/isox"; mkdir -p "$W/isox"
xorriso -osirrox on -indev "$W/stage/iso/disc01.iso" -extract / "$W/isox" >"$W/osirrox.log" 2>&1
ck "  ... and it extracts with xorriso -osirrox" $?
diff -r "$W/stage/discs/disc01" "$W/isox" >/dev/null 2>&1
ck "  ... to exactly the disc directory it was built from" $?
# osirrox restores the recorded modes, and data/ on a disc is read-only — so
# the extraction cannot be deleted until the write bit is back. Without this the
# EXIT trap's rm fails file by file and the whole work tree survives the run.
chmod -R u+w "$W/isox" 2>/dev/null || true

sect "ISO_MODE=eager builds each ISO during the backup"
# One config knob on a deliberately tiny fixture: the point is that the backup
# path itself materialises the image, which the ondemand assertion above cannot
# show. Its own staging, so nothing here disturbs the resumed set.
mkdir -p "$W"/eager/src
head -c 400000 /dev/urandom > "$W/eager/src/small.bin"
cat > "$W/cfg/eager" <<EOF
SOURCE_DIR="$W/eager/src"
STAGING="$W/eager/stage"
AGE_RECIPIENTS_FILE="$W/cfg/recipients.txt"
AGE_IDENTITY="$W/cfg/identity.txt"
DISC_CAPACITY_BYTES=40000000
RESERVE_BYTES=12000000
PAR2_BLOCKS=40
ARCHIVE_NAME="go-eager"
ISO_MODE=eager
EOF
"$BRB" --yes -c "$W/cfg/eager" backup </dev/null > "$W/eager.log" 2>&1
ck "an ISO_MODE=eager backup exits 0" $?
[[ -s "$W/eager/stage/iso/disc01.iso" ]]
ck "  ... and left disc01.iso in staging without an iso command being run" $?
[[ "$(iso_label "$W/eager/stage/iso/disc01.iso")" == "BACKUP_01_OF_01" ]]
ck "  ... labelled BACKUP_01_OF_01" $?

sect "a symlinked directory planted in the DESTINATION is not followed"
# Every symlink this project tests lives in the SOURCE. Nothing asked what
# happens when one is already sitting in the destination: unsquashfs runs with
# -f, and -f follows a symlink that resolves to a directory, so the archive is
# written wherever it points — outside the destination, with the restore's
# privileges. Run here, against the real multi-disc set the resume produced,
# because the guard fires once before the FIRST image and a one-disc fixture
# cannot show that discs 2..N stay behind it.
#
# The victim lives outside the destination and is inspected after the run:
# "the command failed" is not the property, "nothing escaped" is.
VICT="$W/victim"
fresh_victim() { rm -rf "$VICT"; mkdir -p "$VICT/keep"; printf 'canary\n' > "$VICT/canary.txt"; }
victim_entries() { find "$VICT" -mindepth 1 | wc -l; }

fresh_victim
[[ "$(victim_entries)" == 2 && "$(cat "$VICT/canary.txt")" == canary ]]
ck "fixture: a victim directory outside the destination, holding exactly 2 entries" $?

# Named sub/, which the archive really has a directory for, so unsquashfs -f
# genuinely traverses the link and sub/deep.bin lands in the victim when the
# guard is not there. (xcompat-test.sh covers the depth, relative-target,
# chained and dest-is-itself-a-symlink spellings for both readers; what only
# this suite has is a real multi-disc set.)
SYMD="$W/symdest"; rm -rf "$SYMD"; mkdir -p "$SYMD"; ln -s "$VICT" "$SYMD/sub"
[[ -L "$SYMD/sub" && -d "$SYMD/sub" && -f "$W/src/sub/deep.bin" ]]
ck "fixture: the destination holds a symlink named after a real archive directory" $?

# --yes deliberately: this refusal is not a confirmation, so --yes must not be
# able to answer it.
"$BRB" --yes restore "$SYMD" > "$W/symdest.log" 2>&1
symrc=$?
(( symrc != 0 )); ck "restore refuses the destination even under --yes" $?
grep -q 'symlink(s) to directories' "$W/symdest.log"
ck "  ... naming the symlink as the reason" $?
[[ "$(victim_entries)" == 2 && "$(cat "$VICT/canary.txt")" == canary ]]
ck "  ... and wrote nothing into the directory outside the destination" $?

# The refusal must not have overtightened into "no symlinks at all": a link to
# a FILE is safe, unsquashfs unlinks and replaces it as an entry. Named after a
# real archive file so it is genuinely in the way of the extraction.
fresh_victim
SYMF="$W/symfile"; rm -rf "$SYMF"; mkdir -p "$SYMF"; ln -s "$VICT/canary.txt" "$SYMF/blob-01.bin"
[[ -L "$SYMF/blob-01.bin" && -f "$SYMF/blob-01.bin" && ! -d "$SYMF/blob-01.bin" ]]
ck "fixture: the destination holds a symlink to a FILE outside it" $?
"$BRB" --yes restore "$SYMF" > "$W/symfile.log" 2>&1
ck "restore still runs over a symlink to a file" $?
cmp -s "$W/src/blob-01.bin" "$SYMF/blob-01.bin"
ck "  ... replacing the link with the archive's real file" $?
[[ "$(cat "$VICT/canary.txt")" == canary ]]
ck "  ... without writing through it" $?

sect "a staging path full of glob metacharacters"
# No suite ever put a metacharacter in a path, and that is where the last par2
# bug hid — an alternate-copy lookup that globbed the staging path as a pattern.
# The reader half (two differently damaged pressings combined by par2, in both
# readers) lives in xcompat-test.sh; what only this suite can show is the WRITER
# going end to end through such a path: pack, encrypt, par2, ISO and back again.
GNAME='st [a]ge*?q x'
GSTAGE="$W/g/$GNAME"
mkdir -p "$W/g/src" "$GSTAGE"
head -c 900000 /dev/urandom > "$W/g/src/blob.bin"
printf 'hello\n' > "$W/g/src/note.txt"
cat > "$W/cfg/glob" <<EOF
SOURCE_DIR="$W/g/src"
STAGING="$GSTAGE"
AGE_RECIPIENTS_FILE="$W/cfg/recipients.txt"
AGE_IDENTITY="$W/cfg/identity.txt"
DISC_CAPACITY_BYTES=40000000
RESERVE_BYTES=12000000
PAR2_BLOCKS=40
ARCHIVE_NAME="go-glob"
EOF
[[ $GNAME == *'['* && $GNAME == *']'* && $GNAME == *'*'* && $GNAME == *'?'* && $GNAME == *' '* ]]
ck "fixture: the staging directory name holds [, ], *, ? and a space" $?
"$BRB" --yes -c "$W/cfg/glob" backup </dev/null > "$W/glob.log" 2>&1
ck "a backup into that staging path exits 0" $?
[[ -s "$GSTAGE/enc/disc01.squashfs.age" && -s "$GSTAGE/discs/disc01/SHA512SUMS" ]]
ck "  ... and left a real image and a real disc directory there" $?
( cd "$GSTAGE/discs/disc01" && sha512sum -c --quiet SHA512SUMS ) >/dev/null 2>&1
ck "  ... whose SHA512SUMS verifies" $?
( cd "$GSTAGE/discs/disc01/data" && par2 verify -q -- disc01.squashfs.age.par2 ) >/dev/null 2>&1
ck "  ... and whose image par2 set verifies" $?
"$BRB" --yes -c "$W/cfg/glob" iso all > "$W/glob-iso.log" 2>&1
ck "brb iso all builds an ISO from that staging path" $?
[[ "$(iso_label "$GSTAGE/iso/disc01.iso")" == "BACKUP_01_OF_01" ]]
ck "  ... labelled BACKUP_01_OF_01" $?
mkdir -p "$W/g/out"
"$BRB" --yes -c "$W/cfg/glob" restore "$W/g/out" > "$W/glob-restore.log" 2>&1
ck "and the set restores out of that staging path" $?
diff -r "$W/g/src" "$W/g/out" >/dev/null 2>&1
ck "  ... byte-identical to the source" $?

# ...and the place a metacharacter staging path actually bites the writer: the
# stale-recovery-data sweep. par2 refuses to write over an existing recovery
# set, so a run killed inside a disc's par2 window leaves one behind and the
# resume that rebuilds that disc has to remove it first. protect() finds it with
# filepath.Glob(filepath.Join(r.dirs.Enc, base+".age*.par2")) — the staging path
# is joined INTO the pattern, so a directory called "st [a]ge*?q x" is
# interpreted rather than matched, the sweep finds nothing, and par2 create
# fails. Same shape as the reader bug this whole section exists for.
#
# The state is built rather than raced for: leaving only the previous run's
# .par2 pair in enc/ is exactly what a kill in that window leaves, and it is
# deterministic where the kill is not.
leave_only_stale_par2() { # leave_only_stale_par2 STAGING
  find "$1" -mindepth 1 -maxdepth 1 ! -name enc -exec rm -rf {} + 2>/dev/null
  find "$1/enc" -type f ! -name '*.par2' -delete 2>/dev/null
  [[ -f "$1/enc/disc01.squashfs.age.par2" ]] || return 1
  (( $(find "$1/enc" -maxdepth 1 -name 'disc01.squashfs.age.vol*.par2' | wc -l) == 1 )) || return 1
  [[ ! -e "$1/enc/disc01.squashfs.age" ]]        # the image itself must be gone
}
backup_over_stale_par2() { # backup_over_stale_par2 CFG
  "$BRB" --yes -c "$1" backup </dev/null > "$W/stale-$2.log" 2>&1
}

# The control first: the identical scenario through an ORDINARY staging path.
# Without it, an XFAIL below could just as well mean "brb cannot rebuild over a
# stale recovery set at all", which would say nothing about metacharacters.
mkdir -p "$W/g2/plain"
cat > "$W/cfg/glob-plain" <<EOF
SOURCE_DIR="$W/g/src"
STAGING="$W/g2/plain"
AGE_RECIPIENTS_FILE="$W/cfg/recipients.txt"
AGE_IDENTITY="$W/cfg/identity.txt"
DISC_CAPACITY_BYTES=40000000
RESERVE_BYTES=12000000
PAR2_BLOCKS=40
ARCHIVE_NAME="go-glob-plain"
EOF
"$BRB" --yes -c "$W/cfg/glob-plain" backup </dev/null > "$W/glob-plain.log" 2>&1
ck "fixture: a reference backup through an ordinary staging path" $?
leave_only_stale_par2 "$W/g2/plain"
ck "fixture: only the previous run's recovery set is left in that staging" $?
backup_over_stale_par2 "$W/cfg/glob-plain" plain
ck "a rebuild sweeps the stale recovery set away and exits 0 (ordinary path)" $?

leave_only_stale_par2 "$GSTAGE"
ck "fixture: only the previous run's recovery set is left in the metacharacter staging" $?
backup_over_stale_par2 "$W/cfg/glob" meta
metarc=$?
xck "a rebuild sweeps the stale recovery set away and exits 0 (metacharacter path)" "$metarc" \
  "BUG, reproduced: protect() sweeps stale parity with filepath.Glob(filepath.Join(r.dirs.Enc, ...)) at internal/backup/run.go:342, so a staging path holding [ ] * or ? is interpreted, the sweep matches nothing and par2 create dies with 'Par2 file already exists' (exit 3). protectSidecars has the same shape at run.go:575 (warn-only) and tools.removePar2 at internal/tools/par2.go:76. Fix by listing the directory instead, as restore.altCopies now does"
# Only while the defect is still there: an XFAIL that started failing for some
# OTHER reason would otherwise sit here looking like the same known bug. Skipped
# entirely once the rebuild succeeds, so a fix reports one XPASS and nothing else.
if (( metarc != 0 )); then
  grep -q 'already exists' "$W/stale-meta.log"
  ck "  ... and it is still the stale recovery set it dies on, not something else" $?
fi

printf '\n%d passed, %d failed, %d xfail\n' "$pass" "$fail" "$xfail"
(( fail == 0 ))
