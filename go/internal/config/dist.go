package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// SystemDistDirs are the packaged locations searched for the disc payload when
// DIST_DIR is unset and there is no dist directory beside the running program.
// The order is brb.sh's: a locally installed payload wins over a distribution
// one.
//
// It is a variable only so that a test can point the search at a directory it
// controls; nothing in brb assigns to it.
var SystemDistDirs = []string{"/usr/local/share/brb", "/usr/share/brb"}

// ResolveDistDir returns the directory holding the disc payload — the copies of
// brb that go onto every disc — or "" when there is none.
//
// The search is brb.sh's resolve_dist_dir: DIST_DIR (BRB_DIST_DIR in the
// environment) if it is set, then a "dist" directory beside the running
// program, then SystemDistDirs. Only directories that exist are returned.
//
// A DIST_DIR that names nothing is reported as an error rather than falling
// quietly through to the search: an operator who set it meant it, and burning
// twenty discs without the payload because of a typo is not something to
// discover afterwards. It is still only an error to report — a missing payload
// never fails a backup.
func (c *Config) ResolveDistDir() (string, error) {
	return resolveDistDir(c.DistDir, executableDir(), SystemDistDirs)
}

// resolveDistDir is ResolveDistDir with its two environmental inputs supplied,
// so the precedence can be tested without a real executable or a real /usr.
// An empty exeDir simply drops that candidate.
func resolveDistDir(explicit, exeDir string, system []string) (string, error) {
	if explicit != "" {
		switch err := checkDistDir(explicit); {
		case err == nil:
			return explicit, nil
		default:
			return "", fmt.Errorf("DIST_DIR (BRB_DIST_DIR) is set to %s, which %w; "+
				"build one with ./build-dist.sh, or unset it to search the usual locations",
				explicit, err)
		}
	}

	candidates := make([]string, 0, len(system)+1)
	if exeDir != "" {
		candidates = append(candidates, filepath.Join(exeDir, "dist"))
	}
	candidates = append(candidates, system...)
	for _, d := range candidates {
		if checkDistDir(d) == nil {
			return d, nil
		}
	}
	return "", nil
}

// checkDistDir reports why a path is not usable as a payload directory. The
// message completes the sentence "..., which %w".
func checkDistDir(path string) error {
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("does not exist")
	case err != nil:
		return fmt.Errorf("cannot be read: %w", err)
	case !fi.IsDir():
		return errors.New("is not a directory")
	}
	return nil
}

// executableDir is the directory holding the running program, with symlinks
// resolved the way brb.sh's `readlink -f -- "$0"` does. It returns "" when the
// path cannot be determined, which drops one candidate from the search rather
// than failing it.
func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}
