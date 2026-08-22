// Package fsx holds the filesystem operations brb performs outside any one
// stage of the pipeline, in one copy each, because a second implementation of
// any of them is a second chance to get it wrong.
//
// Two are measurements: how many bytes a directory tree holds, and how many
// are still free on the volume under it. Both sides of a disc-sizing decision
// use them — the backup checks a disc directory against the media, the ISO
// builder checks the same tree against the free space it needs to copy it.
//
// Most of the rest are the staging area's safety rules — [SecureStaging],
// [SecureDir], [CreateFresh], [OpenAppend], with [FileOwner] under them. The
// writer and the reader share one staging tree layout and one threat: its
// default lives under a world-writable /var/tmp, and it holds plaintext. These
// rules used to be written once per caller, and each rewrite lost a piece of
// them, which is why they are here and not there.
//
// One more is exclusion rather than a rule: [LockStaging] holds an flock on
// the staging root for as long as a run lasts, so two brb processes cannot
// build into one tree. It is the newest thing here and the least visible in
// its absence — see [LockStaging] for why a set built by two runs at once
// verifies clean and does not restore.
//
// This package is Unix-only, as internal/scan and internal/backup are: the
// rules above are made of flock(2), O_NOFOLLOW, fchmod(2) and statfs(2), and
// there is no useful way to fake any of them. There are no non-Unix stubs, on
// purpose — a build in which the staging lock quietly does nothing is worse
// than no build at all.
package fsx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// DirBytes sums the apparent size of every regular file beneath dir — the
// bytes the files contain, not the blocks they occupy. Directories, symlinks
// and device nodes count for nothing, matching what ends up in an ISO of the
// tree, which is what every caller is really asking about.
//
// ctx is checked once per entry, so a walk of a large tree stops promptly.
func DirBytes(ctx context.Context, dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		total += fi.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("measuring %s: %w", dir, err)
	}
	return total, nil
}

// SyncDir flushes a directory entry so a completed rename survives a crash.
//
// Failures are deliberately ignored: not every filesystem supports fsync on a
// directory, and at every call site the data itself is already durable — the
// rename is what is being made visible, not the bytes. Callers that cared about
// the error could not do anything useful with it.
//
// It lives here rather than in each caller because it was written twice,
// identically, in two packages that both already depend on this one.
func SyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// CopyBufSize is the chunk size of [CopyCtx].
//
// Large enough that the per-chunk context check and syscall overhead disappear
// against the copy itself, small enough that a cancelled multi-gigabyte stream
// stops within a chunk rather than within a file.
const CopyBufSize = 1 << 20

// CopyCtx copies src into dst in fixed-size chunks, checking ctx between chunks
// so a multi-gigabyte stream aborts promptly on cancellation. It returns the
// number of bytes written.
//
// It is a hand-written loop rather than io.Copy because io.Copy will take a
// ReadFrom or WriteTo shortcut when either side offers one, and those copy the
// whole stream inside a single call that never looks at the context. A backup
// or a restore that ignored ^C for forty minutes is the bug this avoids.
//
// The short-write check is deliberate belt-and-braces. io.Writer's contract
// says a Write returning fewer bytes than it was given must also return an
// error, so a conforming writer cannot reach it — but a copy that silently
// stopped early would write a truncated image or a truncated restored file, and
// that is a failure this program must never express as success. Three separate
// versions of this loop existed before it lived here and one of them had lost
// the check, which is the argument for having one.
func CopyCtx(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, CopyBufSize)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		nr, rerr := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			total += int64(nw)
			if werr != nil {
				return total, werr
			}
			if nw != nr {
				return total, io.ErrShortWrite
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return total, nil
			}
			return total, rerr
		}
	}
}
