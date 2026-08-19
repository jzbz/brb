package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jzbz/brb/internal/disc"
)

func TestDefaultMatchesBrbSh(t *testing.T) {
	setHome(t, "/home/tester")
	c := Default()

	if c.SourceDir != "/home/tester" {
		t.Errorf("SourceDir = %q, want $HOME", c.SourceDir)
	}
	if c.ArchiveName != "" {
		t.Errorf("ArchiveName = %q, want it left for ResolveDefaults", c.ArchiveName)
	}
	if c.Staging != "/var/tmp/brb" {
		t.Errorf("Staging = %q", c.Staging)
	}
	if want := "/home/tester/.config/brb/recipients.txt"; c.AgeRecipientsFile != want {
		t.Errorf("AgeRecipientsFile = %q, want %q", c.AgeRecipientsFile, want)
	}
	if c.AgeIdentity != "" {
		t.Errorf("AgeIdentity = %q, want empty", c.AgeIdentity)
	}
	if c.DiscType != disc.BD25 {
		t.Errorf("DiscType = %q, want bd25", c.DiscType)
	}
	if c.DiscCapacityBytes != 0 {
		t.Errorf("DiscCapacityBytes = %d, want 0", c.DiscCapacityBytes)
	}
	if c.Compression != "zstd" || c.CompressionLevel != 19 || c.BlockSize != "1M" {
		t.Errorf("compression defaults = %q/%d/%q, want zstd/19/1M",
			c.Compression, c.CompressionLevel, c.BlockSize)
	}
	if c.PackRatio != 1.00 {
		t.Errorf("PackRatio = %v, want 1.00", c.PackRatio)
	}
	// brb.sh: PACK_RATIO_ADAPT=1, PACK_RATIO_WINDOW=3, PACK_RATIO_MARGIN=1.05.
	// Adaptation is on by default in both, so a set built by either implementation
	// comes out on the same number of discs.
	if !c.PackRatioAdapt || c.PackRatioWindow != 3 || c.PackRatioMargin != 1.05 {
		t.Errorf("pack ratio adaptation defaults = %v/%d/%v, want true/3/1.05",
			c.PackRatioAdapt, c.PackRatioWindow, c.PackRatioMargin)
	}
	// brb.sh: PAR2_REDUNDANCY=10, PAR2_BLOCKS= (empty), PAR2_MEMORY_MB= (empty).
	// Both blanks are deliberate. An empty block count means "size the blocks
	// from the image", which is what keeps a 22 GB image at ~1 MiB blocks
	// instead of 7.5 MiB ones; an empty memory cap leaves par2 its own default
	// of half of RAM, where a low cap forces extra full passes over the image.
	if c.Par2Redundancy != 10 || c.Par2Blocks != 0 || c.Par2MemoryMB != 0 {
		t.Errorf("par2 defaults = %d/%d/%d, want 10/0/0",
			c.Par2Redundancy, c.Par2Blocks, c.Par2MemoryMB)
	}
	if c.Burner != "/dev/sr0" || c.BurnSpeed != 4 {
		t.Errorf("burner defaults = %q/%d, want /dev/sr0 and 4", c.Burner, c.BurnSpeed)
	}
	if c.LabelPrefix != "BACKUP" {
		t.Errorf("LabelPrefix = %q, want BACKUP", c.LabelPrefix)
	}
	if c.MaxShrinkAttempts != 4 {
		t.Errorf("MaxShrinkAttempts = %d, want 4", c.MaxShrinkAttempts)
	}
	if c.ReserveBytes != 104857600 {
		t.Errorf("ReserveBytes = %d, want 104857600", c.ReserveBytes)
	}
	if c.Jobs != 0 {
		t.Errorf("Jobs = %d, want 0", c.Jobs)
	}

	// The two arrays are the load-bearing part: getting one of these wrong
	// silently changes what is backed up.
	wantPrune := []string{
		".cache",
		".local/share/Trash",
		".local/share/Steam",
		".thumbnails",
		".var/app",
		"snap",
		".npm/_cacache",
		".cargo/registry",
		".rustup/toolchains",
		".gradle/caches",
		".m2/repository",
		"go/pkg/mod",
		".local/share/containers",
		".local/share/docker",
		".vagrant.d/boxes",
	}
	if !reflect.DeepEqual(c.PruneDirs, wantPrune) {
		t.Errorf("PruneDirs =\n%q\nwant\n%q", c.PruneDirs, wantPrune)
	}
	wantExclude := []string{"*.pyc", "*.pyo", "core.[0-9]*", ".DS_Store"}
	if !reflect.DeepEqual(c.ExcludeMasks, wantExclude) {
		t.Errorf("ExcludeMasks = %q, want %q", c.ExcludeMasks, wantExclude)
	}
}

func TestDefaultReturnsIndependentSlices(t *testing.T) {
	setHome(t, "/home/tester")
	a, b := Default(), Default()
	a.PruneDirs[0] = "clobbered"
	a.ExcludeMasks[0] = "clobbered"
	if b.PruneDirs[0] == "clobbered" || b.ExcludeMasks[0] == "clobbered" {
		t.Fatal("Default shares its slices between calls")
	}
	if DefaultPruneDirs()[0] == "clobbered" {
		t.Fatal("Default aliases the package-level defaults")
	}
}

func TestDefaultConfigPath(t *testing.T) {
	setHome(t, "/home/tester")
	if got, want := DefaultConfigPath(), "/home/tester/.config/brb/config"; got != want {
		t.Errorf("DefaultConfigPath() = %q, want %q", got, want)
	}
	t.Setenv("BRB_CONFIG", "/etc/brb.conf")
	if got := DefaultConfigPath(); got != "/etc/brb.conf" {
		t.Errorf("DefaultConfigPath() with BRB_CONFIG = %q", got)
	}
}

func TestDirs(t *testing.T) {
	c := &Config{Staging: "/var/tmp/brb"}
	want := Dirs{
		Work:    "/var/tmp/brb/work",
		Img:     "/var/tmp/brb/img",
		Enc:     "/var/tmp/brb/enc",
		Discs:   "/var/tmp/brb/discs",
		ISO:     "/var/tmp/brb/iso",
		Restore: "/var/tmp/brb/restore",
	}
	if got := c.Dirs(); got != want {
		t.Errorf("Dirs() = %+v, want %+v", got, want)
	}
}

func TestCapacityAndBudget(t *testing.T) {
	setHome(t, "/home/tester")
	c := Default()
	if got := c.Capacity(); got != 25025314816 {
		t.Errorf("Capacity() = %d, want the bd25 capacity", got)
	}
	c.DiscCapacityBytes = 12345678
	if got := c.Capacity(); got != 12345678 {
		t.Errorf("Capacity() with an override = %d", got)
	}

	c = Default()
	b, err := c.Budget()
	if err != nil {
		t.Fatalf("Budget: %v", err)
	}
	// The same integer arithmetic brb.sh performs.
	usable := int64(25025314816) * 98 / 100
	image := (usable - 104857600) * 100 / (100 + 10 + 1)
	if b.Usable != usable || b.Image != image {
		t.Errorf("Budget = %+v, want usable %d image %d", b, usable, image)
	}

	c.ReserveBytes = 1 << 60
	if _, err := c.Budget(); err == nil {
		t.Error("Budget with an absurd reserve succeeded, want an error")
	}
}

func TestArchiveNameFor(t *testing.T) {
	when := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct{ in, want string }{
		{"/home/jz", "jz-2026-08-06"},
		{"/home/jz/", "jz-2026-08-06"},
		{"/srv/photos", "photos-2026-08-06"},
		{"/", "backup-2026-08-06"},
		{"", "backup-2026-08-06"},
	}
	for _, tc := range tests {
		if got := ArchiveNameFor(tc.in, when); got != tc.want {
			t.Errorf("ArchiveNameFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveDefaultsKeepsAnExplicitName(t *testing.T) {
	c := &Config{SourceDir: "/srv/photos", ArchiveName: "chosen"}
	c.ResolveDefaults()
	if c.ArchiveName != "chosen" {
		t.Errorf("ArchiveName = %q, want it left alone", c.ArchiveName)
	}
	c.ArchiveName = ""
	c.ResolveDefaults()
	if !strings.HasPrefix(c.ArchiveName, "photos-") {
		t.Errorf("ArchiveName = %q, want photos-<date>", c.ArchiveName)
	}
}

func TestApplyAllKeys(t *testing.T) {
	setHome(t, "/home/tester")
	vals := map[string]Value{
		"SOURCE_DIR":          {Scalar: "/srv/photos", Line: 1},
		"ARCHIVE_NAME":        {Scalar: "photos-2026", Line: 2},
		"STAGING":             {Scalar: "/scratch", Line: 3},
		"AGE_RECIPIENTS_FILE": {Scalar: "/keys/recipients.txt", Line: 4},
		"AGE_IDENTITY":        {Scalar: "/keys/identity.txt", Line: 5},
		"DISC_TYPE":           {Scalar: "BDXL128", Line: 6},
		"DISC_CAPACITY_BYTES": {Scalar: "999", Line: 7},
		"COMPRESSION":         {Scalar: "XZ", Line: 8},
		"COMPRESSION_LEVEL":   {Scalar: "9", Line: 9},
		"BLOCK_SIZE":          {Scalar: "128K", Line: 10},
		"PACK_RATIO":          {Scalar: "0.65", Line: 11},
		"PACK_RATIO_ADAPT":    {Scalar: "0", Line: 26},
		"PUBLIC_ARCHIVE":      {Scalar: "0", Line: 27},
		"PACK_RATIO_WINDOW":   {Scalar: "5", Line: 27},
		"PACK_RATIO_MARGIN":   {Scalar: "1.20", Line: 28},
		"PAR2_REDUNDANCY":     {Scalar: "5", Line: 12},
		"PAR2_BLOCKS":         {Scalar: "2000", Line: 13},
		"PAR2_MEMORY_MB":      {Scalar: "512", Line: 14},
		"BURNER":              {Scalar: "/dev/sr1", Line: 15},
		"BURN_SPEED":          {Scalar: "2", Line: 16},
		"LABEL_PREFIX":        {Scalar: "PHOTOS", Line: 17},
		"MAX_SHRINK_ATTEMPTS": {Scalar: "7", Line: 18},
		"RESERVE_BYTES":       {Scalar: "1048576", Line: 19},
		"JOBS":                {Scalar: "3", Line: 20},
		"DIST_DIR":            {Scalar: "/srv/brb-dist", Line: 21},
		"ISO_MODE":            {Scalar: "EAGER", Line: 22},
		"KEEP_ISOS":           {Scalar: "1", Line: 23},
		"PRUNE_DIRS":          {Array: []string{"a"}, IsArray: true, Line: 24},
		"EXCLUDE_MASKS":       {Array: []string{"*.tmp"}, IsArray: true, Line: 25},
	}
	if len(vals) != len(Keys()) {
		t.Fatalf("this test covers %d keys but Keys() lists %d", len(vals), len(Keys()))
	}
	for _, k := range Keys() {
		if _, ok := vals[k]; !ok {
			t.Fatalf("key %s from Keys() is not covered by this test", k)
		}
	}

	c := Default()
	if err := c.Apply(vals); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := &Config{
		SourceDir:         "/srv/photos",
		ArchiveName:       "photos-2026",
		Staging:           "/scratch",
		AgeRecipientsFile: "/keys/recipients.txt",
		AgeIdentity:       "/keys/identity.txt",
		DiscType:          disc.BDXL128,
		DiscCapacityBytes: 999,
		Compression:       "xz",
		CompressionLevel:  9,
		BlockSize:         "128K",
		PackRatio:         0.65,
		PackRatioAdapt:    false,
		PackRatioWindow:   5,
		PackRatioMargin:   1.20,
		Par2Redundancy:    5,
		Par2Blocks:        2000,
		Par2MemoryMB:      512,
		Burner:            "/dev/sr1",
		BurnSpeed:         2,
		LabelPrefix:       "PHOTOS",
		MaxShrinkAttempts: 7,
		ReserveBytes:      1048576,
		ISOMode:           ISOEager,
		KeepISOs:          true,
		PruneDirs:         []string{"a"},
		ExcludeMasks:      []string{"*.tmp"},
		Jobs:              3,
		DistDir:           "/srv/brb-dist",
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("Apply produced\n%+v\nwant\n%+v", c, want)
	}
}

func TestApplyErrors(t *testing.T) {
	setHome(t, "/home/tester")
	tests := []struct {
		name string
		vals map[string]Value
		want string
	}{
		{"unknown key", map[string]Value{"NOPE": {Scalar: "x", Line: 3}}, `unknown configuration key "NOPE"`},
		{"unknown key names its line", map[string]Value{"NOPE": {Scalar: "x", Line: 3}}, "line 3"},
		{"bad integer", map[string]Value{"BURN_SPEED": {Scalar: "fast", Line: 2}}, "invalid integer"},
		{"bad float", map[string]Value{"PACK_RATIO": {Scalar: "half", Line: 2}}, "invalid number"},
		{"bad disc type", map[string]Value{"DISC_TYPE": {Scalar: "dvd", Line: 2}}, "unknown disc type"},
		{"bad iso mode", map[string]Value{"ISO_MODE": {Scalar: "nonsense", Line: 2}},
			`unknown ISO_MODE "nonsense" (expected ondemand or eager)`},
		{"iso mode names its line", map[string]Value{"ISO_MODE": {Scalar: "nonsense", Line: 2}}, "line 2"},
		{"keep isos is not a word", map[string]Value{"KEEP_ISOS": {Scalar: "yes", Line: 2}},
			`KEEP_ISOS: expected 0 or 1, got "yes"`},
		{"array for a scalar", map[string]Value{"STAGING": {Array: []string{"a"}, IsArray: true, Line: 2}},
			"does not take an array"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			err := c.Apply(tc.vals)
			if err == nil {
				t.Fatalf("Apply(%v) succeeded, want an error", tc.vals)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

// TestEmptyScalarLeavesTheDefault pins the ${VAR:-default} reading of KEY=:
// the sample config in `brb help` and the README writes DISC_CAPACITY_BYTES=
// and DIST_DIR= to mean "not overridden", and a parser that turned that into
// "invalid integer" rejected its own documentation. Every scalar key gets the
// same treatment, an unknown key is still refused, and the array keys keep
// their "empty means none" meaning.
func TestEmptyScalarLeavesTheDefault(t *testing.T) {
	setHome(t, "/home/tester")
	var b strings.Builder
	for _, k := range Keys() {
		if isArrayKey(k) {
			continue
		}
		// Both spellings an operator reaches for, alternating.
		if len(k)%2 == 0 {
			b.WriteString(k + "=\n")
		} else {
			b.WriteString(k + "=\"\"   # left blank on purpose\n")
		}
	}
	vals, err := Parse(b.String(), "test")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c := Default()
	if err := c.Apply(vals); err != nil {
		t.Fatalf("Apply of every scalar key left empty: %v\n%s", err, b.String())
	}
	if !reflect.DeepEqual(c, Default()) {
		t.Errorf("empty values changed the configuration:\n got %+v\nwant %+v", c, Default())
	}

	c = Default()
	err = c.Apply(map[string]Value{"KEEP_IMAGES": {Scalar: "", Line: 4}})
	if err == nil || !strings.Contains(err.Error(), `unknown configuration key "KEEP_IMAGES"`) {
		t.Errorf("an empty unknown key was not refused: %v", err)
	}

	vals, err = Parse("PRUNE_DIRS=\nEXCLUDE_MASKS=()\n", "test")
	if err != nil {
		t.Fatal(err)
	}
	c = Default()
	if err := c.Apply(vals); err != nil {
		t.Fatal(err)
	}
	if len(c.PruneDirs) != 0 || len(c.ExcludeMasks) != 0 {
		t.Errorf("empty array settings did not switch the defaults off: prune %q, masks %q",
			c.PruneDirs, c.ExcludeMasks)
	}
}

func TestArrayKeysReplaceRatherThanAppend(t *testing.T) {
	setHome(t, "/home/tester")

	t.Run("from a file", func(t *testing.T) {
		vals, err := Parse("PRUNE_DIRS=( only-this )\nEXCLUDE_MASKS=( *.iso )\n", "test")
		if err != nil {
			t.Fatal(err)
		}
		c := Default()
		if len(c.PruneDirs) < 2 {
			t.Fatal("the defaults should be non-trivial for this test to mean anything")
		}
		if err := c.Apply(vals); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.PruneDirs, []string{"only-this"}) {
			t.Errorf("PruneDirs = %q, want exactly [only-this]", c.PruneDirs)
		}
		if !reflect.DeepEqual(c.ExcludeMasks, []string{"*.iso"}) {
			t.Errorf("ExcludeMasks = %q, want exactly [*.iso]", c.ExcludeMasks)
		}
	})

	t.Run("from the environment", func(t *testing.T) {
		c := Default()
		env := map[string]string{"PRUNE_DIRS": "( only-this and-this )"}
		if err := c.ApplyEnv(func(k string) string { return env[k] }); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.PruneDirs, []string{"only-this", "and-this"}) {
			t.Errorf("PruneDirs = %q", c.PruneDirs)
		}
	})

	t.Run("a plain environment value is one element", func(t *testing.T) {
		c := Default()
		env := map[string]string{"PRUNE_DIRS": "my dir"}
		if err := c.ApplyEnv(func(k string) string { return env[k] }); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.PruneDirs, []string{"my dir"}) {
			t.Errorf("PruneDirs = %q, want one element, as bash would see it", c.PruneDirs)
		}
	})

	t.Run("an empty value prunes nothing", func(t *testing.T) {
		vals, err := Parse(`PRUNE_DIRS=""`, "test")
		if err != nil {
			t.Fatal(err)
		}
		c := Default()
		if err := c.Apply(vals); err != nil {
			t.Fatal(err)
		}
		if len(c.PruneDirs) != 0 {
			t.Errorf("PruneDirs = %q, want empty", c.PruneDirs)
		}
	})
}

func TestApplyEnv(t *testing.T) {
	setHome(t, "/home/tester")
	c := Default()
	env := map[string]string{
		"SOURCE_DIR":        "~/photos",
		"STAGING":           "$HOME/scratch",
		"DISC_TYPE":         "bd50",
		"COMPRESSION_LEVEL": "12",
		"ARCHIVE_NAME":      "", // unset values are ignored
	}
	if err := c.ApplyEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if c.SourceDir != "/home/tester/photos" {
		t.Errorf("SourceDir = %q", c.SourceDir)
	}
	if c.Staging != "/home/tester/scratch" {
		t.Errorf("Staging = %q", c.Staging)
	}
	if c.DiscType != disc.BD50 {
		t.Errorf("DiscType = %q", c.DiscType)
	}
	if c.CompressionLevel != 12 {
		t.Errorf("CompressionLevel = %d", c.CompressionLevel)
	}
	if c.ArchiveName != "" {
		t.Errorf("ArchiveName = %q, want an empty env var to be ignored", c.ArchiveName)
	}

	c = Default()
	err := c.ApplyEnv(func(k string) string {
		if k == "PAR2_BLOCKS" {
			return "lots"
		}
		return ""
	})
	if err == nil || !strings.Contains(err.Error(), "PAR2_BLOCKS") {
		t.Errorf("ApplyEnv error = %v, want it to name PAR2_BLOCKS", err)
	}
}

func TestLoadPrecedence(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	src := filepath.Join(home, "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(home, "config")
	body := "" +
		"SOURCE_DIR=" + src + "\n" +
		"STAGING=/from-file\n" +
		"DISC_TYPE=bd50\n" +
		"PRUNE_DIRS=( from-file )\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// File only.
	t.Setenv("STAGING", "")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Staging != "/from-file" || c.DiscType != disc.BD50 {
		t.Errorf("file layer not applied: %+v", c)
	}
	if !reflect.DeepEqual(c.PruneDirs, []string{"from-file"}) {
		t.Errorf("PruneDirs = %q, want the file's value to replace the defaults", c.PruneDirs)
	}
	if c.ArchiveName == "" {
		t.Error("Load did not resolve ArchiveName")
	}
	if !strings.HasPrefix(c.ArchiveName, "src-") {
		t.Errorf("ArchiveName = %q, want src-<date> from the file's SOURCE_DIR", c.ArchiveName)
	}
	// Untouched keys keep their defaults.
	if c.Burner != "/dev/sr0" {
		t.Errorf("Burner = %q, want the default", c.Burner)
	}

	// Environment beats the file.
	t.Setenv("STAGING", "/from-env")
	t.Setenv("DISC_TYPE", "bdxl100")
	t.Setenv("PRUNE_DIRS", "( from-env )")
	c, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Staging != "/from-env" {
		t.Errorf("Staging = %q, want the environment to win", c.Staging)
	}
	if c.DiscType != disc.BDXL100 {
		t.Errorf("DiscType = %q, want the environment to win", c.DiscType)
	}
	if !reflect.DeepEqual(c.PruneDirs, []string{"from-env"}) {
		t.Errorf("PruneDirs = %q, want the environment to win", c.PruneDirs)
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	c, err := Load(filepath.Join(home, "no-such-config"))
	if err != nil {
		t.Fatalf("Load on a missing file: %v", err)
	}
	if c.Staging != "/var/tmp/brb" {
		t.Errorf("Staging = %q, want the default", c.Staging)
	}
}

func TestLoadMalformedFileIsAnError(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	path := filepath.Join(home, "config")
	if err := os.WriteFile(path, []byte("ARCHIVE_NAME=$(date)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load on a malformed file succeeded, want an error")
	}
}

// TestISOModeIsCheckedAtLoad keeps a typo from being interpreted. brb.sh tests
// for "eager" and treats every other value as ondemand, so "ISO_MODE=egaer"
// would silently skip the ISO build the operator asked for — and here it would
// have to survive as far as Validate, which not every command runs.
func TestISOModeIsCheckedAtLoad(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	path := filepath.Join(home, "config")

	if err := os.WriteFile(path, []byte("ISO_MODE=nonsense\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted ISO_MODE=nonsense")
	}
	for _, want := range []string{"nonsense", "ondemand", "eager"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}

	// The environment layer is checked the same way; brb.sh reads it too.
	if err := os.WriteFile(path, []byte("ISO_MODE=eager\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ISO_MODE", "later")
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted ISO_MODE=later from the environment")
	}
	t.Setenv("ISO_MODE", "ONDEMAND")
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ISOMode != ISOOnDemand {
		t.Errorf("ISOMode = %q, want the environment to win with %q", c.ISOMode, ISOOnDemand)
	}
	if c.ISOMode.Eager() {
		t.Error("ondemand reports itself as eager")
	}

	// And the default is the cheap one: a backup that builds no ISOs at all.
	if got := Default().ISOMode; got != ISOOnDemand {
		t.Errorf("default ISO_MODE = %q, want %q", got, ISOOnDemand)
	}
	if Default().KeepISOs {
		t.Error("KEEP_ISOS defaults to on; it must default to off")
	}
}

func TestLoadUsesDefaultConfigPath(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	path := filepath.Join(home, "explicit.conf")
	if err := os.WriteFile(path, []byte("LABEL_PREFIX=VIA_BRB_CONFIG\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRB_CONFIG", path)
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.LabelPrefix != "VIA_BRB_CONFIG" {
		t.Errorf("LabelPrefix = %q, want the file named by BRB_CONFIG to be read", c.LabelPrefix)
	}
}

func TestValidate(t *testing.T) {
	setHome(t, "/home/tester")
	dir := t.TempDir()
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	base := func() *Config {
		c := Default()
		c.SourceDir = dir
		c.ResolveDefaults()
		return c
	}

	if err := base().Validate(); err != nil {
		t.Fatalf("the default configuration does not validate: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"missing source", func(c *Config) { c.SourceDir = filepath.Join(dir, "nope") }, "does not exist"},
		{"source is a file", func(c *Config) { c.SourceDir = file }, "not a directory"},
		{"empty source", func(c *Config) { c.SourceDir = "" }, "SOURCE_DIR is empty"},
		{"empty staging", func(c *Config) { c.Staging = "" }, "STAGING is empty"},
		{"unknown disc type", func(c *Config) { c.DiscType = "dvd" }, "DISC_TYPE"},
		{"zero pack ratio", func(c *Config) { c.PackRatio = 0 }, "PACK_RATIO"},
		{"negative pack ratio", func(c *Config) { c.PackRatio = -0.5 }, "PACK_RATIO"},
		// A window of zero has no worst case, so an estimate taken from it is
		// not a measurement; refused by name rather than silently disabling the
		// adaptation the operator asked for.
		{"zero ratio window", func(c *Config) { c.PackRatioWindow = 0 },
			"PACK_RATIO_WINDOW must be at least 1"},
		{"zero ratio window says how to disable adapting", func(c *Config) { c.PackRatioWindow = 0 },
			"PACK_RATIO_ADAPT=0"},
		{"negative ratio window", func(c *Config) { c.PackRatioWindow = -1 }, "PACK_RATIO_WINDOW"},
		// Below 1.0 the margin plans every disc to overshoot, and each
		// overshoot costs a full rebuild of a multi-gigabyte image.
		{"margin below one", func(c *Config) { c.PackRatioMargin = 0.9 }, "PACK_RATIO_MARGIN"},
		{"zero margin", func(c *Config) { c.PackRatioMargin = 0 }, "PACK_RATIO_MARGIN"},
		{"bad compression", func(c *Config) { c.Compression = "brotli" }, "COMPRESSION"},
		{"zstd level too high", func(c *Config) { c.CompressionLevel = 23 }, "out of range"},
		{"zstd level too low", func(c *Config) { c.CompressionLevel = 0 }, "out of range"},
		{"gzip level too high", func(c *Config) { c.Compression = "gzip" }, "out of range"},
		{"negative reserve", func(c *Config) { c.ReserveBytes = -1 }, "RESERVE_BYTES"},
		{"negative capacity", func(c *Config) { c.DiscCapacityBytes = -1 }, "DISC_CAPACITY_BYTES"},
		{"negative burn speed", func(c *Config) { c.BurnSpeed = -1 }, "BURN_SPEED"},
		{"negative shrink attempts", func(c *Config) { c.MaxShrinkAttempts = -1 }, "MAX_SHRINK_ATTEMPTS"},
		{"negative jobs", func(c *Config) { c.Jobs = -1 }, "JOBS"},
		{"negative par2 memory", func(c *Config) { c.Par2MemoryMB = -1 }, "PAR2_MEMORY_MB"},
		// PAR2_BLOCKS=0 is no longer an error: it is the default, and it means
		// "size the blocks from the image". Only nonsense values are rejected.
		{"negative par2 blocks", func(c *Config) { c.Par2Blocks = -1 }, "PAR2_BLOCKS"},
		{"par2 blocks above par2's ceiling", func(c *Config) { c.Par2Blocks = 32769 }, "PAR2_BLOCKS"},
		{"redundancy zero", func(c *Config) { c.Par2Redundancy = 0 }, "PAR2_REDUNDANCY"},
		{"redundancy negative", func(c *Config) { c.Par2Redundancy = -5 }, "PAR2_REDUNDANCY"},
		{"redundancy over 100", func(c *Config) { c.Par2Redundancy = 101 }, "PAR2_REDUNDANCY"},
		{"block size not a size", func(c *Config) { c.BlockSize = "big" }, "BLOCK_SIZE"},
		{"block size empty", func(c *Config) { c.BlockSize = "" }, "BLOCK_SIZE"},
		{"block size not a power of two", func(c *Config) { c.BlockSize = "3K" }, "power of two"},
		{"block size too large", func(c *Config) { c.BlockSize = "2M" }, "power of two"},
		{"capacity too small", func(c *Config) { c.DiscCapacityBytes = 1000 }, "disc capacity too small"},
		// ARCHIVE_NAME goes verbatim into MANIFEST.txt and every disc's README.
		// A pasted newline splits the README title and injects a second heading;
		// the config parser accepts one inside a double-quoted value, so this is
		// reachable from a real brb.conf.
		{"archive name with a newline", func(c *Config) { c.ArchiveName = "evil\n# injected" }, "ARCHIVE_NAME"},
		{"archive name with an escape", func(c *Config) { c.ArchiveName = "evil\x1b[31m" }, "ARCHIVE_NAME"},
		{"archive name with a tab", func(c *Config) { c.ArchiveName = "a\tb" }, "ARCHIVE_NAME"},
		{"archive name with a slash", func(c *Config) { c.ArchiveName = "a/b" }, "ARCHIVE_NAME"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate succeeded, want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	setHome(t, "/home/tester")
	c := Default()
	c.SourceDir = filepath.Join(t.TempDir(), "nope")
	c.PackRatio = 0
	c.Par2Redundancy = 500
	err := c.Validate()
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"does not exist", "PACK_RATIO", "PAR2_REDUNDANCY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestValidateAcceptsEveryCompressor(t *testing.T) {
	setHome(t, "/home/tester")
	dir := t.TempDir()
	for _, comp := range Compressions() {
		c := Default()
		c.SourceDir = dir
		c.Compression = comp
		if lo, _, used := compressionLevelRange(comp); used {
			c.CompressionLevel = lo
		}
		c.ResolveDefaults()
		if err := c.Validate(); err != nil {
			t.Errorf("compression %q does not validate: %v", comp, err)
		}
	}
}

func TestBlockSizeBytes(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"1M", 1024 * 1024, false},
		{"128K", 131072, false},
		{"131072", 131072, false},
		{"4k", 4096, false},
		{"1m", 1024 * 1024, false},
		{" 1M ", 1024 * 1024, false},
		{"", 0, true},
		{"M", 0, true},
		{"0", 0, true},
		{"-4K", 0, true},
		{"1G", 0, true},
	}
	for _, tc := range tests {
		got, err := BlockSizeBytes(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("BlockSizeBytes(%q) = %d, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("BlockSizeBytes(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("BlockSizeBytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestPackRatioAdaptationFromFileAndEnvironment. The three settings decide how
// full every disc after the first comes out, and brb.sh reads them from both
// layers under exactly these names.
func TestPackRatioAdaptationFromFileAndEnvironment(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	path := filepath.Join(home, "config")
	body := "PACK_RATIO_ADAPT=0\nPACK_RATIO_WINDOW=5\nPACK_RATIO_MARGIN=1.20\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PackRatioAdapt || c.PackRatioWindow != 5 || c.PackRatioMargin != 1.20 {
		t.Errorf("from the file: %v/%d/%v, want false/5/1.20",
			c.PackRatioAdapt, c.PackRatioWindow, c.PackRatioMargin)
	}

	t.Setenv("PACK_RATIO_ADAPT", "1")
	t.Setenv("PACK_RATIO_WINDOW", "2")
	t.Setenv("PACK_RATIO_MARGIN", "1.10")
	c, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.PackRatioAdapt || c.PackRatioWindow != 2 || c.PackRatioMargin != 1.10 {
		t.Errorf("from the environment: %v/%d/%v, want true/2/1.10",
			c.PackRatioAdapt, c.PackRatioWindow, c.PackRatioMargin)
	}

	// PACK_RATIO_ADAPT is one of brb.sh's (( VAR )) settings, where a word is
	// an unset variable name and reads as 0 — the opposite of what it says.
	t.Setenv("PACK_RATIO_ADAPT", "yes")
	if _, err := Load(path); err == nil ||
		!strings.Contains(err.Error(), "PACK_RATIO_ADAPT: expected 0 or 1") {
		t.Errorf("Load error = %v, want PACK_RATIO_ADAPT=yes refused", err)
	}
}
