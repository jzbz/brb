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
# reports something other than the thing being tested. What makes that safe here
# is NOT that the two halves share a line: about fifty of the sixty-five sites
# are written on two lines and always have been, whatever this header used to
# say. It is that the ck call is the very next STATEMENT after its condition —
# same line or the one below, with nothing but comments between. That is a
# convention rather than something a linter can hold you to, which is why the
# disable is file-wide and why the rule is written down here. If you ever need
# something between a condition and its ck, capture the status into a variable
# first, the way the symlink-destination check further down does with symrc.
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

# kill_after_first_disc CFG STATE LOG — run a backup until state.json records a
# completed disc, kill the whole run, and report in KILLED_AT the disc count it
# reached (empty if the kill never landed).
#
# One harness, two callers. The public-archive section further down needs the
# same interruption and used to carry its own character-for-character copy of
# it: the same sed, the same 4000-iteration poll at 0.05s, the same kill of the
# process group. This is the least deterministic code in the suite and the part
# most likely to need retuning when a runner gets slower, and a tuning applied
# to one copy would silently not apply to the other. What the callers keep is
# their own not-killed branch, which is the one place they deliberately differ:
# the resume section cannot prove anything without the kill and gives up, while
# the public section records a failure and carries on.
#
# The count comes back in a global rather than on stdout: a command substitution
# would put the backgrounded backup in a subshell whose pipe the caller then
# holds open, and this is not the place to be clever with job control.
KILLED_AT=""
kill_after_first_disc() {
  local cfg=$1 state=$2 log=$3 pid n=""
  KILLED_AT=""
  # Own process group: a single kill has to take the whole tree down, or a
  # surviving child still writing into staging would make the resume prove
  # nothing.
  set -m
  "$BRB" --yes -c "$cfg" backup </dev/null > "$log" 2>&1 &
  pid=$!
  set +m
  for _ in $(seq 1 4000); do
    # discs_done out of state.json, without needing jq. A state file that does
    # not exist yet reads as empty, which is the same as "no disc finished".
    n="$(sed -n 's/.*"discs_done"[[:space:]]*:[[:space:]]*\([0-9]\+\).*/\1/p' "$state" 2>/dev/null | head -1)"
    if [[ -n "$n" ]] && (( n >= 1 )); then
      KILLED_AT="$n"
      kill -9 -- "-$pid" 2>/dev/null
      break
    fi
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.05
  done
  wait "$pid" 2>/dev/null
}

sect "kill a multi-disc backup mid-set"
kill_after_first_disc "$W/cfg/config" "$STATE" "$W/run1.log"
killed_at=$KILLED_AT
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
# Matched against resumeFilter's own sentence and nothing else. Two alternatives
# have been struck off this pattern for the same reason: '|resum' was a strict
# superset of the "says it resumed" check above, and '|already written' was
# satisfied by prepareState's resume banner ("resuming after N completed
# disc(s), M file(s) already written", preflight.go), which prints on every
# ordinary --resume before resumeFilter is ever reached. Either one could only
# fail once some other check had already failed, so it certified nothing of its
# own. resumeFilter prints "resume: N file(s) already on disc, M still to write"
# and it is the only thing that does.
grep -qF 'file(s) already on disc' "$W/run2.log"
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
# Was an XFAIL: protect() swept stale parity with filepath.Glob over a pattern
# that included the staging directory, so a path holding [ ] * or ? matched
# nothing and par2 create died on the survivors with 'Par2 file already exists'.
# The sweeps (run.go protect and protectSidecars, tools.removePar2) list the
# directory and match the base name now, so this is an ordinary assertion.
ck "a rebuild sweeps the stale recovery set away and exits 0 (metacharacter path)" $?

sect "a --public-archive backup killed mid-set resumes with the SAME key"
# The defect this pins was silent and permanent: the minted key lived only in
# process memory until the end of a completed run, so an interrupted public
# backup took it to the grave and the resume minted another, encrypted the rest
# of the set to that, and stamped it onto every disc — including the discs
# whose images were encrypted to the dead key. Those verified clean and were
# undecryptable forever. The key is persisted to staging before disc 1 now,
# and state.json records the mode and the public key; this proves it, with a
# real kill -9 at exactly the window the defect lived in.
PUBW=$W/stage-pub
mkdir -p "$PUBW" "$W/cfg/pubkeys"
# The recipients path points into an empty directory on purpose: a public set
# must need no key material on the machine, and the restore below must find
# the key in staging rather than beside a recipients file.
cat > "$W/cfg/public" <<EOF
SOURCE_DIR="$W/src"
STAGING="$PUBW"
AGE_RECIPIENTS_FILE="$W/cfg/pubkeys/no-such-recipients.txt"
DISC_CAPACITY_BYTES=40000000
RESERVE_BYTES=12000000
COMPRESSION=none
ARCHIVE_NAME="go-e2e-public"
PUBLIC_ARCHIVE=1
EOF
pub_state=$PUBW/state.json
kill_after_first_disc "$W/cfg/public" "$pub_state" "$W/pub-run1.log"
pub_killed_at=$KILLED_AT
[[ -n "$pub_killed_at" ]] && (( pub_killed_at >= 1 ))
ck "public backup was killed after disc ${pub_killed_at:-?} completed" $?
if [[ -z "$pub_killed_at" ]]; then
  printf '\ncould not interrupt the public run; the rest of this section would prove nothing\n'
  fail=$((fail+1))
else
  key_before=$(grep -o 'AGE-SECRET-KEY-1[A-Z0-9]*' "$PUBW/enc/identity.txt" 2>/dev/null | head -1)
  [[ -n "$key_before" ]]
  ck "  ... and its key was already on disk in staging before the kill" $?
  grep -q '"public_archive"' "$pub_state" && grep -q '"public_key"' "$pub_state"
  ck "  ... and state.json records the mode and the public key" $?

  # Forgetting the mode must be refused, not quietly turned into an ordinary
  # set. PUBLIC_ARCHIVE=1 lives in the config file here, so the flagless case is
  # driven with the setting switched off in a copy of it — and with a REAL
  # recipients file, because an ordinary-mode preflight reads that file before
  # it ever looks at the resume state: without one the run is refused for the
  # missing file, and the assertion below that the refusal names the flag is
  # what tells that wrong reason from the right one.
  sed -e 's/^PUBLIC_ARCHIVE=1$/PUBLIC_ARCHIVE=0/' \
      -e "s|^AGE_RECIPIENTS_FILE=.*|AGE_RECIPIENTS_FILE=\"$W/cfg/recipients.txt\"|" \
      "$W/cfg/public" > "$W/cfg/public-off"
  "$BRB" --yes -c "$W/cfg/public-off" backup --resume </dev/null > "$W/pub-off.log" 2>&1
  (( $? != 0 )); ck "resuming with PUBLIC_ARCHIVE turned off is refused" $?
  grep -q -- '--public-archive' "$W/pub-off.log"
  ck "  ... and the refusal names the flag to pass" $?

  "$BRB" --yes -c "$W/cfg/public" backup --resume </dev/null > "$W/pub-run2.log" 2>&1
  ck "--resume finishes the public set" $?
  grep -qi 'resumed with its recorded key' "$W/pub-run2.log"
  ck "  ... reloading the recorded key rather than minting" $?
  ! grep -qi 'a keypair was generated' "$W/pub-run2.log"
  ck "  ... (no new keypair anywhere in the log)" $?
  [[ ! -f "$pub_state" ]]; ck "  ... and removed the state file on success" $?

  n_pub=$(find "$PUBW/discs" -mindepth 1 -maxdepth 1 -type d -name 'disc*' 2>/dev/null | wc -l)
  (( n_pub >= 2 )); ck "the public set spans $n_pub discs" $?
  pub_all=0
  for d in "$PUBW"/discs/disc*; do
    k=$(grep -o 'AGE-SECRET-KEY-1[A-Z0-9]*' "$d/identity.txt" 2>/dev/null | head -1)
    [[ "$k" == "$key_before" ]] || pub_all=1
    grep -q "$key_before" "$d/MANIFEST.txt" && grep -q "$key_before" "$d/README.md" || pub_all=1
    ( cd "$d" && sha512sum -c --quiet SHA512SUMS ) >/dev/null 2>&1 || pub_all=1
    img=$(basename "$d"); img="$d/data/$img.squashfs.age"
    age -d -i "$d/identity.txt" -o /dev/null "$img" >/dev/null 2>&1 || pub_all=1
  done
  ck "every disc — before and after the kill — carries the ORIGINAL key and decrypts with it" "$pub_all"

  # And the tool itself opens the set with no identity configured anywhere:
  # the key it finds is the one the writer left in staging.
  "$BRB" --yes -c "$W/cfg/public" restore "$W/pub-out" > "$W/pub-restore.log" 2>&1
  ck "brb restore opens the public set from staging with no configured identity" $?
  diff -r --no-dereference "$W/src" "$W/pub-out" >/dev/null 2>&1
  ck "  ... byte-identical to the source" $?
fi

sect "a disc that does not belong to the set"
# age encrypts to a PUBLIC key, and MANIFEST.txt on every disc prints the
# recipients the set was encrypted to. Anyone holding ONE disc can therefore
# pack a squashfs image of their own choosing, encrypt it to that key, write
# its .sha512 sidecars, its par2 volumes and a SHA512SUMS over the lot, and
# hand back a disc that decrypts, verifies clean, passes par2 and is extracted
# by 'unsquashfs -f' into the operator's destination. None of it needs the
# private key, and nothing on the read path used to ask whether a disc
# belonged to the operator's SET.
#
# Two halves answer it, and neither is any use alone: the index staged from the
# first disc is PINNED, which forces an attacker to ship the genuine index
# (they cannot forge one they cannot read); and each image is CROSS-CHECKED
# against the rows that index gives its disc, which the forged image then
# cannot satisfy. What it catches is that the discs of a set DISAGREE — not
# which of them is lying, and nothing at all about a single disc forged in both
# halves at once. It is not a signature; this format has no signing key.
#
# xcompat-test.sh owns the both-readers half of this, including the pin at
# ingest, which needs a pty. What only this suite has is a set this
# implementation wrote itself, forged with the same tools an attacker would
# reach for.
#
# The patterns below are how the checks read the reader's messages, kept in one
# place so a wording that lands differently is one edit to reconcile. Each is
# asserted both ways — present where a refusal or a degradation is required,
# absent from the genuine restore this suite already made — so a pattern too
# loose fails one half and one too tight fails the other.
XCHK_RX='index.*(disagree|differ|does not|do not|not list|unexpected)|(disagree|differ|does not|do not|not list|unexpected).*index'
DEGRADE_RX='newline|cross-?check'

# The genuine restore of the resumed multi-disc set, up at the top of this
# file, is the regression guard: it already proved the tree comes back
# byte-identical. What is added here is that it came back QUIETLY. A
# cross-check that refuses, or silently gives up on, a legitimate set has taken
# the tool away from its operator — a worse outcome than the forgery it guards
# against — and the source tree that restore covered holds a filename with a
# TAB in it, which the index escapes as \t and 'unsquashfs -ll' prints
# literally. A comparison that forgot to unescape the index rows would refuse
# that disc; one that degraded on every escaped name would quietly check
# nothing. A tab is not a newline, and neither reader may treat it as one.
! grep -qiE "$XCHK_RX" "$W/restore.log"
ck "the genuine multi-disc restore claimed no disagreement between image and index" $?
! grep -qiE "$DEGRADE_RX" "$W/restore.log"
ck "  ... and did not degrade the check over the filename holding a tab" $?

# A set of its own to forge a disc of: small, but genuinely multi-disc, because
# the refusal has to hold on a disc reached AFTER a good one has been read and
# extracted — which is the shape of the attack, one disc of a set that is
# otherwise the operator's own.
FG=$W/fg
mkdir -p "$FG/src"
for i in $(seq -w 1 12); do head -c 3000000 /dev/urandom > "$FG/src/blob-$i.bin"; done
cat > "$W/cfg/forge" <<EOF
SOURCE_DIR="$FG/src"
STAGING="$FG/stage"
AGE_RECIPIENTS_FILE="$W/cfg/recipients.txt"
AGE_IDENTITY="$W/cfg/identity.txt"
DISC_CAPACITY_BYTES=40000000
RESERVE_BYTES=12000000
PAR2_BLOCKS=40
ARCHIVE_NAME="go-forge"
EOF
"$BRB" --yes -c "$W/cfg/forge" backup </dev/null > "$W/fg-backup.log" 2>&1
ck "fixture: a set to forge a disc of" $?
n_fg=$(find "$FG/stage/discs" -mindepth 1 -maxdepth 1 -type d -name 'disc*' 2>/dev/null | wc -l)
(( n_fg >= 2 )); ck "  ... spanning $n_fg discs" $?
FG_N=2   # the disc whose image is replaced

# The index's paths for one disc, spelled the way the index spells them.
fg_rows() { # fg_rows DISC
  age -d -i "$W/cfg/identity.txt" "$FG/stage/enc/index.tsv.gz.age" 2>/dev/null \
    | gzip -dc 2>/dev/null | awk -F'\t' -v d="$1" '$1+0==d { print $2 }'
}

# forge_image OUT NAME... — a squashfs image whose root holds exactly the named
# regular files, with contents that were never in anybody's backup. mksquashfs
# without -keep-as-directory puts the directory's CONTENTS at the image root,
# which is the shape brb's own images have: 'unsquashfs -ll' prints them as
# squashfs-root/<name>.
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

# forge_staging TAG IMAGE — a private copy of the set's staging with disc
# $FG_N's image replaced by IMAGE and every artifact a reader checks
# regenerated over it: the ciphertext sidecar, the plaintext sidecar and the
# par2 set. All of it with the public key out of the recipients file, which is
# the same key MANIFEST.txt prints on every disc.
#
# The result is then verified the way a reader will, ciphertext and plaintext
# both, and this fails if any of it comes back damaged: a refusal that could be
# explained by a corrupt image would say nothing about the cross-check.
forge_staging() {
  local tag=$1 img=$2
  local st=$FG/$tag enc base tmp
  base=$(printf 'disc%02d.squashfs' "$FG_N")
  rm -rf "$st"
  cp -a "$FG/stage" "$st" || return 1
  rm -rf "$st/restore"
  enc=$st/enc; tmp=$st/.forge
  [[ -f "$enc/$base.age" ]] || { echo "fixture: no $base.age in staging to forge over" >&2; return 1; }
  mkdir -p "$tmp" || return 1
  cp -f "$img" "$tmp/$base" || return 1
  rm -f "$enc/$base.age"
  age -e -R "$W/cfg/recipients.txt" -o "$enc/$base.age" "$tmp/$base" || return 1
  ( cd "$enc" && sha512sum "$base.age" ) > "$enc/$base.age.sha512" || return 1
  ( cd "$tmp" && sha512sum "$base" ) > "$enc/$base.sha512" || return 1
  rm -f "$enc/$base".age*.par2
  ( cd "$enc" && par2 create -q -q -b40 -- "$base.age.par2" "$base.age" ) >/dev/null 2>&1 || return 1
  ( cd "$enc" && sha512sum -c --quiet "$base.age.sha512" ) >/dev/null 2>&1 || return 1
  ( cd "$enc" && par2 verify -q -- "$base.age.par2" ) >/dev/null 2>&1 || return 1
  rm -f "$tmp/$base"
  age -d -i "$W/cfg/identity.txt" -o "$tmp/$base" "$enc/$base.age" || return 1
  ( cd "$tmp" && sha512sum -c --quiet "$enc/$base.sha512" ) >/dev/null 2>&1 || return 1
  rm -rf "$tmp"
  cat > "$W/cfg/$tag" <<EOF
SOURCE_DIR="$FG/src"
STAGING="$st"
AGE_RECIPIENTS_FILE="$W/cfg/recipients.txt"
AGE_IDENTITY="$W/cfg/identity.txt"
DISC_CAPACITY_BYTES=40000000
RESERVE_BYTES=12000000
PAR2_BLOCKS=40
ARCHIVE_NAME="go-forge"
EOF
}

# The attacker's disc: two files nobody backed up, and — deliberately — one
# path the index really does give this disc. The two strangers are what the
# cross-check must catch. The third is what makes the --only check below mean
# anything: a reader asks an image whether it holds the requested path before
# extracting it, and an image holding none of it is skipped without being
# compared with anything at all. An attacker who wants their bytes handed to an
# operator who typed --only names their file after a file the index says is
# there, so that is the disc this fixture builds.
fg_one=$(fg_rows "$FG_N" | head -1)
[[ -n "$fg_one" && "$fg_one" != *[\\/]* ]]
ck "fixture: the index gives disc $FG_N a flat path to ask for ($fg_one)" $?
forge_image "$FG/img-alien" 'passwd' 'authorized_keys' "$fg_one"
ck "fixture: a squashfs holding two files nobody backed up, plus that one" $?
fg_rows "$FG_N" | grep -qxE 'passwd|authorized_keys'
(( $? != 0 )); ck "  ... and the index gives disc $FG_N neither of the strangers" $?
forge_staging alien "$FG/img-alien"
ck "fixture: it is disc $FG_N's image now, and sidecars and par2 all accept it" $?

# The destination is NOT empty afterwards and must not be asserted to be: disc
# 1 is genuine and is extracted before the forged disc is reached. The property
# is that nothing the attacker chose came out of it.
rm -rf "$W/fg-out"; mkdir -p "$W/fg-out"
"$BRB" --yes --no-color -c "$W/cfg/alien" restore "$W/fg-out" > "$W/fg-alien.log" 2>&1
(( $? != 0 )); ck "restore refuses a disc whose image is not what the index says it holds" $?
(( $(find "$W/fg-out" \( -name passwd -o -name authorized_keys \) | wc -l) == 0 ))
ck "  ... and not one file the forged image chose was written" $?
grep -qiE "$XCHK_RX" "$W/fg-alien.log"
ck "  ... saying the image and the index disagree" $?

# --only narrows the EXTRACTION, not the disc: the image still holds the whole
# disc, so the whole of it is still compared with the whole of that disc's
# index rows however few paths were asked for. Nothing narrows this disc away
# — it holds the path being asked for — so without the cross-check this run
# ends 0 having written the attacker's version of a file the operator named.
# That is why it is not enough for it to exit non-zero: the file must not be
# there either.
rm -rf "$W/fg-out-only"; mkdir -p "$W/fg-out-only"
"$BRB" --yes --no-color -c "$W/cfg/alien" restore "$W/fg-out-only" --only "$fg_one" > "$W/fg-only.log" 2>&1
(( $? != 0 )); ck "restore --only is refused by the same disc" $?
[[ ! -e "$W/fg-out-only/$fg_one" ]]
ck "  ... and the forged $fg_one it asked for was not written" $?

# The limit, written down as a passing check rather than left implied: a forged
# image whose files agree with the index is extracted, because with one disc
# forged in both halves there is nothing left to compare it against. Overselling
# this guard would be worse than not having it.
mapfile -t fg_agree < <(fg_rows "$FG_N")
(( ${#fg_agree[@]} >= 2 ))
ck "fixture: the index gives disc $FG_N ${#fg_agree[@]} row(s)" $?
printf '%s\n' "${fg_agree[@]}" | grep -q '[\\/]'
(( $? != 0 )); ck "  ... all of them flat, unescaped names this fixture can use as filenames" $?
forge_image "$FG/img-agreeing" "${fg_agree[@]}"
ck "fixture: an image holding exactly those paths, with contents nobody backed up" $?
forge_staging agreeing "$FG/img-agreeing"
ck "fixture: it is disc $FG_N's image now, and sidecars and par2 all accept it" $?
rm -rf "$W/fg-out-agree"; mkdir -p "$W/fg-out-agree"
"$BRB" --yes --no-color -c "$W/cfg/agreeing" restore "$W/fg-out-agree" > "$W/fg-agree.log" 2>&1
ck "a forgery whose files agree with the index IS extracted — the limit this guard does not cover" $?
[[ -s "$W/fg-out-agree/$fg_one" ]] && ! cmp -s "$FG/src/$fg_one" "$W/fg-out-agree/$fg_one"
ck "  ... and what came out is the forgery's content, not the backup's" $?

sect "a filename with a newline degrades the cross-check, and never refuses"
# 'unsquashfs -ll' is line-based, so a file called "new<newline>line.txt" is
# listed as two half-lines and no line-based listing can carry it faithfully —
# which is why the index has an escaping contract at all. The cross-check has
# to notice and say so, and must NOT refuse: a backup tool that cannot restore
# a legitimate set is worse than one that misses an exotic attack. Its own tiny
# set, because a newline in a name breaks 'find | wc -l' and every count this
# suite takes of the main source tree.
NL=$W/nl
mkdir -p "$NL/src"
printf 'y' > "$NL/src/$(printf 'new\nline.txt')"
printf 'w' > "$NL/src/plain.txt"
head -c 300000 /dev/urandom > "$NL/src/blob.bin"
(( $(find "$NL/src" -type f -printf 'x' | wc -c) == 3 ))
ck "fixture: three files, one of them named across a line break" $?
cat > "$W/cfg/nl" <<EOF
SOURCE_DIR="$NL/src"
STAGING="$NL/stage"
AGE_RECIPIENTS_FILE="$W/cfg/recipients.txt"
AGE_IDENTITY="$W/cfg/identity.txt"
DISC_CAPACITY_BYTES=40000000
RESERVE_BYTES=12000000
PAR2_BLOCKS=40
ARCHIVE_NAME="go-newline"
EOF
"$BRB" --yes -c "$W/cfg/nl" backup </dev/null > "$W/nl-backup.log" 2>&1
ck "a backup of it exits 0" $?
mkdir -p "$NL/out"
"$BRB" --yes --no-color -c "$W/cfg/nl" restore "$NL/out" > "$W/nl-restore.log" 2>&1
ck "and it still restores" $?
diff -r --no-dereference "$NL/src" "$NL/out" >/dev/null 2>&1
ck "  ... byte-identical to the source" $?
grep -qiE "$DEGRADE_RX" "$W/nl-restore.log"
ck "  ... having said the cross-check could not be made exact for that disc" $?

printf '\n%d passed, %d failed, %d xfail\n' "$pass" "$fail" "$xfail"
(( fail == 0 ))
