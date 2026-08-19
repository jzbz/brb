package restore

import (
	"fmt"

	"github.com/jzbz/brb/internal/fsx"
)

// secureStaging makes the staging directory, and the subdirectories this
// command is about to write into, safe to hold plaintext.
//
// The rules, and the reasoning behind each of them, live in [fsx.SecureDir]:
// the writer's preflight secures its own staging tree with the same three
// checks, and the two used to disagree about which of them mattered. All this
// adds is the "restore: " prefix the operator sees on everything this package
// refuses.
func (o Options) secureStaging(subs ...string) error {
	if err := fsx.SecureStaging(o.Cfg.Staging, subs...); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	return nil
}
