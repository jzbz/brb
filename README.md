# brb — Blu-ray Backup

[![CI](https://github.com/jzbz/brb/actions/workflows/ci.yml/badge.svg)](https://github.com/jzbz/brb/actions/workflows/ci.yml)

Independent, mountable, encrypted backup discs.

`brb` bin-packs a directory tree into disc-sized groups, builds one self-contained
SquashFS image per disc, encrypts it with [age](https://age-encryption.org),
computes par2 recovery data over the ciphertext, and wraps each disc in an ISO
ready to burn to BD-R, BD-R DL or BDXL — including
[M-DISC](#m-disc-and-what-media-to-actually-buy), which is what you want if the
discs are meant to outlive the decade.

```
bin-pack the tree into disc-sized groups
  -> mksquashfs   (one self-contained image per disc)
    -> age        (encrypt the image)
      -> par2     (recovery data over the CIPHERTEXT)
        -> xorriso (one ISO per disc)
          -> BD-R / BD-R DL / BDXL
```

**Every disc is independent.** Losing disc 7 loses exactly the files on disc 7;
every other disc still restores on its own. The last step of a restore is

```bash
mount -o loop,ro disc07.squashfs /mnt
```

which needs nothing but the Linux kernel — no `brb`, no Python, no bespoke
archive format.

## Two programs, one format

`brb` ships as two implementations with a deliberate split:

| | | |
|---|---|---|
| **Go** | `brb-linux-amd64` / `brb-linux-aarch64` | **writes and reads.** Packing, mksquashfs, encryption, par2, ISOs, burning, resume — and every restore command too |
| **bash** | `brb.sh` | **reads only.** doctor, ingest, restore, mount, list, index, verify-disc |

The Go build is the tool you use day to day. The bash script exists for one
reason: a restore fifteen years from now, by someone holding a disc and no
particular reason to trust an 8 MB binary. It is a bit over a thousand lines,
comments and all, and can be read end to end in an afternoon, so the answer to
"what is this going to do to my bytes" is available without running anything.

`brb.sh` refuses `backup`, `plan`, `burn`, `iso` and `init-key` by name, and says
where they went, rather than failing as an unknown command.

The two are held to the same on-disc format by `xcompat-test.sh`, which builds a
set with the Go build and reads it with both, and asserts they produce identical
answers — down to the encrypted index and the byte-for-byte restored tree.

---

## Requirements

The distinction that matters is not between the two programs but between
**writing** discs and **reading** them. Writing needs the most tooling and you
need it today. Reading is what someone needs in fifteen years, and it is
deliberately tiny: age, par2, unsquashfs — and for the final step, nothing but
the Linux kernel. Every member of that set is a small, specified,
widely-implemented format that outlives whoever wrote this.

Run `doctor` on either program at any time; each checks the dependencies it
actually uses and reports what it found.

### What each dependency is for

**The Go build**, which writes and reads:

| Binary | Package (Fedora) | Needed for |
|---|---|---|
| `mksquashfs` | `squashfs-tools` **≥ 4.5** | building each disc image — backup only |
| `unsquashfs` | `squashfs-tools` | extracting and listing an image — restore |
| `par2` | `par2cmdline` | parity over the ciphertext, and repair — both |
| `xorriso` | `xorriso` | building and burning ISOs — backup and burn |

It does **not** need the `age` binary: encryption uses
[filippo.io/age](https://filippo.io/age) as a library, so the format is exactly
the same and one fewer thing has to be installed.

**The bash reader (`brb.sh`)**, which restores:

| Binary | Package (Fedora) | Needed for |
|---|---|---|
| `age` | `age` | decrypting an image |
| `par2` | `par2cmdline` | repairing a rotted disc |
| `unsquashfs` | `squashfs-tools` | extracting and listing |
| `sha512sum`, `stat`, `cp`, `cut`, `sort`, `tr`, `dd`, `df`, `readlink` | `coreutils` | hashing, accounting, and the destination symlink check |
| `gzip` / `gunzip` | `gzip` | reading the encrypted index |
| `find`, `awk`, `sed` | `findutils`, `gawk`, `sed` | locating and formatting |
| `bash` **≥ 4.4** | `bash` | the script itself (uses `mapfile -d`) |

No `python3` for either — the bin-packer that once needed it is Go code now.

And to build the Go program from source: **Go 1.25 or newer**. Nothing else
needs it, and a restore never does.

Optional, but worth having:

| Binary | Package (Fedora) | Why |
|---|---|---|
| `ddrescue` | `ddrescue` | salvages partially readable discs; `cp` stops at the first I/O error, `ddrescue` does not — this is what makes par2 usable on a scratched disc |
| `udisksctl` | `udisks2` | lets `verify-disc` and `ingest` mount the drive for you |
| `eject`, `findmnt` | `eject`, `util-linux` | disc swapping during `ingest` |
| `pv` | `pv` | progress on long pipes |

`age-keygen` is deliberately not on that list. `brb init-key` generates both the
primary and the rescue keypair with the age library it already links, so neither
implementation ever execs it.

### Install

**Fedora / RHEL / CentOS**

```bash
sudo dnf install age squashfs-tools par2cmdline xorriso findutils gawk coreutils gzip
```

```bash
sudo dnf install ddrescue udisks2 pv eject util-linux
```

**Debian / Ubuntu**

```bash
sudo apt install age squashfs-tools par2 xorriso findutils gawk coreutils gzip
```

```bash
sudo apt install gddrescue udisks2 pv eject util-linux
```

Note the package name differences from Fedora: par2cmdline is `par2`, and
ddrescue is `gddrescue` (the `ddrescue` package on Debian is a different,
unrelated program).

**Arch**

```bash
sudo pacman -S age squashfs-tools par2cmdline libisoburn gawk coreutils gzip
```

```bash
sudo pacman -S ddrescue udisks2 pv util-linux
```

`xorriso` is provided by `libisoburn` on Arch.

### Version requirements worth checking

- **squashfs-tools must be 4.5 or newer.** `brb` feeds `mksquashfs` a
  NUL-delimited file list via `-cpiostyle0`, which does not exist in 4.4.
  Fedora 36+, Debian 12+ and Ubuntu 22.04+ ship 4.5 or newer; Debian 11 and
  Ubuntu 20.04 ship 4.4 and will not work. `brb doctor` tests for the flag
  directly rather than parsing a version string, so trust it over this table.
- **`age`** is packaged on Fedora, Debian 12+, Ubuntu 22.04+ and Arch. If your
  distribution does not carry it, take a static binary from the
  [releases page](https://github.com/FiloSottile/age/releases) — it is a single
  file with no runtime dependencies.
- **Go 1.25+** only if you build the Go program from source. A prebuilt static
  binary from any disc needs nothing at all.

### Install brb itself

The Go build is the one to install — it is the only one that can write a set:

```bash
cd go && go build -o ~/.local/bin/brb ./cmd/brb
```

Or take a prebuilt static binary from any disc (`brb-linux-amd64` /
`brb-linux-aarch64`, matching `uname -m`) — they need no libc, no interpreter
and no shared libraries.

The bash reader is a single self-contained script with nothing to build:

```bash
install -Dm755 brb.sh ~/.local/bin/brb.sh
```

Install it under a distinct name. Both name themselves after however they were
invoked, so installing both as `brb` would make their help text and every
suggested command ambiguous — and one of them cannot back up.

---

### Building the disc payload

Every disc carries the tool itself, three ways: the bash script, a static Go binary for
each of the two architectures worth caring about, and the complete source. A restorer in
fifteen years needs none of it — the manual restore path is four standard commands — but
having it costs about 10 MiB out of a 100 MiB reserve, so there is no reason not to.

```bash
./build-dist.sh
```

That cross-compiles `brb-linux-amd64` and `brb-linux-aarch64` (static, `CGO_ENABLED=0`,
so no libc and no shared libraries), vendors every Go dependency, and packs
`brb-src.tar.gz`. It writes to a directory you name, deliberately outside the working
tree — build output does not belong in the source:

```bash
./build-dist.sh /path/to/dist
```

With no argument it uses `$BRB_DIST_OUT`, falling back to `~/brb-dist`.

Then point `brb` at the result, either way:

```bash
export BRB_DIST_DIR=/path/to/dist
```

```bash
ln -sfn /path/to/dist ./dist
```

`brb` also looks in `/usr/local/share/brb` and `/usr/share/brb`. If it finds nothing it
warns and burns discs carrying only a copy of the binary that is running (`brb-linux-amd64`
on x86-64) — a missing payload never fails a backup. `brb doctor` reports exactly what it
found.

Building the payload needs a Go toolchain (1.25+); nothing else in `brb` does, and a
restore never does.

The source tarball vendors its dependencies, so it rebuilds with no network:

```bash
tar xzf brb-src.tar.gz && cd brb-*/go && go build -mod=vendor ./cmd/brb
```

## Quick start

Everything here is the Go build — it is the one that writes. `brb.sh` can run
the last two steps (`ingest`, `restore`) and nothing before them.

```bash
brb doctor
```

```bash
brb init-key
```

`init-key` writes `~/.config/brb/identity.txt` (mode 0400) and appends its public
key to `~/.config/brb/recipients.txt`. **Back that identity file up somewhere
that is not these discs** — a password manager, and printed on paper. Losing it
means losing every backup, permanently and irrecoverably.

Optionally, give yourself a second way in:

```bash
brb init-key --rescue-key
```

That adds a second recipient whose identity is kept encrypted under a passphrase,
so the set restores with either the key file *or* something you remember. See
[Keys, and the rescue key](#keys-and-the-rescue-key) for what it does and why it
is built the way it is. It can be run later, on an archive that already exists —
discs built from that point on carry both keys. It asks for the passphrase on
the terminal, so it refuses to run under `--yes`.

```bash
brb plan
```

`plan` scans and reports how many discs the archive needs, without building
anything. Use it to sanity-check before committing to a multi-hour run.

```bash
brb backup
```

```bash
brb burn all
```

```bash
brb verify-disc 1
```

```bash
brb restore /tmp/testrestore
```

Do that last step at least once, before you trust the set with anything. A backup
you have never restored is a hypothesis, not a backup.

Then wipe staging, which held plaintext:

```bash
rm -rf /var/tmp/brb
```

---

## Commands

**Writing a set — the Go build only.** `brb.sh` refuses these by name and points
here.

| Command | What it does |
|---|---|
| `brb init-key` | generate an age keypair and recipients file |
| `brb init-key --rescue-key` | additionally add a second recipient whose identity is passphrase-protected. It asks for the passphrase on the terminal, so it is refused under `--yes` |
| `brb plan` | scan and show the disc layout without building anything |
| `brb backup` | build, encrypt, protect and image every disc. `--verify-roundtrip` decrypts each image back and compares hashes before the plaintext is deleted — it needs a readable, *unencrypted* `AGE_IDENTITY` and refuses to start without one |
| `brb burn <n\|n-m\|n-\|all>` | burn ISOs, confirming before each; builds each ISO if it is missing and removes it again after a successful burn |
| `brb iso <n\|n-m\|n-\|all>` | build ISO images without burning, for burning elsewhere |

**Reading a set — either implementation.** Substitute `brb.sh` for `brb` below
and the behaviour is the same; that equivalence is what `xcompat-test.sh` exists
to prove. The few places they differ are called out where they come up: `--only`
repetition and `--keep-images` below, `KEEP_IMAGES` in a shared config file under
[Configuration](#configuration). One more worth knowing before you script
anything: `brb.sh ingest` prompts on `/dev/tty` between discs even with a mount
path and `--yes`, and fails outright with no terminal, where the Go build's
`ingest` runs unattended.

| Command | What it does |
|---|---|
| `brb doctor` | check dependencies and report which key a restore would use |
| `brb ingest [mount]` | copy discs back onto disk, prompting for each, any order |
| `brb restore <dest> [opts]` | repair, decrypt and extract, **overwriting** what is already in `<dest>` — see below |
| `brb mount <n> <mountpoint>` | decrypt one disc's image and mount it read-only |
| `brb list <n>` | list the contents of one disc's image |
| `brb index [pattern]` | which disc holds a given path |
| `brb verify-disc <n> [mount]` | read a burned disc back and check every hash |
| `brb help` | full help text |

Global flags, before the command: `--yes` / `-y` to skip confirmations and
`-c CONFIG` to point at an alternate config file, on both; `--no-color` on the
Go build only. Per-command flags go *after* the command, and are recognised only
where they are documented — `brb --resume backup` is a usage error, `brb backup
--resume` is the resume.

`restore` takes `--only <path-in-archive>` to extract a single path, and
`--disc <n>` to restore only one disc. `--only` is repeatable on the Go build
and every path given is extracted; `brb.sh` takes one path and a second `--only`
silently replaces the first. The Go build additionally takes `--keep-images`,
which is `KEEP_IMAGES=1` for one run.

### restore overwrites the destination

`restore` runs `unsquashfs -f` for every image, because discs 2..N extract into
a tree disc 1 already populated. Into a directory that has contents of its own
that means existing files are **replaced** by the backup's versions, mode and
mtime included. Both implementations guard it the same way, and both guards run
for `--only` and `--disc` too:

- A `<dest>` that is not empty is named, its first few entries listed, and the
  overwrite confirmed before anything is extracted. **`--yes` answers that
  confirmation**, so a `--yes` restore into a live directory overwrites it
  without asking. Answering no aborts with *"restore into an empty directory and
  merge by hand"*.
- A `<dest>` that holds a symlink resolving to a directory — at any depth — is
  **refused outright**, because `unsquashfs -f` follows such a link and writes
  the backup's files outside the destination. `--yes` cannot wave this one
  through; remove the link, or restore somewhere empty. A symlink to a *file* is
  harmless and is left alone.

### Resuming an interrupted backup

A twenty-disc set takes days to build. `backup` records its progress in
`$STAGING/state.json` after every completed disc, so an interruption — a reboot,
a full disk, a Ctrl-C — costs only the disc that was in flight:

```bash
brb backup --resume
```

`--resume` is a flag of `backup`, so it goes after the command; `brb --resume
backup` is rejected as an unknown global flag.

It picks up at the disc after the last complete one. The state file lists every
path already written to a disc, and the learned pack ratios every finished disc
measured, so the resumed run re-scans the source tree and then skips exactly
what is already on a disc — files added since the run started land on later
discs, files deleted since are simply absent, and the run warns when the tree's
measured size has changed. `ARCHIVE_NAME` and `SOURCE_DIR` must match the
interrupted run, or the resume stops rather than write two different trees into
one set. Without `--resume`, a `backup` that finds finished discs in staging
refuses to start rather than overwrite days of work.

### ISOs

By default (`ISO_MODE=ondemand`) no ISOs are built during `backup`. `burn`
images each disc at the moment it goes into the drive and deletes the ISO once
it is written, which keeps staging near the size of the compressed set instead
of roughly 2.2x it for the whole length of a burn campaign — an ISO is a full
second copy of its disc directory. Set `KEEP_ISOS=1` to keep them, or
`ISO_MODE=eager` for the old behaviour of building every ISO before the first
burn. To materialise them as files without burning — to take to another machine,
or a different burner — use `brb iso all`.

---

## Configuration

Config lives at `~/.config/brb/config` (override with `BRB_CONFIG`). It is a
plain list of assignments, and every setting can equally be given as an
environment variable.

**Nearly everything here is a writer setting, read only by the Go build.** How a
set was built — source tree, disc geometry, compression, pack ratio, par2
parameters, ISO mode — was decided when it was written and is recorded in
`MANIFEST.txt` on every disc. The bash reader ignores all of it and uses only
`STAGING`, `AGE_IDENTITY`, `AGE_RECIPIENTS_FILE`, `BURNER` and `KEEP_IMAGES`.

**`brb.sh` `source`s the config file — it executes it as bash.** Anything in it
runs with your privileges, not just `KEY=value` lines: a config containing `rm
-rf ~` deletes your home directory the moment any `brb.sh` command loads it. So
trust the file exactly as much as you trust `brb.sh` itself. **Never point `-c`
or `BRB_CONFIG` at a file you did not write** — least of all one carried on a
disc, which is data from wherever that disc has been. The script's own header and
`brb.sh help` say the same thing.

The Go build does not source anything: it parses the file, accepts only
`KEY=value` (and the two array settings), and reports anything else as a syntax
error naming the line. Pointing it at the same hostile file is an error message,
not an execution.

**One sharp edge if you share a config between them.** Because `brb.sh` sources
the file, an unknown key is simply a variable nobody reads. The Go build
*validates*, and rejects an unknown key by refusing to run at all. `KEEP_IMAGES`
is the case that bites today: it is a real bash setting, and putting it in a
shared config stops the Go build dead. Use its `--keep-images` flag instead, or
keep the readers on separate config files.

```bash
SOURCE_DIR=/home/you
STAGING=/var/tmp/brb
DISC_TYPE=bd25                  # bd25 | bd50 | bdxl100 | bdxl128 (M-DISC uses the same value)
DISC_CAPACITY_BYTES=            # override for unusual media
COMPRESSION=zstd                # zstd | xz | gzip | lz4 | lzo | none
COMPRESSION_LEVEL=19            # zstd 1-22
BLOCK_SIZE=1M
PACK_RATIO=1.00                 # expected compressed/raw; lower = fuller discs
PAR2_REDUNDANCY=10
BURNER=/dev/sr0
BURN_SPEED=4
LABEL_PREFIX=BACKUP
ISO_MODE=ondemand               # ondemand | eager — see "ISOs" above
KEEP_ISOS=0                     # 1 keeps each ISO after a successful burn
AGE_RECIPIENTS_FILE=~/.config/brb/recipients.txt
AGE_IDENTITY=~/.config/brb/identity.txt      # restore only; see below
DIST_DIR=                       # copies of brb for every disc; empty = auto-locate

KEEP_IMAGES=0                   # reader: 1 keeps each decrypted image

PRUNE_DIRS=( ".cache" ".local/share/Trash" "snap" )   # paths relative to SOURCE_DIR
EXCLUDE_MASKS=( "*.pyc" "core.[0-9]*" )               # filename patterns
```

That is the useful subset, not the whole set of keys. Since the Go build rejects
a key it does not know, take the authoritative list from `brb help`, which prints
every setting with the value actually in force.

**Note the mask is `core.[0-9]*`, not `core`.** A bare `core` matches every
*directory* named `core/` as well as the dump files, and a matching directory is
pruned whole — its contents vanish from the backup without appearing even in the
disc's directory skeleton, so a restored tree shows no sign anything is missing.
Any Go, Drupal or kernel checkout would lose its `core/`. This was a real bug;
the default now matches the dumps glibc actually writes and leaves directories
alone. Directory-shaped exclusions belong in `PRUNE_DIRS`.

`DIST_DIR` is the disc payload described under [Building the disc
payload](#building-the-disc-payload). In the environment it is spelled
`BRB_DIST_DIR`; left empty, `brb` looks beside itself for a `dist` directory,
then in `/usr/local/share/brb` and `/usr/share/brb`. Setting it to a directory
that is not there is reported rather than ignored, and a payload that cannot be
found never fails a backup — the discs simply carry fewer copies of the tool.

`AGE_IDENTITY` is only read by the restore side (`restore`, `mount`, `list`,
`index`), by `doctor`'s round-trip check, and by `backup --verify-roundtrip`. A
plain `backup` never touches it: it encrypts to public keys and needs no secret
at all. Left empty, `brb` looks next to the recipients file for
`identity.txt`, then `identity.txt.age`, then
`rescue-identity.txt.age`, and uses the first one it finds — so a machine whose
plaintext identity has been shredded, or lost, keeps working with no
configuration change. An age-encrypted identity works anywhere a plain one does;
`brb` asks for the passphrase once per command, not once per disc.

Setting `PRUNE_DIRS` or `EXCLUDE_MASKS` in the config **replaces** the built-in
defaults rather than adding to them. The defaults prune the usual regenerable
caches: `.cache`, Trash, Steam, `.var/app`, `snap`, `.npm/_cacache`,
`.cargo/registry`, `.rustup/toolchains`, `.gradle/caches`, `.m2/repository`,
`go/pkg/mod`, container and Docker storage, and Vagrant boxes.

### Disc capacities

| `DISC_TYPE` | Media | Raw capacity | Usable image budget |
|---|---|---|---|
| `bd25` | BD-R single layer | 23.31 GiB | 20.49 GiB |
| `bd50` | BD-R DL dual layer | 46.61 GiB | 41.07 GiB |
| `bdxl100` | BDXL triple layer | 93.23 GiB | 82.22 GiB |
| `bdxl128` | BDXL quad layer | 119.21 GiB | 105.16 GiB |

The image budget is what is left after reserving 2% for ISO 9660 overhead,
`RESERVE_BYTES` (100 MiB by default) for the plaintext files carried on every
disc, and room for the par2 recovery data. Budgets above assume the default
`PAR2_REDUNDANCY=10`; raising redundancy shrinks the budget proportionally.

### M-DISC, and what media to actually buy

If the point of this exercise is *long-term* storage, buy **M-DISC BD-R**.

Ordinary recordable Blu-ray writes to an organic dye layer, which is a
consumable: it degrades with heat, humidity and light, and the disc's lifetime is
the dye's lifetime. M-DISC records into an inorganic, heat-resistant layer
instead — the bit pattern is physically etched rather than chemically stained, so
there is no dye left to fade. That is the whole difference, and it is a real one.

**It needs no special support from brb or from your drive.** M-DISC BD-R is
ordinary BD-R as far as the format is concerned, so any Blu-ray writer burns it
and any Blu-ray reader reads it. (This is unlike M-DISC *DVD*, which needed an
"M-Ready" burner — a distinction that still confuses people shopping for media.)
Nothing in this tool changes: use the same `DISC_TYPE` as the equivalent
conventional disc.

| `DISC_TYPE` | M-DISC available? |
|---|---|
| `bd25` | yes — M-DISC BD-R 25 GB |
| `bd50` | yes — M-DISC BD-R DL 50 GB |
| `bdxl100` | yes — M-DISC BDXL 100 GB |
| `bdxl128` | **no** — quad-layer 128 GB is conventional BD-R XL only |

**On the longevity numbers, be skeptical.** You will see "1,000 years" quoted.
That figure is an extrapolation from accelerated-aging tests, not an observation
— nobody has had one of these for a thousand years, and independent testing has
been less enthusiastic than the marketing. What is *defensible* is the physical
argument: an inorganic recording layer has no organic dye to break down, and that
removes the failure mode that kills ordinary BD-R sitting in a warm cupboard.
Treat M-DISC as clearly better than conventional BD-R for archival use, and treat
any specific century count as a number somebody wanted to sell you.

Two practical notes:

- **Burn slower than you think.** `BURN_SPEED=4` is the default here and is a
  reasonable ceiling for M-DISC; 2x is a defensible choice for a set you intend
  to keep. Write speed is not where you want to economise on a disc you are
  buying for its lifetime.
- **It costs more per gigabyte.** That is the trade. For a set you plan to
  re-cut every few years, conventional BD-R is fine; for the set you want to
  still read in thirty years, the media is the cheapest part of the exercise.

None of this changes the honest caveat that the on-disc README already makes to
whoever finds these later: **the realistic failure mode is drive availability,
not disc decay.** M-DISC buys you a disc that outlasts the drives, which makes
"can I still source a Blu-ray reader" the question that actually decides whether
the archive survives. Plan for that too — and test your restores.

### About PACK_RATIO

Discs are packed by *uncompressed* size, so `brb` has to guess how well the
content will compress before it compresses it. `PACK_RATIO` is that guess,
expressed as compressed ÷ raw.

The default of `1.00` assumes no compression at all. That is always safe but
leaves discs partly empty whenever the content actually is compressible. If your
first run reports images compressing to, say, 0.62 of raw, set `PACK_RATIO=0.65`
and re-run for fuller discs.

You are not required to get this right. If an image overshoots its budget, `brb`
measures the real ratio, re-packs that disc with it, and continues on its own —
up to `MAX_SHRINK_ATTEMPTS` (4) times. A bad guess costs rebuild time, not
correctness.

You rarely have to tune it at all: `PACK_RATIO_ADAPT=1` is the default, so every
finished disc feeds its measured ratio back and the next disc is planned from the
worst of the last `PACK_RATIO_WINDOW` (3) discs times `PACK_RATIO_MARGIN` (1.05).
The estimate moves in both directions, so a stretch of incompressible files
raises it again rather than packing the rest of the set to a ratio only the early
discs achieved. `PACK_RATIO` is then the starting guess for disc 1. Set
`PACK_RATIO_ADAPT=0` to hold the configured value fixed for the whole set.

### Why two compressors

`brb` uses zstd for the disc images and gzip for the index and the source
tarball. That looks inconsistent. It is deliberate, and the deciding question is
not the compression ratio — it is **who has to decompress it, and what they need
installed at the time.**

| Artifact | Compressor | Size | Decompressed by |
|---|---|---|---|
| `discNN.squashfs` | zstd | hundreds of GB | the **kernel**, on `mount -o loop` |
| `index.tsv.gz.age` | gzip | KB to tens of MB | a **person**, with a userspace tool |
| `brb-src.tar.gz` | gzip | ~1–2 MB | a **person**, with `tar xzf` |

zstd costs nothing on the images, because SquashFS-zstd is decompressed by the
Linux kernel itself. The promise that `mount -o loop,ro disc07.squashfs /mnt`
needs no userspace tooling survives intact, and across hundreds of gigabytes the
ratio is worth real money in media.

The index inverts that calculus. It is the artifact you reach for when a disc has
been **lost** — you are asking "what was on it?", quite possibly from a rescue
USB or a borrowed machine. `gunzip` has been on every Unix since 1992 and is in
busybox and every base install; `zstd` dates from 2016 and is not guaranteed on a
minimal or elderly system. What you would gain is a rounding error: even at two
million files the index is perhaps 40 MB gzipped against 30 MB with zstd, on a
23 GiB disc. That is not a trade worth making in the one code path that runs when
things have already gone wrong. The source tarball follows the same reasoning.

So the rule is: **zstd where the kernel decompresses it and the data is large,
gzip where a person needs a userspace tool in a degraded situation and the data
is small.**

One tradeoff runs the other way, and it is worth knowing. SquashFS-zstd requires
kernel 4.14 or newer (2017). A restorer booting a genuinely ancient rescue kernel
would find `mount` fails on a zstd image where a gzip one would have worked; they
would fall back to `unsquashfs`, which still works but is no longer "nothing but
the kernel". If you value maximum mountability decades out over space, set
`COMPRESSION=gzip`; for maximum compression instead, `COMPRESSION=xz`. zstd is
the right default, not a free lunch.

---

## What ends up on each disc

```
README.md                       restore instructions, written for a stranger
MANIFEST.txt                    the whole set, and the exact tool versions used
SHA512SUMS                      hashes of every file on this disc
brb.sh                          the bash reader — restores this disc, and is short
                                enough to read before you trust it
brb-linux-amd64                 static Go binary, 64-bit Intel/AMD
brb-linux-aarch64               static Go binary, 64-bit ARM
brb-src.tar.gz                  complete source for both, dependencies vendored
data/
  discNN.squashfs.age           the filesystem image, encrypted
  discNN.squashfs.age.sha512    hash of the encrypted image
  discNN.squashfs.sha512        hash of the image AFTER decryption
  discNN.squashfs.age.par2      par2 index
  discNN.squashfs.age.vol*.par2 recovery data
  index.tsv.gz.age              encrypted map of which disc holds which file
  index.tsv.gz.age.sha512       hash of the encrypted index
  sidecars.par2                 par2 index over the small .sha512 files
  sidecars.vol*.par2            recovery data for them
```

The `.sha512` sidecars get their own parity because they are tiny and a single
rotted byte in one of them would otherwise condemn an image that is perfectly
intact. If a sidecar fails to verify but par2 says the image is whole, it is the
sidecar that is corrupt: `par2 repair -- sidecars.par2` from the disc's `data/`
directory puts it back.

Every disc carries the **full directory skeleton** of the original tree —
directories, symlinks, device nodes — so mounting any single disc shows you the
whole shape of the backup, with the files that live on that disc present and the
rest absent. Skeleton entries carry no data, so replicating them across discs is
nearly free.

Parity is computed over the **encrypted** bytes, so it protects exactly what is
physically on the disc.

---

## Restoring without brb

This is the point of the design, so it is worth stating plainly. Given one disc
and your age identity:

```bash
cp /mnt/data/disc07.squashfs.age* .
```

The glob is deliberate: it brings the `.sha512` sidecar and the `.par2` files
along, which the next line needs.

```bash
sha512sum -c disc07.squashfs.age.sha512 || par2 repair -- disc07.squashfs.age.par2
```

```bash
age -d -i /path/to/identity.txt -o disc07.squashfs disc07.squashfs.age
```

```bash
sudo mount -o loop,ro disc07.squashfs /mnt
```

That is the whole restore path. `unsquashfs -d /dest disc07.squashfs` extracts
instead of mounting; run it as root if you want original ownership back.

If a disc will not read cleanly, pull it off with `ddrescue` first — it fills
unreadable regions with zeros and keeps going, which is exactly what par2 needs:

```bash
ddrescue -d -r3 /mnt/data/disc07.squashfs.age ./disc07.squashfs.age ./disc07.mapfile
```

---

## Keys, and the rescue key

Backup needs **public keys only**. Every line of the recipients file is an age
recipient, every image is encrypted to all of them, and any single one of the
matching identities restores the whole set on its own. No secret has to sit on
the backup machine for the days or weeks a set takes to build, so compromising
that machine yields nothing that outlives the compromise.

The price of that design is a single point of failure in the other direction:
lose `identity.txt` and the archive is gone. `brb init-key --rescue-key` is the
answer.

```bash
brb init-key --rescue-key
```

This is the Go build. `brb.sh` has no `init-key` at all — it reads disc sets and
refuses every writing command by name — so a rescue key is minted with the Go
binary (or by hand, below) and read by both.

It mints a second keypair and appends its public key to the recipients file, so
every disc from then on is encrypted to both. Discs already burned are not, and
still need the key they were built with. What makes it a *rescue* key is how the
private half is stored — the shape is this:

```
age-keygen | age -p -o ~/.config/brb/rescue-identity.txt.age
```

The identity goes from `age-keygen`'s stdout straight into `age -p`'s stdin,
never through a file. `brb` does the same thing in-process with the age library
it already links, so it needs no `age-keygen` binary and the plaintext identity
is never written anywhere: it is generated in memory and handed straight to the
scrypt container. That matters because there would be nothing useful to shred
afterwards — `shred` cannot promise anything on a copy-on-write, compressed or
flash-translated filesystem, and the plaintext would be in the page cache
regardless. What lands on disk is a ~400-byte file, mode 0400, encrypted under a
passphrase you chose and typed twice. Copy it to a USB stick, a cloud drive, a
relative's machine: it is inert without the passphrase.

The path is not configurable: `rescue-identity.txt.age`, beside the recipients
file, is the last place both readers look for an identity. An existing one is
never overwritten — move it aside first — and an existing `identity.txt` is left
untouched, which is what makes `--rescue-key` safe to run on an archive that
already exists.

Restoring with it needs no special flags, on either implementation. Once
`identity.txt` is gone, `brb` finds `rescue-identity.txt.age`, asks for the
passphrase once, and carries on:

```bash
brb restore /path/to/destination
# or, explicitly:
AGE_IDENTITY=/media/usb/rescue-identity.txt.age brb restore /path/to/destination
```

Do not add `--yes` to that: the Go build refuses to unlock an encrypted identity
under it, since a run that promised to be unattended has nobody to type a
passphrase. Run it without, and answer the one prompt.

Without `brb`, `age` takes the encrypted identity wherever a plain one goes:

```bash
age -d -i rescue-identity.txt.age -o disc07.squashfs disc07.squashfs.age
```

### Why not just encrypt the discs with a passphrase?

The obvious request is "let me use a passphrase instead of a key file". `brb`
deliberately does not do that, for three reasons:

1. **age will not express "my key OR my passphrase" in one file.** A passphrase
   in age is an scrypt stanza, and age refuses to mix an scrypt stanza with
   recipient stanzas. Encrypting with a passphrase therefore means giving up
   recipients entirely — no multi-key sets, no rescue key, no third party who
   can restore for your estate.
2. **A stolen disc is ciphertext forever.** Whoever holds the disc holds the
   attack surface, on their hardware, with unlimited time. A passphrase a human
   can memorise loses that race over the decades this medium is meant to last;
   an X25519 key does not. Passphrase-encrypting the images would move the
   secret *onto the thing you lose control of*.
3. **Backup would need a secret on the machine.** Today it needs only public
   keys. Passphrase encryption would mean the passphrase — and so the ability
   to decrypt every disc — is present on the backup host for the whole run.

The rescue key inverts all three. The discs stay encrypted to public keys, and
the passphrase guards one small file the thief does not have. Guessing at it
requires stealing that file first, and if they have stolen it, you can generate
a new key and rebuild — which you cannot do with discs already in the wild.

The corollary: **never put the rescue file on these discs.** A file stored next
to its own passphrase, or on the media it unlocks, is one secret, not two.

`brb doctor` reports whether a rescue key is present, and which identity a
restore on this machine would use.

### A public archive, with no secret at all

```bash
brb backup --public-archive
```

Encryption is a second way to lose an archive. Media that outlives its key is
still landfill, and for a set meant to be mounted by a stranger in forty years —
a family photo archive, a public record, anything where there is nobody left to
ask for the key — that risk can outweigh confidentiality entirely.

`--public-archive` (or `PUBLIC_ARCHIVE=1`) makes a set that **keeps no secret**.
brb mints a keypair for that archive, encrypts to it exactly as usual, and
writes the secret key onto every disc as `identity.txt`. The set opens with
nothing but the disc in hand:

```bash
age -d -i /mnt/identity.txt -o disc01.squashfs /mnt/data/disc01.squashfs.age
```

Two things are worth being explicit about.

**It is not encryption.** Shipping the key alongside the ciphertext is, in
cryptographic terms, no protection at all, and that is the intent. What it buys
is that nothing else about the format changes: same age container, same par2
over the same ciphertext, same `SHA512SUMS`, same two readers. A public set is
an ordinary set that happens to carry its own key, not a second on-disc format —
so the manual recipe on the disc (`age -d -i /mnt/identity.txt …`) needs nothing
new. One honest limit: neither `brb` reader searches a disc root or its own
staging for a key on its own, so a `brb restore` of a public set wants
`AGE_IDENTITY` pointed at a copy of the disc's `identity.txt`; the on-disc README
says exactly that in its worked example.

**The key is always freshly generated.** `AGE_RECIPIENTS_FILE` is not consulted
and neither is `AGE_IDENTITY`, deliberately: publishing a key you already use
would retroactively expose every other archive encrypted to it. One flag must
never be able to disclose unrelated backups, so the published key belongs to
that one archive and to nothing else.

The key is written three times per disc — `identity.txt`, `MANIFEST.txt` and
`README.md` — because it is not in `sidecars.par2` (par2 will not reach above
its own working directory, and the key sits at the disc root where both readers
look for it). An age secret key is 74 bech32 characters with a checksum, so if
one copy rots another can be retyped and will be either accepted or rejected
outright, never silently wrong.

Each disc's README says plainly, at the top, that the archive is not
confidential. An ordinary set is completely unaffected: no `identity.txt`, no
key in its documents, and its README still says the key is not on the disc and
never will be.

---

## Security notes

- **Staging holds unencrypted images while a backup runs.** Each image is deleted
  as soon as it is encrypted, but at any moment one full disc's worth of your
  plaintext is sitting in `$STAGING`. Put staging on an encrypted volume, or wipe
  it afterwards. `brb` warns about this and asks for confirmation before starting.
- **`$STAGING/restore` holds decrypted images** — plaintext, mode 0700. `restore`
  deletes each one as soon as its contents are on disk, so by default nothing is
  left there but what a failed run dropped. `KEEP_IMAGES=1` (or `--keep-images`)
  keeps them all, and `list` and `mount` leave the image they decrypted behind
  by design. Remove them when you are done: `rm -rf $STAGING/restore`.
- **The recipients file contains public keys only** and is harmless. The identity
  file is the secret, and it is never written to a disc.
- Encrypting to multiple recipients is supported: append more `age1...` public
  keys to the recipients file and every image becomes decryptable by any of them.
  `brb init-key --rescue-key` is that mechanism applied to your own second key —
  see [Keys, and the rescue key](#keys-and-the-rescue-key).
- **An identity can be passphrase-protected** (`age -p`) and used exactly like a
  plain one on the restore side; `brb` unlocks it once per command. The
  passphrase is read from `/dev/tty` and nowhere else, so it is the one prompt
  `--yes` cannot answer: with no terminal, `brb` says so and stops rather than
  hanging, and an empty passphrase is reported as an empty passphrase rather
  than as a missing terminal. **The Go build refuses `--yes` outright** when the
  only identity it can find is encrypted — a run that promised to be unattended
  should not stop to ask — and tells you to run without it; `brb.sh` prompts
  anyway. An unattended `backup` is unaffected either way — it
  encrypts to public keys and reads no identity at all. The exception is
  `backup --verify-roundtrip`, which decrypts every image back to prove the set
  readable: that needs an unencrypted `AGE_IDENTITY` and refuses to start
  without one, rather than stopping for a passphrase hours into a run.
- Run `brb backup` as root if you want to capture files you cannot read as your
  own user, and to record real ownership. As a non-root user, unreadable files are
  skipped and ownership is recorded as yours.

---

## Limitations

Known and deliberate, but you should hear them before you rely on this:

- **Not incremental.** Every run is a full backup of the whole tree. A completed
  set cannot be updated in place; the next backup builds a new one.
- **Resuming is deliberate, not automatic.** An interrupted run continues with
  `brb backup --resume`, but a plain `backup` refuses to start on top of one
  rather than silently throwing the work away. The resumed run must agree with
  the interrupted one on `ARCHIVE_NAME` and `SOURCE_DIR`, and needs the state
  file in `$STAGING`; if staging has been cleared, the set is built again from
  scratch. A resume re-scans the source and skips what is already on a disc, so
  files added since the run started are picked up on later discs and a set can
  span two points in time — it warns when the tree's measured size has changed.
- **A single file larger than one disc cannot be stored.** `brb` detects this
  during `plan`/`backup` and stops rather than silently dropping it. Exclude it,
  use larger media, or split it yourself.
- **A restore overwrites its destination.** `unsquashfs -f` replaces existing
  files with the backup's versions, mode and mtime included, and `--yes` answers
  the confirmation that would otherwise have stopped it. A destination holding a
  symlink to a directory is refused outright, `--yes` or not. See
  [restore overwrites the destination](#restore-overwrites-the-destination).
- **A restore needs room for the extracted tree plus one decrypted image.** Each
  image is removed as soon as its contents are on disk, so only one exists at a
  time. `KEEP_IMAGES=1` (or `--keep-images` on the Go build) keeps them all for
  repeated restores, and then you do need room for the whole archive twice.
- **ISOs are ISO 9660 level 3 only**, not UDF. Level 3 multi-extent is what
  allows the >4 GiB images. Any Linux system reads these; some appliances that
  expect UDF on Blu-ray may not.
- **`COMPRESSION_LEVEL` only applies to `zstd` and `gzip`.** It is silently
  ignored for `xz`, `lz4` and `lzo`, which mksquashfs tunes through different
  flags. Those compressors run at their own defaults.

---

## Testing

Two suites, with no overlap. Both build real disc sets with the real tools —
mksquashfs, age, par2, xorriso — and restore them; neither mocks anything.

```bash
./go-e2e-test.sh
```

The writer. Backs up a multi-disc set, **kills it mid-set with `kill -9` on the
process group**, resumes it, and asserts the discs finished before the kill come
back byte-identical and the completed set restores byte-identical to its source.
Also covers ISO modes and that a finished run leaves no resume state behind.

```bash
./xcompat-test.sh
```

The format contract. Builds a set with the Go build and reads it with **both**
implementations, asserting they agree — the restored trees, the encrypted index,
the disc inventory, `list`, `--only`, `KEEP_IMAGES`. It also runs the recipe
printed on the disc itself, with neither implementation involved, and damages a
set on purpose: a rotted `.sha512` sidecar must not condemn an image par2 proves
is whole, while an image par2 *cannot* repair must still be refused rather than
decrypted.

Where the two genuinely differ, the check is written the way it ought to pass
and marked `XFAIL` with the divergence named. An `XFAIL` that starts passing is
reported as `XPASS` and **counted as a failure**, so a fixed divergence gets
promoted to a real assertion instead of sitting in the ledger forever. Nothing
known-broken is quietly omitted.

Plus the Go unit tests:

```bash
cd go && go test ./...
```

### Continuous integration

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs all of the above on
every push and pull request, weekly on a schedule, and on demand. Both suites and
`build-dist.sh` are invoked there with **no arguments and no environment**, which
is the only arrangement that catches a machine-specific path getting baked into a
default again.

The four shell scripts are linted with a pinned, checksummed **shellcheck
0.11.0** at its most inclusive severity, and all four are clean at it. The
version is pinned deliberately: Ubuntu ships 0.9.0, which does not report the
same set, and a lint gate that changes its mind when the runner image is
rebuilt is worse than no gate. The two places where a deliberate idiom trips a
check are disabled at the site, with the reason written next to them, rather
than switched off across the repo.

One step in it is load-bearing and worth knowing about: CI verifies every
external tool is present *before* running anything. The tool-dependent Go tests
call `t.Skip` when detection fails and the shell suites exit 77, which is right
on a laptop and dangerously quiet on a build runner — a runner missing `par2`
would otherwise report a green `go test ./...` in which the integration tests
never ran at all. A missing tool fails the run instead.

The release job rebuilds `brb-src.tar.gz` with `GOPROXY=off`, so the offline
rebuild the on-disc README promises is re-proven on every run rather than resting
on the last time somebody tried it by hand. It also checks the two cross-compiled
binaries are genuinely x86-64 and aarch64 and both static, and that `go.mod` and
`go.sum` are already tidy — `build-dist.sh` runs `go mod tidy` itself, so any
drift there means the committed module no longer describes what ships on a disc.

---

## License

MIT — see [LICENSE](LICENSE). The full notice is also embedded in the header of
`brb.sh` itself, so the copy of the script carried on every disc stays properly
licensed even when separated from this repository.

No warranty. Test your restores.
