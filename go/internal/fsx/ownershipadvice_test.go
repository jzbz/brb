package fsx

import (
	"strings"
	"testing"
)

// The chown in the ownership advice is pasted as root, so the staging path goes
// into it as one shell word. Bare, a STAGING of "/srv/brb staging" told root to
// run "chown -R root /srv/brb staging", which hands an existing /srv/brb — and
// a ./staging wherever it was pasted — to root down to the last file, and
// leaves the staging tree as it was. Both of the advice's chown spellings, the
// sudo one and the plain one, are checked, and the default path still reads
// bare. The advice is only ever given to a root run, so the test stands in for
// root through adviceEuid, which lets it run unprivileged.
func TestOwnershipAdviceQuotesTheStagingPath(t *testing.T) {
	saved := adviceEuid
	adviceEuid = func() int { return 0 }
	t.Cleanup(func() { adviceEuid = saved })
	for _, sudo := range []string{"", "1000"} {
		t.Setenv("SUDO_UID", sudo)
		for dir, want := range map[string]string{
			"/srv/brb staging": "chown -R root '/srv/brb staging' ",
			"/var/tmp/brb":     "chown -R root /var/tmp/brb ",
		} {
			got := ownershipAdvice(dir, 1000)
			if !strings.Contains(got, want) {
				t.Errorf("SUDO_UID=%q, STAGING=%q: advice = %q, want it to contain %q", sudo, dir, got, want)
			}
			if sudo != "" && !strings.Contains(got, "the account you ran sudo from") {
				t.Errorf("SUDO_UID=%q: advice = %q, want the sudo spelling", sudo, got)
			}
		}
	}
}
