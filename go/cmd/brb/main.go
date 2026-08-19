// Command brb makes independent, mountable, encrypted Blu-ray backup discs.
//
// It bin-packs a directory tree into disc-sized groups, builds one SquashFS
// image per disc, encrypts each image with age, adds par2 recovery data over
// the ciphertext and writes an ISO ready to burn. Every disc restores on its
// own: losing one loses only the files that were on it.
//
// Run "brb help" for the commands and "brb doctor" for a check of the local
// dependencies and configuration.
//
// This file is deliberately thin. Everything it does is turn signals into a
// cancelled context and an exit status into an os.Exit, so that the whole
// program remains testable through cli.Main.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/jzbz/brb/internal/cli"
)

func main() {
	ctx, stop := signalContext()
	defer stop()

	status := cli.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(status)
}

// signalContext returns a context that the first Ctrl-C (or SIGTERM) cancels,
// arranged so that a second one kills brb outright.
//
// Every long operation in the program watches the context, so the first
// signal is an orderly stop: the current step is abandoned, staging is left
// resumable, and Main says so. The second is for when that is not prompt
// enough — a child process ignoring SIGINT, a write that will not return —
// and it only has its default effect once nothing is catching it. NotifyContext
// keeps catching until its stop function is called, so on its own it swallows
// every signal after the first, and a repeat Ctrl-C did nothing at all. The
// goroutine here calls stop the moment the context is cancelled: the context
// stays cancelled and cli.Main still winds down as before, but the signal's
// default disposition is back for the next one, which ends the process the way
// the operator expects. The stop returned to the caller is safe to call again
// afterwards; it is idempotent.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}
