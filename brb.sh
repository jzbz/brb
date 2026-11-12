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
#       mount -o loop,ro,nosuid,nodev discNN.squashfs /mnt  (or unsquashfs)
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

VERSION="0.4.0"
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
# ASSUME_YES belongs on this list and was missing from it. It and KEEP_IMAGES
# are the pair this script reads from the config file, and the Go build applies
# every key from the environment after the file, so the two readers ordered the
# same two settings differently: an exported ASSUME_YES=0 lost to a config
# saying 1 here and won there. That is the setting that answers the
# confirmation in front of a restore overwriting its destination, so the reader
# that ignored the environment was ignoring it in the dangerous direction.
#
# Each value is remembered with ~ and $HOME expanded in it, by env_home_expand
# below. That is the Go build's expandHomePath (go/internal/config/parse.go),
# which ApplyEnv runs on every environment value, and this script used to take
# the value exactly as written. A shell has normally expanded both already, so
# the difference only shows when a literal marker arrives some other way — a
# systemd Environment= line, an env file, a quoted export — and then
# STAGING='~/brb' was $HOME/brb to the Go build and a directory named "~" under
# the current one here: ingest with one reader and restore with the other, and
# they were not looking at the same staging tree.
#
# env_home_expand VALUE — sets BRB_EXPANDED to VALUE with expandHomePath's three
# rules applied, in its order: a lone ~ or a leading ~/ becomes $HOME, then
# every ${HOME}, then every $HOME that is not the start of a longer name
# ($HOMEBREW stays). With HOME unset or empty the value is left as it is, as
# there. A variable rather than output, because $( ) would strip a trailing
# newline that the Go build keeps; and no ${s//pat/rep}, because bash 5.2 reads
# '&' in the replacement as the match, and HOME may contain one.
#
# shellcheck disable=SC2016  # every '$HOME' and '${HOME}' below is the literal
# text being searched for, single-quoted precisely so that it is not expanded.
env_home_expand() {
  local s="$1" h="${HOME:-}" out="" c
  BRB_EXPANDED="$s"
  [[ -n "$h" ]] || return 0
  # \~, not ~: a case pattern is tilde-expanded like any other word.
  case "$s" in
    \~)   BRB_EXPANDED="$h"; return 0 ;;
    \~/*) s="$h${s:1}" ;;
  esac
  while [[ "$s" == *'${HOME}'* ]]; do
    out+=${s%%'${HOME}'*}$h
    s=${s#*'${HOME}'}
  done
  s="$out$s"
  out=""
  while [[ "$s" == *'$HOME'* ]]; do
    out+=${s%%'$HOME'*}
    s=${s#*'$HOME'}
    c="${s:0:1}"
    # The letters spelled out: a range such as [A-Za-z] follows the locale's
    # collation in bash 4, and the Go side tests ASCII bytes.
    case "$c" in
      [ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_]) out+='$HOME' ;;
      *) out+=$h ;;
    esac
  done
  BRB_EXPANDED="$out$s"
}
BRB_ENV_OVERRIDES=()
for _v in STAGING AGE_RECIPIENTS_FILE AGE_IDENTITY BURNER KEEP_IMAGES ASSUME_YES; do
  # ${!_v:+x}, not ${!_v+x}: the second is true for a variable that is exported
  # but EMPTY, which made `KEEP_IMAGES= brb.sh restore ...` push an empty string
  # over whatever the config said, and bool_setting reads empty as 0. The Go
  # build skips an empty environment variable and says it does so "exactly as
  # brb.sh's ${VAR:-default} treats it" — which is the rule this line is meant
  # to implement, and ${VAR:-default} ignores an empty value too.
  #
  # An if, not `&&`: on the last iteration a false test would become the for
  # loop's status and errexit would end the script before it started.
  if [[ -n "${!_v:+x}" ]]; then
    env_home_expand "${!_v}"
    BRB_ENV_OVERRIDES+=( "$_v=$BRB_EXPANDED" )
  fi
done
unset _v

# Only the settings a restore uses. Everything that shaped how a set was BUILT —
# the source tree, the disc geometry, compression, the pack ratio, par2
# parameters, ISO mode — was decided when the set was written and is recorded in
# MANIFEST.txt on every disc. Re-declaring it here could only ever contradict
# what is already burned.
#
# A function, because load_config applies these a second time, after the config
# file and the environment; see there for why once was not enough.
reader_defaults() {
  STAGING="${STAGING:-/var/tmp/brb}"
  AGE_RECIPIENTS_FILE="${AGE_RECIPIENTS_FILE:-$HOME/.config/brb/recipients.txt}"
  AGE_IDENTITY="${AGE_IDENTITY:-}"                   # the secret key; without it nothing decrypts
  BURNER="${BURNER:-/dev/sr0}"                       # where discs are read back from
  KEEP_IMAGES="${KEEP_IMAGES:-0}"                    # 1 keeps each decrypted image for re-runs
}
reader_defaults

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
  # have_tty, not [[ -r /dev/tty && -w /dev/tty ]]. The mode bits are true in a
  # session with no controlling terminal — a cron job, a systemd unit, a
  # detached ssh command — where opening the file fails with ENXIO, so this
  # branch was taken, the read failed, and `|| return 1` turned that into a
  # plain "no". The caller then aborted the restore as though the operator had
  # declined it, and the die() below, written for exactly this case, could not
  # be reached to say "re-run with --yes if you mean it".
  #
  # have_tty exists because of that distinction and says so at its definition;
  # prompt_media and the passphrase prompt already use it. The Go reader opens
  # /dev/tty to find out too (openTTY in internal/ui/ui.go), so this is the
  # order both readers already documented and now the same test.
  if have_tty; then read -r -p "$1 [y/N] " reply </dev/tty || return 1
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
  # {1,9}, not +. 10# forces base 10 but not range, and bash arithmetic wraps
  # at 2^64 without a word: "18446744073709551618" came through this as 2, so
  # `restore DEST --disc 18446744073709551618` extracted disc 2 over the
  # destination with unsquashfs -f and exited 0, having been asked for a disc
  # that does not exist. Nine digits is past any disc count a set can have --
  # the number here only ever names a disc -- and it makes the wrap
  # unreachable by construction rather than by a range test of its own. The Go
  # reader refuses the same argument as a usage error. The digits are spelled
  # out, not [0-9], for the reason disc_number_of gives: under a UTF-8 locale
  # the range also admits "٣", and the arithmetic below then ends the run with
  # bash's own syntax error in place of this message.
  [[ "${1:-}" =~ ^[0123456789]{1,9}$ ]] || die "${2:-argument} must be a number, got '${1:-}'"
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
#
# The file holds the pid of whoever has the lock, and nothing else, because
# that is what LockStaging writes and what its refusal reads back to name the
# other run. This script used to open it with `>`, which truncates on open —
# BEFORE flock had said whether the lock was free — so a brb.sh that was about
# to be refused had already erased the pid a running Go backup wrote, and the
# next Go contender's message lost its " (pid N)". It opens with `<>` now,
# which creates the file without truncating it, and replaces the pid only once
# the lock is its own, as LockStaging does.
BRB_LOCK_FD=""
lock_staging() {
  [[ -z "$BRB_LOCK_FD" ]] || return 0
  if ! command -v flock >/dev/null 2>&1; then
    step "flock is not installed, so this run cannot tell whether another brb is already using $STAGING — run one brb at a time"
    return 0
  fi
  # Before the redirect, because bash has no O_NOFOLLOW to give it. `<>`
  # follows a symlink, and once the lock is held the pid write below truncates
  # what it opened, so a link left at this name empties the file it points at
  # with this run's privileges — under the sudo restore the README recommends,
  # any file on the machine. LockStaging opens the same path with O_NOFOLLOW
  # for exactly this reason (go/internal/fsx/lock.go) and refuses with the same
  # advice, so refuse here too rather than leaving the Go build the only one
  # that does.
  #
  # This is a name test and then an open, which is weaker than the flag: a
  # symlink planted in the gap is still followed. secure_dir has already proven
  # $STAGING is a real directory this account owns, so anything that could win
  # that race can write in there anyway; the case this catches is the link that
  # is already sitting at the name, which is the one that occurs.
  if [[ -L "$STAGING/.brb.lock" ]]; then
    die "the staging lock $(esc_str "$STAGING/.brb.lock") is a symlink (-> $(esc_str "$(readlink -- "$STAGING/.brb.lock" 2>/dev/null || true)")); this run truncates it and writes its pid into it, so following the link would empty a file that is none of this run's business — remove the link, or point STAGING at a directory you own"
  fi
  exec {BRB_LOCK_FD}<>"$STAGING/.brb.lock" \
    || die "could not open the staging lock $(esc_str "$STAGING/.brb.lock") — fix its permissions, or point STAGING at a directory you own"
  flock -n "$BRB_LOCK_FD" \
    || die "another brb is using the staging directory $(esc_str "$STAGING") — wait for it to finish, or point STAGING at a directory of its own; two runs sharing one staging tree can write the same image at the same time, and the result verifies clean and does not restore"
  # The lock is ours, so any pid in the file belongs to a run that has ended.
  # Replace it with this one. Through /dev/fd/N rather than the path: on Linux
  # that reopens the very file this descriptor locked, wherever the name now
  # points, and `>` there truncates it before the write. Best effort, exactly
  # as in LockStaging — the pid only ever decorates another run's refusal — so
  # a system with no /dev/fd simply keeps the stale one.
  { printf '%s\n' "$$" >"/dev/fd/$BRB_LOCK_FD"; } 2>/dev/null || true
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
# alike, so it can hide nothing. For the mount point the Go reader does not use
# Clean but dirPath (go/internal/restore/mount.go), which is this function rule
# for rule, so that both hand mount(8) the same bytes.
#
# The result is handed to unsquashfs -d, and in cmd_mount to mkdir and mount,
# as well as to the guards, so the two can never disagree about which directory
# they mean. "/" survives as "/", and an empty argument stays empty for the
# caller to reject.
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
# ownershipAdvice in go/internal/fsx/staging.go.
#
# The chown is pasted as root, so DIR goes into it through %q, as one shell
# word: bare, a STAGING with a space in it hands chown -R the existing directory
# before the space as well. %q spells control bytes as $'...' escapes, so it
# keeps the terminal safety esc_str gave this line.
ownership_advice() {  # ownership_advice DIR OWNER-UID
  if (( EUID == 0 )); then
    printf 'chown -R root %s if it is yours, or point STAGING at a directory root owns' "$(printf '%q' "$1")"
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
  # Something is at the path and it is not a file: refused whether -c or
  # BRB_CONFIG named it, in the Go build's words (config.Load). With -c this
  # used to say "not found", which a directory or a FIFO plainly is not; with
  # BRB_CONFIG it said "no config file" and ran on the defaults, while the Go
  # build read the FIFO, the device or the `<(...)` pipe — so
  # BRB_CONFIG=<(printf 'STAGING=...') gave the two readers two staging
  # directories — or refused the directory. -e and -f both follow a symlink, as
  # os.Stat does, so a link to a real config is still a config, and a dangling
  # one is still "not there" to both.
  if [[ -d "$CONFIG_FILE" ]]; then
    die "config file $CONFIG_FILE is a directory"
  fi
  if [[ -e "$CONFIG_FILE" && ! -f "$CONFIG_FILE" ]]; then
    die "config file $CONFIG_FILE is not a regular file"
  fi
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
  # And the defaults again, now that nothing else will set these. KEY= in the
  # file means "not overridden" to the Go build, which skips an empty value
  # (config.go, Config.set), and the README's own sample writes DIST_DIR= and
  # DISC_CAPACITY_BYTES= that way. Here the defaults were applied only before
  # the file was sourced, so a STAGING=, AGE_RECIPIENTS_FILE= or BURNER= in a
  # shared config overwrote them with nothing: an empty STAGING, which made
  # ENC_DIR "/enc" and left every staging command to die in secure_dir, an
  # identity looked for in the current directory instead of beside
  # ~/.config/brb/recipients.txt, and no burner at all, while the Go build ran
  # on all three defaults. The environment has already been put back above, so
  # an exported value still wins, and an empty one is still ignored.
  reader_defaults
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
#
# The exclusion is a command to paste, so STAGING goes into it through %q, as
# one shell word, the way ownership_advice gives its chown. In double quotes, a
# valid STAGING holding a $, a backtick, a " or a \ was expanded, run as a
# command or cut short by the shell it was pasted into, and root's tee wrote
# whatever came out into /etc/tmpfiles.d.
warn_staging_volatile() {
  case "$STAGING" in
    /var/tmp/*|/tmp/*)
      warn "$STAGING is under systemd-tmpfiles cleanup (/var/tmp: 30 days, /tmp: 10 days)"
      warn "a slow restore can lose ingested images there; prefer a dedicated path:"
      warn "  STAGING=/srv/brb-staging   (ideally on an encrypted volume)"
      warn "or exclude it:  printf 'x %s\\n' $(printf '%q' "$STAGING") | sudo tee /etc/tmpfiles.d/brb.conf"
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
  step "and none of them for the last step: mount -o loop,ro,nosuid,nodev discNN.squashfs /mnt"
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
    # numbered_images, as check_complete on the next line counts them: a glob
    # here also counted discx, disc00, and a directory under an image's name,
    # so this line could report more images than the line after it found.
    # Staging that holds only such names is not the same as empty staging,
    # though, so check_complete still runs then and names what is missing.
    # The warning is worded to be true of both kinds: a directory under
    # disc01's image name does carry a number, it is just not a file.
    local n; n="$(numbered_images | wc -l)"
    if (( n > 0 )); then
      ok "$n disc image(s) ingested in $ENC_DIR"
      check_complete || true
    elif [[ -n "$(find "$ENC_DIR" -maxdepth 1 -name 'disc*.squashfs.age' -print -quit 2>/dev/null || true)" ]]; then
      warn "nothing in $ENC_DIR named like a disc image is a file with a disc number in its name, so none of it counts towards the set"
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
#
# -type f, because the Go reader's dataFiles lists regular files only. Without
# it a FIFO, a directory or a symlink under the image's name passed this gate:
# a FIFO image that SHA512SUMS did not list had verify-disc call the disc
# verified, and one it did list hung the hash, where the Go reader called the
# same disc unrecognised.
#
# And the first name that carries a disc number, in byte order, because that
# is the name discOfDataFiles picks: dataFiles sorts, and NumberOf skips every
# name it cannot read a number from. This used to take whichever name find
# reported first, in directory order and numbered or not, so a hand-made disc
# with a discx.squashfs.age beside its disc01 could read as "discx" here: then
# verify-disc refused a disc the Go reader verified, and ingest staged the
# sidecar parity under the flat name, warning that the disc carried no numbered
# image, where the Go reader staged sidecars-disc01.par2. find prints one name
# per line, so a name holding a newline comes back in pieces, and one piece can
# read as a numbered name; the -f test on what was picked keeps such a piece
# from counting unless a file of that name really is there.
disc_identity() {
  local f
  while IFS= read -r f; do
    if [[ "$(disc_number_of "$f")" != 0 && -f "$1/data/$f" && ! -L "$1/data/$f" ]]; then
      printf '%s' "$f"; return 0
    fi
  done < <(find "$1/data" -maxdepth 1 -type f -name 'disc*.squashfs.age' -printf '%f\n' 2>/dev/null | LC_ALL=C sort)
  return 0
}

# The disc number in an image name such as disc07.squashfs.age, as a plain
# decimal integer; 0 when the name is not one this format produces. Any number
# of digits, so a set past 99 discs still works — the same rule as NumberOf in
# go/internal/disc/disc.go, which the Go writer and reader share.
#
# Bounded the way strconv.Atoi bounds it there, and never put through
# $((10#...)) to get there. That used to be the whole function, and bash
# arithmetic wraps at 2^64 without a word: disc18446744073709551617 came back
# as disc 1, so ingest staged its sidecars as disc 1's and a restore
# cross-checked it as disc 1, while disc9223372036854775808 came back negative,
# which refuse_foreign_image reads as "a name that says no disc". The leading
# zeros are stripped as text and the length decides the rest, so nothing that
# can wrap ever reaches arithmetic: up to eighteen significant digits always
# fit, nineteen fit up to Atoi's ceiling of 9223372036854775807 (compared as an
# eighteen-digit head and a last digit, each of which fits on its own), and
# anything longer is "not a disc number" here exactly as it is to Atoi.
#
# The digits are spelled out rather than written [0-9]. bash hands =~ to the C
# library's regcomp, which reads a range in the order of the locale's collation,
# and under en_US.UTF-8 that order puts the Arabic-Indic, fullwidth and
# superscript digits between 0 and 9: disc٣.squashfs.age came back as "٣", which
# is not a number to anything downstream, and every arithmetic test it then met
# printed a bash syntax error. NumberOf accepts the ten ASCII digits and nothing
# else, in any locale, and so does an explicit list.
disc_number_of() {
  local d="" head
  if [[ "${1:-}" =~ ^disc([0123456789]+)\.squashfs\.age$ ]]; then
    d="${BASH_REMATCH[1]}"
    d="${d#"${d%%[!0]*}"}"
  fi
  if [[ -n "$d" ]] && (( ${#d} <= 18 )); then printf '%s' "$d"; return 0; fi
  if (( ${#d} == 19 )); then
    head="${d:0:18}"
    if (( 10#$head < 922337203685477580 || (10#$head == 922337203685477580 && ${d:18:1} <= 7) )); then
      printf '%s' "$d"; return 0
    fi
  fi
  printf '0'
}

# Every staged image whose name carries a disc number, one path per line in
# ascending disc order: listNumbered in go/internal/restore/restore.go, which
# skips every other name. It is the set a whole-set restore works through and
# the set check_complete counts. Both used to be a bare glob of
# disc*.squashfs.age, which also matches discx, disc00 and disc+2 — names this
# format never writes, and which ingest nonetheless stages under whatever the
# disc called them. Such an image reached refuse_foreign_image as disc 0 and was
# extracted with the index cross-check "degraded", because its name did not say
# which disc it was, and it counted towards a complete set while it was at it.
# A forged disc carrying the genuine index needed nothing more than that name to
# land its tree in the destination, with no question asked. The Go reader never
# selects such a name, so neither does this.
#
# A glob rather than find, so that a name holding a newline stays one word
# rather than two half-lines, the second of which can read as a numbered name
# in the current directory. The glob comes back as itself when nothing matches,
# and a directory is skipped the way listNumbered skips one — by lstat, so a
# symlink is kept, as it is there. Every path printed has a basename that
# passed disc_number_of, which admits no newline, so one per line is safe to
# read back with mapfile.
numbered_images() {
  local f n line
  for f in "$ENC_DIR"/disc*.squashfs.age; do
    if [[ ! -e "$f" && ! -L "$f" ]]; then continue; fi
    if [[ -d "$f" && ! -L "$f" ]]; then continue; fi
    n="$(disc_number_of "${f##*/}")"
    if (( n > 0 )); then printf '%s\t%s\n' "$n" "$f"; fi
  done | sort -t $'\t' -k1,1n | while IFS= read -r line; do
    printf '%s\n' "${line#*$'\t'}"
  done
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
# MAX_SUM_BYTES is what bounds every sha512sum-format file this script reads —
# the disc's own SHA512SUMS in ingest and verify-disc, and a staged .sha512
# sidecar in restore — and it is agecrypt.maxSumBytes: the same number, counted
# the same way (sum_file_too_big). These files come off a disc somebody else
# handed over and are read with nothing in between so much as looking at their
# size. The line count below used to be called that bound and never was: a line
# can be any length, and `read -r` and awk both hold a whole line before
# anything counts it, so a newline-free SHA512SUMS of a few gigabytes on a
# "disc" that is a directory got this process OOM-killed with the count still
# at 1. A brb disc's SHA512SUMS lists on the order of ten files in a few
# kilobytes, so sixteen megabytes is unreachable rather than tight.
MAX_SUM_BYTES=16777216
# The line ceiling mirrors agecrypt.maxSumLines. Under MAX_SUM_BYTES it can only
# be reached by a file of blank or comment lines, which store nothing, so lines
# READ are counted rather than entries stored; it stays so that the two readers
# refuse the same files.
MAX_SUM_LINES=1048576

# sum_file_too_big FILE — true when FILE holds more than MAX_SUM_BYTES bytes, and
# true as well when it cannot be read to find out, so that a failure counts
# against the file rather than letting it through.
#
# Counted on bytes actually read, never taken from stat, for the three things a
# stat gets wrong: `stat -c %s` reports a symlink's own length while every read
# after it follows the link to its target; a procfs or FUSE file can report 0
# and then stream without end (a link to /proc/self/pagemap is a "regular file"
# of size 0); and a count of lines read plus one per newline misses carriage
# returns and invents a final newline. head stops one byte past the cap, so
# nothing larger is ever read in full. Failing closed is not a nicety: head gets
# EINVAL on pagemap's unaligned last read, and the count it had reached by then
# was exactly the cap, which would have let that file through.
sum_file_too_big() {  # sum_file_too_big FILE
  local sz
  sz="$(head -c "$(( MAX_SUM_BYTES + 1 ))" -- "$1" 2>/dev/null | wc -c)" || return 0
  sz="${sz//[[:space:]]/}"
  [[ "$sz" =~ ^[0-9]+$ ]] || return 0
  (( sz > MAX_SUM_BYTES ))
}

# sum_feed FILE — FILE as SUM_AWK is to read it: through head, so that what the
# parse holds is bounded by what is read rather than by what sum_file_too_big
# measured a moment earlier. Those are two opens of the file, and one that grew
# in between, or a FUSE file that serves more the second time, would otherwise
# hand awk an unbounded line after all. head stops two bytes past the cap, not
# one, because SUM_AWK's own count cannot tell whether the last line ended in a
# newline and may come out one byte over: at two past, a file head cut short is
# always counted past the cap and refused, never parsed as if it ended there.
sum_feed() {  # sum_feed FILE
  head -c "$(( MAX_SUM_BYTES + 2 ))" -- "$1"
}

# SUM_AWK — ReadSumFile and parseSumLine from go/internal/agecrypt/sums.go, as
# awk, so that a sha512sum-format file read here gets the answer the Go reader
# gets. A caller runs it under LC_ALL=C, where length() counts bytes, passes
# -v sum_max_lines="$MAX_SUM_LINES" -v sum_max_bytes="$MAX_SUM_BYTES", feeds it
# sum_feed FILE, appends its own rules and calls sum_line() on every record.
# sum_line returns 1 for a new entry, leaving the digest in sd and the name in
# sn, and 0 for a line that adds nothing: blank, a '#' comment, or a name
# already listed with the same digest. Anything Go's parse refuses ends the
# program with sum_bad set to "LINE: reason", and the caller's END says what
# that means to it. With -v sum_lenient=1 a line that does not parse is skipped
# instead (read_disc_sums says why); the caps, and one name listed with two
# digests, still end it.
#
# The rules, in Go's order. More bytes than sum_feed lets through, more than
# sum_max_lines lines, or a line of 1048576 bytes or more counting a trailing
# CR (bufio.Scanner's limit at maxSumLine), refuses the file, a comment line
# included. Every trailing CR is trimmed, as strings.TrimRight trims them. Then
# an optional leading backslash marking an escaped name, 128 hex digits in
# either case, one space or tab, an optional second space or '*' for binary
# mode, and a name that is not empty. One name listed twice with two different
# digests is a refusal, not a coin toss. GNU `sha512sum -c` accepts more than
# this — a BSD-style "SHA512 (name) = digest" line, and leading whitespace —
# which is half of why verify-disc stopped handing it the file (see
# verify_disc_sums).
#
# sn is not the name as Go's unescapeName leaves it but that name spelled for
# bash's `printf '%b'`, which turns it back into exactly those bytes: every
# backslash as \x5c, a newline as \n, a carriage return as \r, and nothing else
# touched. The spelling is one-to-one with the name, so duplicates are found on
# sn just as Go finds them on the name, and sn never holds a raw newline, which
# lets it travel to bash one record per line. A name holding none of the three
# — every name brb writes — passes through as it stands. \x5c rather than a
# doubled backslash because it lets sum_spell be a few gsub calls: a gsub
# replacement can write a backslash before a letter in every awk, where what
# two backslashes in a row come out as differs from one awk to the next; and
# one gsub is one pass over the name, where building it a byte at a time with
# out = out c copies the whole string per byte in mawk, which is quadratic in a
# megabyte-long name.
#
# The one byte that does not survive the trip to bash is NUL, which read drops.
# sum_read deals with that; every caller that wants the names goes through it.
# shellcheck disable=SC2016  # awk source, not shell: its $0 is awk's record and
# must reach awk unexpanded, exactly as in the awk programs written inline.
SUM_AWK='
function sum_fail(why) { sum_bad = NR ": " why; exit }
function sum_skip(why) { if (!sum_lenient) sum_fail(why); return 0 }
function sum_spell(s, esc) {
  if (s !~ /[\\\r\n]/) return s
  if (esc) {
    # unescapeName reads escapes left to right, so each \\ pair goes first and
    # is held as a raw newline, which no record contains. Every backslash left
    # after that escapes nothing — unescapeName keeps it, and the byte after it
    # — unless it starts \n or \r, which already spell the byte they stand for.
    gsub(/\\\\/, "\n", s)
    gsub(/\\/, "\\x5c", s)
    gsub(/\\x5cn/, "\\n", s)
    gsub(/\\x5cr/, "\\r", s)
    gsub(/\n/, "\\x5c", s)
  } else {
    gsub(/\\/, "\\x5c", s)
    gsub(/\n/, "\\n", s)
  }
  gsub(/\r/, "\\r", s)
  return s
}
function sum_line(    line, esc, c) {
  sum_bytes += length($0) + 1
  if (sum_bytes > sum_max_bytes + 1) sum_fail("more than " sum_max_bytes " bytes")
  if (NR > sum_max_lines) sum_fail("more than " sum_max_lines " lines")
  if (length($0) >= 1048576) sum_fail("a line of 1048576 bytes or more")
  line = $0; sub(/\r+$/, "", line)
  if (line == "" || substr(line, 1, 1) == "#") return 0
  esc = substr(line, 1, 1) == "\\"
  if (esc) line = substr(line, 2)
  if (length(line) < 130) return sum_skip("line too short for a sha-512 checksum")
  sd = tolower(substr(line, 1, 128))
  if (sd !~ /^[0-9a-f]+$/) return sum_skip("line does not start with a sha-512 hex digest")
  c = substr(line, 129, 1)
  if (c != " " && c != "\t") return sum_skip("missing separator after the digest")
  sn = substr(line, 130); c = substr(sn, 1, 1)
  if (c == " " || c == "*") sn = substr(sn, 2)
  if (sn == "") return sum_skip("missing file name")
  sn = sum_spell(sn, esc)
  if (sn in sum_seen) {
    if (sum_seen[sn] != sd) sum_fail("a name listed twice with different digests")
    return 0
  }
  sum_seen[sn] = sd
  return 1
}
'

# sum_read FILE [LENIENT] — the entries of a sha512sum-format file, as SUM_AWK
# reads it through sum_feed (with sum_lenient=LENIENT), left in SUM_DS (the
# digest), SUM_NS (the name, unescaped) and SUM_ZS (1 for a name that held a
# NUL), index for index, with awk's verdict in SUM_TAIL: "OK", or "LINE: reason"
# for a file Go's parse refuses. A verdict that never arrives, as when awk is
# killed, leaves SUM_TAIL empty, which no caller takes for a clean parse.
#
# Each entry crosses as one record: "E", the digest, the length of the name's
# spelling as eight digits, and the spelling. The length is for NUL, the one
# byte read drops: a spelling that arrives shorter than awk measured it held
# one, and a name holding a NUL is no file's name — not to the kernel, and not
# to Go, whose open refuses it. Without this, "./READ<NUL>ME.md" carrying
# README.md's digest verified in this reader and was a mismatch in the other,
# and the image's own line spelled with a NUL in it counted as listing the
# image here and not there. The comparison is of bytes, which is why LC_ALL=C
# is set here and only here: the callers go on in the operator's locale.
declare -a SUM_DS=() SUM_NS=() SUM_ZS=()
SUM_TAIL=""
sum_read() {  # sum_read FILE [LENIENT]
  local LC_ALL=C rec sp n
  SUM_DS=(); SUM_NS=(); SUM_ZS=(); SUM_TAIL=""
  while IFS= read -r rec; do
    case "$rec" in
      E*)
        SUM_DS+=( "${rec:1:128}" ); sp="${rec:137}"
        if (( ${#sp} == 10#${rec:129:8} )); then SUM_ZS+=( 0 ); else SUM_ZS+=( 1 ); fi
        printf -v n '%b' "$sp"; SUM_NS+=( "$n" ) ;;
      Z*) SUM_TAIL="${rec:1}" ;;
    esac
  done < <(sum_feed "$1" | LC_ALL=C awk -v sum_max_lines="$MAX_SUM_LINES" \
             -v sum_max_bytes="$MAX_SUM_BYTES" -v sum_lenient="${2:-0}" "$SUM_AWK"'
      sum_line() { printf "E%s%08d%s\n", sd, length(sn), sn }
      END { if (sum_bad != "") print "Z" sum_bad; else print "ZOK" }
    ')
}

# How many missing disc numbers check_complete will spell out. The same bound
# as maxNamedMissing in go/internal/restore/complete.go, and for the reason
# given there: MANIFEST.txt is read off a disc that may have rotted or been
# edited, and "discs : 3" is one flipped byte from "discs : 30000000".
MAX_NAMED_MISSING=64
# What check_complete last found missing — the list it printed and the
# manifest's count — kept for the closing lines of a restore that was told to
# carry on without those discs (reportOutcome in go/internal/restore/
# extract.go). Empty when the set was complete or its size could not be told.
SET_MISSING=""
SET_WANT=""

read_disc_sums() {  # read_disc_sums MOUNTPOINT
  DISC_SUMS=()
  local i n
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
  # Before a line is read, because awk holds a whole line before anything can
  # count it: one newline-free gigabyte was an OOM kill at line 1. The Go reader
  # refuses the same file in ReadSumFile, and discSums warns and checks nothing,
  # as this does.
  if sum_file_too_big "$1/SHA512SUMS"; then
    warn "SHA512SUMS on this disc is larger than $MAX_SUM_BYTES bytes, or could not be read; copies cannot be checked as they are made"
    step "a brb disc's SHA512SUMS lists about ten files in a few kilobytes, so this one was not written by brb"
    return 0
  fi
  # The file is read with SUM_AWK, the grammar the Go reader and verify-disc
  # read it with — every trailing CR trimmed, an escaped name unescaped, one
  # name with two digests refused — and one difference kept on purpose, which
  # is sum_lenient: a line SHA512SUMS's own rot has mangled is simply not a hash
  # for anything, so the file it named is copied unchecked, exactly as if it
  # were absent, and every other line is still used. The Go reader is stricter
  # — one such line and discSums checks nothing — which README's table of
  # reader differences records. Whatever else ReadSumFile refuses a file for,
  # this refuses it for too, and then nothing is checked, as there.
  #
  # It used to be `read -r h p` over the file itself. A CRLF file then kept the
  # CR on every name, so not one copy was checked against it and nothing said
  # so, where the Go reader checked them all; a million blank lines took a
  # minute or two; and a megabyte-long name took about nineteen seconds per
  # ${p#...} that stripped a '*' or a './' that was not there.
  sum_read "$1/SHA512SUMS" 1
  if [[ "$SUM_TAIL" != OK ]]; then
    # Nothing from a file the Go reader refuses is trusted, not even the lines
    # before the one it refused it for: discSums discards all of it.
    warn "SHA512SUMS on this disc cannot be read${SUM_TAIL:+ (line $SUM_TAIL)}; copies cannot be checked as they are made"
    step "a brb disc's SHA512SUMS lists about ten files in a few kilobytes, so this one is damaged or was not written by brb"
    return 0
  fi
  for (( i = 0; i < ${#SUM_NS[@]}; i++ )); do
    # A name that held a NUL is no file's name (sum_read), so this is nobody's
    # hash; nor is one that is "./" and nothing more.
    (( SUM_ZS[i] )) && continue
    n="${SUM_NS[i]}"; [[ "$n" == ./* ]] && n="${n:2}"
    [[ -n "$n" ]] || continue
    DISC_SUMS["$n"]="${SUM_DS[i]}"
  done
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
# The parse is SUM_AWK, which is ReadSumFile/parseSumLine from
# go/internal/agecrypt/sums.go, caps and all; of each name only the base is
# compared, as recordedSum compares filepath.Base — trailing slashes dropped
# first, as Base drops them. Two entries whose bases both match with two
# different digests are a refusal, as they are in recordedSum; two that merely
# share some other base are not, because recordedSum never looks at them. An
# escaped name is compared as Go unescapes it: sn is that name in SUM_AWK's
# one-to-one spelling, and NAME is spelled the same way before the two meet
# (no artifact this format writes contains a backslash, a newline or a
# carriage return, so on a disc brb wrote this never matters). A file larger
# than MAX_SUM_BYTES is rotted without being read, which is what ReadSumFile
# makes of it too.
recorded_sum() {  # recorded_sum SUMFILE NAME
  [[ -f "$1" ]] || return 1
  ! sum_file_too_big "$1" || return 2
  local out=""
  out="$(N="$2" LC_ALL=C awk -v sum_max_lines="$MAX_SUM_LINES" -v sum_max_bytes="$MAX_SUM_BYTES" "$SUM_AWK"'
    BEGIN { want = sum_spell(ENVIRON["N"], 0) }
    {
      if (!sum_line()) next
      b = sn; sub(/\/+$/, "", b); sub(/.*\//, "", b)
      if (b != want) next
      if (got != "" && got != sd) sum_fail("two different digests for " b)
      got = sd
    }
    END { if (sum_bad != "") exit 2; if (got == "") exit 1; print got }
  ' < <(sum_feed "$1"))" || return $?
  printf '%s' "$out"
}

# disc_sidecar_sum FILE [quiet] — the digest FILE's own per-file sidecar on the
# disc (FILE.sha512, beside it in data/) records for it, printed on stdout;
# nothing when there is no usable one. It is the second witness ingest
# consults, after SHA512SUMS, and for a disc whose SHA512SUMS is missing or
# rotted it is the only one left for the image and the index. The Go reader
# consults the same one in the same order (discWant in
# go/internal/restore/ingest.go); it did not until the two were found to
# disagree about the same disc — an image that failed its sidecar was counted
# bad here and staged as fine there, and a rotted index this disc's sidecar
# vouched against was ingested here as damage and refused there as another
# set's.
#
# The sidecar is read by the rules every other name off a disc is read by: -L
# before -f, because -f follows a link to a file that is not on the disc; and
# through recorded_sum, so it records a digest but does not get to say which
# file it is for. One that is a link, or does not parse, is no witness — said
# so unless "quiet" is passed, and then treated exactly as an absent one, which
# is also how restore treats the same sidecar once it is staged. Always
# returns 0: it is called inside an assignment, under set -e.
disc_sidecar_sum() {  # disc_sidecar_sum FILE [quiet]
  local f="$1" quiet="${2:-}" base out rc=0
  base="$(esc_str "${f##*/}")"
  if [[ -L "$f.sha512" ]]; then
    [[ -n "$quiet" ]] || warn "$base.sha512 on this disc is a symbolic link, which no disc brb wrote carries; $base cannot be checked against it"
    return 0
  fi
  [[ -f "$f.sha512" ]] || return 0
  out="$(recorded_sum "$f.sha512" "${f##*/}")" || rc=$?
  if (( rc == 0 )); then
    printf '%s' "$out"
  elif (( rc == 2 )) && [[ -z "$quiet" ]]; then
    warn "$base.sha512 on this disc is damaged, so $base cannot be checked against it either"
  fi
  return 0
}

# alt_copy_name DST — where a further pressing's copy of the staged DST goes:
# DST.copy<epoch>, the name prepare_image hands par2, at the first epoch at or
# after now whose name is FREE. It used to be now, whatever was there: cp
# truncates an existing file in place, and the Go reader renames over one, so
# two pressings read within one second — a clock stepped back, a machine with
# no RTC that boots to the same second, a script looping ingest over small
# images — had the second destroy the first, the very copy par2 might need,
# without a word. The test is -e or -L, and anything else — a name too long
# for the filesystem, an unreadable directory — reads as free, so the copy
# fails on it and says why instead of the loop running on; altCopyName in the
# Go reader stops on exactly the same answers. The staging lock keeps any
# other run from taking the name in between.
alt_copy_name() {  # alt_copy_name DST
  local t
  t="$(date +%s)"
  while [[ -e "$1.copy$t" || -L "$1.copy$t" ]]; do t=$(( t + 1 )); done
  printf '%s' "$1.copy$t"
}

# same_alt_copy DST ALT [DIGEST] — the name of a copy of DST already staged
# beside it whose bytes are ALT's, printed on stdout; nothing when there is
# none. Re-reading a pressing that is already staged gives par2 nothing new,
# and is not worth a whole image of staging. DIGEST is ALT's hash when the
# caller already has it; without it ALT is hashed here, and only once there is
# a candidate to compare it with, so the usual case — no earlier copy at all —
# costs no second read of an image just written.
#
# Only a complete pressing's copy is a candidate: a regular file named
# DST.copy* (-L before -f, so a link is not followed to one), and not ALT
# itself, a salvage's map file, a copy still being written, or an incomplete
# salvage — a copy with a map file beside it, whose unread regions are zeros
# the drive never returned, so a complete read matches it only by accident. A
# copy that cannot be hashed reads as different, and so does an ALT that
# cannot, so the new one is kept: keeping is the side to err on. sameAltCopy
# in the Go reader compares the same names the same way. The directory part of
# the glob is quoted, so a staging path holding glob metacharacters is matched
# literally (see par2_repair_image); with no nullglob, an unmatched pattern
# comes through as itself and -f drops it.
same_alt_copy() {  # same_alt_copy DST ALT [DIGEST]
  local c sum="${3:-}"
  for c in "$1".copy*; do
    [[ ! -L "$c" && -f "$c" && "$c" != "$2" && "$c" != *.mapfile && "$c" != *.part ]] || continue
    [[ ! -e "$c.mapfile" && ! -L "$c.mapfile" ]] || continue
    if [[ -z "$sum" ]]; then
      sum="$(sha512_of "$2")" || sum=""
      [[ -n "$sum" ]] || return 0
    fi
    if [[ "$(sha512_of "$c")" == "$sum" ]]; then printf '%s' "${c##*/}"; return 0; fi
  done
  return 0
}

# ingest_one SRC DST WANT — bring one file off the disc into staging, or say
# why not. DST is where it is stored (the staged name, which for the sidecar
# parity differs from the on-disc name); WANT is the hash the disc records for
# it — its SHA512SUMS entry, or failing that its own sidecar (disc_sidecar_sum)
# — and "" when the disc records nothing usable. Returns non-zero for a copy
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
# beside the first as <name>.copy<epoch>, the name prepare_image passes (the
# first such name that is free; see alt_copy_name). A copy that is the same
# bytes as one already staged is NOT a second pressing, and is not kept twice
# (see same_alt_copy).
ingest_one() {
  local src="$1" dst="$2" want="$3"
  local base name got have alt same
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
      have="$got"
      warn "the staged $name does not match the hash on this disc; reading this disc's copy"
      alt="$(alt_copy_name "$dst")"
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
        # Only DIFFERENT damage is worth a second copy. A disc that returns
        # the same wrong bytes every time — burned wrong, read through a
        # faulty drive, or carrying a sums line that was wrong from the start —
        # used to leave one more byte-identical .copy per re-read, each a whole
        # image of staging and one more operand for par2, with no new block in
        # any of them; and the summary at the end of cmd_ingest used to invite
        # exactly those re-reads. replaceOrKeepBoth in the Go reader drops the
        # same copies. The new copy's digest is tested for being there at
        # all: the caller runs this function on the left of ||, where set -e
        # is off, so a sha512_of that failed leaves an empty one rather than
        # ending the run, and two empty digests are not two equal images.
        same=""
        if [[ -n "$got" && "$got" == "$have" ]]; then
          same="the staged one"
        else
          same="$(same_alt_copy "$dst" "$alt" "$got")"
          [[ -z "$same" ]] || same="$(esc_str "$same"), staged from an earlier read"
        fi
        if [[ -n "$same" ]]; then
          rm -f -- "$alt" "$alt.mapfile"
          warn "$name: this disc's copy is byte for byte $same, and does not match the hash this disc records for it either, so it was not kept a second time — reading this disc again will not help; ingest another pressing, or let par2 repair it during '$PROG restore'"
          return 1
        fi
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
      alt="$(alt_copy_name "$dst")"
      warn "already have $name, and it differs from the copy on this disc"
      step "ingesting this disc's copy as $(esc_str "${alt##*/}") for par2 to combine during '$PROG restore'"
      copy_file_robustly "$src" "$alt" || return 1
      # It differs from the primary, but it may be a pressing read before: the
      # same disc ingested twice is the same bytes twice. same_alt_copy hashes
      # the new copy itself, and only when there is an earlier one to compare.
      same="$(same_alt_copy "$dst" "$alt")"
      if [[ -n "$same" ]]; then
        rm -f -- "$alt" "$alt.mapfile"
        step "this disc's copy of $name is byte for byte $(esc_str "$same"), staged from an earlier read, so it was not kept a second time"
      fi
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
      # exists at all, so nothing good is overwritten. A second ingest of
      # another pressing then lands as the .copy<epoch> above, exactly the pair
      # par2 can combine, or replaces it outright once proven whole; a second
      # ingest of THIS disc, the same bytes again, is recognised there and not
      # kept twice. (Filing it away under a .bad name kept bytes par2 could often
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
# Runs after refuse_foreign_index and before the data files are touched, so a
# disc refused by either leaves nothing behind. Returns non-zero for a copy
# that could not be trusted, which the caller counts like any other bad file;
# the same key is on every other disc.
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
# Runs before anything from the disc is staged — ahead of the published key as
# well as the data files — so a refused disc leaves nothing behind.
refuse_foreign_index() {  # refuse_foreign_index MOUNTPOINT
  local mp="$1" src="$1/data/index.tsv.gz.age" dst="$ENC_DIR/index.tsv.gz.age" want
  # -L before -f, as everywhere a disc's names are read: the copy loop's
  # `find -type f` never stages a linked index, so its target must not be what
  # decides whether the disc is refused (the Go reader judges only the regular
  # files dataFiles lists).
  [[ ! -L "$src" && -f "$src" && -f "$dst" ]] || return 0
  # cmp fails as well when it cannot read the disc's copy at all, and that is
  # taken for a difference below: refused as another set's when nothing on the
  # disc records the index's hash, read as rot when something does. The Go
  # reader judges no unreadable copy here — it stages what it can and fails
  # the disc at its index — which README.md's table of reader differences
  # records.
  cmp -s -- "$src" "$dst" && return 0
  # The same two witnesses cmd_ingest uses for every other file, in the same
  # order: the disc's SHA512SUMS, then the per-file sidecar beside it. Quiet,
  # because the copy loop asks again for the index and says so then if the
  # sidecar is damaged.
  want="${DISC_SUMS[data/index.tsv.gz.age]:-}"
  [[ -n "$want" ]] || want="$(disc_sidecar_sum "$src" quiet)"
  if [[ -n "$want" && "$(sha512_of "$src")" != "$want" ]]; then
    warn "the index on this disc differs from the staged one, but does not match the hash this disc records for it either — reading that as rot on this disc, not as a second set"
    return 0
  fi
  unmount_disc
  die "the disc at $(esc_str "$mp") carries a different index (index.tsv.gz.age) from the one already in $dst, and every disc of one set carries the identical index: either two different sets are being ingested into one staging area, or one of these discs is not from your set. Nothing from this disc has been staged. Ingest each set into a STAGING of its own; if instead the STAGED index is the rotted one, $(sidecar_repair_hint 0), or delete $dst and ingest this disc again to take its copy."
}

# verify_disc_sums MOUNTPOINT IMAGE — check a disc against its own SHA512SUMS
# the way agecrypt.VerifyDir checks it for the Go reader: read every name the
# file lists, then hash each one that is a regular file on the disc reached
# without a symbolic link, and compare. Returns 0 when every listed file
# matches and 1 after naming each one that does not; dies when the file cannot
# vouch for the disc at all. IMAGE is the image's name (discNN.squashfs.age),
# which has to be listed.
#
# WHY NOT `sha512sum -c` ANY MORE: the reason recorded_sum gives, met here
# twice. It hashes whatever path each line names, so a SHA512SUMS reading
# "<digest of the empty file>  /dev/null" had the disc reported verified without
# a byte of it read. A textual check in front of it closed that spelling and not
# the others. --strict also takes a BSD-style "SHA512 (/dev/null) = ..." line
# and one with leading whitespace, which the check skipped as unparseable; and
# sha512sum follows a symlink anywhere at all, so the image linked to /dev/null
# beside the empty file's digest was "disc 1 verified", a listed link to a host
# file carrying that file's digest verified too, and a listed link to /dev/zero
# read forever. The sums file gets to record digests; it does not get to choose
# what is read. So the file is parsed here with ReadSumFile's grammar (SUM_AWK)
# and every name is held to the rule hashInRoot in
# go/internal/agecrypt/sums.go applies, so that the two readers refuse the same
# discs:
#
#   - The file is at most MAX_SUM_BYTES and every line parses. A line that does
#     not refuses the whole file, as it does in Go, rather than being a warning
#     that leaves the file it named unhashed — the case --strict was there for.
#   - It lists something, and it lists data/IMAGE, with or without a leading
#     ./. Without these an empty or comment-only file, or one that simply leaves
#     the image out, reported a disc verified with its image never read.
#   - No name is absolute or has a .. component. That refuses the disc outright
#     rather than counting a mismatch: it is not damage, it is a checksum file
#     brb did not write. (The Go reader counts it as a mismatch; both exit 1.)
#   - Then each name, with one leading ./ dropped, must be under 4096 bytes, no
#     directory on the way to it may be a symbolic link or anything but a
#     directory, and its last component must be a regular file and not a link
#     (sum_name_is_file). A name that fails is a mismatch, as in Go, and so is a
#     missing file. The names are resolved from inside the disc root, so the
#     kernel holds them to the same PATH_MAX Go's maxSumName stands for.
#   - A name holding a NUL byte is a mismatch without being looked up, and it
#     is not the image's line either, however the rest of it reads: no file
#     name can hold a NUL, and Go's open refuses it (sum_read says how it is
#     seen at all, given that bash's read drops it).
#
# Nothing brb writes fails any of this: WriteSums lists every regular file as
# ./relative/path, never through a link, and the image always among them.
verify_disc_sums() {  # verify_disc_sums MOUNTPOINT IMAGE
  local mp="$1" image="$2" n rel got i bad=0 found=0
  ! sum_file_too_big "$mp/SHA512SUMS" \
    || die "SHA512SUMS on this disc is larger than $MAX_SUM_BYTES bytes, or could not be read; a brb disc's lists a handful of files in a few kilobytes, so this one is damaged or was not written by brb"
  sum_read "$mp/SHA512SUMS"
  [[ "$SUM_TAIL" == OK ]] \
    || die "SHA512SUMS on this disc does not parse${SUM_TAIL:+ (line $SUM_TAIL)} — it is damaged or was not written by brb, so it cannot vouch for this disc"
  (( ${#SUM_NS[@]} > 0 )) \
    || die "SHA512SUMS on this disc lists no files, so it cannot vouch for any"
  for (( i = 0; i < ${#SUM_NS[@]}; i++ )); do
    rel="${SUM_NS[i]}"; [[ "$rel" == ./* ]] && rel="${rel:2}"
    if (( ! SUM_ZS[i] )) && [[ "$rel" == "data/$image" ]]; then found=1; fi
    if [[ "$rel" == /* || "/$rel/" == */../* ]]; then
      die "SHA512SUMS on this disc records a hash for $(esc_str "${SUM_NS[i]}"), which is not a file on the disc — hashing it would call the disc verified without reading it. This is not a checksum file brb wrote; treat the disc as untrusted."
    fi
  done
  (( found )) \
    || die "SHA512SUMS on this disc records no hash for data/$(esc_str "$image"), so it cannot vouch for the disc's image — it is damaged or was not written by brb"
  # From inside the disc root, in a subshell so the working directory and the
  # mount are left as they were: every path handed to the kernel is then the
  # name itself, which is what holds it to PATH_MAX exactly as the Go reader's
  # maxSumName does. The subshell's status is this function's.
  ( CDPATH='' cd -P -- "$mp" || exit 1
    for (( i = 0; i < ${#SUM_NS[@]}; i++ )); do
      n="${SUM_NS[i]}"; rel="$n"; [[ "$rel" == ./* ]] && rel="${rel:2}"
      if (( SUM_ZS[i] )); then
        warn "mismatch: $(esc_str "$n") — as listed, the name holds a NUL byte, which no file name can"; bad=1; continue
      fi
      if ! sum_name_is_file "$rel"; then
        warn "mismatch: $(esc_str "$n")"; bad=1; continue
      fi
      got="$(sha512_of "$rel")" || got=""
      [[ "$got" == "${SUM_DS[i]}" ]] || { warn "mismatch: $(esc_str "$n")"; bad=1; }
    done
    exit "$bad" )
}

# sum_name_is_file REL — true when REL, a path relative to the working directory,
# reaches a regular file without passing through a symbolic link: hashInRoot's
# walk in go/internal/agecrypt/sums.go, component for component. Empty and "."
# components are skipped, as the Go walk skips them; a name ending in "/" or "."
# names a directory and is not a file; .. never gets this far (verify_disc_sums
# refuses it) and is refused again here so the function stands on its own.
#
# Every step is linear in the name. A name of 4096 bytes or more is refused
# before any of it — the kernel would refuse it anyway, and walking a megabyte
# of name a component at a time costs a megabyte per component. ${#rest} counts
# characters, so a shorter name of many-byte characters gets walked, and then
# the kernel says ENAMETOOLONG instead, which is the same answer.
sum_name_is_file() {  # sum_name_is_file REL
  local rest="$1" cur="" comp
  (( ${#rest} < 4096 )) || return 1
  while [[ "$rest" == */* ]]; do
    comp="${rest%%/*}"; rest="${rest:${#comp}+1}"
    [[ -z "$comp" || "$comp" == . ]] && continue
    [[ "$comp" != .. ]] || return 1
    cur+="$comp"
    [[ ! -L "$cur" && -d "$cur" ]] || return 1
    cur+=/
  done
  [[ -n "$rest" && "$rest" != . && "$rest" != .. ]] || return 1
  [[ ! -L "$cur$rest" && -f "$cur$rest" ]]
}

cmd_verify_disc() {
  local n mp want actual marc
  # Validate before any printf '%02d': bash rejects "08" as bad octal and the
  # failing assignment would end the run with nothing but a printf error.
  n="$(num_arg "${1:-}" 'disc number')"
  mount_disc "${2:-}"; mp="$MOUNT_POINT"
  trap unmount_disc EXIT
  # -L first: -f follows a link, so a SHA512SUMS linked to a file off the disc
  # had its hashes read from wherever it pointed and the disc called verified.
  # Ingest only warns (read_disc_sums), since there SHA512SUMS is advice beside
  # each file's own sidecar; here it is the whole verdict. The Go reader refuses
  # the same name through discEntry.
  [[ -L "$mp/SHA512SUMS" ]] && die "$(esc_str "$mp"): SHA512SUMS on this disc is a symbolic link, which no disc brb wrote carries — nothing was read through it"
  [[ -f "$mp/SHA512SUMS" ]] || die "no SHA512SUMS at $mp — is this one of ours?"
  # -L before the identity check, as ingest has it: a disc brb wrote holds
  # data/ as a real directory, and `find -P` will not descend a symlinked one,
  # so without this the disc read as unrecognised rather than hostile. The Go
  # reader refuses the same name through discEntry.
  [[ -L "$mp/data" ]] && die "$(esc_str "$mp"): data on this disc is a symbolic link, which no disc brb wrote carries — nothing was read through it"

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

  log "verifying disc $n at $mp"
  if verify_disc_sums "$mp" "$want"; then
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
  # Spelled out rather than [0-9], which under a UTF-8 locale also admits "٣"
  # (see disc_number_of): a manifest reading "discs : ٣" got through here, and
  # check_complete's own arithmetic then ended the whole restore with a bash
  # syntax error, where Atoi refuses the field and the Go reader goes on
  # without knowing the set's size.
  [[ "$v" =~ ^[0123456789]+$ ]] || return 1
  # A string of digits is not yet a number. check_complete puts this through
  # (( )), where a leading zero means OCTAL and anything past 2^64 wraps: a
  # manifest reading "discs : 010" let 8 staged images satisfy a ten-disc set,
  # and "discs : 18446744073709551616" let any number of images satisfy any
  # set — the completeness check announcing "all present" for a partial one,
  # which is the single thing it exists to prevent. MANIFEST.txt is read off a
  # disc, so those spellings are an attacker's to choose.
  #
  # The Go reader takes the same field with strconv.Atoi, which is decimal and
  # refuses what will not fit, so strip the zeros and refuse the rest here.
  # Past 18 digits this answers "cannot tell" where Atoi still parses to
  # 2^63-1; both readers then decline to act on the number, and no manifest
  # either of them writes can hold one.
  v="${v#"${v%%[!0]*}"}"
  [[ -n "$v" && ${#v} -le 18 ]] || return 1
  printf '%s' "$v"
}

# Every disc carries the full directory skeleton, so a partial set restores a
# complete-looking tree with files silently absent. Name the missing discs.
#
# What is counted is the distinct disc NUMBERS from 1 to the manifest's count
# that have an image in staging, as checkComplete in go/internal/restore/
# complete.go counts them — not how many names match a glob. A count of names
# was satisfied by things that are not discs of this set at all: disc001 beside
# disc01 (two names, one disc), a disc03 in a two-disc set, a discx, even a
# directory with an image's name. Each of them made "all 2 disc image(s)
# present" true of a set with no disc 2, and so took away the one warning, and
# the one prompt, that a partial restore exists to raise. "N present" counts
# the same thing, the distinct disc numbers of this set that are staged, so
# that it and the MISSING list add up to the manifest's count; it used to
# count names, and disc001 beside disc01 read "2 discs; 2 present.
# MISSING: 2". Go's checkComplete prints the same figure.
#
# Returns 1 for an incomplete set, with SET_MISSING and SET_WANT saying what
# was missing; 0, with both empty, for a complete one or an unknown size.
check_complete() {
  local want inrange=0 missing=() i f n
  local -A present=()
  SET_MISSING="" SET_WANT=""
  # || true: expected_discs returns 1 when there is no usable manifest, and a
  # failing command substitution in an assignment is fatal under set -e.
  want="$(expected_discs || true)"
  [[ -n "$want" ]] || { warn "no MANIFEST.txt in $STAGING — cannot tell how many discs this set has"; return 0; }
  # n >= 1 is tested here and not left to numbered_images, which already drops
  # a name that says no disc, so that the bound is this function's own, as it
  # is Go's.
  while IFS= read -r f; do
    n="$(disc_number_of "${f##*/}")"
    if (( n >= 1 && n <= want )) && [[ -z "${present[$n]:-}" ]]; then inrange=$(( inrange + 1 )); fi
    present[$n]=1
  done < <(numbered_images)
  (( inrange == want )) && { ok "all $want disc image(s) present"; return 0; }
  # Named at most MAX_NAMED_MISSING of them, and the count carries the rest.
  # This loop used to run to $want, which is a number off a disc: a manifest
  # claiming two billion discs spent the restore building a two-billion-entry
  # array and a warning line to match. fsx's side bounds the same list at 64
  # (maxNamedMissing in go/internal/restore/complete.go) and prices the
  # unbounded version at 1.6 GB and a 168 MB line; this is that bound. It stops
  # early in every real case too, since it can only run past the last staged
  # image by the number of names it is still allowed to print. It asks the set
  # of numbers rather than the file system, so a disc staged as disc001 is
  # disc 1 here as it is to Go, and the rest is reckoned from the discs that
  # count rather than from the names that happen to be there.
  for (( i = 1; i <= want && ${#missing[@]} < MAX_NAMED_MISSING; i++ )); do
    [[ -n "${present[$i]:-}" ]] || missing+=("$i")
  done
  local shown="${missing[*]}" rest=$(( want - inrange - ${#missing[@]} ))
  (( rest > 0 )) && shown="$shown ... and $rest more"
  warn "MANIFEST says $want discs; $inrange present. MISSING: $shown"
  warn "files on those discs will NOT be restored"
  SET_MISSING="$shown" SET_WANT="$want"
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
    # And a file at the top of it, which is all the copy loop below reads: the
    # Go reader fails a data/ with none as empty (dataFiles lists the same one
    # level of regular files). This reader used to take an empty data/ for a
    # disc with nothing left to stage and report the run a success — and, once
    # the loop listed one level only, a data/ holding nothing but a
    # subdirectory too.
    [[ -n "$(find "$mp/data" -mindepth 1 -maxdepth 1 -type f -print -quit 2>/dev/null || true)" ]] \
      || die "$(esc_str "$mp")/data holds no file — is this one of ours? (the trap above has unmounted it, so the tray will open)"
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
    # The index first: it is the one file every disc of a set carries
    # identically, so a disc whose copy disagrees is either from another set
    # or forged, and is refused before any of its same-named files can be
    # taken for damaged copies of this set's. refuse_foreign_index only reads.
    # It has to come before the published key, not after: staging the key is
    # a write, and a public disc refused here used to leave its key behind in
    # $ENC_DIR/identity.txt — under a refusal saying nothing had been staged,
    # and live, since resolve_identity then offered it as the archive's own
    # key. The Go reader checks at the same point (refuseForeignIndex).
    refuse_foreign_index "$mp"
    # The published key second, by the same rule: a disc from a different
    # public set is refused before any of its files land. Neither check has
    # written anything when it refuses, so a refused disc leaves staging
    # untouched.
    ingest_public_identity "$mp" || bad=$(( bad + 1 ))
    # One level only, as dataFiles lists it in the Go reader and disc_identity
    # above: the format's data/ is flat, and a file nested in a subdirectory
    # used to be staged under its bare name here — data/sub/disc01.squashfs.age
    # as disc01's image — while the Go reader ignored it.
    while IFS= read -r f; do
      base="${f##*/}"
      # Names staging gives a meaning of its own (stagingOwnName in
      # go/internal/restore/ingest.go, which says what each one is). No disc
      # brb wrote carries one in data/, and staged under its own name each
      # would be taken for staging's: a disc's *.mapfile as a salvage map the
      # Go reader resumes INTO the staged image, a *.copy* as another pressing
      # for par2, data/identity.txt as the published key, sidecars-disc* as
      # another disc's parity.
      case "$base" in
        *.part|*.mapfile|*.squashfs.age.copy*|identity.txt|sidecars-disc*)
          warn "$(esc_str "$base") on this disc has a name staging keeps for its own bookkeeping, and no disc brb wrote carries one in data/ — not staging it"
          continue ;;
      esac
      want="${DISC_SUMS[data/$base]:-}"
      [[ -n "$want" ]] || want="$(disc_sidecar_sum "$f")"
      ingest_one "$f" "$ENC_DIR/$(staged_name "$base" "$disc")" "$want" || bad=$(( bad + 1 ))
    done < <(find "$mp/data" -mindepth 1 -maxdepth 1 -type f | sort -V)
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
  # Counted as check_complete below counts them, and as the Go reader's
  # summary does (listNumbered): this used to count every disc*.age under
  # staging at any depth, which could contradict the very next line.
  ok "ingested $(numbered_images | wc -l) image(s)"
  (( bad == 0 )) \
    || warn "$bad file(s) are incomplete or failed their hash — ingest another pressing of those discs, or let par2 repair them during '$PROG restore'"
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
  # name is whatever the disc's data/ directory called it, and ingest stages it
  # under exactly those bytes. The restore glob used to pass on any name of the
  # disc*.squashfs.age shape whatever sat in the middle; every caller now hands
  # over a numbered name (numbered_images, or one built from a disc number), so
  # this is the second line of defence rather than the only one. Raw $base is
  # what the paths and the comparisons use; every message below prints $ebase,
  # so an ESC in a name retitles nobody's terminal on the way past.
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

# check_index_intact — compare the staged encrypted index with the hash
# recorded beside it, before anything reads it; checkIndexIntact in
# go/internal/restore/extract.go, with the same three answers. A matching hash
# is a step and a missing one a warning, since age's authentication still
# stands between a damaged index and a plausible wrong answer. A mismatch, or a
# sidecar too damaged to parse, returns 1 with the sentence in INDEX_FAULT, and
# what that costs is the caller's to decide, as it is in Go: 'index' stops on
# it, and a restore takes it as an index that will not read — --only searches
# every disc rather than trust its map, and the image cross-check degrades.
#
# This script never used to look at that sidecar, so an index rotted past its
# first age chunk was read as the map of which disc holds what for every row
# that came out before the damage, and nothing said the hash beside it
# disagreed — in the reader whose whole reason to exist is the day something
# has rotted, while the Go reader refused to trust the same file.
#
# A hash that could not be taken is a fault of its own, not a mismatch: with
# sha512sum missing or the index unreadable, sha512_of prints nothing, and
# comparing that nothing with the recorded digest told the operator their index
# had rotted and sent them to par2 over a file nobody had looked at. Go fails
# the same way, with "hashing index.tsv.gz.age" and the reason.
INDEX_FAULT=""
check_index_intact() {
  local idx="$ENC_DIR/index.tsv.gz.age" want="" got="" rc=0
  INDEX_FAULT=""
  want="$(recorded_sum "$idx.sha512" index.tsv.gz.age)" || rc=$?
  case "$rc" in
    0) ;;
    1) warn "no recorded hash for index.tsv.gz.age; age's own authentication is the only check performed"
       return 0 ;;
    *) INDEX_FAULT="index.tsv.gz.age.sha512 is damaged and cannot be read — $(sidecar_repair_hint 0), then retry"
       return 1 ;;
  esac
  got="$(sha512_of "$idx")" || {
    INDEX_FAULT="could not hash index.tsv.gz.age to check it against index.tsv.gz.age.sha512 (the error is above)"
    return 1; }
  if [[ "$got" == "$want" ]]; then
    step "index.tsv.gz.age matches its recorded hash"
    return 0
  fi
  INDEX_FAULT="index.tsv.gz.age does not match the hash in index.tsv.gz.age.sha512 — one of the two has rotted; $(sidecar_repair_hint 0), then retry"
  return 1
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
# staging to compare against or will not read — which includes failing its own
# recorded hash, as it does for the Go reader's crossCheckRows — or the image's
# own name does not say which disc it is. The warning names which, so the
# operator knows the check did not run.
#
# The hash arm has a price that both readers pay. Ingest pins the index and not
# the hash beside it, which is an ordinary sidecar there, replaced by any later
# disc whose own SHA512SUMS vouches for a different one. So a disc carrying the
# genuine index and a junk index.tsv.gz.age.sha512 switches this check off for
# every image of the set, loudly. It gives a forger nothing that a
# newline-named file in their own image does not already give them, and
# README's Limitations says so; closing it is a change to ingest on both sides.
#
# That last one is a guard and not a path cmd_restore takes: it chooses images
# through numbered_images, or by --disc N, so every image it hands here has a
# number. The arm used to be reachable, and was the whole of an attack — a
# forged image named discx.squashfs.age was waved through by it — which is why
# a name that says no disc is now never selected rather than merely warned
# about; crossCheckRows in go/internal/restore/extract.go says the same of its
# own n == 0 arm.
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
  elif ! check_index_intact; then
    why="the index would not read ($INDEX_FAULT)"
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
    #
    # Exit 4 is rows that do not parse, on any disc, counted on a last line
    # that starts with '#'. Such rows used to be skipped without a word, so the
    # list compared below was short by whatever they had held, and an image
    # holding those paths was refused as foreign. A damaged index cannot say
    # what the disc holds, so indexRowsForDisc in go/internal/restore/locate.go
    # degrades the check for it instead, and so does this. A newline outranks
    # it there too.
    #
    # A row is read as indexfmt.ParseLine reads it, because the verdict here
    # is the Go reader's verdict or the two disagree about the same disc:
    # everything up to the first tab is the disc number, which strconv.Atoi
    # takes with a '+' in front and leading zeros, and refuses below 1 or past
    # 2^63-1 (on the 64-bit builds brb ships); everything after it is the
    # path, raw tabs included, and must not be empty. The number is compared
    # as a string, both sides stripped of their zeros, since awk's numbers are
    # doubles and lose digits past 2^53. The --only lookup in cmd_restore is
    # stricter: it holds its numbers to nine digits, for bash's arithmetic, so
    # a '+', a number past 999999999 and a raw tab are rows it cannot use.
    # Nothing here reaches bash's arithmetic, and taking those rules here would
    # degrade the check on rows the Go reader compares exactly, and extract an
    # image it refuses. The writer produces none of these rows.
    #
    # And a path is compared in one spelling. list_image escapes the image's
    # names as indexfmt.EscapePath does, but the Go reader compares the
    # index's paths after UnescapePath, so a row it reads fine that is not in
    # that spelling, a raw tab or a backslash before a letter the format does
    # not define, is put into it here first: canon is EscapePath(UnescapePath(p))
    # in one pass. Only a path with a tab or a backslash in it can need that,
    # which keeps the walk off every other row.
    rows="$(age_d "$idx" | gunzip -c | N="$n" LC_ALL=C awk -F'\t' '
      function canon(s,   i, c, e, out) {
        out = ""
        for (i = 1; i <= length(s); i++) {
          c = substr(s, i, 1)
          if (c == "\t") { out = out "\\t"; continue }
          if (c != "\\") { out = out c; continue }
          e = substr(s, i + 1, 1)
          if (e == "\\" || e == "t" || e == "n") {
            if (e == "n") nl = 1
            out = out c e; i++
          } else out = out "\\\\"
        }
        return out
      }
      BEGIN { n = ENVIRON["N"] ""; sub(/^0+/, "", n) }
      $0 == "" { next }
      NF < 2 || (NF == 2 && $2 == "") || $1 !~ /^[+]?[0-9]+$/ { bad++; next }
      { d = $1; sub(/^[+]?0*/, "", d) }
      d == "" || length(d) > 19 || (length(d) == 19 && d > "9223372036854775807") { bad++; next }
      (d "") == n {
        p = NF == 2 ? $2 : substr($0, length($1) + 2)
        if (NF > 2 || index(p, "\\")) p = canon(p)
        print "i" p
      }
      END { if (nl) exit 3; if (bad) { print "#" bad; exit 4 } }
    ')" || rc=$?
    case "$rc" in
      0) ;;
      3) why="the index lists a path for disc $n whose name contains a newline, which a line-based 'unsquashfs -ll' listing cannot carry faithfully" ;;
      4) why="${rows##*#} record(s) in index.tsv.gz.age do not parse, so the index's list for this disc is incomplete ($(sidecar_repair_hint "$n"))" ;;
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
  #
  # The index's rows go first, so that by the time the image's entries arrive
  # every path the index gives this disc is already known: an image path then
  # either ticks off its row and is forgotten, or is an extra and is kept, in
  # order, for the examples. Only the index's side and the extras are ever held
  # — which is how auditImage in go/internal/restore/extract.go streams the
  # image past the index — where image-first held two copies of every path the
  # image has, a key and an ordered value, only to test them against the index
  # at the end. On a clean disc of a million files that halved this awk's peak.
  #
  # And the entries are printed one at a time rather than as one printf over
  # "${IMAGE_ENTRIES[@]}": that expansion builds the whole array a second time
  # as an argument list inside the pipe's subshell, and at a million entries
  # that copy outweighed the awk. The loop costs a few seconds per million
  # entries, against hundreds of megabytes on the machine least able to spare
  # them — the trade list_image makes, for the same machine. IMAGE_ENTRIES is
  # contiguous from 0: mapfile fills it so, and list_image only ever unsets its
  # last elements. k is assigned in the pipe's subshell and never leaks.
  local -a verdict=()
  mapfile -t verdict < <( { printf '%s\n' "$rows"
                            for (( k = 0; k < ${#IMAGE_ENTRIES[@]}; k++ )); do
                              printf '%s\n' "${IMAGE_ENTRIES[k]}"
                            done; } | LC_ALL=C awk '
    {
      t = substr($0, 1, 1); p = substr($0, 2)
      if (t == "i") { if (!(p in idx)) { idx[p] = 0; iord[++ni] = p } }
      else if (t == "f") {
        if (p in idx) { if (!idx[p]) { idx[p] = 1; nf++ } }
        else if (!(p in extra)) { extra[p] = 1; eord[++nx] = p; nf++ }
      }
    }
    END {
      # Bounded on purpose: a forged image can differ by every path it holds,
      # and ten thousand lines of them is not a message anybody reads.
      for (i = 1; i <= ni; i++) if (!idx[iord[i]]) { nm++; if (nm <= 3) print "M" iord[i] }
      for (i = 1; i <= nx; i++) { ne++; if (ne <= 3) print "E" eord[i] }
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
  local only="" onedisc="" partial=0
  while (( $# )); do
    case "$1" in
      # ${2:?} produces bash's own "2: parameter null or not set", which names
      # neither the option nor the script.
      #
      # Every leading '/' and './' and every trailing '/' is stripped, so that
      # "/notes", "./notes" and "notes//" all mean the path the user was
      # pointing at: matching below is exact, not substring, and would
      # otherwise miss on the cosmetic difference. It is the rule onlyPath in
      # go/internal/restore/extract.go applies, and it has to be the same rule:
      # this used to strip one trailing '/' and one leading './' and never a
      # leading '/', while the Go reader trimmed every '/' and never './', so
      # "./notes" restored here and failed there and "/notes" and "notes//"
      # did the reverse. What is left is what the index, the listing, the
      # unsquashfs operand and the post-check all see. A spelling with nothing
      # left, or only ".", names the archive root — the whole tree, which is
      # what a restore without --only is for — and is refused as Go refuses it.
      --only) only="${2:-}"
              [[ -n "$only" ]] || die "--only needs a path"
              while :; do
                case "$only" in
                  /*)  only="${only#/}" ;;
                  ./*) only="${only#./}" ;;
                  *)   break ;;
                esac
              done
              while [[ "$only" == */ ]]; do only="${only%/}"; done
              [[ -n "$only" && "$only" != "." ]] \
                || die "--only $2: that is the whole tree — run restore without --only"
              shift 2 ;;
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
    # numbered_images, not a glob: see there for the forged image a glob let
    # through, and for why its name was all that forgery needed.
    mapfile -t encs < <(numbered_images)
    (( ${#encs[@]} > 0 )) || die "no images in $ENC_DIR — run '$PROG ingest' first"
    # An if, so that the answer is remembered: a partial set the operator said
    # yes to must not end on the same line as a complete one (see the end).
    if ! check_complete; then
      confirm "Restore the partial set anyway?" || die "aborted"
      partial=1
    fi
  fi

  # --only used to par2-verify and decrypt every image in the set — hours and
  # hundreds of GiB — to pull back one file, printing "extracted" for each disc
  # whether or not the path was on it. The index already knows where it lives.
  #
  # This is narrowByIndex in go/internal/restore/locate.go, down to its refusal
  # to fail over the index itself. An index that is not staged, fails its
  # recorded hash, or will not decrypt and decompress is a warning, and the
  # restore searches every image it was going to search anyway — it used to
  # die here "--only needs the encrypted index" when there was none, and when
  # there was one that would not read it reported the path "not in the index",
  # sending the operator off to look for a file the index had never been able
  # to answer for. Worse, the `|| true` that hid age's and gunzip's status
  # believed whatever rows came out before the damage, so an index rotted past
  # its first age chunk narrowed a directory to the discs named in the readable
  # part and restored part of it, exit 0.
  #
  # With --disc N as well, the index is still asked, but it can only refuse
  # that disc, never widen the choice: when it is sure the path is somewhere
  # else the run stops before a whole image is hashed and decrypted to find out
  # the same thing. This used to skip the index whenever --disc was given, and
  # for a directory whose files are all on another disc the skeleton copy of it
  # on disc N then satisfied the pre-check below, so the run restored an empty
  # directory and said "restore complete" where the Go reader refuses.
  if [[ -n "$only" ]]; then
    local -a discs=() sel=()
    local -A bynum=()
    local d e line esc_only out="" irc=0 here=0 unparsed=0 unread="" idx="$ENC_DIR/index.tsv.gz.age"
    if [[ ! -f "$idx" ]]; then
      warn "no index.tsv.gz.age in $ENC_DIR, so there is nothing to say which disc(s) hold '$only'"
      unread=1
    elif ! check_index_intact; then
      warn "the encrypted index could not be read: $INDEX_FAULT"
      unread=1
    else
      # Resolve the disc by EXACT path, not by 'index'-style substring search:
      # a fuzzy hit selects a disc for --only notes when only notes2025 exists,
      # and the extraction filter downstream is exact — so the run used to
      # report "restore complete" having written nothing. The index stores
      # paths with backslash, tab and newline escaped (backslash first — the
      # same order the writer uses; any other order fails to round-trip), so
      # the pattern is escaped the same way, then matched as the whole path or
      # as a directory prefix of one. ENVIRON, not -v: awk -v mangles backslash
      # sequences. The status is kept — pipefail carries age's or gunzip's
      # failure out of the substitution — and awk and sort read to the end, so
      # no early exit can fake one.
      #
      # Field 1 came off a disc, so awk takes it apart before anything else
      # sees it: a run of ASCII digits (awk under C reads [0-9] as nothing
      # else), leading zeros dropped, 1 or more, as indexfmt.ParseLine
      # requires, and at most nine digits, for the reason num_arg gives —
      # $((10#...)) wraps at 2^64, and a wrapped field would name a disc the
      # image lookup never used. It used to be `sort -un` on the raw field and
      # the test afterwards, and sort keeps the FIRST of two numerically equal
      # lines: a row reading 00000000001, which Go takes as disc 1, came out in
      # place of the row reading 1 and was then dropped as too long, so the
      # path was "not in the index". Every row is parsed, not only the matching
      # ones, so that the rows that do not parse are counted and reported as
      # indexDiscs in go/internal/restore/locate.go reports them; the count
      # comes last, on a line of its own that starts with '#'. ParseLine reads
      # a few rows that this cannot use — a '+' before the number, a number
      # past 999999999, a raw tab in the path — and they count here, which is
      # what they are to this reader. The writer produces none of them.
      esc_only="${only//\\/\\\\}"
      esc_only="${esc_only//$'\t'/\\t}"
      esc_only="${esc_only//$'\n'/\\n}"
      out="$(age_d "$idx" | gunzip -c \
        | P="$esc_only" LC_ALL=C awk -F'\t' '
            $0 == "" { next }
            NF != 2 || $2 == "" || $1 !~ /^[0-9]+$/ { bad++; next }
            { d = $1; sub(/^0+/, "", d) }
            d == "" || length(d) > 9 { bad++; next }
            ($2 == ENVIRON["P"] || index($2, ENVIRON["P"] "/") == 1) && !(d in seen) { seen[d] = 1; print d }
            END { print "#" bad + 0 }' \
        | LC_ALL=C sort -n)" || irc=$?
      if (( irc != 0 )); then
        warn "the encrypted index could not be read (age or gunzip exited $irc) — $(sidecar_repair_hint 0)"
        unread=1
      fi
    fi
    if [[ -n "$unread" ]]; then
      if [[ -z "$onedisc" ]]; then
        warn "every one of the ${#encs[@]} ingested image(s) will be decrypted to look for it; pass --disc N to restrict that to one disc"
      fi
    else
      # What awk printed: the distinct discs holding the path, ascending, and
      # the '#' line. A row with no usable number is no evidence of where the
      # path is, so a path whose rows are all like that counts as unlisted —
      # it used to be "on disc(s) 0", and with --disc it would have refused
      # the disc with "pass --disc 0".
      while IFS= read -r line; do
        case "$line" in
          '#'*) unparsed="${line#\#}" ;;
          ?*)   discs+=( "$line" ) ;;
        esac
      done <<<"$out"
      if (( unparsed > 0 )); then
        warn "$unparsed record(s) in index.tsv.gz.age do not parse and were ignored; $(sidecar_repair_hint 0) if a path seems to be missing"
      fi
      if [[ -n "$onedisc" ]]; then
        for d in ${discs[@]+"${discs[@]}"}; do
          if (( d == onedisc )); then here=1; fi
        done
        if (( ${#discs[@]} == 0 )); then
          warn "the index does not list '$only'; searching disc $onedisc anyway, because --disc names it"
        elif (( here )); then
          step "the index puts '$only' on disc(s) ${discs[*]}"
        else
          die "the index puts '$only' on disc(s) ${discs[*]}, not on disc $onedisc — drop --disc, or pass --disc ${discs[0]}"
        fi
      else
        # The sentence on the end used to add "with no leading '/'", from when
        # a leading slash made a path miss here; it is stripped with the rest
        # of the spelling now, so it is never the reason.
        (( ${#discs[@]} )) \
          || die "'$only' is not in the index — check '$PROG index ${only##*/}' for the exact path, or pass --disc N. Paths are relative to the archive root, not the path the file had on the machine that was backed up."
        # Each disc's image is found by its NUMBER among the images this
        # restore was going to work through, as narrowByIndex finds it in
        # listNumbered's: a disc staged as disc001 is disc 1 here, as it is to
        # check_complete and to Go. Building the name disc%02d instead found
        # nothing for such a disc, warned that it had not been ingested, and
        # refused a path the rest of the restore could see. Where two names
        # claim one number, the one listed later wins, as in Go's map.
        for e in "${encs[@]}"; do bynum[$(disc_number_of "${e##*/}")]="$e"; done
        for d in "${discs[@]}"; do
          if [[ -n "${bynum[$d]:-}" ]]; then sel+=( "${bynum[$d]}" )
          else warn "'$only' is partly on disc $d, whose image is not in $ENC_DIR"; fi
        done
        (( ${#sel[@]} )) || die "'$only' is on disc(s) ${discs[*]}, none of which have been ingested"
        encs=( "${sel[@]}" )
        step "'$only' is on disc(s) ${discs[*]}"
      fi
    fi
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
    #
    # awk reads the listing to its end rather than exiting at the first match,
    # and unsquashfs's own status is taken apart from awk's before awk's answer
    # is believed — the two things pathsPresent does by draining the listing
    # and then checking the child. Exiting early closed the pipe under
    # unsquashfs, which neither ignores nor handles SIGPIPE, so it died on its
    # next write as soon as a few kilobytes of listing followed the match, and
    # pipefail turned that 141 into "not on this image": on any real disc a
    # path near the top of the listing was reported absent, and a directory
    # spanning discs was silently skipped on the disc holding most of it while
    # the run went on to say "restore complete". And a listing that really
    # failed — an unsquashfs built without the image's compression, say — said
    # exactly the same thing with its error sent to /dev/null, where list_image
    # stops on the same failure with the error in view. Now it stops here too,
    # and the decrypted image stays where it is, as list_image leaves it.
    local precheck="$only" lrc=0 arc=0
    if [[ "$only" == *$'\n'* || "$only" == *$'\r'* ]]; then precheck=""; fi
    if [[ -n "$precheck" ]]; then
      unsquashfs -l "$img" | P="squashfs-root/$only" LC_ALL=C awk \
          '!f && ($0 == ENVIRON["P"] || index($0, ENVIRON["P"] "/") == 1) { f = 1 } END { exit !f }' \
        || { lrc=${PIPESTATUS[0]} arc=${PIPESTATUS[1]}; }
      (( lrc == 0 )) \
        || die "could not list the contents of $(esc_str "$(basename "$img")"): 'unsquashfs -l' exited $lrc (its own error is above), so whether '$only' is on it cannot be told. An unsquashfs that calls the image's compression unsupported needs a newer squashfs-tools — the Go reader runs the same unsquashfs and would fail the same way; otherwise fix or re-ingest that image and retry."
      if (( arc != 0 )); then
        step "$only is not on $(esc_str "$(basename "$img")")"
        if (( ! KEEP_IMAGES )); then rm -f -- "$img"; fi
        continue
      fi
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
    #
    # The first three lines are picked by an awk that reads the whole log, not
    # by `grep | head -3`: under pipefail and errexit, head exiting after its
    # third line left grep to die of SIGPIPE on a log of more than a few
    # kilobytes, and that 141 ended the whole restore on the spot — no failure
    # message, the next disc never reached, and every rerun stopping on the same
    # disc — where this branch and the README both promise a warning and the
    # next disc. A destination that refuses xattrs or special files is exactly
    # what writes a log that long.
    if (( urc == 2 )); then
      warn "unsquashfs reported non-fatal errors on $(esc_str "$(basename "$img")") — see $ulog"
      LC_ALL=C awk '$0 != "" && ++n <= 3' "$ulog" | esc_controls | sed 's/^/    /' >&2
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

  # A restore of a set with discs missing must not end on the line a complete
  # one ends on. The tree it produced has the right shape and the wrong
  # contents — every disc carries the whole skeleton — and on a multi-disc run
  # the MISSING warning it opened with has long scrolled away, a few status
  # lines per disc and hours later, so this is the last chance to say so.
  # reportOutcome in go/internal/restore/extract.go, word for word; this used
  # to print "restore complete" either way.
  local closing="restore complete"
  if (( partial )); then
    warn "this restore was PARTIAL: disc(s) $SET_MISSING of $SET_WANT were not in the staging area"
    warn "files that lived only on those discs are NOT in $dest, and the empty directories left behind do not show it"
    warn "ingest the missing disc(s) and run the same restore again to fill them in"
    closing="partial restore complete"
  fi

  # The rm -rf is a command to paste, so the directory goes into it through %q
  # as one shell word: RESTORE_DIR is $STAGING/restore, and a STAGING of
  # "/srv/backup tmp" printed bare would delete /srv/backup.
  cat >&2 <<EOF

$(ok "$closing: $dest")

  $RESTORE_DIR holds decrypted (plaintext) images: every one of them with
  KEEP_IMAGES=1, otherwise only what a failed run left behind.
  Remove them when done:  rm -rf $(printf '%q' "$RESTORE_DIR")

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
  # A mount point that is itself a symlink is refused: mount(8) run as root
  # resolves the whole path and mounts on the link's TARGET, so a link left at
  # /shared/browse pointing at /etc gets the image mounted over /etc, and
  # mkdir -p takes the link for an existing directory without a word. Mount in
  # go/internal/restore/mount.go refuses the same paths with the same words.
  #
  # dir_path first, because "link/" and "link/." are resolved through the link
  # before [[ -L ]] sees them, and the same spelling is what mkdir and mount get
  # below, so the check and the act are about one path. Go's dirPath is this
  # function rule for rule rather than filepath.Clean, which would collapse a
  # "link/../x" lexically and mount somewhere the kernel, and this script, do
  # not. It is still a name test followed by an act — a link planted in the gap
  # is followed, which nothing can close while mount(8) takes a path.
  #
  # Before the root check, so the refusal is the same for everyone and can be
  # tested without root, and before secure_staging, as Mount orders it too, so
  # neither reader touches staging for a run it is about to refuse.
  #
  # The trailing "x" keeps the path's own bytes. $(...) drops every trailing
  # newline, so "browse/<newline>" came back as "browse/" — which [[ -L ]]
  # answers through the link, and which mkdir and mount then followed onto its
  # target — and "browse<newline>" as "browse", a different file. Go keeps
  # the newline, so the two readers would also have mounted in different
  # places. dir_path prints no newline of its own for the "x" to keep.
  mp="$(dir_path "$mp"; printf x)"; mp="${mp%x}"
  if [[ -L "$mp" ]]; then
    die "mount point $(esc_str "$mp") is a symlink (-> $(esc_str "$(readlink -- "$mp" 2>/dev/null || true)")); mount(8) follows it and would mount the image over whatever it points at — pass the directory itself"
  fi
  (( EUID == 0 )) || die "mounting requires root"
  secure_staging "$ENC_DIR" "$RESTORE_DIR"
  resolve_identity
  local enc img
  enc="$(printf '%s/disc%02d.squashfs.age' "$ENC_DIR" "$n")"
  [[ -f "$enc" ]] || die "no image for disc $n in $ENC_DIR"
  prepare_image "$enc"; img="$PREPARED_IMG"
  # Created under the umask 077 secure_staging set, so every directory made here
  # is 0700, as Mount's MkdirAll makes it: the parents decide whether another
  # account can reach the decrypted tree at all.
  mkdir -p -- "$mp"
  # nosuid,nodev because the image is a backup, not this host's system: ro alone
  # leaves the kernel's suid and dev defaults on, so a setuid-root binary from a
  # backup of / — an old sudo with a known local root bug — or a device node in
  # the image would act on this host for any account that can reach the mount.
  # Browsing and copying out lose nothing; the reasoning is at length on
  # mountArgs in go/internal/restore/mount.go, which passes this same list. And
  # "--", so a mount point with a leading dash is not read as options.
  mount -o loop,ro,nosuid,nodev -- "$img" "$mp" || die "mount failed"
  ok "disc $n mounted read-only at $mp"
  step "unmount with: umount $(printf '%q' "$mp")"
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
#
# awk joins the lines with \n, one record at a time and only when there is a
# newline to join. ${1//$'\n'/\\n} gave the same string in time quadratic in
# its length, because for every replacement bash's pattern matcher walks the
# rest of the string again: verify-disc prints a listed name it cannot match,
# and one line of an escaped SHA512SUMS unescapes to half a million newlines,
# which took most of a minute to print.
esc_str() {
  if [[ "$1" != *$'\n'* ]]; then printf '%s\n' "$1" | esc_controls; return; fi
  printf '%s\n' "$1" | LC_ALL=C awk '{ printf "%s%s", (NR > 1 ? "\\n" : ""), $0 } END { print "" }' \
    | esc_controls
}

cmd_index() {
  # sha512sum for check_index_intact, which hashes the index before it is read.
  need age; need sha512sum
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
  # Before a byte of it is believed, as Index in go/internal/restore/extract.go
  # does: a mismatch is the index (or its sidecar) having rotted, and par2 can
  # put either back on the spot.
  check_index_intact || die "$INDEX_FAULT"
  local pattern="${1:-}"
  local -a ps=()
  # gunzip, matching build_index: the index is deliberately the one artifact a
  # person can unpack with tooling that predates this script by thirty years.
  #
  # When the pipeline fails, its statuses are read from the RIGHT, because a
  # failure on the output side explains everything to its left: `index | head`
  # closes the pipe, esc_out dies of it, and everything upstream then fails to
  # write — of SIGPIPE ordinarily, or with EPIPE and an exit of 1 or 2 where
  # SIGPIPE is ignored, as systemd services ignore it — none of which says
  # anything about the index. Only once the output side and the filter are
  # accounted for is a failing age or gunzip the index failing to read, and
  # only grep's 1 with everything else clean is a search that found nothing.
  # Both used to come out as "no match": a rotted index, or one the identity
  # cannot open, was reported as an archive without the file, even after
  # printing the rows that came out before the damage.
  if [[ -z "$pattern" ]]; then
    age_d "$idx" | gunzip -c | esc_out || {
      ps=( "${PIPESTATUS[@]}" )
      (( ps[2] == 0 )) || return 1
      die "could not read $idx (age exited ${ps[0]}, gunzip ${ps[1]}) — $(sidecar_repair_hint 0), then retry"; }
  else
    age_d "$idx" | gunzip -c | index_match "$pattern" | esc_out || {
      ps=( "${PIPESTATUS[@]}" )
      (( ps[3] == 0 && ps[2] <= 1 )) || return 1
      (( ps[0] == 0 && ps[1] == 0 )) \
        || die "could not read $idx (age exited ${ps[0]}, gunzip ${ps[1]}) — $(sidecar_repair_hint 0), then retry"
      warn "no match for '$(esc_str "$pattern")'"; return 1; }
  fi
}

# index_match PATTERN — the index rows containing PATTERN, matched as
# filterIndex in go/internal/restore/extract.go matches them: as a literal,
# case-insensitive substring, and never as a regular expression. Exits 1 when
# nothing matched, as grep does.
#
# -F, because this was `grep -i` and so read the pattern as a basic regular
# expression: a name with a bracket in it — "notes[1].txt", "Photo [2019].jpg"
# — matched nothing, an unbalanced one ("a[1") made grep fail outright, and
# both came out as "no match" for a file that is in the index — from the very
# command this script's own refusals send the operator to ('check brb.sh index
# ...') — while a dot or a star quietly matched more than was typed.
#
# -a, because GNU grep in a UTF-8 locale takes a row holding a name that is not
# valid UTF-8 — an old Latin-1 name, say — for binary data, prints "binary file
# matches" in its place, and exits 0: the row is neither shown nor reported
# missing. GNU and BSD grep accept the flag. busybox grep is believed to accept
# it and ignore it, having no binary-file test to switch off — read from its
# source, not tested.
#
# A newline cannot be in the pattern: grep -F would split it into several
# patterns and print any row matching one of them, and no row can contain one,
# since the index escapes it. Such a pattern matches nothing — after the index
# has been read to the end like any other, so that an index that will not read
# still says so, as it does in Go.
#
# What is left of the difference is case folding, which grep takes from the
# locale (busybox's grep -F -i, by its source, from ASCII alone) and Go from
# Unicode; README's table of reader differences has it.
index_match() {
  if [[ "$1" == *$'\n'* ]]; then cat >/dev/null; return 1; fi
  grep -a -i -F -- "$1"
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
                                               archive root (a leading '/' or
                                               './' is dropped); located via
                                               the encrypted index, so only the
                                               discs holding it are decrypted
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
    mkdir -p image && sudo mount -o loop,ro,nosuid,nodev discNN.squashfs image

  The mount needs only the kernel. Not over /mnt if the disc is mounted there:
  the image would hide the disc's data/ directory. And nosuid,nodev because ro
  alone leaves a setuid program or a device node in the backup working on the
  machine that reads it.

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
