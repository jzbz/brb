package backup

import (
	"strings"
	"testing"

	"github.com/jzbz/brb/internal/config"
)

// testConfigWithPrunes is a configuration carrying just the prune list, which
// is all manifestPrunes reads.
func testConfigWithPrunes(prunes ...string) *config.Config {
	c := config.Default()
	c.PruneDirs = prunes
	return c
}

// A path the scan could not open is not on any disc. That is reported once,
// while the run is happening, to a terminal that is long gone by the time
// anyone reads the discs — and a restore missing a file looks exactly like a
// backup that never had it. So the manifest carries it too, the same way it
// already carries the mount points the scan refused to cross.
func TestManifestPrunesNamesUnreadablePaths(t *testing.T) {
	t.Run("named, and marked as unreadable rather than pruned", func(t *testing.T) {
		r := &runner{}
		r.cfg = testConfigWithPrunes(".cache")
		r.unreadable = []string{"secret/keys", "vault/db"}
		r.unreadableCount = 2

		got := strings.Join(r.manifestPrunes(), "\n")
		for _, want := range []string{
			".cache",
			"secret/keys  (could not be read: NOT on any disc)",
			"vault/db  (could not be read: NOT on any disc)",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("manifestPrunes missing %q, got:\n%s", want, got)
			}
		}
	})

	// An unreadable directory of a million files must not become a million
	// lines. The count is the part that cannot be truncated away: it is what
	// tells a restorer the set is short, and by how much.
	t.Run("over the cap it names some and counts the rest", func(t *testing.T) {
		r := &runner{}
		r.cfg = testConfigWithPrunes()
		for i := 0; i < maxNamedUnreadable; i++ {
			r.unreadable = append(r.unreadable, "p")
		}
		r.unreadableCount = maxNamedUnreadable + 500

		got := strings.Join(r.manifestPrunes(), "\n")
		if !strings.Contains(got, "... and 500 more path(s) that could not be read") {
			t.Errorf("the overflow count is missing, got:\n%s", got)
		}
	})

	// Nothing unreadable and nothing skipped must leave the list exactly as
	// configured, or every ordinary set grows a section it does not need.
	t.Run("a clean scan changes nothing", func(t *testing.T) {
		r := &runner{}
		r.cfg = testConfigWithPrunes(".cache", "snap")
		got := r.manifestPrunes()
		if len(got) != 2 || got[0] != ".cache" || got[1] != "snap" {
			t.Errorf("manifestPrunes = %q, want the configured prunes untouched", got)
		}
	})
}
