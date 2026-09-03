#!/usr/bin/env bash
#
# brb (Blu-ray Backup) — the READER for independent, encrypted backup discs
#
# This script restores a disc set. It does not create one: writing discs — the
# bin packing, mksquashfs, the ISO building and the burning — lives in the Go
# implementation (go/ in the source tree, shipped on every disc as
# brb-linux-amd64 and brb-linux-aarch64).
#
# It exists as a separate, deliberately small, deliberately readable program
# because of what a restore actually looks like. Someone is holding a disc,
# possibly years from now, possibly on a rescue system, and wants to know what
# will happen to their bytes before they run anything. One file of shell can be
# read end to end in an afternoon, and `wc -l` on the copy in front of you is the
# only honest count of how much there is. An 8 MB static binary cannot be read at
# all.
#
# It is frozen against the on-disc format, so it changes only when that format
# changes. The two implementations are held to the same format by
# xcompat-test.sh, which builds a set with the Go build and reads it with this.
#
# The config file is bash, executed by this script, so it must be trusted
# exactly as much as brb.sh itself: never point -c or BRB_CONFIG at a file you
# did not write — least of all one carried on a disc.
#
# The restore path, undoing the pipeline in reverse:
#
#   par2   -> repair the ciphertext if the disc has rotted
#     age  -> decrypt the image
#       mount -o loop,ro discNN.squashfs /mnt     (or unsquashfs to extract)
#
# Every disc is independent. Losing disc 7 loses exactly the files on disc 7;
# every other disc still restores on its own. That last step needs nothing but
# the Linux kernel — no age, no par2, and none of this script.
#
# No warranty. Test your restores.
#
# ---------------------------------------------------------------------------
# MIT License
#
# Copyright (c) 2026 Jonathan Zeppettini
#
# Permission is hereby granted, free of charge, to any person obtaining a copy
# of this software and associated documentation files (the "Software"), to deal
# in the Software without restriction, including without limitation the rights
# to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
# copies of the Software, and to permit persons to whom the Software is
# furnished to do so, subject to the following conditions:
#
# The above copyright notice and this permission notice shall be included in all
# copies or substantial portions of the Software.
#
# THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
# IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
# FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
# AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
# LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
# OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
# SOFTWARE.
# ---------------------------------------------------------------------------
set -Eeuo pipefail

VERSION="0.1.1"
PROG="${0##*/}"

# ---------------------------------------------------------------------------
# Defaults (override in the config file or via environment)
# ---------------------------------------------------------------------------
CONFIG_FILE="${BRB_CONFIG:-$HOME/.config/brb/config}"
CONFIG_EXPLICIT=""                                   # set by -c, makes a missing file fatal
CLI_ASSUME_YES=""                                    # --yes, applied after the config is read

# Precedence is environment > config file > these defaults, and the config file
# is sourced long after this block runs — a plain assignment in it would beat an
# export the user made deliberately. Remember which names arrived from the
# environment so load_config can put them back. This has to happen BEFORE the
# ${VAR:-default} lines below, which would otherwise make every name look set.
BRB_ENV_OVERRIDES=()
for _v in STAGING AGE_RECIPIENTS_FILE AGE_IDENTITY BURNER KEEP_IMAGES; do
  # An if, not `&&`: on the last iteration a false test would become the for
  # loop's status and errexit would end the script before it started.
  if [[ -n "${!_v+x}" ]]; then BRB_ENV_OVERRIDES+=( "$_v=${!_v}" ); fi
done
unset _v

# Only the settings a restore uses. Everything that shaped how a set was BUILT —
# the source tree, the disc geometry, compression, the pack ratio, par2
# parameters, ISO mode — was decided when the set was written and is recorded in
# MANIFEST.txt on every disc. Re-declaring it here could only ever contradict
# what is already burned.
STAGING="${STAGING:-/var/tmp/brb}"
AGE_RECIPIENTS_FILE="${AGE_RECIPIENTS_FILE:-$HOME/.config/brb/recipients.txt}"
AGE_IDENTITY="${AGE_IDENTITY:-}"                     # the secret key; without it nothing decrypts
BURNER="${BURNER:-/dev/sr0}"                         # where discs are read back from
KEEP_IMAGES="${KEEP_IMAGES:-0}"                      # 1 keeps each decrypted image for re-runs

# ---------------------------------------------------------------------------
# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------
if [[ -t 2 ]]; then
  C_RED=$'\033[31m'; C_YEL=$'\033[33m'; C_GRN=$'\033[32m'
  C_BLU=$'\033[34m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else
  C_RED=""; C_YEL=""; C_GRN=""; C_BLU=""; C_DIM=""; C_OFF=""
fi
log()  { printf '%s==>%s %s\n' "$C_BLU" "$C_OFF" "$*" >&2; }
ok()   { printf '%s  ok%s %s\n' "$C_GRN" "$C_OFF" "$*" >&2; }
warn() { printf '%swarn%s %s\n' "$C_YEL" "$C_OFF" "$*" >&2; }
die()  { printf '%sfail%s %s\n' "$C_RED" "$C_OFF" "$*" >&2; exit 1; }
step() { printf '%s   .%s %s\n' "$C_DIM" "$C_OFF" "$*" >&2; }

ASSUME_YES="${ASSUME_YES:-0}"
confirm() {
  [[ "$ASSUME_YES" == "1" ]] && { printf '%s [auto-yes]\n' "$1" >&2; return 0; }
  local reply
  if [[ -r /dev/tty && -w /dev/tty ]]; then read -r -p "$1 [y/N] " reply </dev/tty || return 1
  elif [[ -t 0 ]]; then read -r -p "$1 [y/N] " reply || return 1
  else die "no terminal available to confirm '$1' — re-run with --yes if you mean it"
  fi
  [[ "$reply" == [yY] || "$reply" == [yY][eE][sS] ]]
}
# -r /dev/tty is true even in a session with no controlling terminal, where
# opening it fails with ENXIO — so probe the open itself rather than the mode
# bits. Anything that hands the terminal to another program (age asking for a
# passphrase, prompt_media asking for a disc) has to ask this first, or it dies
# with a bash redirection error, or worse, hangs.
have_tty() { ( exec </dev/tty ) 2>/dev/null; }

prompt_media() {
  MEDIA_REPLY=""
  # have_tty probes the open itself, so this fails with a sentence rather than a
  # bash redirection error in a session with no controlling terminal.
  local src=""
  if have_tty; then src=/dev/tty
  elif [[ -t 0 ]]; then src=/dev/stdin
  else die "swapping discs needs a terminal — this step cannot be automated"
  fi
  printf '%s\n' "$1" >&2
  read -r MEDIA_REPLY <"$src" || return 1
}

human() {
  awk -v b="$1" 'BEGIN{
    split("B KiB MiB GiB TiB", u, " ")
    i=1; while (b >= 1024 && i < 5) { b /= 1024; i++ }
    printf (i==1 ? "%.0f %s\n" : "%.2f %s\n"), b, u[i]
  }'
}
# Free bytes on the filesystem holding $1. Note -P and --output are mutually
# exclusive in GNU df. Prints nothing but still succeeds when df cannot answer:
# every caller tests the result for digits, and a command substitution that
# exits non-zero inside an assignment kills the script outright under set -e.
free_bytes() { df -B1 --output=avail "$1" 2>/dev/null | tail -1 | tr -dc '0-9' || true; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1${2:+  ($2)}"; }

# Every disc is labelled "disc 08 of 12" and its files are named disc08.*, so
# typing the leading zero is the natural thing to do — and bash's printf rejects
# 08 as an invalid octal number, which under errexit ends the run with nothing
# but a printf error. 10# forces base 10, and the regex keeps garbage out of the
# arithmetic. Callers must assign on a line of their own (`local n; n="$(...)"`)
# or the `local` builtin's own exit status hides the failure.
num_arg() {  # num_arg <value> <what-it-is>
  [[ "${1:-}" =~ ^[0-9]+$ ]] || die "${2:-argument} must be a number, got '${1:-}'"
  printf '%d' "$(( 10#$1 ))"
}

# The unlocked identity, held in memory for the life of one command so that a
# passphrase-protected key is asked for once rather than once per disc. It is
# never written to a file and never appears in a command line.
AGE_IDENTITY_TEXT=""

# A public archive (--public-archive in the Go writer) carries its own secret key
# on every disc, as identity.txt at the disc root beside README.md, and ingest
# copies that file into $ENC_DIR/identity.txt. It is a second identity, never a
# replacement: the identity search below runs exactly as it always has, and this
# path is handed to age as an additional -i when it exists — or as the only one,
# when nothing else is configured, which is the point of a public set: it opens
# with nothing but the discs. Set by resolve_identity; "" when there is none.
AGE_IDENTITY_EXTRA=""

# The one line of an age-keygen file that is the key. Everything else in it is
# comment, and the writer stamps every disc's copy with its own "# created:"
# time, so two copies of the same key are rarely byte-identical.
identity_key_of() { grep -m1 '^AGE-SECRET-KEY-' "$1" 2>/dev/null || true; }

# Both age container formats announce themselves on their first line; an
# age-keygen identity starts with "# created:" or "AGE-SECRET-KEY-".
identity_is_encrypted() {
  local first=""
  IFS= read -r first < "$1" 2>/dev/null || true
  [[ "$first" == "age-encryption.org/v1"* \
     || "$first" == "-----BEGIN AGE ENCRYPTED FILE-----"* ]]
}

# Where this machine's identity lives, or nothing. Order is deliberate: the
# plaintext identity, then the passphrase-protected copy of it, then the rescue
# key — which exists precisely for the day the first two are gone, so it must
# never be picked while a cheaper key is available.
#
# The rescue key is a second recipient whose identity is protected by a
# passphrase. The discs stay encrypted to public keys — the passphrase never
# guards the ciphertext, only a small file an attacker holding the discs does
# not have. That ordering is the whole point: `age -p` on the images would hand
# a thief unlimited offline guesses against something a human has to remember,
# and age refuses to mix an scrypt stanza with recipient stanzas anyway, so "my
# key OR my passphrase" cannot be written into one file.
find_identity() {
  local d; d="$(dirname "$AGE_RECIPIENTS_FILE")"
  local -a cands=()
  if [[ -n "${AGE_IDENTITY:-}" ]]; then cands=( "$AGE_IDENTITY" "$AGE_IDENTITY.age" )
  else                                  cands=( "$d/identity.txt" "$d/identity.txt.age" ); fi
  cands+=( "$d/rescue-identity.txt.age" )
  local c
  for c in "${cands[@]}"; do
    if [[ -f "$c" && -r "$c" ]]; then printf '%s\n' "$c"; return 0; fi
  done
  return 1
}

# Ask for the passphrase once, on the terminal, and hold the result.
unlock_identity() {
  local f="$1"
  [[ -n "$AGE_IDENTITY_TEXT" ]] && return 0          # already unlocked this run
  have_tty || die "$f is passphrase-protected and there is no terminal to ask on. age reads passphrases from /dev/tty and never from a pipe, so this cannot be automated — run it from an interactive shell, or point AGE_IDENTITY at an unencrypted identity."
  warn "identity $f is passphrase-protected"
  local txt=""
  txt="$(age -d "$f")" \
    || die "could not unlock $f — wrong passphrase, or that file is not a passphrase-protected age identity"
  [[ "$txt" == *AGE-SECRET-KEY-* ]] \
    || die "$f decrypted, but what came out is not an age identity (no AGE-SECRET-KEY- line)"
  AGE_IDENTITY_TEXT="$txt"
  ok "identity unlocked — this command will not ask again"
}

# Every decryption goes through here, so no caller has to know whether the
# identity is a file on disk or the unlocked text held in memory, nor whether a
# public archive's own key is along for the ride. Arguments are passed straight
# to age: age_d -o OUT IN, or age_d IN, or age_d reading stdin.
#
# age accepts several -i and tries each identity against the file, so a public
# set's key is simply added; when it is the only identity, resolve_identity has
# already made it AGE_IDENTITY and the extra is empty. ${x:+...} expands to
# nothing at all when unset, so no empty argument reaches age.
age_d() {
  if [[ -n "$AGE_IDENTITY_TEXT" ]]; then
    age -d -i <(printf '%s\n' "$AGE_IDENTITY_TEXT") ${AGE_IDENTITY_EXTRA:+-i "$AGE_IDENTITY_EXTRA"} "$@"
  else
    age -d -i "$AGE_IDENTITY" ${AGE_IDENTITY_EXTRA:+-i "$AGE_IDENTITY_EXTRA"} "$@"
  fi
}

# Restore and ingest write the same plaintext the backup path warns about, into
# a directory the README tells people to put under /var/tmp — which is 1777, so
# every local account can create $STAGING before the operator does and own the
# whole working namespace of the restore from then on. This used to set the
# umask, mkdir -p, and then chmod with the failure thrown away (`|| true`),
# which meant a directory somebody else owned was simply carried on with.
#
# It now refuses instead, by the same three rules as secureStaging in
# go/internal/restore/staging.go, applied to $STAGING first and then to each
# subdirectory this command is about to write into — the root first, because a
# symlinked root would make every check below it a check on the link's target.
#
# umask is set here and not inside secure_dir because it has to cover
# everything the command creates afterwards, not just these directories.
secure_staging() {  # secure_staging [SUBDIR ...]
  umask 077
  local d
  secure_dir "$STAGING"
  # After $STAGING has been vetted and never before, exactly as lock.go says:
  # the lock file has to be created somewhere this process has already proven
  # it owns.
  lock_staging
  for d in "$@"; do secure_dir "$d"; done
}

# One exclusive lock on $STAGING/.brb.lock, held for the life of this command,
# so that two brb runs cannot write into one staging tree at once. It is the
# same lock on the same path with the same words as LockStaging in
# go/internal/fsx/lock.go, and taking it here is what makes that lock mean
# anything: the Go writer and every Go reader command take it, but a lock only
# the Go build respects does not stop the case it was written for, which is a
# stranger reading discs back with the script carried on the media while a
# multi-day backup is still running. cmd_ingest's opening sweep of
# $ENC_DIR/*.part is the concrete collision — those are the very files a Go
# backup is writing at that moment, and deleting one ends the run at its
# rename.
#
# OPPORTUNISTIC, deliberately. flock(1) is util-linux, which is not in this
# reader's dependency set and may simply not exist on the rescue system someone
# is holding a disc on. A reader that refused to restore because a locking tool
# is missing would be a worse failure than the accident the lock prevents, so a
# missing flock says so once and the run carries on unguarded.
#
# The descriptor is never closed: process exit releases the lock, however the
# process ends. The file is never removed either — unlinking a file another
# process is about to lock is how advisory locking is commonly got wrong, and
# LockName in lock.go gives the same reason for the same file.
BRB_LOCK_FD=""
lock_staging() {
  [[ -z "$BRB_LOCK_FD" ]] || return 0
  if ! command -v flock >/dev/null 2>&1; then
    step "flock is not installed, so this run cannot tell whether another brb is already using $STAGING — run one brb at a time"
    return 0
  fi
  exec {BRB_LOCK_FD}>"$STAGING/.brb.lock" \
    || die "could not open the staging lock $(esc_str "$STAGING/.brb.lock") — fix its permissions, or point STAGING at a directory you own"
  flock -n "$BRB_LOCK_FD" \
    || die "another brb is using the staging directory $(esc_str "$STAGING") — wait for it to finish, or point STAGING at a directory of its own; two runs sharing one staging tree can write the same image at the same time, and the result verifies clean and does not restore"
}

# dir_path DIR — the spelling of a directory path that lstat answers about the
# directory ITSELF, for the guards that have to know whether it is a symlink.
#
# A path ending in a slash, or in "/.", is resolved THROUGH a symlink by the
# kernel before lstat ever sees it: "ln -s real link" and then [[ -L link/ ]],
# [[ -L link/. ]] are both false, and "find -P link/ -type l" starts inside the
# target and never reports the link. So every trailing "/" and "/." comes off,
# repeatedly — "${d%/}" strips exactly one, which left "dest//" defeating both
# destination guards and "STAGING=/var/tmp/brb//" skipping secure_dir's first
# rule outright while the extraction still went to the raw path. The Go reader
# collapses the lot with filepath.Clean (go/internal/restore/extract.go), and
# this is that, for the trailing components which are the ones that matter:
# an interior "//" or "/./" names the same file to lstat and to unsquashfs
# alike, so it can hide nothing.
#
# The result is handed to unsquashfs -d as well as to the guards, so the two
# can never disagree about which directory they mean. "/" survives as "/", and
# an empty argument stays empty for the caller to reject.
dir_path() {  # dir_path DIR
  local d="${1:-}"
  while [[ -n "$d" && "$d" != "/" ]]; do
    case "$d" in
      */)  d="${d%/}" ;;
      */.) d="${d%/.}" ;;
      *)   break ;;
    esac
    [[ -n "$d" ]] || d="/"
  done
  printf '%s' "$d"
}

# secure_dir DIR — make one directory fit to hold plaintext, or die saying what
# to do about it. The three rules, in the order secure_dir applies them:
#
#   - It must not be a symlink. A restore writes decrypted images into these
#     directories by name, and a link planted at "restore" or "enc" before the
#     run sends every one of them wherever the planter chose. -L is a test on
#     the link itself, dangling or not, and never on what it points at.
#   - It is created if missing, and its mode is forced to 0700 whether or not
#     it was just created: mkdir -p applies the umask only to what it makes, so
#     a directory the operator made by hand keeps whatever mode it had. A chmod
#     that FAILS is fatal — for as long as a restore runs this tree holds the
#     whole archive in the clear, and "could not lock the door" is not a thing
#     to walk past.
#   - It must belong to the user running this command. A directory another
#     account owns is one that account can rename, replace or fill at will,
#     between any check made here and any write made later — the ownership
#     check is what turns the two checks above from a race into a guarantee.
#     It is also what the plaintext cache in prepare_image rests on: that cache
#     trusts a hash sidecar sitting in these same directories, and whoever owns
#     the directory can write both halves of a matching pair.
secure_dir() {
  local d owner
  d="$(dir_path "$1")"
  [[ -n "$d" ]] || die "no STAGING directory configured — set STAGING in $CONFIG_FILE or in the environment"
  if [[ -L "$d" ]]; then
    die "$(esc_str "$d") is a symlink (-> $(esc_str "$(readlink -- "$d" 2>/dev/null || true)")); a staging directory must be a real directory, because decrypted images are written into it by name and would follow the link — remove the link, or point STAGING at the directory itself"
  fi
  mkdir -p -- "$d" \
    || die "could not create $(esc_str "$d"); if something that is not a directory is in the way, remove it, or point STAGING elsewhere"
  chmod 700 -- "$d" \
    || die "could not secure $(esc_str "$d"), which will hold plaintext — fix its permissions, or point STAGING at a directory you own"
  [[ -d "$d" ]] \
    || die "$(esc_str "$d") exists and is not a directory — remove it, or point STAGING elsewhere"
  owner="$(dir_owner "$d")"
  [[ -n "$owner" ]] \
    || die "could not read who owns $(esc_str "$d") — this check is the only thing standing between a restore's plaintext and a directory somebody else controls, so it is not skipped: install GNU coreutils (stat), or point STAGING at a directory on a filesystem that reports ownership"
  if (( owner != EUID )); then
    die "$(esc_str "$d") is owned by uid $owner, not by this process (uid $EUID); whoever owns it can replace anything under it while a restore is writing plaintext there — $(ownership_advice "$d" "$owner")"
  fi
}

# What to do about a staging directory somebody else owns. Under sudo the
# likeliest story is a directory the operator made earlier without it, in which
# case chown is the fix; otherwise the fix is a directory of one's own. Mirrors
# ownershipAdvice in go/internal/restore/staging.go.
ownership_advice() {  # ownership_advice DIR OWNER-UID
  if (( EUID == 0 )); then
    printf 'chown -R root %s if it is yours, or point STAGING at a directory root owns' "$(esc_str "$1")"
  elif (( $2 == 0 )); then
    printf 'run this command as root, or point STAGING at a directory you own'
  else
    printf 'point STAGING at a directory you own'
  fi
}

# The numeric owner of a path, or nothing at all when neither tool can say.
# stat -c is GNU (and busybox); ls -ldn is the fallback for anything else, and
# its third field is the numeric owner on every implementation that supports
# -n. Callers treat "nothing" as a refusal, never as a pass.
dir_owner() {  # dir_owner DIR
  local u=""
  u="$(stat -c %u -- "$1" 2>/dev/null || true)"
  # shellcheck disable=SC2012  # ls, not find: the only field taken is the
  # numeric owner in a fixed column, never a filename, so the parsing hazard
  # SC2012 warns about does not arise — and find's %U is a GNU extension, which
  # is exactly what this line is the fallback for.
  [[ "$u" =~ ^[0-9]+$ ]] || u="$(ls -ldn -- "$1" 2>/dev/null | awk 'NR == 1 { print $3 }' || true)"
  [[ "$u" =~ ^[0-9]+$ ]] || u=""
  printf '%s' "$u"
}

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
load_config() {
  # A mistyped -c is otherwise indistinguishable from no config at all, and the
  # default it falls back to is STAGING=/var/tmp/brb — an empty staging
  # directory, and a restore that reports finding no discs at all.
  if [[ -n "$CONFIG_EXPLICIT" && ! -f "$CONFIG_FILE" ]]; then
    die "config file not found: $CONFIG_FILE"
  fi
  if [[ -f "$CONFIG_FILE" ]]; then
    # shellcheck disable=SC1090
    source "$CONFIG_FILE"
    step "config: $CONFIG_FILE"
  else
    step "no config file at $CONFIG_FILE — using defaults"
  fi
  # Put back anything the environment set. The README tells restorers to export
  # STAGING and AGE_IDENTITY before running ingest, and a config file left over
  # from the backup would otherwise send them back into the backup's staging.
  local kv
  if (( ${#BRB_ENV_OVERRIDES[@]} )); then
    for kv in "${BRB_ENV_OVERRIDES[@]}"; do declare -g "$kv"; done
  fi
  [[ -z "$CLI_ASSUME_YES" ]] || ASSUME_YES=1
  # The two yes/no settings are used in (( )) arithmetic further down, and under
  # set -u a KEEP_IMAGES=true from the config — the natural thing to write —
  # ended the restore with nothing but "true: unbound variable". Normalise them
  # to 1/0 here, once, and refuse anything that is not recognisably a boolean:
  # a typo silently read as "no" would quietly delete every decrypted image.
  bool_setting KEEP_IMAGES
  bool_setting ASSUME_YES
  # A config file written for the Go build will carry SOURCE_DIR, DISC_TYPE,
  # COMPRESSION and the rest. Sourcing it here simply defines variables this
  # script never reads, which lets one config serve both — but sourcing is
  # execution, so the config must be trusted exactly like brb.sh itself.
  ENC_DIR="$STAGING/enc"
  RESTORE_DIR="$STAGING/restore"
}

# bool_setting NAME — rewrite the named setting in place as 1 or 0. Accepts
# 1/0, true/false, yes/no and on/off in any case; an empty value is "no".
# Anything else is fatal and says which setting, which value, and where it was
# read from, so the operator can go straight to the line.
bool_setting() {
  local name="$1" v="${!1:-}"
  case "${v,,}" in
    1|true|yes|on)     declare -g "$name=1" ;;
    0|false|no|off|"") declare -g "$name=0" ;;
    *) die "$name must be 1 or 0 (also true/false, yes/no, on/off), got '$v' — check $CONFIG_FILE and the environment" ;;
  esac
}

# /var/tmp is swept by systemd-tmpfiles (30 days; /tmp is 10), and an ingested
# set can sit in staging for weeks between reading the discs and finishing the
# restore. Every command here — ingest, restore, mount, list, index — reads
# staging, so it has to survive between them.
warn_staging_volatile() {
  case "$STAGING" in
    /var/tmp/*|/tmp/*)
      warn "$STAGING is under systemd-tmpfiles cleanup (/var/tmp: 30 days, /tmp: 10 days)"
      warn "a slow restore can lose ingested images there; prefer a dedicated path:"
      warn "  STAGING=/srv/brb-staging   (ideally on an encrypted volume)"
      warn "or exclude it:  printf 'x %s\\n' \"$STAGING\" | sudo tee /etc/tmpfiles.d/brb.conf"
      ;;
  esac
}

# ---------------------------------------------------------------------------
# doctor
# ---------------------------------------------------------------------------
cmd_doctor() {
  local missing=0
  log "checking the tools a restore needs"
  # This is the whole dependency set. It is deliberately small, and every member
  # of it is a small, specified, widely-implemented format: that is what makes a
  # disc readable in fifteen years by someone who has never heard of brb.
  # gunzip is only for the index (and restore --only, which resolves discs
  # through it) — but a minimal rescue initramfs can genuinely lack it.
  for t in age par2 unsquashfs sha512sum gunzip; do
    if command -v "$t" >/dev/null 2>&1; then ok "$t  $(command -v "$t")"
    else warn "$t  MISSING"; missing=1; fi
  done
  step "and none of them for the last step: mount -o loop,ro discNN.squashfs /mnt"
  step "needs nothing but the Linux kernel"

  echo >&2
  # Every name here is one this script actually runs. pv used to be on the list
  # and never was: nothing in either implementation has ever invoked it, so it
  # told an operator preparing a rescue system to install a package that changes
  # nothing. flock replaces it because lock_staging does run it when it is there.
  for t in ddrescue udisksctl findmnt eject flock; do
    if command -v "$t" >/dev/null 2>&1; then
      ok "$t  (optional)"
    else
      step "$t  not found (optional)"
    fi
  done
  step "ddrescue is the one worth having: cp stops at the first I/O error on a"
  step "scratched disc, ddrescue does not, and par2 needs the rest of the bytes"
  step "flock is what stops two brb runs sharing one staging directory; without"
  step "it a second run is not noticed, so run them one at a time"

  echo >&2
  command -v age  >/dev/null 2>&1 && step "age $(age --version 2>&1 | head -1)"
  command -v par2 >/dev/null 2>&1 && step "$(par2 -V 2>&1 | head -1)"
  command -v unsquashfs >/dev/null 2>&1 && step "$(unsquashfs -version 2>&1 | head -1)"

  echo >&2
  log "configuration"
  step "staging         $STAGING"
  step "burner          $BURNER"
  step "keep images     $KEEP_IMAGES  (1 keeps each decrypted image for re-runs)"
  warn_staging_volatile

  echo >&2
  # Which key would a restore on this machine actually use, and is there a
  # second way in? Both questions are cheap here and expensive to answer for the
  # first time on the day the primary identity is gone.
  local keydir; keydir="$(dirname "$AGE_RECIPIENTS_FILE")"
  local id; id="$(find_identity || true)"
  if [[ -z "$id" && -f "$ENC_DIR/identity.txt" ]]; then
    # A public archive's key, ingested off one of its discs: enough on its own.
    ok "restore would use the archive's own published key, $ENC_DIR/identity.txt"
  elif [[ -z "$id" ]]; then
    warn "no age identity found — a restore cannot decrypt anything without one"
    step "set AGE_IDENTITY, or put identity.txt in $keydir"
    step "(a public archive carries its key as identity.txt on every disc; ingest stages it as $ENC_DIR/identity.txt)"
    missing=1
  elif identity_is_encrypted "$id"; then
    ok "restore would use $id  (passphrase-protected: it will ask once per command)"
  else
    ok "restore would use $id"
  fi
  if [[ -n "$id" && -f "$ENC_DIR/identity.txt" ]]; then
    step "and the archive's published key, $ENC_DIR/identity.txt, alongside it"
  fi
  if [[ -f "$keydir/rescue-identity.txt.age" ]]; then
    ok "rescue key present: $keydir/rescue-identity.txt.age"
  else
    step "no rescue key in $keydir"
  fi

  echo >&2
  if [[ -d "$ENC_DIR" ]]; then
    local n; n="$(find "$ENC_DIR" -maxdepth 1 -name 'disc*.squashfs.age' 2>/dev/null | wc -l)"
    if (( n > 0 )); then
      ok "$n disc image(s) ingested in $ENC_DIR"
      check_complete || true
    else
      step "no disc images in $ENC_DIR yet — run '$PROG ingest'"
    fi
  else
    step "no staging directory yet: $ENC_DIR"
  fi

  (( missing == 0 )) || die "fix the issues above before trusting a restore"
  ok "ready to restore"
}

# ---------------------------------------------------------------------------
# verify / ingest
# ---------------------------------------------------------------------------
# Changing the disc is the one thing --yes must never answer for. Auto-answering
# it turns "ingest" into an endless re-read of whatever is still in the tray.
# The answer is left in MEDIA_REPLY so callers can offer a "q to stop".
MEDIA_REPLY=""
MOUNTED_BY_US=""
MOUNT_POINT=""
# Returns the mount point in MOUNT_POINT, not on stdout. Every caller used to
# say mp="$(mount_disc ...)", which runs the whole function in a subshell — so
# MOUNTED_BY_US was set in a process that then exited, unmount_disc always
# returned at its first line, and the disc was left mounted with eject failing
# on a busy device. Set MOUNT_POINT on every path so it can never go stale.
mount_disc() {
  local mp="${1:-}"
  MOUNT_POINT=""
  if [[ -n "$mp" ]]; then
    [[ -d "$mp" ]] || die "no such mount point: $mp"
    MOUNT_POINT="$mp"; return 0
  fi
  mp="$(findmnt -n -o TARGET --source "$BURNER" 2>/dev/null | head -1 || true)"
  [[ -n "$mp" ]] && { MOUNT_POINT="$mp"; return 0; }
  if command -v udisksctl >/dev/null 2>&1; then
    udisksctl mount -b "$BURNER" >/dev/null 2>&1 || true
    mp="$(findmnt -n -o TARGET --source "$BURNER" 2>/dev/null | head -1 || true)"
    [[ -n "$mp" ]] && { MOUNTED_BY_US="$BURNER"; MOUNT_POINT="$mp"; return 0; }
  fi
  die "could not mount $BURNER — mount it yourself and pass the path"
}
unmount_disc() {
  [[ -n "$MOUNTED_BY_US" ]] || return 0
  udisksctl unmount -b "$MOUNTED_BY_US" >/dev/null 2>&1 || true
  MOUNTED_BY_US=""
}

# Which disc is actually in the drive, by the name of the image on it. Empty if
# there is nothing recognisable there.
disc_identity() {
  find "$1/data" -maxdepth 1 -name 'disc*.squashfs.age' -printf '%f' -quit 2>/dev/null || true
}

# The disc number in an image name such as disc07.squashfs.age, as a plain
# integer; 0 when the name is not one this format produces. Any number of
# digits, so a set past 99 discs still works — the same rule as discNumberOf
# in go/internal/restore/restore.go.
disc_number_of() {
  if [[ "${1:-}" =~ ^disc([0-9]+)\.squashfs\.age$ ]]; then printf '%d' "$(( 10#${BASH_REMATCH[1]} ))"
  else printf '0'; fi
}

# The name a file from a disc's data/ directory is stored under in staging.
# Every name comes across unchanged except the sidecar parity set: each disc
# carries a sidecars.par2 (plus sidecars.vol*.par2) of its own, covering that
# disc's .sha512 files and the shared index, and staging is one flat directory.
# Copied in under the on-disc name the N sets collided and only one disc's
# survived — every other disc's sidecars were left with no parity at all. So
# in staging the set is named per disc: sidecars.par2 becomes
# sidecars-disc03.par2, sidecars.vol00+40.par2 becomes
# sidecars-disc03.vol00+40.par2. par2 finds a set's volumes by the index file's
# stem, so the renamed set repairs exactly as the original does. What is on the
# disc is untouched — the on-disc name is frozen format. This mirrors
# stagedSidecarName in go/internal/restore/restore.go byte for byte, so either
# reader's staging is the other's. A disc whose number cannot be told (0)
# keeps the flat name.
staged_name() {  # staged_name BASENAME DISC-NUMBER
  local base="$1" disc="$2"
  if (( disc > 0 )) && [[ "$base" == sidecars.* && "$base" == *.par2 ]]; then
    printf 'sidecars-disc%02d%s' "$disc" "${base#sidecars}"
  else
    printf '%s' "$base"
  fi
}

# The staged sidecar parity that covers disc N's small files, or the encrypted
# index when N is 0: the index is on every disc and in every disc's set, so any
# staged set will do for it, but a numbered disc's .sha512 files are only in
# that disc's own set — another disc's parity knows nothing about them, and
# sending an operator to run a repair that cannot work is worse than saying
# nothing. The flat sidecars.par2 comes last: a staging area filled by an
# older reader holds one set under the on-disc name. Prints the names that
# exist in $ENC_DIR, one per line, most specific first (stagedSidecarSets in
# go/internal/restore/restore.go).
staged_sidecar_sets() {  # staged_sidecar_sets DISC-NUMBER
  local n="$1" nm
  local -a cands=()
  if (( n > 0 )); then
    cands+=( "$(printf 'sidecars-disc%02d.par2' "$n")" )
  else
    for nm in "$ENC_DIR"/sidecars-disc*.par2; do
      [[ -e "$nm" && "${nm##*/}" != *.vol* ]] || continue
      cands+=( "${nm##*/}" ); break
    done
  fi
  cands+=( sidecars.par2 )
  for nm in "${cands[@]}"; do [[ -f "$ENC_DIR/$nm" ]] && printf '%s\n' "$nm"; done
  return 0
}

# What to tell the operator when a small file — a .sha512 sidecar, the index —
# is what rotted rather than the image: every place the parity for it actually
# is, in staging first and always ending with the disc itself, which is where it
# can always be found. DISC is the disc whose sidecars the file belongs to, or 0
# when any disc's set will do. Same words as sidecarRepairHint in the Go reader.
sidecar_repair_hint() {  # sidecar_repair_hint DISC-NUMBER
  local n="$1" nm from="the disc" out=""
  while IFS= read -r nm; do
    out+="par2 repair -- $nm in $ENC_DIR, or "
  done < <(staged_sidecar_sets "$n")
  (( n > 0 )) && from="disc $n"
  printf '%s' "repair it from parity: ${out}par2 repair -- sidecars.par2 in ${from}'s data/ directory"
}

# The disc's own SHA512SUMS, read once per disc into DISC_SUMS keyed by the
# path relative to the disc root with the leading ./ dropped ("data/disc03.
# squashfs.age", "identity.txt"), so that EVERY file ingest stages can be
# checked against the hash this disc records for it — the par2 index and
# volumes, the .sha512 sidecars, the published key — and not only the two files
# that happen to carry a sidecar of their own. A disc without a readable
# SHA512SUMS is usable but unverifiable, and says so (discSums in
# go/internal/restore/ingest.go).
declare -A DISC_SUMS=()
# The ceiling mirrors agecrypt.maxSumLines, and for the same reason: this file
# comes off a disc somebody else handed over, restore reads it with nothing in
# between so much as looking at its size, and every parsed line is retained in
# DISC_SUMS until the command ends. A "disc" that is a directory rather than
# real media has no size limit at all, so a generated multi-gigabyte SHA512SUMS
# turns into an OOM kill mid-ingest. A million is unreachable rather than tight:
# a brb disc directory holds on the order of ten files. Lines READ are counted,
# not entries stored, so a file of a million repeated names stops here too
# rather than being scanned forever for free.
MAX_SUM_LINES=1048576

read_disc_sums() {  # read_disc_sums MOUNTPOINT
  DISC_SUMS=()
  local h p n=0
  # -L before -f: a disc brb wrote carries no links at its own names, and -f
  # alone follows one, so a SHA512SUMS symlinked to a file off the disc would be
  # read as this disc's record of itself. The Go reader refuses the same names
  # through an os.Root plus an Lstat (restore/ingest.go, discEntry).
  if [[ -L "$1/SHA512SUMS" ]]; then
    warn "SHA512SUMS on this disc is a symbolic link, which no disc brb wrote carries; copies cannot be checked as they are made"
    return 0
  fi
  if [[ ! -f "$1/SHA512SUMS" ]]; then
    warn "no SHA512SUMS on this disc; copies cannot be checked as they are made"
    return 0
  fi
  while read -r h p; do
    if (( ++n > MAX_SUM_LINES )); then
      warn "SHA512SUMS on this disc has more than $MAX_SUM_LINES lines; reading stopped there"
      step "a brb disc lists about ten files, so this one was not written by brb"
      break
    fi
    # A line SHA512SUMS's own rot has mangled is simply not a hash for anything;
    # the file it named is then copied unchecked, exactly as if it were absent.
    [[ "$h" =~ ^[0-9A-Fa-f]{128}$ && -n "$p" ]] || continue
    p="${p#\*}"; p="${p#./}"
    DISC_SUMS["$p"]="${h,,}"
  done < "$1/SHA512SUMS"
}

# The SHA-512 of a file, from its contents alone — no name in the output to
# escape or parse.
sha512_of() { sha512sum < "$1" | awk '{print $1}'; }

# recorded_sum SUMFILE NAME — the digest SUMFILE records FOR NAME, printed on
# stdout. Exit 0 with the digest; 1 when there is no usable entry for that name
# (including no file at all); 2 when the file exists but does not parse, which
# is a rotted sidecar and a different thing entirely from silence.
#
# WHY NOT `sha512sum -c`. Because it hashes whatever path each line names, and
# the line comes off the media: ingest copies data/* into staging under the
# on-disc name and never reads a sidecar's name field, so a disc carrying
# "<digest of the empty file>  /dev/null" as discNN.squashfs.sha512 made
# `sha512sum -c` exit 0 without touching the image at all. That turned the
# plaintext cache — the one place a restore hands over an image age never
# authenticated — into "reuse whatever is at this path", and the post-decryption
# check into a no-op. The sidecar gets to record a digest; it does not get to
# choose which file is hashed. recordedSum in go/internal/restore/restore.go
# draws the line in the same place, and hashes the file itself afterwards.
#
# The parse is ReadSumFile/parseSumLine from go/internal/agecrypt/sums.go: an
# optional leading backslash for an escaped name, 128 hex digits, one space or
# tab, an optional second space or '*' for binary mode, then the name — of
# which only the base is compared, as recordedSum compares filepath.Base. Blank
# lines and '#' comments are skipped; one name listed twice with two different
# digests is a refusal, not a coin toss. An escaped name is left escaped rather
# than unescaped: no artifact this format writes contains a backslash, a newline
# or a carriage return, so such a line simply matches nothing and reads as "no
# digest recorded for this file", which is the safe answer.
recorded_sum() {  # recorded_sum SUMFILE NAME
  [[ -f "$1" ]] || return 1
  local out=""
  out="$(N="$2" LC_ALL=C awk '
    { line = $0; sub(/\r$/, "", line) }
    line == "" || substr(line, 1, 1) == "#" { next }
    {
      if (substr(line, 1, 1) == "\\") line = substr(line, 2)
      if (length(line) < 130) { bad = 1; exit }
      d = tolower(substr(line, 1, 128))
      if (d !~ /^[0-9a-f]{128}$/) { bad = 1; exit }
      sep = substr(line, 129, 1)
      if (sep != " " && sep != "\t") { bad = 1; exit }
      n = substr(line, 130)
      c = substr(n, 1, 1)
      if (c == " " || c == "*") n = substr(n, 2)
      if (n == "") { bad = 1; exit }
      sub(/.*\//, "", n)
      if (n in seen && seen[n] != d) { bad = 1; exit }
      seen[n] = d
      if (n == ENVIRON["N"] && got == "") got = d
    }
    END { if (bad) exit 2; if (got == "") exit 1; print got }
  ' "$1")" || return $?
  printf '%s' "$out"
}

# ingest_one SRC DST WANT — bring one file off the disc into staging, or say
# why not. DST is where it is stored (the staged name, which for the sidecar
# parity differs from the on-disc name); WANT is the hash the disc's SHA512SUMS
# records for it, "" when the disc carries none. Returns non-zero for a copy
# that is not proven good — incomplete, or not matching the hash — which the
# caller counts and reports at the end.
#
# Two rules, both learned the hard way, and the same two reconcileExisting in
# go/internal/restore/ingest.go follows. A staged copy is never overwritten
# unless its replacement is already proven better — never removed first and
# re-copied after, because the moment between is where a power cut lands. And a
# differing copy of an IMAGE is never thrown away: two pressings of the same
# disc, each rotted past its own par2 redundancy, can still hold every block
# between them, but only if both copies are on disk for par2 to combine — and
# par2 only combines files named on its command line, so the second copy sits
# beside the first as <name>.copy<epoch>, the name prepare_image passes.
ingest_one() {
  local src="$1" dst="$2" want="$3"
  local base name got alt
  base="${src##*/}"
  # $name is this function's own copy of the on-disc name, and it exists only
  # to be printed. The name comes off the media, where Rock Ridge lets it carry
  # any byte a filesystem will hold — an ESC among them, which raw on a
  # terminal retitles the operator's window or worse. So it is escaped once,
  # here, and every message below prints $name; $base stays raw for the tests
  # and the paths, where the bytes have to be the real ones. The Go reader
  # escapes centrally instead, in its Printer (ui.visible).
  name="$(esc_str "$base")"
  # An operator who has just been told to run par2 against sidecars-disc03.par2
  # needs to have seen where it came from.
  [[ "${dst##*/}" == "$base" ]] || name="$name (staged as $(esc_str "${dst##*/}"))"

  if [[ -f "$dst" ]]; then
    if [[ -n "$want" ]]; then
      # The hash speaks first: a staged copy that matches is done, whatever a
      # leftover ddrescue map file claims — an interrupted salvage's map
      # survives a later clean re-ingest and would otherwise brand it
      # incomplete forever. "Already have" has to mean "already have a file
      # proven good": otherwise a truncated copy from a bad sector sticks
      # forever, and — because backup leaves its own images in ENC_DIR — the
      # post-backup test restore reads staging instead of the discs it was
      # supposed to be testing.
      got="$(sha512_of "$dst")"
      if [[ "$got" == "$want" ]]; then
        rm -f -- "$dst.mapfile"
        step "already have $name, and it matches the hash on this disc"; return 0
      fi
      warn "the staged $name does not match the hash on this disc; reading this disc's copy"
      alt="$dst.copy$(date +%s)"
      if ! copy_file_robustly "$src" "$alt"; then
        if [[ "$base" == *.squashfs.age ]]; then
          # The partial salvage — and its map file — stay under the .copy name:
          # zeros and all, it is more raw material for par2.
          step "keeping the partial copy as $(esc_str "${alt##*/}") for par2 to combine during '$PROG restore'"
        else
          rm -f -- "$alt" "$alt.mapfile"
        fi
        return 1
      fi
      got="$(sha512_of "$alt")"
      if [[ "$got" == "$want" ]]; then
        # A copy proven whole is better than an unverified staged one, so it
        # becomes the primary — the only name prepare_image's par2 repair and
        # decryption ever read. It was written under its own name and hashed
        # first, and only now renamed over the staged one: the staged bytes are
        # never destroyed ahead of the replacement's proof.
        mv -f -- "$alt" "$dst"
        rm -f -- "$alt.mapfile" "$dst.mapfile"
        step "replaced the staged $name with this disc's verified copy"; return 0
      fi
      if [[ "$base" == *.squashfs.age ]]; then
        warn "this disc's copy of $name does not match the recorded hash either; keeping both — par2 will combine them during '$PROG restore'"
        return 1
      fi
      # Not an image: a second bad copy is no use to par2 under this name, and
      # two unverifiable sidecars are no better than one.
      rm -f -- "$alt" "$alt.mapfile"
      warn "$name: neither the staged copy nor this disc's matches the hash this disc records for it"
      return 1
    fi
    # No recorded hash, so nothing can judge either copy. Compare the two files
    # themselves: identical is done; a differing image is kept as a second copy
    # for par2; anything else keeps the staged copy and says how to change that.
    if cmp -s -- "$src" "$dst"; then
      step "already have $name, byte for byte"; return 0
    fi
    if [[ "$base" == *.squashfs.age ]]; then
      alt="$dst.copy$(date +%s)"
      warn "already have $name, and it differs from the copy on this disc"
      step "ingesting this disc's copy as $(esc_str "${alt##*/}") for par2 to combine during '$PROG restore'"
      copy_file_robustly "$src" "$alt" || return 1
      return 0
    fi
    warn "already have $name, and it differs from the copy on this disc; keeping the staged copy"
    step "delete $(esc_str "$dst") and ingest this disc again if you want the disc's copy instead"
    return 0
  fi

  # Copy to .part and check it against the hash recorded on this disc before
  # promoting it. Putting a partial file under the real name is exactly what
  # made the old skip test sticky.
  step "copying $name"
  if ! copy_file_robustly "$src" "$dst.part"; then
    warn "$name could not be read off this disc — leaving it for another copy"
    rm -f -- "$dst.part" "$dst.part.mapfile"; return 1
  fi
  if [[ -n "$want" ]]; then
    got="$(sha512_of "$dst.part")"
    if [[ "$got" != "$want" ]]; then
      # The drive returned wrong data without reporting an error. Keep the
      # damaged bytes under the REAL name: restore's par2 repair only ever
      # looks there, and this branch is reached only when no copy of the file
      # exists at all, so nothing good is overwritten. A second ingest of this
      # disc — or of another pressing — then lands as the .copy<epoch> above,
      # exactly the pair par2 can combine, or replaces it outright once proven
      # whole. (Filing it away under a .bad name kept bytes par2 could often
      # repair where no repair path would ever read them.)
      warn "$name does not match the hash this disc records for it — keeping the damaged copy for par2 repair, or for a second pressing to replace"
      mv -f -- "$dst.part" "$dst"
      rm -f -- "$dst.part.mapfile"
      return 1
    fi
  fi
  mv -f -- "$dst.part" "$dst"
  rm -f -- "$dst.part.mapfile"
  if [[ -n "$want" ]]; then step "$name copied and verified"; else step "$name copied"; fi
}

# A public archive (--public-archive in the Go writer) carries its own secret
# key on every disc, as identity.txt at the disc root beside README.md — the
# whole point of such a set is that it opens with nothing but the discs. Ingest
# brings that file into $ENC_DIR/identity.txt, which is also where the Go
# writer leaves the same key while it builds the set, and resolve_identity
# picks it up from there. Checked against SHA512SUMS like everything else, and
# refused outright when staging already holds a DIFFERENT key: that can only
# mean two different public sets are being ingested into one staging area,
# where their same-named images would be taken for damaged copies of each
# other. Two copies of the same key are compared by the key line alone, because
# the writer stamps every disc's copy with its own "# created:" time.
#
# Runs before the data files are touched, so a refused disc leaves nothing
# behind. Returns non-zero for a copy that could not be trusted, which the
# caller counts like any other bad file; the same key is on every other disc.
ingest_public_identity() {  # ingest_public_identity MOUNTPOINT
  local mp="$1" src="$1/identity.txt" dst="$ENC_DIR/identity.txt" want got key have
  if [[ -L "$src" ]]; then
    warn "identity.txt on this disc is a symbolic link, which no disc brb wrote carries — not staging it"
    return 1
  fi
  [[ -f "$src" ]] || return 0
  want="${DISC_SUMS[identity.txt]:-}"
  if [[ -n "$want" ]]; then
    got="$(sha512_of "$src")"
    if [[ "$got" != "$want" ]]; then
      warn "identity.txt on this disc does not match the hash the disc records for it — not staging it (every disc of the set carries the same key)"
      return 1
    fi
  fi
  key="$(identity_key_of "$src")"
  if [[ -z "$key" ]]; then
    warn "identity.txt on this disc has no AGE-SECRET-KEY- line, so it is not an age identity — not staging it"
    return 1
  fi
  if [[ -f "$dst" ]]; then
    have="$(identity_key_of "$dst")"
    if [[ "$have" == "$key" ]]; then step "already have the archive's published key, $dst"; return 0; fi
    unmount_disc
    die "the disc at $mp carries a different published key (identity.txt) from the one already in $dst: two different public archives are being ingested into one staging area. Point STAGING at an empty directory for this set and ingest it again."
  fi
  cp -- "$src" "$dst.part" || { rm -f -- "$dst.part"; warn "could not copy identity.txt off this disc"; return 1; }
  mv -f -- "$dst.part" "$dst"
  step "staged the archive's published key as $dst — this set opens with nothing but the discs"
}

# refuse_foreign_index MOUNTPOINT — the index staged from the FIRST disc is the
# one this staging area belongs to, and a later disc carrying a different one
# is refused rather than merged.
#
# WHY THIS EXISTS, AND WHAT IT IS HALF OF. age encrypts to a PUBLIC key, and
# MANIFEST.txt on every disc names the recipients the set was encrypted to. So
# anyone who gets hold of one disc can build a squashfs image of their own
# choosing, encrypt it to that same public key, write the .sha512 sidecars, the
# par2 volumes and a SHA512SUMS over the lot, and hand back a disc that
# decrypts, verifies clean, repairs clean, and is extracted by `unsquashfs -f`
# straight into the operator's destination. Nothing else on the read path ever
# asks whether a disc belongs to the operator's SET.
#
# What the forger does NOT have is the private key, and that cuts two ways:
#
#   They CAN copy the genuine index.tsv.gz.age off a real disc onto their
#   forgery, byte for byte — copying ciphertext needs no key at all. Comparing
#   the index across discs therefore catches nothing on its own.
#
#   They CANNOT read it. So they cannot know which paths it says their disc
#   carries, and cannot make a forged image agree with it.
#
# This function is the first half: it pins the index, which forces a forger
# down the first road — ship the genuine index, or be refused here. The second
# half, refuse_foreign_image below, then holds the image to what that index
# says. Neither half is worth anything without the other.
#
# WHAT IT DOES NOT DO. It detects that the discs of a set DISAGREE. It cannot
# say which of them is lying: ingest the forgery first and its index becomes
# the pinned one, and the genuine discs are what gets refused. It is not a
# signature, and this format has no signing key to make it one — nothing here
# authenticates a disc, it only holds a set to being self-consistent.
#
# WHY A REFUSAL AND NOT THE USUAL "keeping the staged copy". Every disc of one
# set carries the identical index, because the writer copies one file onto all
# of them; two that differ mean either two sets are being ingested into one
# staging area — where their same-named images would then be taken for damaged
# copies of each other — or one of the discs is not from this set. Both are
# things the operator must decide, and both are exactly the shape the published
# key already refuses a few lines above. The keep-both behaviour is deliberately
# left alone for IMAGES: two partially rotted pressings of one image are
# combined by par2 on purpose, which is a different situation entirely.
#
# ROT IS NOT DISAGREEMENT, and this is the one nuance that keeps a legitimate
# restore working: a disc whose own SHA512SUMS (or index sidecar) says its
# index is not the bytes it should be is DAMAGED, not different, so it is left
# to ingest_one, which keeps the staged copy and counts the bad file. Only a
# copy the disc itself vouches for — or one with no recorded hash at all, where
# nothing says it is damaged — is read as a second, disagreeing index.
#
# The cost is that ingest no longer silently heals a staged index that has
# rotted, because "replace the staged index with this disc's" is precisely the
# substitution a forger wants. The message below says so, and says how to do it
# on purpose.
#
# Runs before any data file is staged, so a refused disc leaves nothing behind.
refuse_foreign_index() {  # refuse_foreign_index MOUNTPOINT
  local mp="$1" src="$1/data/index.tsv.gz.age" dst="$ENC_DIR/index.tsv.gz.age" want
  [[ -f "$src" && -f "$dst" ]] || return 0
  cmp -s -- "$src" "$dst" && return 0
  # The same two witnesses cmd_ingest uses for every other file, in the same
  # order: the disc's SHA512SUMS, then the per-file sidecar beside it.
  want="${DISC_SUMS[data/index.tsv.gz.age]:-}"
  if [[ -z "$want" && -f "$src.sha512" ]]; then want="$(awk '{print tolower($1); exit}' "$src.sha512")"; fi
  if [[ -n "$want" && "$(sha512_of "$src")" != "$want" ]]; then
    warn "the index on this disc differs from the staged one, but does not match the hash this disc records for it either — reading that as rot on this disc, not as a second set"
    return 0
  fi
  unmount_disc
  die "the disc at $(esc_str "$mp") carries a different index (index.tsv.gz.age) from the one already in $dst, and every disc of one set carries the identical index: either two different sets are being ingested into one staging area, or one of these discs is not from your set. Nothing from this disc has been staged. Ingest each set into a STAGING of its own; if instead the STAGED index is the rotted one, $(sidecar_repair_hint 0), or delete $dst and ingest this disc again to take its copy."
}

# refuse_escaping_sums MOUNTPOINT — every name in a disc's SHA512SUMS has to be
# a path ON that disc, or the one command whose whole job is the integrity claim
# is not making it.
#
# `sha512sum -c` hashes whatever each line names, resolved against the directory
# it runs in, and never learns which files the caller meant. A disc whose
# SHA512SUMS said "<digest of the empty file>  /dev/null" on every line would
# therefore be reported "disc N verified" without a single byte of the disc
# being read — and SHA512SUMS is a plain text file on media nobody here wrote.
# The Go reader cannot be fooled that way: VerifyDir opens each name through an
# os.Root rooted at the disc, which refuses an absolute path or one that climbs
# out (go/internal/agecrypt/sums.go). This is that rule, applied before the
# delegation rather than inside it.
#
# Lines that do not parse are left alone: --strict below is what answers those,
# and a name is only judged when there is a name to judge.
refuse_escaping_sums() {  # refuse_escaping_sums MOUNTPOINT
  local bad
  bad="$(LC_ALL=C awk '
    { line = $0; sub(/\r$/, "", line) }
    line == "" || substr(line, 1, 1) == "#" { next }
    {
      if (substr(line, 1, 1) == "\\") line = substr(line, 2)
      if (length(line) < 130) next
      if (tolower(substr(line, 1, 128)) !~ /^[0-9a-f]{128}$/) next
      n = substr(line, 130)
      c = substr(n, 1, 1)
      if (c == " " || c == "*") n = substr(n, 2)
      if (n == "") next
      if (substr(n, 1, 1) == "/" || n == ".." || index(n, "../") == 1 \
          || index(n, "/../") > 0 || n ~ /\/\.\.$/) { print n; exit }
    }
  ' "$1/SHA512SUMS")"
  [[ -z "$bad" ]] \
    || die "SHA512SUMS on this disc records a hash for $(esc_str "$bad"), which is not a file on the disc — 'sha512sum -c' would hash that instead and call the disc verified without reading it. This is not a checksum file brb wrote; treat the disc as untrusted."
}

cmd_verify_disc() {
  local n mp want actual marc
  # Validate before any printf '%02d': bash rejects "08" as bad octal and the
  # failing assignment would end the run with nothing but a printf error.
  n="$(num_arg "${1:-}" 'disc number')"
  mount_disc "${2:-}"; mp="$MOUNT_POINT"
  trap unmount_disc EXIT
  [[ -f "$mp/SHA512SUMS" ]] || die "no SHA512SUMS at $mp — is this one of ours?"

  # SHA512SUMS is per-disc and self-consistent, so ANY disc of the set passes
  # against its own copy. Without this, a tray that did not re-mount lets disc 3
  # be recorded as "disc 7 verified" and disc 7 is never read again.
  want="$(printf 'disc%02d.squashfs.age' "$n")"
  actual="$(disc_identity "$mp")"
  # The image's name is read off the media; escaped before printing, like every
  # other byte this command repeats back from a disc.
  [[ "$actual" == "$want" ]] \
    || die "the drive holds $(esc_str "${actual:-an unrecognised disc}"), not disc $n — insert disc $n"
  # ARCHIVE_NAME is a writer setting this script does not define, so a restorer
  # with a reader-side config has nothing to compare against — and every real
  # disc carries an archive name, so an unguarded expansion died here under
  # set -u before a single hash was checked. Warn only when a writer config
  # happens to be loaded and disagrees with the disc.
  if [[ -f "$mp/MANIFEST.txt" && -n "${ARCHIVE_NAME:-}" ]]; then
    marc="$(sed -n 's/^archive name[[:space:]]*:[[:space:]]*//p' "$mp/MANIFEST.txt" | head -1 || true)"
    # The comparison is against the raw field; only what is PRINTED is escaped.
    # MANIFEST.txt is a plain text file on the disc, so its archive-name line is
    # as much the media's choice as a filename is.
    [[ -z "$marc" || "$marc" == "$ARCHIVE_NAME" ]] \
      || warn "this disc belongs to archive '$(esc_str "$marc")', not '$ARCHIVE_NAME'"
  fi

  refuse_escaping_sums "$mp"
  log "verifying disc $n at $mp"
  # --strict matters: without it a line inside SHA512SUMS that has itself rotted
  # is a WARNING, sha512sum still exits 0, and the file that line named is never
  # hashed at all — so the one command whose job is the integrity claim would
  # assert it falsely. The single-line .sha512 sidecar checks elsewhere are safe
  # without it: a sidecar whose only line is malformed has no valid lines, and
  # sha512sum already fails on that.
  if ( cd "$mp" && sha512sum -c --quiet --strict SHA512SUMS ); then
    ok "disc $n verified"
  else
    warn "disc $n has mismatches — par2 may still recover it; run '$PROG ingest' then '$PROG restore'"
    unmount_disc; trap - EXIT; return 1
  fi
  unmount_disc; trap - EXIT
  # Leaving the disc in the drive is how the next verify-disc ends up re-reading
  # this one. Only touch the tray when we were the ones who found the disc in it.
  if [[ -z "${2:-}" ]]; then
    eject "$BURNER" 2>/dev/null || warn "could not eject $BURNER — eject it by hand before the next disc"
  fi
}

copy_file_robustly() {
  local src="$1" dst="$2" rc=0 err=""
  # Keep cp's own words: it fails for write-side reasons too — staging full,
  # quota, a read-only mount — and calling every failure a "read error" sends
  # an operator with a full disk off to install ddrescue instead of freeing
  # space. (The assignment's status drives the &&, so set -e is safe here.)
  err="$(cp -- "$src" "$dst" 2>&1)" && return 0
  # cp quotes the path it failed on back at us, and that path is a name off the
  # media — so what reaches the terminal is escaped, here and below.
  warn "copy failed: $(esc_str "${err:-unknown error copying ${src##*/}}")"
  if command -v ddrescue >/dev/null 2>&1; then
    warn "falling back to ddrescue (gaps become zeros; par2 should repair)"
    # Keep an existing mapfile so a repeated attempt resumes where it stopped;
    # only a stray partial cp with no mapfile is worthless.
    [[ -f "$dst.mapfile" ]] || rm -f -- "$dst"
    ddrescue -n -- "$src" "$dst" "$dst.mapfile" || rc=1
    ddrescue -d -r3 -- "$src" "$dst" "$dst.mapfile" || rc=1
    # Both invocations used to end in `|| true` and the function returned 0
    # regardless, so a 40%-recovered file was indistinguishable from a good one
    # and the caller's "copied incompletely" warning could never fire. A mapfile
    # status other than '+' means bytes are still missing.
    if grep -qE '^0x[0-9A-Fa-f]+ +0x[0-9A-Fa-f]+ +[?*/-]' "$dst.mapfile" 2>/dev/null; then
      warn "$(esc_str "${src##*/}"): unreadable regions remain — see $(esc_str "$dst.mapfile")"; rc=1
    fi
    [[ "$(stat -c%s "$dst" 2>/dev/null || echo x)" == "$(stat -c%s "$src" 2>/dev/null || echo y)" ]] || rc=1
    return "$rc"
  fi
  warn "install gddrescue to salvage partially readable discs"
  return 1
}

# How many discs this set is supposed to have, according to the MANIFEST.txt
# that ingest copies off every disc. Never fatal: a missing or hand-edited
# manifest must not abort a restore.
expected_discs() {
  local v=""
  [[ -f "$STAGING/MANIFEST.txt" ]] && v="$(sed -n 's/^discs[[:space:]]*:[[:space:]]*//p' "$STAGING/MANIFEST.txt" | head -1 || true)"
  [[ "$v" =~ ^[0-9]+$ ]] && printf '%s' "$v"
}

# Every disc carries the full directory skeleton, so a partial set restores a
# complete-looking tree with files silently absent. Name the missing discs.
check_complete() {
  local want have missing=() i
  # || true: expected_discs returns 1 when there is no usable manifest, and a
  # failing command substitution in an assignment is fatal under set -e.
  want="$(expected_discs || true)"
  have=$(find "$ENC_DIR" -maxdepth 1 -name 'disc*.squashfs.age' | wc -l)
  [[ -n "$want" ]] || { warn "no MANIFEST.txt in $STAGING — cannot tell how many discs this set has"; return 0; }
  (( have >= want )) && { ok "all $want disc image(s) present"; return 0; }
  for (( i = 1; i <= want; i++ )); do
    [[ -f "$(printf '%s/disc%02d.squashfs.age' "$ENC_DIR" "$i")" ]] || missing+=("$i")
  done
  warn "MANIFEST says $want discs; $have present. MISSING: ${missing[*]}"
  warn "files on those discs will NOT be restored"
  return 1
}

cmd_ingest() {
  # Ingest writes ciphertext, but restore decrypts into the same tree and the
  # README tells people to point STAGING at /var/tmp, which is 1777. secure_dir
  # creates $ENC_DIR as well as vetting it, so there is no bare mkdir here.
  secure_staging "$ENC_DIR"
  # A .part from an interrupted copy must never be mistaken for a finished
  # file. Its ddrescue mapfile dies with it: a mapfile that outlives its data
  # marks regions of the DELETED file as already read, so the next attempt
  # would trust it (copy_file_robustly keeps an existing mapfile) and produce
  # a copy that can never complete.
  rm -f -- "$ENC_DIR"/*.part "$ENC_DIR"/*.part.mapfile "$STAGING/MANIFEST.txt.part"
  # unmount_disc in the trap, not only on the paths that expect to fail: any die
  # between mount_disc and the unmount at the foot of the loop — the foreign
  # disc at the data/ check most of all — used to leave the drive mounted, and
  # the kernel will not eject a mounted disc. The operator was then stuck with a
  # tray that would not open and a second ingest that found the stale mount,
  # took it for the disc to read, and failed the same way. unmount_disc only
  # acts on a mount this script made, so it is a no-op on every other path.
  trap 'rm -f -- "$ENC_DIR"/*.part "$ENC_DIR"/*.part.mapfile "$STAGING/MANIFEST.txt.part"; unmount_disc' EXIT
  local mp f base want prev_id="" this_id disc bad=0
  while :; do
    # Deliberately not prompt_enter/confirm: both auto-return under --yes, so
    # neither loop exit was reachable and `brb --yes ingest` ran forever.
    prompt_media "insert the next disc (any order), then press Enter — or type q to stop" || break
    [[ "$MEDIA_REPLY" == [qQ] ]] && break
    mount_disc "${1:-}"; mp="$MOUNT_POINT"
    # udisks mounts a disc at a path named after its volume label, which is
    # media-derived like every other name printed here.
    # -L first: -d follows a link, so data -> a directory of the operator's
    # would pass this check and `find -P` would then decline to descend it and
    # list nothing, which reads as an empty disc rather than a hostile one. A
    # disc brb wrote holds data/ as a real directory, so refuse and say why —
    # the Go reader refuses the same name for the same reason through an
    # os.Root plus an Lstat (restore/ingest.go, discEntry).
    [[ -L "$mp/data" ]] && die "$(esc_str "$mp"): data on this disc is a symbolic link, which no disc brb wrote carries — nothing was read from it (the trap above has unmounted it, so the tray will open)"
    [[ -d "$mp/data" ]] || die "$(esc_str "$mp") has no data/ directory — is this one of ours? (the trap above has unmounted it, so the tray will open)"
    # eject is silently a no-op on a disc the user cannot unmount, and findmnt
    # then keeps reporting the old mount point. Nothing else in the loop notices.
    this_id="$(disc_identity "$mp")"
    if [[ -n "$prev_id" && "$this_id" == "$prev_id" ]]; then
      # disc_identity is a filename read off the disc, so it is escaped before
      # it reaches a terminal, exactly like the names ingest_one prints.
      warn "this is the same disc as last time ($(esc_str "${this_id:-unrecognised}")) — the tray may not have opened"
      confirm "Read it again anyway?" || { unmount_disc; continue; }
    fi
    prev_id="$this_id"
    # The sidecar parity is the one thing on a disc whose name is the same on
    # every disc of the set, so it needs the disc number to survive being copied
    # into one flat staging directory (see staged_name).
    disc="$(disc_number_of "$this_id")"
    (( disc > 0 )) \
      || warn "$mp carries no numbered image, so its sidecars.par2 is staged under the flat name and another disc's may already be there"
    read_disc_sums "$mp"
    # The published key first: a disc from a different public set is refused
    # before any of its same-named files can be taken for damaged copies of
    # this set's.
    ingest_public_identity "$mp" || bad=$(( bad + 1 ))
    # And the index second, for the same reason and by the same rule: it is the
    # one file every disc of a set carries identically, so a disc whose copy
    # disagrees is either from another set or forged. Both checks run before a
    # single data file is copied, so a refused disc leaves staging untouched.
    refuse_foreign_index "$mp"
    while IFS= read -r f; do
      base="${f##*/}"
      want="${DISC_SUMS[data/$base]:-}"
      # A disc whose SHA512SUMS is unreadable still carries the two per-file
      # sidecars; they are the only witness left for the image and the index.
      if [[ -z "$want" && -f "$f.sha512" ]]; then want="$(awk '{print tolower($1); exit}' "$f.sha512")"; fi
      ingest_one "$f" "$ENC_DIR/$(staged_name "$base" "$disc")" "$want" || bad=$(( bad + 1 ))
    done < <(find "$mp/data" -type f | sort -V)
    # The manifest is the same on every disc, so a rotted copy is simply not
    # taken: the next disc's will do, and check_complete copes without one.
    if [[ -L "$mp/MANIFEST.txt" ]]; then
      warn "MANIFEST.txt on this disc is a symbolic link, which no disc brb wrote carries — not copying it"
      bad=$(( bad + 1 ))
    elif [[ -f "$mp/MANIFEST.txt" ]]; then
      want="${DISC_SUMS[MANIFEST.txt]:-}"
      if [[ -n "$want" && "$(sha512_of "$mp/MANIFEST.txt")" != "$want" ]]; then
        warn "MANIFEST.txt on this disc does not match the hash the disc records for it — not copying it"
        bad=$(( bad + 1 ))
      # Through a .part and a rename, like every other file ingest stages, and
      # counted rather than fatal. A bare `cp` over the staged copy truncated it
      # first, so a read error part way through a scratched disc destroyed the
      # good manifest AND — an untested simple command under set -e — ended the
      # ingest on the spot, mid-set, with the remaining discs never offered.
      # Nothing in a restore depends on this file (check_complete copes without
      # one) and every disc carries the same copy, which is why the Go reader
      # also treats a failure here as a warning: ingest.go:561-576.
      elif cp -f -- "$mp/MANIFEST.txt" "$STAGING/MANIFEST.txt.part" \
           && mv -f -- "$STAGING/MANIFEST.txt.part" "$STAGING/MANIFEST.txt"; then
        :
      else
        rm -f -- "$STAGING/MANIFEST.txt.part"
        warn "could not copy MANIFEST.txt off this disc — keeping whatever is already staged (every disc carries the same one)"
        bad=$(( bad + 1 ))
      fi
    fi
    unmount_disc
    # A silenced eject failure is how the loop ends up re-reading the same
    # disc — but only touch the tray when mount_disc found the disc in $BURNER
    # itself: with an explicit mount path the drive was never read, and
    # ejecting it would pop unrelated media (the same guard verify-disc has).
    if [[ -z "${1:-}" ]]; then
      eject "$BURNER" 2>/dev/null \
        || warn "could not eject $BURNER (still mounted?) — eject it by hand before inserting the next disc"
    fi
    prompt_media "another disc? press Enter to continue, or type q to stop" || break
    [[ "$MEDIA_REPLY" == [qQ] ]] && break
  done
  ok "ingested $(find "$ENC_DIR" -name 'disc*.age' 2>/dev/null | wc -l) image(s)"
  (( bad == 0 )) \
    || warn "$bad file(s) are incomplete or failed their hash — re-ingest those discs (or a second copy) before restoring"
  check_complete || true
}

# ---------------------------------------------------------------------------
# restore
# ---------------------------------------------------------------------------
resolve_identity() {
  local found="" pub="$ENC_DIR/identity.txt"
  found="$(find_identity || true)"
  # A public archive's own key, staged by ingest (or left by the Go writer),
  # rides along as a second identity, or serves alone when nothing else is
  # configured. It is never passphrase-protected, so there is nothing to ask;
  # but a file under that name that is not an identity at all is a wrong
  # answer worth stopping on rather than a mystery "decryption failed" later.
  AGE_IDENTITY_EXTRA=""
  if [[ -f "$pub" ]]; then
    [[ -n "$(identity_key_of "$pub")" ]] \
      || die "$pub is not an age identity (no AGE-SECRET-KEY- line). Ingest stages a public archive's published key there; remove or replace it."
    AGE_IDENTITY_EXTRA="$pub"
  fi
  if [[ -z "$found" ]]; then
    [[ -n "$AGE_IDENTITY_EXTRA" ]] \
      || die "no age identity found: looked for ${AGE_IDENTITY:-identity.txt}, ${AGE_IDENTITY:-identity.txt}.age and rescue-identity.txt.age near $AGE_RECIPIENTS_FILE, and no published key at $pub  (set AGE_IDENTITY=/path/to/identity.txt — or, for a public archive, ingest one of its discs: its key comes across as $pub)"
    step "using the archive's own published key, $pub"
    AGE_IDENTITY="$pub"; AGE_IDENTITY_EXTRA=""
    return 0
  fi
  # Say so when the file used is not the one asked for: falling back to the
  # rescue key is a decision the operator should see, not discover from a
  # passphrase prompt.
  [[ "$found" == "${AGE_IDENTITY:-}" ]] || step "using identity $found"
  AGE_IDENTITY="$found"
  # Unlock here, once, rather than letting age prompt inside every prepare_image.
  if identity_is_encrypted "$AGE_IDENTITY"; then unlock_identity "$AGE_IDENTITY"; fi
}

# par2-repair and decrypt one image. Sets PREPARED_IMG rather than echoing the
# path: par2 repair prints to stdout, and command substitution would swallow
# that into the "path".
PREPARED_IMG=""
prepare_image() {
  local enc="$1" base ebase plain intact
  base="$(basename "$enc" .age)"
  # $ebase exists only to be printed, the same split ingest_one makes: the image
  # name is whatever the disc's data/ directory called it, ingest stages it
  # under exactly those bytes, and the restore glob matches any name of the
  # disc*.squashfs.age shape whatever sits in the middle. Raw $base is what the
  # paths and the comparisons use; every message below prints $ebase, so an ESC
  # in a name retitles nobody's terminal on the way past.
  ebase="$(esc_str "$base")"
  plain="$RESTORE_DIR/$base"
  # Cheap, and re-checked per image on purpose: every caller secures the
  # staging tree up front, but a long restore gives a co-tenant time, and this
  # is the last moment before a decrypted image is written under $RESTORE_DIR.
  secure_dir "$RESTORE_DIR"

  # Which disc's sidecar parity to point at when a small file turns out to be
  # the corrupt party. 0 (a name this format did not produce) means any set.
  local disc; disc="$(disc_number_of "$base.age")"

  # The cache below is only worth having if what it hands back is known good.
  # A previous run that died on the hash check left its corrupt plaintext right
  # here, and returning it unchecked would make the next restore "succeed".
  #
  # And "known good" reaches only as far as the ownership of the directory the
  # two files are read from: the image and the sidecar it is checked against
  # both live in staging, so anyone who can write there can plant a matching
  # pair and have it extracted with no age authentication at all. secure_dir
  # above is what makes this check mean anything — that, and hashing the image
  # OURSELVES against the digest recorded for it, rather than handing the
  # sidecar to `sha512sum -c` and letting it choose the file (see recorded_sum).
  local want rc
  if [[ -f "$plain" ]]; then
    rc=0; want="$(recorded_sum "$ENC_DIR/$base.sha512" "$base")" || rc=$?
    if (( rc == 0 )) && [[ "$(sha512_of "$plain")" == "$want" ]]; then
      step "reusing verified $ebase"; PREPARED_IMG="$plain"; return 0
    fi
    if (( rc == 0 )); then
      warn "cached $ebase is corrupt — discarding and decrypting again"
    elif (( rc == 2 )); then
      # A sidecar too damaged to parse cannot condemn the image either; the
      # plaintext is simply unvouched-for, and decrypting again costs exactly
      # what reusing it was saving. reuseDecrypted in
      # go/internal/restore/restore.go treats missing and unreadable alike.
      warn "$ebase.sha512 cannot be read, so the existing $ebase in $RESTORE_DIR cannot be checked against it — discarding it and decrypting again"
      warn "  ($(sidecar_repair_hint "$disc"))"
    else
      # No recorded plaintext hash, so nothing here can vouch for what is at
      # this path — and it need not be ours: $RESTORE_DIR is shared by every
      # set that passes through this staging area, so a disc02.squashfs left by
      # an earlier restore of a DIFFERENT archive would be handed over as this
      # set's disc 2 and extracted over the destination unchecked. Discarding
      # costs one decryption, which is what reusing it was saving.
      # reuseDecrypted in go/internal/restore/restore.go discards here too.
      warn "no recorded plaintext hash to check the existing $ebase in $RESTORE_DIR against — discarding it and decrypting again"
    fi
    rm -f -- "$plain"
  fi

  local cwant=""
  intact=unknown
  rc=0; cwant="$(recorded_sum "$ENC_DIR/$base.age.sha512" "$base.age")" || rc=$?
  if (( rc == 0 )); then
    if [[ "$(sha512_of "$enc")" == "$cwant" ]]; then
      intact=yes
    else
      intact=no; warn "$ebase.age does not match its recorded hash" >&2
    fi
  elif (( rc == 2 )); then
    # The likeliest shape of a rotted sidecar: the flipped byte landed in the
    # digest and the line no longer parses, so it says nothing about the image
    # rather than disagreeing with it. Refusing over that would throw away a
    # multi-gigabyte ciphertext on the word of 150 bytes that have their own
    # parity, so fall through as if no hash had been recorded — par2 becomes
    # the authority, and the decrypted image is still checked below.
    # recordedCipherSum in go/internal/restore/restore.go does the same.
    warn "$ebase.age.sha512 cannot be read, so it is the sidecar that is corrupt, not $ebase.age" >&2
    warn "  ($(sidecar_repair_hint "$disc"))" >&2
  fi

  if [[ "$intact" == "unknown" ]]; then
    # Nothing has checked this file, so calling it damaged would be wrong —
    # this used to die "damaged and has no par2 data" over a ciphertext no
    # check had ever failed. Use par2 when it is there, and otherwise proceed:
    # age authenticates as it decrypts, so a corrupted ciphertext fails loudly
    # rather than quietly, and the decrypted image is still checked against its
    # own .sha512 below. Same three answers as checkCiphertextNoSum in
    # go/internal/restore/restore.go.
    warn "no recorded hash for $ebase.age" >&2
    if [[ ! -f "$enc.par2" ]]; then
      warn "no par2 data either; relying on age's authentication to detect damage" >&2
    elif ! command -v par2 >/dev/null 2>&1; then
      warn "par2 is not installed to check it either; relying on age's authentication to detect damage" >&2
    else
      step "checking $ebase.age with par2 instead"
      par2_repair_image "$base" \
        || die "par2 could not repair $ebase.age. If you burned a second copy of the set, ingest that disc into $ENC_DIR too and retry."
    fi
  elif [[ "$intact" == "no" ]]; then
    [[ -f "$enc.par2" ]] || die "$ebase.age is damaged and has no par2 data. Ingest another copy of that disc and retry."
    # mount and list do not insist on par2 up front, because a clean image never
    # needs it. This one does: the honest failure is the missing tool, not
    # "unrepairable" data that par2 has not been allowed to look at.
    need par2 "$ebase.age is damaged and cannot be repaired without it"
    warn "attempting par2 repair of $ebase.age" >&2
    par2_repair_image "$base" \
      || die "par2 could not repair $ebase.age. If you burned a second copy of the set, ingest that disc into $ENC_DIR too and retry."
    # par2 covers the ciphertext only. When par2 says the image is whole and the
    # 170-byte .sha512 sidecar disagrees, the sidecar is what rotted — dying
    # here would throw away a 22 GB image that is provably byte-for-byte
    # correct. The decrypted image is still checked against .sha512 below, so
    # nothing is decrypted on trust.
    if [[ "$(sha512_of "$enc")" == "$cwant" ]]; then
      ok "repaired $ebase.age" >&2
    else
      warn "$ebase.age passes par2 but not its .sha512 sidecar — the sidecar is what is corrupt, not the image" >&2
      warn "  ($(sidecar_repair_hint "$disc"))" >&2
    fi
  fi

  age_d -o "$plain.part" "$enc" \
    || { rm -f -- "$plain.part"; die "decryption failed for $ebase"; }
  mv -- "$plain.part" "$plain"

  # Delete on failure: a rejected image left behind is what the cache above
  # would otherwise pick up and trust on the next run. age authenticates as it
  # decrypts, so the ciphertext is intact and the two candidates are the image
  # that was encrypted and the 150-byte sidecar recording its hash — and the
  # sidecar is the one with its own parity, so say how to put it back.
  rc=0; want="$(recorded_sum "$ENC_DIR/$base.sha512" "$base")" || rc=$?
  if (( rc == 2 )); then
    # Unlike the ciphertext, there is nothing else to appeal to here: par2
    # covers the .age file, not the image inside it, so an unreadable sidecar
    # means the decrypted image cannot be checked at all. Leaving it on disk
    # would let the cache above hand it over unchecked on the very next run —
    # the run made straight after repairing this sidecar. PrepareImage in
    # go/internal/restore/restore.go removes it here for the same reason.
    rm -f -- "$plain"
    die "$ebase.sha512 is damaged and cannot be read, so nothing can check the image decrypted from $ebase.age — $(sidecar_repair_hint "$disc"), then retry"
  elif (( rc != 0 )); then
    warn "no recorded hash for the decrypted $ebase; age's own authentication is the only check performed" >&2
  elif [[ "$(sha512_of "$plain")" != "$want" ]]; then
    rm -f -- "$plain"
    die "decrypted image $ebase does not match the hash in $ebase.sha512; the ciphertext decrypted cleanly, so either the image is not what was backed up or that sidecar has rotted — $(sidecar_repair_hint "$disc"), then retry"
  fi
  PREPARED_IMG="$plain"
}

# par2_repair_image BASE — verify, and if need be repair, BASE.age in $ENC_DIR
# from its own par2 set. par2 ignores files it was not told about, so the
# alternate copies ingest saved off a second burn have to be named explicitly
# to be of any use.
#
# A plain glob, and the directory part QUOTED so that it is matched literally:
# compgen -G took the whole thing as one pattern, so a staging path containing
# [ ] * or ? was interpreted rather than matched, found no copies at all, and a
# set that par2 could have repaired from a second burn was declared
# unrepairable. ${c##*/} strips the directory with a fixed pattern; the old
# ${extras[@]/#$ENC_DIR\/} substituted $ENC_DIR AS a pattern and carried
# exactly the same hazard. No nullglob: an unmatched pattern comes through as
# itself and the -e test drops it. par2's own chatter goes to stderr, because
# the caller's caller reads stdout for nothing and a person reads stderr.
par2_repair_image() {
  local base="$1"
  local -a extras=(); local c
  for c in "$ENC_DIR/$base.age.copy"*; do
    [[ -e "$c" ]] || continue
    extras+=( "${c##*/}" )
  done
  ( cd "$ENC_DIR" && par2 repair -- "$base.age.par2" ${extras[@]+"${extras[@]}"} >&2 )
}

# Refuse a destination that already holds a symlink resolving to a directory.
# Mirrors refuseSymlinkedDirs in go/internal/restore/extract.go, message and
# all: unsquashfs -f traverses such a link — at any depth, not just the top
# level — and writes the archive's files through it, OUTSIDE the destination,
# with this process's privileges, which the README recommends be root's. So it
# is a hard refusal rather than a question, and --yes does not answer it.
#
# A symlink to a FILE is safe (unsquashfs unlinks and replaces it as an entry)
# and is left alone. -P and dir_path's normalisation keep $dest itself from
# being followed: a destination that IS a symlink to a directory is the same
# escape, and lands the whole archive in the target. cmd_restore has already
# normalised what it passes here — this call is the belt to that brace, and
# both must use the same spelling as the -d handed to unsquashfs, or the guard
# and the extraction end up talking about two different directories.
#
# Within one run this needs checking only before the first image: the skeleton
# on every disc makes a path either a directory or a leaf across the whole set,
# so nothing a disc extracts turns into a traversal for the next one.
#
# One deliberate divergence from the Go guard: where WalkDir turns an unreadable
# subdirectory into a hard error, find reports it on stderr (deliberately not
# silenced here) and carries on with the rest of the tree. A destination the
# restore cannot even read is one unsquashfs is about to fail on anyway.
refuse_symlinked_dirs() {
  local d; d="$(dir_path "$1")"; [[ -n "$d" ]] || d="/"
  local -a bad=()
  local p
  while IFS= read -r -d '' p; do
    # -d follows the link, so this is true only for a symlinked DIRECTORY.
    [[ -d "$p" ]] || continue
    # Both halves are chosen by whoever planted the link, and this line goes to
    # a terminal — so no control bytes reach it raw (see esc_controls).
    bad+=( "$(esc_str "$p -> $(readlink -- "$p" 2>/dev/null || true)")" )
    (( ${#bad[@]} < 5 )) || break
  done < <(find -P "$d" -type l -print0)
  # An if, not `&&`: a false test would become this function's status and
  # errexit would end the run right here, silently, on a clean destination.
  if (( ${#bad[@]} == 0 )); then return 0; fi
  local list; list="$(printf '%s, ' "${bad[@]}")"; list="${list%, }"
  die "$d contains symlink(s) to directories ($list); unsquashfs -f would follow them and write the backup's files OUTSIDE the destination — remove them, or restore into an empty directory and merge by hand"
}

# The check above lets a symlink to a FILE through, and that is right as far as
# it goes: where the archive holds a file, unsquashfs -f unlinks the link and
# writes the file as a fresh entry. But where the archive holds a DIRECTORY,
# unsquashfs -f takes the EEXIST from mkdir as "already there", tries to write
# the directory's children through the link (which fails, harmlessly, with
# ENOTDIR) — and then sets the directory's mode, owner and mtime on the path,
# with chmod, chown and utimes, all of which follow the link. So a link planted
# at, say, dest/etc pointing at /etc/shadow gets the archive's directory mode
# and mtime, and, under root, its owner, written onto /etc/shadow. Verified
# against unsquashfs 4.7: the target's mode changed.
#
# So, before each image is extracted, its directory paths are listed and the
# destination is refused if it holds a symlink — to anything, or to nothing —
# at any of them. Only directory paths: a symlink where the archive holds a
# file or a symlink of its own is the ordinary case of restoring over a tree
# that already has some of the archive's own links in it, and the skeleton on
# every disc makes a path either a directory or a leaf across the whole set, so
# what an earlier disc extracted can never trip this for a later one. The
# destination itself counts as the root directory.
#
# With --only, only the directories the extraction will actually touch are
# checked, so a live $HOME's unrelated symlinks do not block fetching one file
# back into it — which is the whole use --only exists for. "Touches" is
# extractionTouches in go/internal/restore/extract.go, and it is deliberately
# wider than "is the requested path": unsquashfs sets the archive's attributes
# on every directory it descends THROUGH, so an ancestor of the requested path
# is as much a hazard as the path itself. This function ignored --only
# altogether until now, which made brb.sh refuse restores the Go reader carried
# on the same disc performed — with the comment here claiming the two agreed.
# The refusal below is the Go reader's sentence, word for word, for the same
# reason.
#
# unsquashfs -ll prints one entry per line, so a directory whose name carries a
# newline is listed as two half-lines and neither is checked — the same limit
# pathsPresent has, and no worse than it: that name is chosen by whoever could
# plant a file in the backed-up tree, and this guard is against whoever can
# plant a link in the destination, and it takes both to slip past it.
#
# The listing IS the guard, so this fails closed on anything that would leave
# it with nothing to compare (see list_image, which is where that failing is
# now done).
refuse_symlinks_at_dirs() {  # refuse_symlinks_at_dirs IMAGE DEST [ONLY]
  local img="$1" d only="${3:-}"
  d="$(dir_path "$2")"; [[ -n "$d" ]] || d="/"
  local -a bad=()
  local p e
  # A destination that is itself a link is caught above; the belt to that
  # brace, so this guard is complete on its own.
  [[ -L "$d" ]] && bad+=( "$(esc_str "$d")" )

  # One listing per image, shared with the cross-check below; a second call for
  # the same image costs nothing.
  list_image "$img"

  for e in ${IMAGE_ENTRIES[@]+"${IMAGE_ENTRIES[@]}"}; do
    [[ "${e:0:1}" == "d" ]] || continue
    p="${e:1}"
    if [[ -n "$only" ]] && ! extraction_touches "$only" "$p"; then continue; fi
    [[ -L "$d/$p" ]] || continue
    bad+=( "$(esc_str "$d/$p -> $(readlink -- "$d/$p" 2>/dev/null || true)")" )
    (( ${#bad[@]} < 5 )) || break
  done
  if (( ${#bad[@]} == 0 )); then return 0; fi
  local list; list="$(printf '%s, ' "${bad[@]}")"; list="${list%, }"
  die "$d holds symlink(s) where $(esc_str "${img##*/}") has directories ($list); unsquashfs -f would apply the backup's directory mode, owner and times THROUGH them to whatever they point at — remove them, or restore into an empty directory and merge by hand"
}

# One image, listed once. Two guards need that listing — the symlink guard
# above needs the paths the image holds as DIRECTORIES, and refuse_foreign_image
# below needs the paths it holds as REGULAR FILES — and an image is a whole
# disc, so asking `unsquashfs -ll` the second question separately would be a
# second pass over 25 GB. Both call this; the second call is a no-op.
#
# What it leaves behind, for the image named in IMAGE_LISTED:
#
#   IMAGE_ENTRIES  one line per entry the parse recognised and kept, tagged by
#                  its first character, because a path may contain anything at
#                  all — spaces, tabs, a leading 'd' — and a tag character is
#                  the only prefix that cannot be mistaken for part of one:
#                    d<path>  a directory, archive-relative
#                    f<path>  a regular file, archive-relative and escaped
#                             exactly as the index escapes a path (backslash
#                             first, then tab — indexfmt.EscapePath), so the
#                             cross-check compares one spelling with itself
#                  The archive root itself is recognised but kept out: it is
#                  the destination directory, not an entry inside it.
#   IMAGE_STRAY    how many listing lines the parse did NOT recognise. Zero for
#                  every listing this format produces, because `unsquashfs -ll`
#                  writes nothing to stdout but entries — so a non-zero count
#                  means either a name that a line-based listing cannot carry
#                  (a newline in a filename splits its entry over two lines,
#                  and this project supports such names on purpose) or an
#                  unsquashfs whose output has drifted. Either way the file
#                  list is no longer known to be complete, which is why the
#                  cross-check degrades to a warning rather than refusing on it.
#
# The parse is the one listedDir makes in go/internal/restore/extract.go rather
# than an anchored date-and-time regex: the line is "<mode> <user>/<group>
# <size> <date> <time> <path>", so the path is everything after the single
# space that follows the fifth field, whatever it contains, and neither a
# padded size column nor a differently formatted timestamp can push a real
# entry out of the list. Nothing is trimmed off the end: a trailing '\r' is
# part of a name. Only '-' entries are files: a symlink's line ends in
# " -> target", which is why they are not comparable to an index row and why
# every disc's replicated skeleton of directories, symlinks and specials stays
# out of the cross-check entirely.
#
# This fails closed. It used to run the listing with stderr on /dev/null and
# never look at the exit status: a listing that failed, or one whose format had
# drifted past the parse, produced an empty list — and an empty list is
# indistinguishable from "this image holds no directories a link could be
# planted at", so the guard returned 0 and the restore went ahead unguarded,
# looking exactly like a guard that had run. Now the exit status is checked and
# so is the number of lines the parse recognised, and either one failing stops
# the restore by name.
#
# The listing streams into the parse rather than being read whole first. Its
# exit status still has to be taken before a single line of it is believed, and
# at the head of a pipeline that status is the easiest thing in shell to drop —
# so it is carried in the stream itself, as a final '!<rc>' line popped before
# anything else. The pipeline runs under '|| prc=$?' rather than bare because
# errexit would otherwise kill the subshell at the failure, before the line
# saying so could be printed; pipefail, set at the top of this script, is what
# makes that status unsquashfs's rather than awk's opinion of unsquashfs's
# truncated output.
#
# It used to read the listing into a variable and then parse that variable into
# the array, so the raw listing and the entries parsed out of it sat in memory
# together and a disc holding a million files cost a few hundred megabytes
# twice. Only the parsed entries are ever needed. This is the reader written for
# a rescue machine fifteen years from now, which is the one place in this script
# where a few hundred megabytes is worth a sentinel line: it runs where there
# may not be any to spare.
IMAGE_ENTRIES=()
IMAGE_STRAY=0
IMAGE_LISTED=""
list_image() {  # list_image IMAGE
  [[ "$IMAGE_LISTED" == "$1" ]] && return 0
  local img="$1" rc=0 status="" summary last
  # unsquashfs's own stderr is deliberately NOT silenced: when this dies, the
  # reason it gives should be readable next to what the tool itself said.
  IMAGE_ENTRIES=()
  mapfile -t IMAGE_ENTRIES < <(
    prc=0
    unsquashfs -ll "$img" | LC_ALL=C awk '
    BEGIN { root = "squashfs-root" }
    {
      rest = $0; fields = 1
      for (i = 0; i < 5; i++) {
        sub(/^ +/, "", rest)
        j = index(rest, " ")
        if (j == 0) { fields = 0; break }
        rest = substr(rest, j)
      }
      if (!fields || length(rest) < 2) { stray++; next }
      path = substr(rest, 2)
      # Recognised as a listing entry: the archive root, or something under it.
      if (path != root && index(path, root "/") != 1) { stray++; next }
      n++
      if (path == root) next
      rel = substr(path, length(root) + 2)
      kind = substr($0, 1, 1)
      if (kind == "d") { print "d" rel; next }
      if (kind != "-") next
      # The index escaping contract, in the order indexfmt fixes it: backslash
      # first, then tab. Doing it the other way round turns a literal tab into
      # "\\t", which reads back as a backslash and a t. A newline cannot appear
      # here — it is what split the line in the first place, and stray counts it.
      gsub(/\\/, "\\\\", rel)
      gsub(/\t/, "\\t", rel)
      print "f" rel
    }
    END { print "#" n + 0 " " stray + 0 }
    ' || prc=$?
    printf '!%d\n' "$prc"
  )
  # The exit status of the pipeline, carried out of the subshell as its last
  # line because errexit would have killed the subshell before any other way
  # of reporting it could run. Popped before the summary, and before a single
  # entry above it is believed.
  if (( ${#IMAGE_ENTRIES[@]} > 0 )); then
    last=$(( ${#IMAGE_ENTRIES[@]} - 1 ))
    status="${IMAGE_ENTRIES[last]}"
    unset "IMAGE_ENTRIES[last]"
  fi
  [[ "$status" =~ ^!([0-9]+)$ ]] || die "could not list the contents of $(esc_str "${img##*/}"): the listing pipeline did not report an exit status, so nothing it printed can be trusted. The guards below have nothing to compare and will not pass by default, so the restore stops here."
  rc="${BASH_REMATCH[1]}"
  (( rc == 0 )) || die "could not list the contents of $(esc_str "${img##*/}"): 'unsquashfs -ll' exited $rc. That listing is the only thing that says which paths this image holds as directories — without which a symlink planted in the destination cannot be told from one the backup itself carries — and which files it holds, which is what the index is cross-checked against. Both guards would have nothing to compare, so the restore stops here rather than extract past a check that compared nothing. Fix or re-ingest that image and retry, or restore it with the Go reader on the disc (brb-linux-amd64)."
  # END always runs, and always last, so the summary is the final line — unless
  # awk produced nothing at all, which the empty summary below turns into the
  # same refusal as a listing that did not parse.
  summary=""
  if (( ${#IMAGE_ENTRIES[@]} > 0 )); then
    last=$(( ${#IMAGE_ENTRIES[@]} - 1 ))
    summary="${IMAGE_ENTRIES[last]}"
    unset "IMAGE_ENTRIES[last]"
  fi
  # Every image this format produces lists at least its own root, so a
  # recognised count of zero means the listing was not in the shape this parses
  # — not that the image is empty.
  [[ "$summary" =~ ^#([1-9][0-9]*)\ ([0-9]+)$ ]] \
    || die "could not read the listing of $(esc_str "${img##*/}"): 'unsquashfs -ll' succeeded, but not one line of it was in the '<mode> <user>/<group> <size> <date> <time> squashfs-root/<path>' form this reads. The guards below have nothing to compare and will not pass by default, so the restore stops here. Please report the unsquashfs version; meanwhile restore with the Go reader carried on every disc (brb-linux-amd64 / brb-linux-aarch64)."
  IMAGE_STRAY="${BASH_REMATCH[2]}"
  IMAGE_LISTED="$img"
}

# refuse_foreign_image IMAGE DISC-NUMBER — this image must hold exactly the
# files the index says disc N holds.
#
# THE OTHER HALF OF refuse_foreign_index. That one pins the index, which leaves
# a forger — who has the set's public key, off MANIFEST.txt, but not its
# private key — only one way to get a disc past ingest: copy the genuine
# index.tsv.gz.age across, which needs no key. This is the check that then
# catches them, because reading that index does need the key. They cannot know
# which paths it claims their disc carries, so they cannot make their image
# agree with it.
#
# WHAT IT GUARANTEES, EXACTLY. It detects that the discs of a set disagree with
# each other. It does not say which one is lying: ingest the forged disc first
# and ITS index is the pinned one, and the genuine discs are what this refuses.
# And it gives no protection at all when the forgery is the only disc involved
# — the forger then controls both the index and the image, and a self-consistent
# pair passes. This is not a signature and cannot be turned into one: the format
# carries no signing key, so nothing here authenticates a disc. What it buys is
# that an attacker's tree is no longer extracted silently over the operator's
# files on the strength of "it decrypted, so it must be ours".
#
# REGULAR FILES ONLY, on both sides. Every disc carries the whole directory
# skeleton — directories, symlinks and specials are replicated onto every disc
# by design, so that any disc restores on its own — and the index lists files.
# Comparing anything else would refuse every legitimate restore ever made.
#
# --only NARROWS EXTRACTION, NOT THE DISC. The image holds the whole disc
# whatever is being extracted from it, so the whole image is compared against
# the whole of that disc's index rows regardless.
#
# WHERE IT DEGRADES, AND WHY IT MUST. `unsquashfs -ll` is line-based and this
# format deliberately supports a filename containing a newline (that is why the
# index has an escaping contract at all). Such a name is listed as two
# half-lines, so the file list is no longer known to be complete and an exact
# comparison would refuse a set that is perfectly good. A backup tool that
# cannot restore a legitimate set is worse than one that misses an exotic
# attack, so this warns loudly and extracts, rather than refusing, whenever
# either side is not exactly comparable: the listing had lines it could not
# parse, the index lists a newline in a path for this disc, the index is not in
# staging to compare against, or the image's own name does not say which disc
# it is. The warning names which, so the operator knows the check did not run.
#
# Restore only, deliberately: mount hands an image to the kernel read-only and
# list prints its contents, and neither writes the archive's files into the
# operator's tree, which is the thing this exists to stop. A disc being
# inspected is one the operator is already looking at with their own eyes.
refuse_foreign_image() {  # refuse_foreign_image IMAGE DISC-NUMBER
  local img="$1" n="$2" name idx="$ENC_DIR/index.tsv.gz.age" why="" rows="" rc=0
  name="$(esc_str "${img##*/}")"
  list_image "$img"
  if (( n <= 0 )); then
    why="its name does not say which disc it is, and the index lists paths by disc number"
  elif [[ ! -f "$idx" ]]; then
    why="there is no index.tsv.gz.age in $ENC_DIR to check it against"
  elif (( IMAGE_STRAY > 0 )); then
    why="$IMAGE_STRAY line(s) of its listing are not entries this can parse, so a name in it is one a line-based listing cannot carry — a filename containing a newline is listed as two half-lines, and this format supports such names"
  fi
  if [[ -z "$why" ]]; then
    # The index rows for THIS disc, tagged 'i' so that one awk below can take
    # both sides down one pipe and still tell them apart. Small next to the
    # image it is vouching for, so it is decrypted per disc rather than held.
    # Exit 3 is this awk saying "a path here contains a newline": the escaped
    # form is a backslash and an 'n', which is not the same thing as the "\\n"
    # a literal backslash-then-n is written as, so the check walks the escapes
    # rather than grepping for two characters. Any other non-zero status is age
    # or gunzip failing, which is a different message.
    rows="$(age_d "$idx" | gunzip -c | N="$n" LC_ALL=C awk -F'\t' '
      function has_newline(s,   i, c) {
        for (i = 1; i <= length(s); i++) {
          c = substr(s, i, 1)
          if (c != "\\") continue
          i++
          if (substr(s, i, 1) == "n") return 1
        }
        return 0
      }
      NF == 2 && $1 ~ /^[0-9]+$/ && $1 + 0 == ENVIRON["N"] + 0 {
        if (index($2, "\\") && has_newline($2)) nl = 1
        print "i" $2
      }
      END { exit (nl ? 3 : 0) }
    ')" || rc=$?
    case "$rc" in
      0) ;;
      3) why="the index lists a path for disc $n whose name contains a newline, which a line-based 'unsquashfs -ll' listing cannot carry faithfully" ;;
      *) why="the index in $ENC_DIR could not be read (age or gunzip exited $rc)" ;;
    esac
  fi
  if [[ -n "$why" ]]; then
    warn "the cross-check of $name against the index is DEGRADED: $why. Extracting it WITHOUT the check that a disc from another set would fail."
    return 0
  fi

  # Both sides down one pipe, each line tagged with where it came from, and
  # compared in the order they were read so the examples below are the same
  # ones on every run and on both readers.
  local -a verdict=()
  mapfile -t verdict < <( { printf '%s\n' ${IMAGE_ENTRIES[@]+"${IMAGE_ENTRIES[@]}"}
                            printf '%s\n' "$rows"; } | LC_ALL=C awk '
    {
      t = substr($0, 1, 1); p = substr($0, 2)
      if (t == "f") { if (!(p in img)) { img[p] = 1; ford[++nf] = p } }
      else if (t == "i") { if (!(p in idx)) { idx[p] = 1; iord[++ni] = p } }
    }
    END {
      # Bounded on purpose: a forged image can differ by every path it holds,
      # and ten thousand lines of them is not a message anybody reads.
      for (i = 1; i <= ni; i++) if (!(iord[i] in img)) { nm++; if (nm <= 3) print "M" iord[i] }
      for (i = 1; i <= nf; i++) if (!(ford[i] in idx)) { ne++; if (ne <= 3) print "E" ford[i] }
      print "=" nm + 0 " " ne + 0 " " ni + 0 " " nf + 0
    }
  ')
  # As in list_image: awk's END is the last line out, so the verdict is the last
  # element — and an awk that printed nothing at all leaves it empty, which the
  # refusal below is the answer to.
  local last=$(( ${#verdict[@]} - 1 )) counts=""
  if (( last >= 0 )); then counts="${verdict[last]}"; fi
  [[ "$counts" =~ ^=([0-9]+)\ ([0-9]+)\ ([0-9]+)\ ([0-9]+)$ ]] \
    || die "could not compare $name with the index: the comparison produced no verdict at all, so nothing was checked and the restore stops here rather than extract past it. Please report this; meanwhile restore with the Go reader carried on every disc (brb-linux-amd64 / brb-linux-aarch64)."
  local nmiss="${BASH_REMATCH[1]}" nextra="${BASH_REMATCH[2]}" nindex="${BASH_REMATCH[3]}"
  if (( nmiss == 0 && nextra == 0 )); then
    step "$name holds exactly the $nindex file(s) the index lists for disc $n"
    return 0
  fi

  local -a miss=() extra=()
  local i e
  for (( i = 0; i < last; i++ )); do
    e="${verdict[i]}"
    # Both sides are paths off media nobody here wrote, and this goes to a
    # terminal: escaped, like every other name this script prints.
    case "${e:0:1}" in
      M) miss+=( "$(esc_str "${e:1}")" ) ;;
      E) extra+=( "$(esc_str "${e:1}")" ) ;;
    esac
  done
  local m_eg="" x_eg="" list
  if (( ${#miss[@]} )); then list="$(printf '%s, ' "${miss[@]}")"; m_eg=" (e.g. ${list%, })"; fi
  if (( ${#extra[@]} )); then list="$(printf '%s, ' "${extra[@]}")"; x_eg=" (e.g. ${list%, })"; fi
  die "$name is not the disc $n the index describes: $nmiss of the $nindex path(s) the index lists for disc $n are not in this image$m_eg, and $nextra file(s) in this image are not in the index for disc $n$x_eg. One index was written for the whole set and copied onto every disc, so an intact set cannot disagree with itself like this: either two different sets have been ingested into one staging area, or one of these discs is not from your set. Nothing in this format signs a disc, so brb cannot tell you which one is honest. Ingest each set into a STAGING of its own; if this is the only set you ingested, treat the staging area as untrusted and re-ingest from discs you kept yourself. The discs that do match the index still restore one at a time with --disc N."
}

# extraction_touches ONLY DIR — will unsquashfs, asked for ONLY, create or
# re-attribute the archive directory DIR? It does when DIR is the requested
# path, is under it, or is an ancestor extraction has to descend through:
# unsquashfs sets the archive's mode, owner and times on every directory it
# passes, not only on the ones named. extractionTouches in
# go/internal/restore/extract.go, with the same three arms; an empty ONLY
# extracts everything and never reaches here.
extraction_touches() {  # extraction_touches ONLY DIR
  [[ "$1" == "" || "$1" == "." || "$2" == "$1" || "$2" == "$1"/* || "$1" == "$2"/* ]]
}

cmd_restore() {
  need age; need par2; need unsquashfs; need sha512sum
  local dest="${1:-}"
  [[ -n "$dest" ]] || die "usage: $PROG restore <destination> [--only <path-in-archive>] [--disc N]"
  # Normalised ONCE, here, and used everywhere below — both symlink guards and
  # the -d handed to unsquashfs. A destination spelled "dest//" or "dest/." is
  # the same directory to unsquashfs but not to lstat, so a guard given the raw
  # string and an extraction given the raw string were checking one path and
  # writing another (see dir_path). The Go reader does the same, with
  # filepath.Clean at the head of Extract.
  dest="$(dir_path "$dest")"
  shift || true
  local only="" onedisc=""
  while (( $# )); do
    case "$1" in
      # ${2:?} produces bash's own "2: parameter null or not set", which names
      # neither the option nor the script.
      # Strip a trailing slash and any leading ./ so "notes/" and "./notes"
      # mean the directory the user was pointing at: matching below is exact,
      # not substring, and would otherwise miss on the cosmetic difference.
      --only) only="${2:-}"; only="${only%/}"; only="${only#./}"
              [[ -n "$only" ]] || die "--only needs a path"; shift 2 ;;
      --disc) onedisc="$(num_arg "${2:-}" '--disc')"; shift 2 ;;
      *) die "unknown option: $1" ;;
    esac
  done
  # Ingest writes ciphertext into staging; the restore path produces plaintext
  # in the same tree, in a directory the README puts under /var/tmp. Before
  # resolve_identity, not after: a public set's key is read out of $ENC_DIR,
  # so that directory has to be one of ours before anything is read from it.
  secure_staging "$ENC_DIR" "$RESTORE_DIR"
  resolve_identity
  mkdir -p -- "$dest"
  warn "$RESTORE_DIR will hold DECRYPTED images (mode 0700, but plaintext on disk)"
  (( EUID == 0 )) || warn "not running as root: ownership will not be restored"

  # unsquashfs is run with -f for every image, because discs 2..N extract into a
  # tree disc 1 already populated. Into a live $HOME that silently overwrites
  # current files with the backup's versions, mode and mtime included.
  # shellcheck disable=SC2012  # ls, not find: this output is only tested for
  # emptiness and then shown to a person, never parsed back into filenames, and
  # ls is the one of the two that a busybox rescue system is sure to have.
  if [[ -n "$(ls -A -- "$dest" 2>/dev/null)" ]]; then
    warn "$dest is not empty. unsquashfs -f will OVERWRITE existing files with the backup versions."
    # The names are whatever is in the destination — not ours — and this goes
    # to a terminal, so control bytes are made visible first (esc_controls).
    warn "  existing entries: $(ls -A -- "$dest" | head -5 | esc_controls | tr '\n' ' ')..."
    confirm "Overwrite the current contents of $dest with this backup?" \
      || die "aborted — restore into an empty directory and merge by hand"
  fi
  # And -f follows a symlink that is already in the destination, so this runs
  # for --only and --disc too, and --yes cannot wave it through.
  refuse_symlinked_dirs "$dest"

  local -a encs=()
  if [[ -n "$onedisc" ]]; then
    encs=( "$(printf '%s/disc%02d.squashfs.age' "$ENC_DIR" "$onedisc")" )
    [[ -f "${encs[0]}" ]] || die "no image for disc $onedisc in $ENC_DIR"
  else
    mapfile -t encs < <(find "$ENC_DIR" -maxdepth 1 -name 'disc*.squashfs.age' | sort -V)
    (( ${#encs[@]} > 0 )) || die "no images in $ENC_DIR — run '$PROG ingest' first"
    check_complete || confirm "Restore the partial set anyway?" || die "aborted"
  fi

  # --only used to par2-verify and decrypt every image in the set — hours and
  # hundreds of GiB — to pull back one file, printing "extracted" for each disc
  # whether or not the path was on it. The index already knows where it lives.
  if [[ -n "$only" && -z "$onedisc" ]]; then
    local -a hits=() sel=(); local d e esc_only
    [[ -f "$ENC_DIR/index.tsv.gz.age" ]] \
      || die "--only needs the encrypted index, which is not in $ENC_DIR — pass --disc N instead"
    # Resolve the disc by EXACT path, not by 'index'-style substring search:
    # a fuzzy hit selects a disc for --only notes when only notes2025 exists,
    # and the extraction filter downstream is exact — so the run used to report
    # "restore complete" having written nothing. The index stores paths with
    # backslash, tab and newline escaped (backslash first — the same order the
    # writer uses; any other order fails to round-trip), so the pattern is
    # escaped the same way, then matched as the whole path or as a directory
    # prefix of one. ENVIRON, not -v: awk -v mangles backslash sequences.
    esc_only="${only//\\/\\\\}"
    esc_only="${esc_only//$'\t'/\\t}"
    esc_only="${esc_only//$'\n'/\\n}"
    mapfile -t hits < <(age_d "$ENC_DIR/index.tsv.gz.age" | gunzip -c \
      | P="$esc_only" awk -F'\t' \
          'NF==2 && ($2 == ENVIRON["P"] || index($2, ENVIRON["P"] "/") == 1) { print $1 }' \
      | sort -un || true)
    (( ${#hits[@]} )) \
      || die "'$only' is not in the index — check '$PROG index ${only##*/}' for the exact path, or pass --disc N. Paths are relative to the archive root, with no leading '/'."
    for d in "${hits[@]}"; do
      [[ "$d" =~ ^[0-9]+$ ]] || continue
      e="$(printf '%s/disc%02d.squashfs.age' "$ENC_DIR" "$((10#$d))")"
      if [[ -f "$e" ]]; then sel+=( "$e" )
      else warn "'$only' is partly on disc $d, whose image is not in $ENC_DIR"; fi
    done
    # ${hits[*]} is field 1 of the decrypted index, which came off a disc: the
    # loop above drops anything that is not a plain number before using it as a
    # disc, but these two lines PRINT it, so they escape it (see esc_controls).
    (( ${#sel[@]} )) || die "'$only' is on disc(s) $(esc_str "${hits[*]}"), none of which have been ingested"
    encs=( "${sel[@]}" )
    step "'$only' is on disc(s) $(esc_str "${hits[*]}")"
  fi

  # The images to be decrypted are sitting right there, so measure them rather
  # than guessing from a configured budget: a decrypted image is a little larger
  # than its ciphertext, and with KEEP_IMAGES=0 only one exists at a time.
  local avail_b need_b=0 sz e
  for e in "${encs[@]}"; do
    sz="$(stat -c%s "$e" 2>/dev/null || echo 0)"
    if (( KEEP_IMAGES )); then need_b=$(( need_b + sz ))
    elif (( sz > need_b )); then need_b="$sz"; fi
  done
  # The decrypted images land in $RESTORE_DIR under STAGING, not in $dest —
  # and on a rescue system those are usually different filesystems (small
  # root disk vs. the big replacement drive). Budgeting them against $dest
  # hides the ENOSPC that later kills age mid-set with nothing but
  # "decryption failed".
  avail_b="$(free_bytes "$RESTORE_DIR")"
  if [[ "$avail_b" =~ ^[0-9]+$ ]] && (( avail_b < need_b )); then
    warn "$RESTORE_DIR has $(human "$avail_b") free; this restore needs about $(human "$need_b") there for the decrypted image(s)"
    (( KEEP_IMAGES )) && warn "  (KEEP_IMAGES=1 keeps every image; unset it to hold only one at a time)"
  fi
  # $dest holds the extracted tree, which decompresses to MORE than the image
  # sizes — if it cannot even hold the images it certainly cannot hold the tree.
  avail_b="$(free_bytes "$dest")"
  if [[ "$avail_b" =~ ^[0-9]+$ ]] && (( avail_b < need_b )); then
    warn "$dest has $(human "$avail_b") free; the extracted tree will need more than $(human "$need_b")"
  fi

  log "restoring ${#encs[@]} image(s) to $dest"
  local enc img found=0
  for enc in "${encs[@]}"; do
    # The name is a staged file name, and ingest stages whatever the disc's
    # data/ directory holds under the on-disc name — Rock Ridge and all. So it
    # is escaped, exactly as ingest escapes it on the way in.
    step "preparing $(esc_str "$(basename "$enc")")"
    prepare_image "$enc"; img="$PREPARED_IMG"
    # unsquashfs exits 0 having created nothing when the requested path is not
    # in the image, so without this the run reports "extracted" for every disc
    # and gives no signal at all about the one file the user asked for. The
    # match is anchored to a whole path component — a bare substring test let
    # "notes" pass on an image holding only notes2025/, and the exact filter
    # below then extracted nothing while the run still claimed success. The
    # listing shows raw filenames (unescaped), so raw $only is right here.
    # A name containing '\n' (or a bare '\r') is one a line-based listing cannot
    # carry faithfully, so the pre-check would answer "not here" about a file
    # that is. Those paths skip it — -no-wildcards below hands unsquashfs the
    # real name — and the post-extraction check judges whether it appeared, the
    # same short-circuit pathsPresent makes in go/internal/restore/extract.go.
    # An if, not `&&`: a false test would become the loop body's status and
    # errexit would end the restore here.
    #
    # Ahead of the symlink guard, as pathsPresent is in the Go reader
    # (go/internal/restore/extract.go): an image this path is not on extracts
    # nothing at all, so there is nothing for the guard to protect and no
    # reason for an unrelated link in the destination to stop the run.
    local precheck="$only"
    if [[ "$only" == *$'\n'* || "$only" == *$'\r'* ]]; then precheck=""; fi
    if [[ -n "$precheck" ]] && ! unsquashfs -l "$img" 2>/dev/null \
        | P="squashfs-root/$only" awk \
            '$0 == ENVIRON["P"] || index($0, ENVIRON["P"] "/") == 1 { f=1; exit } END { exit !f }'; then
      step "$only is not on $(esc_str "$(basename "$img")")"
      if (( ! KEEP_IMAGES )); then rm -f -- "$img"; fi
      continue
    fi
    # Does this image belong to the set the staged index describes? Asked
    # before the symlink guard because it is the more fundamental question —
    # that one asks what this image would do to the destination, this one asks
    # whether it is one of the operator's discs at all. Both read the same
    # single listing (list_image). The disc number comes from the staged
    # ciphertext's name, which is the name the disc's own data/ directory gave
    # it, exactly as prepare_image reads it.
    refuse_foreign_image "$img" "$(disc_number_of "$(basename "$enc")")"
    # The second symlink guard, per image: it needs the image's own list of
    # directories, which exists only now that it is decrypted, and the --only
    # path, which decides which of those directories extraction can reach.
    refuse_symlinks_at_dirs "$img" "$dest" "$only"
    # unsquashfs syntax: unsquashfs [options] FILESYSTEM [paths to extract]
    # The path filter must follow the image, not precede it.
    # -no-wildcards is not optional (and matches the Go build): by default
    # unsquashfs reads each extraction path as a wildcard pattern in which \
    # escapes the next character, so asking for a real file named back\slash.txt
    # matches nothing — and unsquashfs exits 0 having created nothing, which is
    # indistinguishable from success. A path is a path here: it came from the
    # operator or the index, both of which name files literally.
    # On SELinux systems mksquashfs captures security.selinux xattrs, and a
    # non-root unsquashfs cannot write those back: it extracts everything
    # correctly and then exits 2. Restoring as an ordinary user is the
    # documented normal case, so ask only for the xattrs we can actually set.
    local -a ua=( -d "$dest" -f -no-progress -no-wildcards )
    if (( EUID == 0 )); then ua+=( -xattrs ); else ua+=( -user-xattrs ); fi
    ua+=( "$img" )
    [[ -n "$only" ]] && ua+=( "$only" )

    # unsquashfs exits 1 on a fatal error and 2 when it finished but hit
    # non-fatal ones. Treating 2 as failure would abort a restore whose files
    # are all present and correct; treating it as success would hide real
    # trouble. Report it, keep the log, and continue.
    # Declared first and assigned after: `local x=$(cmd)` makes the local
    # builtin's own status the one $? reports, hiding a failure in cmd.
    local ulog urc=0
    ulog="$RESTORE_DIR/unsquashfs.$(basename "$img").log"
    unsquashfs "${ua[@]}" >"$ulog" 2>&1 || urc=$?
    # unsquashfs names the entries it could not write, and those names come out
    # of the image — chosen by whoever could plant one file in the backed-up
    # tree. Its output goes through esc_controls for the same reason ingest's
    # names do, rather than straight to the terminal.
    if (( urc == 2 )); then
      warn "unsquashfs reported non-fatal errors on $(esc_str "$(basename "$img")") — see $ulog"
      grep -v '^$' "$ulog" | head -3 | esc_controls | sed 's/^/    /' >&2
    elif (( urc != 0 )); then
      warn "unsquashfs output:"; esc_controls < "$ulog" | sed 's/^/    /' >&2
      die "unsquashfs failed on $(esc_str "$(basename "$img")") (exit $urc)"
    else
      rm -f -- "$ulog"
    fi
    ok "$(esc_str "$(basename "$img")") extracted"
    # The listing said the path was here, but the destination is the proof:
    # only count --only as found when the path actually materialised. -L covers
    # a dangling symlink, which -e alone reports as absent.
    if [[ -z "$only" || -e "$dest/$only" || -L "$dest/$only" ]]; then found=1; fi
    # Keeping every decrypted image alive means a restore needs roughly twice
    # the archive size. Free each one as soon as its contents are on disk.
    if (( ! KEEP_IMAGES )); then
      rm -f -- "$img"
      step "removed the decrypted image (KEEP_IMAGES=1 keeps them for re-runs)"
    fi
  done
  [[ -z "$only" ]] || (( found )) \
    || die "'$only' was not found on any ingested disc — check '$PROG index $only'"

  cat >&2 <<EOF

$(ok "restore complete: $dest")

  $RESTORE_DIR holds decrypted (plaintext) images: every one of them with
  KEEP_IMAGES=1, otherwise only what a failed run left behind.
  Remove them when done:  rm -rf $RESTORE_DIR

EOF
}

cmd_mount() {
  # Not unsquashfs: this path never runs it — the image is prepared and handed
  # to the kernel. par2 is not demanded up front either, because a clean image
  # never needs it; prepare_image asks for it by name the moment a repair does.
  need age; need sha512sum; need mount
  local n mp="${2:-}"
  [[ -n "${1:-}" && -n "$mp" ]] || die "usage: $PROG mount <disc-number> <mount-point>"
  n="$(num_arg "$1" 'disc number')"
  (( EUID == 0 )) || die "mounting requires root"
  secure_staging "$ENC_DIR" "$RESTORE_DIR"
  resolve_identity
  local enc img
  enc="$(printf '%s/disc%02d.squashfs.age' "$ENC_DIR" "$n")"
  [[ -f "$enc" ]] || die "no image for disc $n in $ENC_DIR"
  prepare_image "$enc"; img="$PREPARED_IMG"
  mkdir -p -- "$mp"
  mount -o loop,ro "$img" "$mp" || die "mount failed"
  ok "disc $n mounted read-only at $mp"
  step "unmount with: umount $mp"
}

# Render C0 control bytes, DEL and the C1 controls visibly, sparing the tab that
# separates an index record's fields. A byte-for-byte mirror of escapeControls in
# go/internal/restore/extract.go: '\r' and '\n' get their C-style escapes, every
# other control becomes \xHH, and anything printable passes through untouched —
# multi-byte characters included, which is why this runs in the C locale.
#
# The names come out of an archive, so they are chosen by whoever could plant
# one file in the backed-up tree; printed raw to a terminal, an ESC ] 0 ; ... BEL
# in a filename retitles the operator's window, and worse where OSC 52 is on.
#
# C1 is covered in both spellings. A terminal decoding UTF-8 acts on U+009B as
# CSI; one that is not acts on the bare 0x9b byte the same way, so escaping one
# and not the other leaves the attack working on half the terminals in use. That
# is why this can no longer work a byte at a time: 0x80..0x9F is also the
# continuation range, and the last byte of a perfectly ordinary CJK filename
# lands in it. Whether a byte is a control or a tail depends on what precedes
# it, so the walk decodes.
esc_controls() {
  LC_ALL=C awk '
    # The length of the valid UTF-8 sequence starting at lead byte a with
    # following bytes b, c, d (0 when past the end of the line), or 0 when what
    # starts here is not a valid sequence. This mirrors Go utf8.DecodeRune down
    # to its rejection of overlong forms and surrogates, because escapeControls
    # escapes whatever that decoder rejects: a reader consuming a different
    # number of bytes here would escape a different set of them, and the two
    # listings would stop matching.
    function seqlen(a, b, c, d) {
      if (a < 194) return 0
      if (a < 224) return (b >= 128 && b <= 191) ? 2 : 0
      if (a < 240) {
        if (b < 128 || b > 191) return 0
        if (a == 224 && b < 160) return 0
        if (a == 237 && b > 159) return 0
        return (c >= 128 && c <= 191) ? 3 : 0
      }
      if (a < 245) {
        if (b < 128 || b > 191) return 0
        if (a == 240 && b < 144) return 0
        if (a == 244 && b > 143) return 0
        if (c < 128 || c > 191) return 0
        return (d >= 128 && d <= 191) ? 4 : 0
      }
      return 0
    }
    BEGIN {
      for (i = 1; i < 256; i++) ord[sprintf("%c", i)] = i
      # The trigger set, as raw bytes inside one bracket expression: C0 except
      # tab (9), DEL, the 0xC2 that leads every UTF-8-spelled C1, and the raw
      # 0x80..0x9F block. None of them is a regex metacharacter. A continuation
      # byte in that last range trips this too, and the walk then passes it
      # through: a false positive costs one slow pass and never a changed byte.
      ctl = ""
      for (i = 1; i < 32; i++) if (i != 9) ctl = ctl sprintf("%c", i)
      ctl = ctl sprintf("%c", 127) sprintf("%c", 194)
      for (i = 128; i < 160; i++) ctl = ctl sprintf("%c", i)
      re = "[" ctl "]"
    }
    # The clean line — every line, almost always — is passed through whole
    # rather than rebuilt a byte at a time.
    $0 !~ re { print; next }
    {
      out = ""; n = length($0); i = 1
      while (i <= n) {
        c = substr($0, i, 1); v = ord[c]
        if (c == "\t")               { out = out c; i++ }
        else if (v == 13)            { out = out "\\r"; i++ }
        else if (v == 10)            { out = out "\\n"; i++ }
        else if (v < 32 || v == 127) { out = out sprintf("\\x%02x", v); i++ }
        else if (v < 128)            { out = out c; i++ }
        else {
          b2 = (i + 1 <= n) ? ord[substr($0, i + 1, 1)] : 0
          b3 = (i + 2 <= n) ? ord[substr($0, i + 2, 1)] : 0
          b4 = (i + 3 <= n) ? ord[substr($0, i + 3, 1)] : 0
          L = seqlen(v, b2, b3, b4)
          if (L == 2 && v == 194 && b2 <= 159) {
            # U+0080..U+009F: a C1 control spelled in UTF-8. Both bytes are
            # spelled out so the escape round-trips through printf.
            out = out sprintf("\\x%02x\\x%02x", v, b2); i += 2
          } else if (L == 0) {
            # Not a valid sequence here. A lone 0x80..0x9F byte is a C1 control
            # to a terminal that is not decoding UTF-8; a higher byte is just a
            # byte, and is left exactly as it was found.
            if (v <= 159) out = out sprintf("\\x%02x", v)
            else          out = out c
            i++
          } else {
            out = out substr($0, i, L); i += L
          }
        }
      }
      print out
    }
  '
}

# Escape only when this command's stdout is a terminal. Piped output stays
# byte-faithful, which is what keeps the awk recipes in the README working and
# lets the two readers' output be diffed against each other.
#
# Two deliberate divergences from the Go reader, both only reachable on a
# terminal, where nothing is being compared byte for byte anyway: it asks
# whether stdout is a character device and so escapes into /dev/null too, where
# -t 1 is true for a terminal only; and awk terminates the last line it prints,
# where the Go writer flushes a trailing partial line as it found it. Neither
# stream escaping applies to — the index, and unsquashfs -ll — ever ends
# without a newline.
esc_out() { if [[ -t 1 ]]; then esc_controls; else cat; fi; }

# One string, escaped for a message. esc_controls works on lines and never sees
# the newline between them, so a newline INSIDE the string is turned into the
# two characters \n first; esc_controls leaves a backslash alone, so the result
# is exactly what a single pass over the bytes would give.
esc_str() { local s="${1//$'\n'/\\n}"; printf '%s\n' "$s" | esc_controls; }

cmd_index() {
  need age
  # Before resolve_identity, for the reason cmd_restore gives: the identity a
  # public set is read with comes out of $ENC_DIR, and so does the ciphertext
  # this decrypts, so that directory has to be one of ours before either is
  # read from it. index was the one staging-reading command that skipped this —
  # against a $STAGING another local account created first (/var/tmp is 1777),
  # it decrypted a planted index with a planted key and printed the result as
  # this archive's disc map, where every sibling command refuses on ownership.
  secure_staging "$ENC_DIR"
  resolve_identity
  local idx="$ENC_DIR/index.tsv.gz.age"
  [[ -f "$idx" ]] || die "no index at $idx — run '$PROG ingest' first"
  local pattern="${1:-}"
  # gunzip, matching build_index: the index is deliberately the one artifact a
  # person can unpack with tooling that predates this script by thirty years.
  if [[ -z "$pattern" ]]; then
    age_d "$idx" | gunzip -c | esc_out
  else
    # pipefail carries grep's "no match" through esc_out, so the || still fires.
    age_d "$idx" | gunzip -c | grep -i -- "$pattern" | esc_out || {
      warn "no match for '$pattern'"; return 1; }
  fi
}

cmd_list() {
  need age; need unsquashfs; need sha512sum
  local n
  [[ -n "${1:-}" ]] || die "usage: $PROG list <disc-number>"
  n="$(num_arg "$1" 'disc number')"
  secure_staging "$ENC_DIR" "$RESTORE_DIR"
  resolve_identity
  local enc img
  enc="$(printf '%s/disc%02d.squashfs.age' "$ENC_DIR" "$n")"
  [[ -f "$enc" ]] || die "no image for disc $n in $ENC_DIR"
  prepare_image "$enc"; img="$PREPARED_IMG"
  # The listing carries attacker-chosen filenames, so it is escaped on a
  # terminal exactly as the index is — and left byte-faithful in a pipe.
  unsquashfs -ll "$img" | esc_out
}

# ---------------------------------------------------------------------------
# ---------------------------------------------------------------------------
usage() {
cat <<EOF
$PROG $VERSION — read and restore brb disc sets

This script RESTORES a disc set. It does not create one: writing discs lives in
the Go build (brb-linux-amd64 / brb-linux-aarch64, carried on every disc, or
go/ in the source tree). This half is kept small and readable on purpose, so
that someone holding a disc years from now can read exactly what will happen to
their bytes before running it.

USAGE
  $PROG [--yes] [-c CONFIG] <command> [args]

COMMANDS
  doctor                     check the tools a restore needs, and which key it
                             would use
  ingest [mount]             copy discs back onto disk (prompts for each, any
                             order); verifies each file against the disc and
                             keeps a second copy of a damaged image so par2 can
                             combine them
  restore <dest> [opts]      repair, decrypt and extract; refuses any image that
                             does not hold exactly the files the encrypted index
                             says its disc holds, because the discs of one set
                             have to agree with each other
                               --only <path>   extract one path, relative to the
                                               archive root, no leading '/';
                                               located via the encrypted index,
                                               so only the discs holding it are
                                               decrypted
                               --disc <n>      only this disc
  mount <n> <mountpoint>     decrypt one disc's image and mount it read-only
  list <n>                   list the contents of one disc's image
  index [pattern]            which disc holds a given path
  verify-disc <n> [mount]    read a burned disc back and check every hash
  help                       this text

CONFIGURATION
  Config file: $CONFIG_FILE
    The file is executed as bash, so trust it like the script itself: never
    point -c or BRB_CONFIG at a file you did not write, e.g. one from a disc.

    STAGING=$STAGING
                                  where ingested images and decrypted images go
    AGE_IDENTITY=                 the secret key. Searched for in order:
                                  \$AGE_IDENTITY, then that path with .age
                                  appended, then identity.txt,
                                  identity.txt.age and
                                  rescue-identity.txt.age beside
                                  $AGE_RECIPIENTS_FILE
                                  A PUBLIC archive carries its own key on every
                                  disc, as identity.txt at the disc root; ingest
                                  stages it as \$STAGING/enc/identity.txt and
                                  restore uses it — alone when nothing above is
                                  found, so such a set needs no key configured
    KEEP_IMAGES=$KEEP_IMAGES                 1 keeps each decrypted image after
                                  extracting it, for repeated restores. The
                                  default removes them, because otherwise a
                                  restore needs room for the whole archive twice
                                  (1/0, or true/false, yes/no, on/off)
    BURNER=$BURNER            the drive discs are read back from

  Everything about how a set was BUILT — source tree, disc geometry,
  compression, par2 parameters — is recorded in MANIFEST.txt on every disc.
  It is not configured here; this script only reads what is already burned.

RESTORING WITHOUT THIS SCRIPT
  The format is meant to outlive its tools. Given one disc and your identity:

    sha512sum -c discNN.squashfs.age.sha512 || par2 repair -- discNN.squashfs.age.par2
    age -d -i identity.txt -o discNN.squashfs discNN.squashfs.age
    mount -o loop,ro discNN.squashfs /mnt          # needs only the kernel

  If a .sha512 sidecar has rotted but par2 says the image is whole, it is the
  sidecar that is corrupt: repair it with 'par2 repair -- sidecars.par2' from
  the disc's data/ directory.

TYPICAL RESTORE
  $PROG doctor
  $PROG ingest                    # feed it every disc, any order
  $PROG index thesis              # which disc holds a given path?
  $PROG restore /path/to/destination
EOF
}

main() {
  local -a rest=()
  while (( $# )); do
    case "$1" in
      --yes|-y) CLI_ASSUME_YES=1; shift ;;
      -c|--config) CONFIG_FILE="${2:-}"; [[ -n "$CONFIG_FILE" ]] || die "-c needs a path"
                   CONFIG_EXPLICIT=1; shift 2 ;;
      *) rest+=("$1"); shift ;;
    esac
  done
  set -- "${rest[@]+"${rest[@]}"}"
  local cmd="${1:-help}"; shift || true
  load_config
  case "$cmd" in
    doctor)       cmd_doctor "$@" ;;
    # Writing discs moved to the Go implementation; this script reads them.
    # Name the commands explicitly rather than letting them fall through to
    # "unknown command", so anyone with the muscle memory is told where to go.
    backup|plan|burn|iso|init-key)
      die "'$cmd' is not in this script: brb.sh reads disc sets, it no longer writes them. Use the Go build (brb-linux-<arch>, or go/ in the source tree) to create, burn and manage a set. Everything needed to READ one is here: ingest, restore, mount, list, index, verify-disc." ;;
    verify-disc)  cmd_verify_disc "$@" ;;
    ingest)       cmd_ingest "$@" ;;
    restore)      cmd_restore "$@" ;;
    mount)        cmd_mount "$@" ;;
    list)         cmd_list "$@" ;;
    index)        cmd_index "$@" ;;
    help|-h|--help) usage ;;
    --version)    echo "$PROG $VERSION" ;;
    *) usage; die "unknown command: $cmd" ;;
  esac
}

main "$@"
