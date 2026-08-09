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
	// A second signal is left to the default handler on purpose: if the first
	// Ctrl-C does not stop things promptly enough — a child process ignoring
	// SIGINT, a write that will not return — the next one kills brb outright.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	status := cli.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(status)
}
