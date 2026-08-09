package agecrypt

import (
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"sync"
)

// The buffer pool behind one asyncHash. Four 1 MiB buffers is enough that the
// producer never waits on a SHA-512 that runs at over a gigabyte a second, and
// small enough that a run with two hashers in flight costs 8 MB.
const (
	asyncHashBufs = 4
	asyncHashSize = 1 << 20
)

// asyncHash is a hash.Hash driven from its own goroutine.
//
// io.MultiWriter is strictly sequential, so an Encrypt that fed the plaintext
// digest, the AEAD and the ciphertext digest through one paid all three on a
// single core while every other core sat idle: measured on this machine, the
// fused pipeline ran at 490-510 MB/s, which is exactly 1/(1/sha + 1/sha + 1/age)
// — no overlap at all. Moving each digest onto its own goroutine takes the
// pipeline back to the speed of a single SHA-512 pass, roughly 2.2x, because
// the digests then run beside the encryption instead of after it.
//
// Bytes are copied into recycled buffers rather than handed over directly: the
// caller's slice is reused by copyCtx as soon as Write returns, and age's
// STREAM writer likewise reuses its chunk buffer.
type asyncHash struct {
	h    hash.Hash
	work chan []byte   // buffers holding data to hash
	free chan []byte   // buffers ready to be filled again
	dead chan struct{} // closed when the goroutine has stopped

	written  int64 // bytes accepted by Write
	consumed int64 // bytes the goroutine actually hashed
	err      error // set by the goroutine before it closes dead

	stop sync.Once
}

// newAsyncHash starts a SHA-512 running in its own goroutine. Every hasher must
// be finished with Sum or released with release, or its goroutine leaks.
func newAsyncHash() *asyncHash {
	a := &asyncHash{
		h:    sha512.New(),
		work: make(chan []byte, asyncHashBufs),
		free: make(chan []byte, asyncHashBufs),
		dead: make(chan struct{}),
	}
	for i := 0; i < asyncHashBufs; i++ {
		a.free <- make([]byte, asyncHashSize)
	}
	go a.run()
	return a
}

// run hashes queued buffers until the work channel is closed.
//
// The recover is not decoration: if this goroutine died, Sum would otherwise
// return the digest of however many bytes happened to be hashed before it did,
// which is a wrong answer presented as a right one. A dead hasher instead
// unblocks Write (via dead) and makes Sum report the failure.
func (a *asyncHash) run() {
	defer close(a.dead)
	defer func() {
		if p := recover(); p != nil {
			a.err = fmt.Errorf("agecrypt: hashing goroutine panicked: %v", p)
		}
	}()
	for b := range a.work {
		a.h.Write(b)
		a.consumed += int64(len(b))
		a.free <- b[:cap(b)]
	}
}

// Write queues p for hashing, copying it into a recycled buffer first. It
// implements io.Writer, so an asyncHash drops into an io.MultiWriter where a
// hash.Hash used to be.
func (a *asyncHash) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		var buf []byte
		select {
		case buf = <-a.free:
		case <-a.dead:
			return total, a.hasherErr()
		}
		n := copy(buf, p)
		select {
		case a.work <- buf[:n]:
		case <-a.dead:
			return total, a.hasherErr()
		}
		p = p[n:]
		total += n
	}
	a.written += int64(total)
	return total, nil
}

// hasherErr describes a hasher that stopped early.
func (a *asyncHash) hasherErr() error {
	if a.err != nil {
		return a.err
	}
	return fmt.Errorf("agecrypt: hashing goroutine stopped early")
}

// Sum finishes the hasher and returns the digest as lowercase hex.
//
// It must not be called until every byte has been written — for a ciphertext
// digest that means after the age writer's Close, which flushes the final
// STREAM chunk. The byte counts are compared rather than trusted: a digest over
// fewer bytes than were handed in would be recorded as the truth about a disc.
func (a *asyncHash) Sum() (string, error) {
	a.release()
	<-a.dead
	if a.err != nil {
		return "", a.err
	}
	if a.consumed != a.written {
		return "", fmt.Errorf("agecrypt: hashed %d byte(s) of %d written", a.consumed, a.written)
	}
	return hex.EncodeToString(a.h.Sum(nil)), nil
}

// release shuts the goroutine down without waiting for the digest, for the
// error paths where the answer is no longer wanted. It is safe to call more
// than once and safe to call after Sum.
func (a *asyncHash) release() { a.stop.Do(func() { close(a.work) }) }
