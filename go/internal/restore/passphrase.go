package restore

import "errors"

// The two ways asking for a passphrase fails that a caller has to tell apart.
// They used to be indistinguishable — every error from readPassphrase was
// reported as "there is no terminal to ask on ... this cannot be automated",
// which is actively misleading when the operator is sitting at a terminal and
// simply pressed Enter. Both still fail closed; only the sentence differs.
var (
	// ErrNoTerminal means /dev/tty could not be opened or is not a terminal,
	// so there is nobody to ask. age reads passphrases from the terminal and
	// never from a pipe, so this genuinely cannot be automated.
	ErrNoTerminal = errors.New("no terminal to ask on")
	// ErrEmptyPassphrase means the prompt was answered with nothing. A
	// terminal was there; the passphrase was not. brb.sh says the same thing:
	// a passphrase cannot be empty.
	ErrEmptyPassphrase = errors.New("the passphrase cannot be empty")
)

// ReadPassphrase asks for a passphrase on the controlling terminal, with echo
// off, and returns it. It is the restore side's prompt, exported so that
// 'init-key --rescue-key' asks in exactly the same place and the same way as
// the restore that will later have to unlock what it wrote: one prompt
// implementation, one set of failure modes.
//
// Errors wrap [ErrNoTerminal] or [ErrEmptyPassphrase] where those apply.
func ReadPassphrase(prompt string) (string, error) { return readPassphrase(prompt) }
