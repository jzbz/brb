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
# will happen to their bytes before they run anything. A few hundred lines of
# shell can be read end to end in an afternoon. An 8 MB static binary cannot.
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

VERSION="1.0.0"
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
# identity is a file on disk or the unlocked text held in memory. Arguments are
# passed straight to age: age_d -o OUT IN, or age_d IN, or age_d reading stdin.
age_d() {
  if [[ -n "$AGE_IDENTITY_TEXT" ]]; then
    age -d -i <(printf '%s\n' "$AGE_IDENTITY_TEXT") "$@"
  else
    age -d -i "$AGE_IDENTITY" "$@"
  fi
}

# Restore and ingest write the same plaintext the backup path warns about, into
# a directory the README tells people to put under /var/tmp — which is 1777.
# umask must be set before the mkdir, and an existing directory needs the chmod.
secure_staging() {
  umask 077
  mkdir -p "$STAGING"
  chmod 700 "$STAGING" 2>/dev/null || true
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
  # A config file written for the Go build will carry SOURCE_DIR, DISC_TYPE,
  # COMPRESSION and the rest. Sourcing it here simply defines variables this
  # script never reads, which lets one config serve both — but sourcing is
  # execution, so the config must be trusted exactly like brb.sh itself.
  ENC_DIR="$STAGING/enc"
  RESTORE_DIR="$STAGING/restore"
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
  for t in ddrescue udisksctl findmnt eject pv; do
    command -v "$t" >/dev/null 2>&1 && ok "$t  (optional)" || step "$t  not found (optional)"
  done
  step "ddrescue is the one worth having: cp stops at the first I/O error on a"
  step "scratched disc, ddrescue does not, and par2 needs the rest of the bytes"

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
  if [[ -z "$id" ]]; then
    warn "no age identity found — a restore cannot decrypt anything without one"
    step "set AGE_IDENTITY, or put identity.txt in $keydir"
    missing=1
  elif identity_is_encrypted "$id"; then
    ok "restore would use $id  (passphrase-protected: it will ask once per command)"
  else
    ok "restore would use $id"
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
  [[ "$actual" == "$want" ]] \
    || die "the drive holds ${actual:-an unrecognised disc}, not disc $n — insert disc $n"
  # ARCHIVE_NAME is a writer setting this script does not define, so a restorer
  # with a reader-side config has nothing to compare against — and every real
  # disc carries an archive name, so an unguarded expansion died here under
  # set -u before a single hash was checked. Warn only when a writer config
  # happens to be loaded and disagrees with the disc.
  if [[ -f "$mp/MANIFEST.txt" && -n "${ARCHIVE_NAME:-}" ]]; then
    marc="$(sed -n 's/^archive name[[:space:]]*:[[:space:]]*//p' "$mp/MANIFEST.txt" | head -1 || true)"
    [[ -z "$marc" || "$marc" == "$ARCHIVE_NAME" ]] \
      || warn "this disc belongs to archive '$marc', not '$ARCHIVE_NAME'"
  fi

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
  warn "copy failed: ${err:-unknown error copying $(basename "$src")}"
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
      warn "$(basename "$src"): unreadable regions remain — see $dst.mapfile"; rc=1
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
  # README tells people to point STAGING at /var/tmp, which is 1777.
  secure_staging
  mkdir -p "$ENC_DIR"
  # A .part from an interrupted copy must never be mistaken for a finished
  # file. Its ddrescue mapfile dies with it: a mapfile that outlives its data
  # marks regions of the DELETED file as already read, so the next attempt
  # would trust it (copy_file_robustly keeps an existing mapfile) and produce
  # a copy that can never complete.
  rm -f -- "$ENC_DIR"/*.part "$ENC_DIR"/*.part.mapfile
  trap 'rm -f -- "$ENC_DIR"/*.part "$ENC_DIR"/*.part.mapfile' EXIT
  local mp f base want got alt prev_id="" this_id bad=0
  while :; do
    # Deliberately not prompt_enter/confirm: both auto-return under --yes, so
    # neither loop exit was reachable and `brb --yes ingest` ran forever.
    prompt_media "insert the next disc (any order), then press Enter — or type q to stop" || break
    [[ "$MEDIA_REPLY" == [qQ] ]] && break
    mount_disc "${1:-}"; mp="$MOUNT_POINT"
    [[ -d "$mp/data" ]] || die "$mp has no data/ directory"
    # eject is silently a no-op on a disc the user cannot unmount, and findmnt
    # then keeps reporting the old mount point. Nothing else in the loop notices.
    this_id="$(disc_identity "$mp")"
    if [[ -n "$prev_id" && "$this_id" == "$prev_id" ]]; then
      warn "this is the same disc as last time (${this_id:-unrecognised}) — the tray may not have opened"
      confirm "Read it again anyway?" || { unmount_disc; continue; }
    fi
    prev_id="$this_id"
    while IFS= read -r f; do
      base="$(basename "$f")"
      # "Already have" has to mean "already have a file proven good". Otherwise
      # a truncated copy from a bad sector sticks forever, and — because backup
      # leaves its own images in ENC_DIR — the post-backup test restore reads
      # staging instead of the discs it was supposed to be testing.
      if [[ -f "$ENC_DIR/$base" ]]; then
        if [[ -f "$ENC_DIR/$base.sha512" ]] && ( cd "$ENC_DIR" && sha512sum -c --status "$base.sha512" ); then
          step "already have a verified $base"; continue
        fi
        if [[ "$base" == *.squashfs.age ]]; then
          # par2 can combine two differently damaged copies, but only ones named
          # on its command line — prepare_image passes these.
          alt="$ENC_DIR/$base.copy$(date +%s)"
          step "have an unverified $base — ingesting this copy as $(basename "$alt") for par2 to combine"
          if ! copy_file_robustly "$f" "$alt"; then
            # The partial salvage stays under the .copy name: zeros and all, it
            # is more raw material for par2.
            warn "$base second copy incomplete"; bad=$(( bad + 1 )); continue
          fi
          # A copy proven whole is better than an unverified staged one, so it
          # becomes the primary — the only name prepare_image's par2 repair and
          # decryption ever read. Without this a pristine pressing was parked as
          # a .copy and the damaged image stayed in charge of the restore.
          # Mirrors replaceOrKeepBoth in go/internal/restore/ingest.go: the
          # staged bytes are never destroyed ahead of the replacement's proof —
          # the copy is written under its own name and hashed first, and only
          # then renamed over the staged one.
          if [[ -f "$f.sha512" ]]; then
            want="$(awk '{print $1; exit}' "$f.sha512")"
            got="$(sha512sum < "$alt" | awk '{print $1}')"
            if [[ "$want" == "$got" ]]; then
              mv -f "$alt" "$ENC_DIR/$base"
              rm -f -- "$alt.mapfile" "$ENC_DIR/$base.mapfile"
              step "replaced the staged $base with this disc's verified copy"
              continue
            fi
            warn "this disc's copy of $base does not match the recorded hash either; keeping both — par2 will combine them during '$PROG restore'"
          fi
        else
          step "already have $base (unverified, not an image — keeping existing)"
        fi
        continue
      fi
      # Copy to .part and check it against the hash recorded on this disc before
      # promoting it. Putting a partial file under the real name is exactly what
      # made the old skip test sticky.
      step "copying $base"
      if ! copy_file_robustly "$f" "$ENC_DIR/$base.part"; then
        warn "$base could not be read off this disc — leaving it for another copy"
        rm -f -- "$ENC_DIR/$base.part" "$ENC_DIR/$base.part.mapfile"; bad=$(( bad + 1 )); continue
      fi
      if [[ -f "$f.sha512" ]]; then
        want="$(awk '{print $1; exit}' "$f.sha512")"
        got="$(sha512sum < "$ENC_DIR/$base.part" | awk '{print $1}')"
        if [[ "$want" != "$got" ]]; then
          # Keep the damaged bytes under the REAL name: restore's par2 repair
          # only ever looks there, and this branch is reached only when no
          # copy of $base exists at all, so nothing good is overwritten. A
          # second ingest of this disc then lands as $base.copy<epoch> above —
          # exactly the pair par2 can combine. (Filing it away under a .bad
          # name kept bytes par2 could often repair where no repair path would
          # ever read them.)
          warn "$base does not match the hash recorded on the disc — keeping the damaged copy for par2 repair during restore"
          mv -f "$ENC_DIR/$base.part" "$ENC_DIR/$base"
          rm -f -- "$ENC_DIR/$base.part.mapfile"
          bad=$(( bad + 1 )); continue
        fi
      fi
      mv -f "$ENC_DIR/$base.part" "$ENC_DIR/$base"
      rm -f -- "$ENC_DIR/$base.part.mapfile"
    done < <(find "$mp/data" -type f | sort -V)
    [[ -f "$mp/MANIFEST.txt" ]] && cp -f "$mp/MANIFEST.txt" "$STAGING/MANIFEST.txt"
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
  local found=""
  found="$(find_identity || true)"
  [[ -n "$found" ]] \
    || die "no age identity found: looked for ${AGE_IDENTITY:-identity.txt}, ${AGE_IDENTITY:-identity.txt}.age and rescue-identity.txt.age near $AGE_RECIPIENTS_FILE  (set AGE_IDENTITY=/path/to/identity.txt)"
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
  local enc="$1" base plain intact
  base="$(basename "$enc" .age)"
  plain="$RESTORE_DIR/$base"
  mkdir -p "$RESTORE_DIR"

  # The cache below is only worth having if what it hands back is known good.
  # A previous run that died on the hash check left its corrupt plaintext right
  # here, and returning it unchecked would make the next restore "succeed".
  if [[ -f "$plain" ]]; then
    if [[ -f "$ENC_DIR/$base.sha512" ]]; then
      cp -f "$ENC_DIR/$base.sha512" "$RESTORE_DIR/"
      if ( cd "$RESTORE_DIR" && sha512sum -c --status "$base.sha512" ); then
        step "reusing verified $base"; PREPARED_IMG="$plain"; return 0
      fi
      warn "cached $base is corrupt — discarding and decrypting again"
      rm -f -- "$plain"
    else
      PREPARED_IMG="$plain"; return 0
    fi
  fi

  intact=unknown
  if [[ -f "$ENC_DIR/$base.age.sha512" ]]; then
    if ( cd "$ENC_DIR" && sha512sum -c --status "$base.age.sha512" ); then
      intact=yes
    else
      intact=no; warn "$base.age does not match its recorded hash" >&2
    fi
  fi

  if [[ "$intact" != "yes" ]]; then
    [[ -f "$enc.par2" ]] || die "$base.age is damaged and has no par2 data. Ingest another copy of that disc and retry."
    warn "attempting par2 repair of $base.age" >&2
    # par2 ignores files it was not told about, so the alternate copies ingest
    # saved off a second burn have to be named explicitly to be of any use.
    #
    # A plain glob, and the directory part QUOTED so that it is matched
    # literally: compgen -G took the whole thing as one pattern, so a staging
    # path containing [ ] * or ? was interpreted rather than matched, found no
    # copies at all, and a set that par2 could have repaired from a second burn
    # was declared unrepairable. ${c##*/} strips the directory with a fixed
    # pattern; the old ${extras[@]/#$ENC_DIR\/} substituted $ENC_DIR AS a pattern
    # and carried exactly the same hazard. No nullglob: an unmatched pattern
    # comes through as itself and the -e test drops it.
    local -a extras=(); local c
    for c in "$ENC_DIR/$base.age.copy"*; do
      [[ -e "$c" ]] || continue
      extras+=( "${c##*/}" )
    done
    ( cd "$ENC_DIR" && par2 repair -- "$base.age.par2" ${extras[@]+"${extras[@]}"} >&2 ) \
      || die "par2 could not repair $base.age. If you burned a second copy of the set, ingest that disc into $ENC_DIR too and retry."
    # par2 covers the ciphertext only. When par2 says the image is whole and the
    # 170-byte .sha512 sidecar disagrees, the sidecar is what rotted — dying
    # here would throw away a 22 GB image that is provably byte-for-byte
    # correct. The decrypted image is still checked against .sha512 below, so
    # nothing is decrypted on trust.
    if [[ -f "$ENC_DIR/$base.age.sha512" ]]; then
      if ( cd "$ENC_DIR" && sha512sum -c --status "$base.age.sha512" ); then
        ok "repaired $base.age" >&2
      else
        warn "$base.age passes par2 but not its .sha512 sidecar — the sidecar is what is corrupt, not the image" >&2
        warn "  (repair it from the disc: par2 repair -- sidecars.par2)" >&2
      fi
    fi
  fi

  age_d -o "$plain.part" "$enc" \
    || { rm -f -- "$plain.part"; die "decryption failed for $base"; }
  mv "$plain.part" "$plain"

  # Delete on failure: a rejected image left behind is what the cache above
  # would otherwise pick up and trust on the next run.
  if [[ -f "$ENC_DIR/$base.sha512" ]]; then
    cp -f "$ENC_DIR/$base.sha512" "$RESTORE_DIR/"
    ( cd "$RESTORE_DIR" && sha512sum -c --status "$base.sha512" ) \
      || { rm -f -- "$plain"; die "decrypted image $base does not match its recorded hash"; }
  fi
  PREPARED_IMG="$plain"
}

# Refuse a destination that already holds a symlink resolving to a directory.
# Mirrors refuseSymlinkedDirs in go/internal/restore/extract.go, message and
# all: unsquashfs -f traverses such a link — at any depth, not just the top
# level — and writes the archive's files through it, OUTSIDE the destination,
# with this process's privileges, which the README recommends be root's. So it
# is a hard refusal rather than a question, and --yes does not answer it.
#
# A symlink to a FILE is safe (unsquashfs unlinks and replaces it as an entry)
# and is left alone. -P and the stripped trailing slash keep $dest itself from
# being followed: a destination that IS a symlink to a directory is the same
# escape, and lands the whole archive in the target.
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
  local d="${1%/}"; [[ -n "$d" ]] || d="/"
  local -a bad=()
  local p
  while IFS= read -r -d '' p; do
    # -d follows the link, so this is true only for a symlinked DIRECTORY.
    [[ -d "$p" ]] || continue
    bad+=( "$p -> $(readlink -- "$p" 2>/dev/null || true)" )
    (( ${#bad[@]} < 5 )) || break
  done < <(find -P "$d" -type l -print0)
  # An if, not `&&`: a false test would become this function's status and
  # errexit would end the run right here, silently, on a clean destination.
  if (( ${#bad[@]} == 0 )); then return 0; fi
  local list; list="$(printf '%s, ' "${bad[@]}")"; list="${list%, }"
  die "$d contains symlink(s) to directories ($list); unsquashfs -f would follow them and write the backup's files OUTSIDE the destination — remove them, or restore into an empty directory and merge by hand"
}

cmd_restore() {
  need age; need par2; need unsquashfs
  local dest="${1:-}"
  [[ -n "$dest" ]] || die "usage: $PROG restore <destination> [--only <path-in-archive>] [--disc N]"
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
  resolve_identity
  # Ingest writes ciphertext into staging; the restore path produces plaintext
  # in the same tree, in a directory the README puts under /var/tmp.
  secure_staging
  mkdir -p "$dest" "$RESTORE_DIR"
  warn "$RESTORE_DIR will hold DECRYPTED images (mode 0700, but plaintext on disk)"
  (( EUID == 0 )) || warn "not running as root: ownership will not be restored"

  # unsquashfs is run with -f for every image, because discs 2..N extract into a
  # tree disc 1 already populated. Into a live $HOME that silently overwrites
  # current files with the backup's versions, mode and mtime included.
  if [[ -n "$(ls -A -- "$dest" 2>/dev/null)" ]]; then
    warn "$dest is not empty. unsquashfs -f will OVERWRITE existing files with the backup versions."
    warn "  existing entries: $(ls -A -- "$dest" | head -5 | tr '\n' ' ')..."
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
    (( ${#sel[@]} )) || die "'$only' is on disc(s) ${hits[*]}, none of which have been ingested"
    encs=( "${sel[@]}" )
    step "'$only' is on disc(s) ${hits[*]}"
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
    step "preparing $(basename "$enc")"
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
    local precheck="$only"
    if [[ "$only" == *$'\n'* || "$only" == *$'\r'* ]]; then precheck=""; fi
    if [[ -n "$precheck" ]] && ! unsquashfs -l "$img" 2>/dev/null \
        | P="squashfs-root/$only" awk \
            '$0 == ENVIRON["P"] || index($0, ENVIRON["P"] "/") == 1 { f=1; exit } END { exit !f }'; then
      step "$only is not on $(basename "$img")"
      if (( ! KEEP_IMAGES )); then rm -f -- "$img" "$RESTORE_DIR/$(basename "$img").sha512"; fi
      continue
    fi
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
    local ulog="$RESTORE_DIR/unsquashfs.$(basename "$img").log" urc=0
    unsquashfs "${ua[@]}" >"$ulog" 2>&1 || urc=$?
    if (( urc == 2 )); then
      warn "unsquashfs reported non-fatal errors on $(basename "$img") — see $ulog"
      grep -v '^$' "$ulog" | head -3 | sed 's/^/    /' >&2
    elif (( urc != 0 )); then
      warn "unsquashfs output:"; sed 's/^/    /' "$ulog" >&2
      die "unsquashfs failed on $(basename "$img") (exit $urc)"
    else
      rm -f -- "$ulog"
    fi
    ok "$(basename "$img") extracted"
    # The listing said the path was here, but the destination is the proof:
    # only count --only as found when the path actually materialised. -L covers
    # a dangling symlink, which -e alone reports as absent.
    if [[ -z "$only" || -e "$dest/$only" || -L "$dest/$only" ]]; then found=1; fi
    # Keeping every decrypted image alive means a restore needs roughly twice
    # the archive size. Free each one as soon as its contents are on disk.
    if (( ! KEEP_IMAGES )); then
      rm -f -- "$img" "$RESTORE_DIR/$(basename "$img").sha512"
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
  need age; need unsquashfs
  local n mp="${2:-}"
  [[ -n "${1:-}" && -n "$mp" ]] || die "usage: $PROG mount <disc-number> <mount-point>"
  n="$(num_arg "$1" 'disc number')"
  (( EUID == 0 )) || die "mounting requires root"
  resolve_identity
  secure_staging
  local enc img
  enc="$(printf '%s/disc%02d.squashfs.age' "$ENC_DIR" "$n")"
  [[ -f "$enc" ]] || die "no image for disc $n in $ENC_DIR"
  prepare_image "$enc"; img="$PREPARED_IMG"
  mkdir -p "$mp"
  mount -o loop,ro "$img" "$mp" || die "mount failed"
  ok "disc $n mounted read-only at $mp"
  step "unmount with: umount $mp"
}

# Render C0 control bytes and DEL visibly, sparing the tab that separates an
# index record's fields. A byte-for-byte mirror of escapeControls in
# go/internal/restore/extract.go: '\r' and '\n' get their C-style escapes, every
# other control byte becomes \xHH, and anything printable passes through
# untouched — multi-byte characters included, which is why this runs in the C
# locale and works one BYTE at a time.
#
# The names come out of an archive, so they are chosen by whoever could plant
# one file in the backed-up tree; printed raw to a terminal, an ESC ] 0 ; ... BEL
# in a filename retitles the operator's window, and worse where OSC 52 is on.
esc_controls() {
  LC_ALL=C awk '
    BEGIN {
      for (i = 1; i < 256; i++) ord[sprintf("%c", i)] = i
      # Everything escaped, as raw bytes inside one bracket expression: C0
      # except tab (9), plus DEL. None of them is a regex metacharacter.
      ctl = ""
      for (i = 1; i < 32; i++) if (i != 9) ctl = ctl sprintf("%c", i)
      re = "[" ctl sprintf("%c", 127) "]"
    }
    # The clean line — every line, almost always — is passed through whole
    # rather than rebuilt a byte at a time.
    $0 !~ re { print; next }
    {
      out = ""; n = length($0)
      for (i = 1; i <= n; i++) {
        c = substr($0, i, 1); v = ord[c]
        if (c == "\t")            out = out c
        else if (v == 13)         out = out "\\r"
        else if (v == 10)         out = out "\\n"
        else if (v < 32 || v == 127) out = out sprintf("\\x%02x", v)
        else                      out = out c
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

cmd_index() {
  need age
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
  need age; need unsquashfs
  resolve_identity
  local n
  [[ -n "${1:-}" ]] || die "usage: $PROG list <disc-number>"
  n="$(num_arg "$1" 'disc number')"
  secure_staging
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
  restore <dest> [opts]      repair, decrypt and extract
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
    KEEP_IMAGES=$KEEP_IMAGES                 1 keeps each decrypted image after
                                  extracting it, for repeated restores. The
                                  default removes them, because otherwise a
                                  restore needs room for the whole archive twice
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
