# Backup disc 01 of 1 — `home-2026-08-07`

Created 2026-08-21T03:54:13-04:00 from `/home/you`.
Written by brb (Blu-ray Backup) 0.1.1.

**This disc is self-contained.** It holds a complete, independent SquashFS
filesystem image containing a subset of the backup. You do not need the other
discs to read this one. If another disc in the set is lost or destroyed, you
lose only the files that were stored on *that* disc — everything on this one
still restores normally.

Every disc carries the full directory structure of the original tree, so
mounting any single disc shows you the whole shape of the backup, with the
files that live on that disc present and the rest absent.

---

## What is on this disc

```
README.md                  this file
MANIFEST.txt               the whole set, and the tool versions used
SHA512SUMS                 hashes of every file here
brb.sh                     the tool as a bash script; it can also restore this
brb-linux-amd64            the tool as a static binary, 64-bit Intel/AMD
brb-linux-aarch64          the tool as a static binary, 64-bit ARM
brb-src.tar.gz             complete source for both, dependencies vendored
data/
  disc01.squashfs.age          the filesystem image, encrypted with age
  disc01.squashfs.age.sha512   hash of the encrypted image
  disc01.squashfs.sha512       hash of the image AFTER decryption
  disc01.squashfs.age.par2     par2 index
  disc01.squashfs.age.vol*.par2  10% recovery data
  index.tsv.gz.age                 encrypted map of which disc holds which file
  index.tsv.gz.age.sha512          hash of that map
  sidecars.par2                    par2 index for the small files above
  sidecars.vol*.par2               50% recovery data for them
```

The pipeline was:

```
mksquashfs  ->  one compressed image per disc   disc01.squashfs
age         ->  encrypt the image               disc01.squashfs.age
par2        ->  parity over the ciphertext      .par2 files
xorriso     ->  ISO 9660 (level 3), burned here
```

The disc is plain **ISO 9660 (level 3, multi-extent so images larger than
4 GiB work)** with Rock Ridge and Joliet name extensions. No UDF is written.
Level 3 is what lifts the 4 GiB per-file limit, which is the only reason these
multi-gigabyte images fit at all.

Parity is computed over the **encrypted** bytes, so it protects exactly what is
physically on the disc. Undo in reverse: repair, decrypt, mount.

The small files — the `.sha512` sidecars and the index — carry their own parity
in `sidecars.par2`, because one rotted byte in a 170-byte hash file is otherwise
enough to make a perfectly good image look damaged, and one flipped bit in the
index destroys the whole map of which disc holds what.

---

## What you need

| Tool | Debian/Ubuntu | Fedora | Arch |
|---|---|---|---|
| squashfs-tools | `squashfs-tools` | `squashfs-tools` | `squashfs-tools` |
| age | `age` | `age` | `age` |
| par2 | `par2` | `par2cmdline` | `par2cmdline` |
| ddrescue *(optional)* | `gddrescue` | `ddrescue` | `ddrescue` |

You do **not** need this program, and you do **not** need Python. The final step
of a restore is a kernel mount.

**You do need the age secret key.** One line beginning `AGE-SECRET-KEY-1...`.
It is not on this disc and never will be. `MANIFEST.txt` lists the public keys
this archive was encrypted to, so you can tell which of your keys is the one.

---

## Restoring, the short way

```sh
# 1. copy the image off the disc — the glob is deliberate: it brings the
#    .sha512 sidecar and the .par2 files along, which step 2 needs
cp /mnt/data/disc01.squashfs.age* .

# 2. check it, and repair it if the hash disagrees
sha512sum -c disc01.squashfs.age.sha512 \
  || par2 repair -- disc01.squashfs.age.par2

# 3. decrypt
age -d -i /path/to/identity.txt \
    -o disc01.squashfs disc01.squashfs.age

# 4a. mount it and browse — needs nothing but the kernel. Deliberately not
#     /mnt: this disc is mounted there, and mounting over it hides the data/
#     directory that step 1 reads from for every remaining disc.
mkdir -p image
sudo mount -o loop,ro disc01.squashfs image

# 4b. or extract it
unsquashfs -d /path/to/destination disc01.squashfs
```

Run `unsquashfs` as root if you want original ownership restored. If you are
**not** root, add `-user-xattrs`: these images carry extended attributes, and an
unprivileged `unsquashfs` fails on the `security.*` and `system.*` namespaces
and exits non-zero — after extracting every file correctly, which makes it look
like a failed restore when it was not.

Repeat for every disc, extracting all of them into the same destination
directory — the discs hold disjoint sets of files, so they merge cleanly. Add
`-f` to
`unsquashfs` when extracting the second and later discs into a directory that
already exists.

---

## Restoring with the tool on this disc

This disc carries the tool that wrote it. None of it is *required* — the section
above is the whole restore path, and it uses nothing but standard utilities.

```sh
uname -m          # x86_64 -> brb-linux-amd64,  aarch64 -> brb-linux-aarch64
```

Each binary here is static: no interpreter, no libc, no shared libraries. Copy
the one that matches your machine off the disc, `chmod +x` it, and run it.

```sh
cp /mnt/brb-linux-amd64 /tmp/brb && chmod +x /tmp/brb && /tmp/brb help
```

`brb.sh` is the reader half of the same tool, as a bash script: it can
ingest, restore, mount, list, index and verify a set — everything a restore
needs — but it deliberately cannot create or burn one, and says so if you ask.
Use a binary or the source for that. It needs
`age`, `par2` and `squashfs-tools` installed, where a static binary needs only
`unsquashfs` and `par2` for a full restore and nothing at all to decrypt and
mount. It is also the readable one: if you would rather know exactly what
happens to your bytes than trust a binary, read it.

`brb-src.tar.gz` is the complete source for everything above, every dependency
vendored inside it, so it rebuilds with no network access — the way out if
nothing else here runs on your machine:

```sh
tar xzf /mnt/brb-src.tar.gz && cd brb-*/go
go build -mod=vendor ./cmd/brb
```

```sh
# copy the tool off the disc first, so this works from any directory —
# including brb-*/go, where the tarball recipe above leaves you
cp /mnt/brb.sh /tmp/brb.sh && chmod +x /tmp/brb.sh

export STAGING=/var/tmp/restore
export AGE_IDENTITY=/path/to/identity.txt

/tmp/brb.sh ingest                          # prompts for each disc, any order
/tmp/brb.sh index thesis                    # which disc holds a given path?
/tmp/brb.sh restore /path/to/destination
/tmp/brb.sh restore /dest --disc 1               # just this one
/tmp/brb.sh mount 1 /mnt/browse                  # decrypt and mount
```

---

## If a disc will not read

`cp` stops at the first I/O error. `ddrescue` does not — it fills unreadable
regions with zeros and keeps going, which is exactly what par2 needs:

```sh
# 1. salvage the image, reading past the bad spots
ddrescue -d -r3 /mnt/data/disc01.squashfs.age \
                ./disc01.squashfs.age \
                ./disc01.mapfile

# 2. bring the sidecar and the parity files over — .age.* and not .age*, so
#    cp does not try the damaged image again and stop at the same error
cp /mnt/data/disc01.squashfs.age.* .

# 3. let par2 rebuild the zeroed regions
par2 repair -- disc01.squashfs.age.par2
```

Plain `par2 repair` cannot read past a hardware error on its own. Getting the
file off the disc with `ddrescue` first is what makes the parity usable — and
par2 can only use parity it can see, so the `.par2` files have to be sitting
beside the image, which is what step 2 is for. If `cp` fails on one of *those*
too, `ddrescue` it the same way; par2 tolerates damage in its own volumes.

Each image carries 10% recovery data, so roughly 10%
of it can be destroyed and still rebuilt. Beyond that, par2 reports how many
blocks it is short. If you burned a second copy of the set, copy the same image
from the other copy into the same directory — par2 can combine two partially
good copies into one good file.

If the image checks out but a `.sha512` sidecar or the index is what rotted,
repair those from the disc's own parity instead — copy the small files and
`sidecars.par2*` into one directory and run:

```sh
par2 repair -- sidecars.par2
```

---

## If a disc is gone entirely

Restore every other disc normally. You lose only the files that lived on the
missing one. To find out exactly what those were, decrypt the index from any
surviving disc:

```sh
age -d -i /path/to/identity.txt /mnt/data/index.tsv.gz.age | gunzip -c | awk -F'\t' '$1==1'
```

The index is a two-column list: disc number, then path. Exactly one row per
file, one line each. A backslash, tab or newline inside a path is written as
`\\`, `\t` and `\n` respectively, so a path can never span two rows. Paths are
slash-separated and relative to the `source` directory named in `MANIFEST.txt`,
never absolute. Only regular files are listed: directories, symbolic links and
device nodes are replicated onto every disc as the skeleton and never appear
here, so a path absent from the index is not necessarily absent from the backup.
Replace `1` above with the number of the disc you lost.

---

## Notes for whoever finds these later

- SquashFS is a read-only filesystem built into the mainline Linux kernel. Any
  Linux system can mount these images directly. That is the main reason this
  format was chosen over a bespoke archive format.
- age is a small, specified file-encryption format. The whole spec is short.
- par2 is the standard parity archive format, widely implemented.
- None of these needs a company to still exist.
- Blu-ray drives will become hard to find long before these discs decay. If you
  are reading this and cannot source a drive, that was the failure mode, not the
  media. That is truer still if this set was burned to M-DISC, whose recording
  layer is inorganic — there is no organic dye in it to fade, which is the thing
  that eventually kills an ordinary recordable disc left in a warm cupboard.
  `MANIFEST.txt` records the disc type but not the brand, so if you need to know
  what these actually are, read the label.
- `MANIFEST.txt` records exact tool versions. Newer versions read older images.

---

## Quick reference

```sh
sha512sum -c SHA512SUMS                        # is this disc still good?
par2 verify -- FILE.age.par2                   # is this image recoverable?
par2 repair -- FILE.age.par2                   # fix it
par2 repair -- sidecars.par2                   # fix a rotted .sha512 or index
age -d -i identity.txt -o OUT.squashfs IN.age  # decrypt
mount -o loop,ro OUT.squashfs /mnt             # browse
unsquashfs -d /dest OUT.squashfs               # extract (add -user-xattrs
                                               #   if you are not root)
unsquashfs -ll OUT.squashfs                    # list contents
```
