package doc

import (
	"regexp"
	"strings"
	"testing"
)

// percentRE finds every "<digits>%" in a rendered document.
var percentRE = regexp.MustCompile(`\d+%`)

// sampleDisc is a representative per-disc README input: a disc carrying the
// full dist payload, which is what a backup with a dist directory produces.
func sampleDisc() DiscData {
	return DiscData{
		Archive:    "home-2026-08-06",
		Disc:       3,
		Total:      12,
		Date:       "2026-08-06T05:21:00-04:00",
		Source:     "/home/jz",
		Redundancy: 10,
		// The parity over the small files is a separate, much higher figure;
		// it is what backup.SidecarRedundancy writes.
		SidecarRedundancy: 50,
		Version:           "1.0.0",
		Tools: []string{
			"brb.sh", "brb-linux-amd64", "brb-linux-aarch64", "brb-src.tar.gz",
		},
	}
}

// sampleManifest is a representative manifest input.
func sampleManifest() ManifestData {
	return ManifestData{
		Archive:     "home-2026-08-06",
		Created:     "2026-08-06T05:21:00-04:00",
		Host:        "workstation",
		Source:      "/home/jz",
		Total:       2,
		DiscType:    "bd25",
		Compression: "zstd",
		Level:       19,
		BlockSize:   "1M",
		Redundancy:  10,
		Version:     "1.0.0",
		ToolVersions: []string{
			"mksquashfs version 4.6.1 (2023/03/25)",
			"age v1.3.1",
			"par2cmdline version 0.8.1",
			"xorriso version 1.5.6",
		},
		Recipients: []string{
			"age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqsjhnrt",
			"age1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzs7zzzz",
		},
		DiscFiles: map[int][]FileEntry{
			1: {
				{Name: "disc01.squashfs.age.vol000+100.par2", Size: 2500000},
				{Name: "disc01.squashfs.age", Size: 24000000000},
				{Name: "index.tsv.gz.age", Size: 4096},
			},
			2: {
				{Name: "disc02.squashfs.age", Size: 12000000000},
				{Name: "index.tsv.gz.age", Size: 4096},
			},
		},
		PruneDirs:    []string{".cache", ".local/share/Trash"},
		ExcludeMasks: []string{"*.pyc", "core"},
	}
}

// assertNoPlaceholders is the invariant that matters most: a document shipped
// to a disc must contain no unsubstituted template markers of any kind.
func assertNoPlaceholders(t *testing.T, name, out string) {
	t.Helper()
	// "./ " is here for the one placeholder text/template does NOT announce: an
	// empty field renders as the empty string, so a worked example whose program
	// name is missing reads "./ ingest" and looks like ordinary prose. Nothing
	// legitimate in either document writes "./" followed by a space.
	for _, bad := range []string{"@@", "<no value>", "{{", "}}", "internal error", "./ "} {
		if strings.Contains(out, bad) {
			t.Errorf("%s contains %q:\n%s", name, bad, out)
		}
	}
}

func TestRenderDiscREADMESubstitutions(t *testing.T) {
	out := RenderDiscREADME(sampleDisc())
	assertNoPlaceholders(t, "README", out)

	tests := []struct {
		name string
		want string
	}{
		{"title carries padded disc and plain total", "# Backup disc 03 of 12 — `home-2026-08-06`"},
		{"creation line", "Created 2026-08-06T05:21:00-04:00 from `/home/jz`."},
		{"version line", "Written by brb (Blu-ray Backup) 1.0.0."},
		{"the bash script is listed under its own name", "brb.sh                     the tool as a bash script"},
		{"the amd64 binary is listed", "brb-linux-amd64            the tool as a static binary, 64-bit Intel/AMD"},
		{"the aarch64 binary is listed", "brb-linux-aarch64          the tool as a static binary, 64-bit ARM"},
		{"the source tarball is listed", "brb-src.tar.gz             complete source for both"},
		{"uname -m maps machines to the binaries that are here",
			"uname -m          # x86_64 -> brb-linux-amd64,  aarch64 -> brb-linux-aarch64"},
		{"rebuilding from source is offline", "tar xzf /mnt/brb-src.tar.gz && cd brb-*/go"},
		{"encrypted image name", "disc03.squashfs.age"},
		{"plaintext hash name", "disc03.squashfs.sha512"},
		{"par2 volume line carries redundancy", "disc03.squashfs.age.vol*.par2  10% recovery data"},
		{"index file", "index.tsv.gz.age"},
		{"the sidecar recovery set is listed", "sidecars.par2                    par2 index for the small files"},
		{"the sidecar volumes carry their own redundancy", "sidecars.vol*.par2               50% recovery data for them"},
		{"the sidecar parity is explained", "carry their own parity\nin `sidecars.par2`"},
		{"repairing a rotted sidecar is spelled out", "par2 repair -- sidecars.par2"},
		{"restore recipe copies the image and its sidecars", "cp /mnt/data/disc03.squashfs.age* ."},
		{"kernel mount recipe", "sudo mount -o loop,ro disc03.squashfs /mnt"},
		{"ddrescue section", "ddrescue -d -r3 /mnt/data/disc03.squashfs.age"},
		{"ddrescue section fetches the parity too", "cp /mnt/data/disc03.squashfs.age.* ."},
		{"the static binary restores with unsquashfs, not mksquashfs",
			"a static binary needs only\n`unsquashfs` and `par2` for a full restore"},
		{"redundancy in prose", "Each image carries 10% recovery data"},
		{"disc gone entirely section", "## If a disc is gone entirely"},
		{"index awk example uses the unpadded number", "'$1==3'"},
		// IDX-5: the recipe above is only correct because of the escaping, so
		// the disc has to say so in brb.sh's own words.
		{"one row per file", "Exactly one row per\nfile, one line each."},
		{"the escaping contract", "A backslash, tab or newline inside a path is written as\n" +
			"`\\\\`, `\\t` and `\\n` respectively, so a path can never span two rows."},
		{"restore via brb, unpadded", "./brb.sh restore /dest --disc 3"},
		{"mount via brb, unpadded", "./brb.sh mount 3 /mnt/browse"},
		{"quick reference survives", "unsquashfs -ll OUT.squashfs                    # list contents"},
		{"notes for the future survive", "## Notes for whoever finds these later"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(out, tt.want) {
				t.Errorf("README missing %q", tt.want)
			}
		})
	}
}

func TestRenderDiscREADMEFactualCorrections(t *testing.T) {
	out := RenderDiscREADME(sampleDisc())

	tests := []struct {
		name    string
		absent  string
		present string
	}{
		{
			name:    "no UDF is claimed",
			absent:  "ISO 9660 + UDF",
			present: "ISO 9660 (level 3, multi-extent so images larger than\n4 GiB work)",
		},
		{
			name:    "UDF is explicitly ruled out",
			absent:  "-udf",
			present: "No UDF is written.",
		},
		{
			name:    "the on-disc program is not called squashfs-backup.sh",
			absent:  "squashfs-backup.sh",
			present: "./brb.sh ingest",
		},
		{
			// A plain "brb" would be a 113 KB shell script on a disc brb.sh
			// wrote and an 8 MB ELF binary on one the Go build wrote. Every
			// artifact is named for what it actually is.
			name:    "nothing on the disc is called plain brb",
			absent:  "\nbrb  ",
			present: "brb.sh                     the tool as a bash script",
		},
		{
			name:    "the no-tooling-required promise is kept",
			absent:  "you do **not** need this script",
			present: "You do **not** need this program, and you do **not** need Python.",
		},
		{
			// The short way once copied only the .age file and then ran
			// `sha512sum -c disc03.squashfs.age.sha512 || par2 repair ...` on
			// a directory holding neither the sidecar nor the parity, so step 2
			// failed on every pristine disc ever burned. The glob is the fix,
			// and this pins it: the bare form must not come back.
			name:    "the short way copies the sidecars, not just the image",
			absent:  "cp /mnt/data/disc03.squashfs.age .",
			present: "cp /mnt/data/disc03.squashfs.age* .",
		},
		{
			// A restore never runs mksquashfs; that is the writer's tool. The
			// Go build extracts with unsquashfs and repairs with par2.
			name:    "the static binary is not said to restore with mksquashfs",
			absent:  "`mksquashfs` and `par2` for a full restore",
			present: "`unsquashfs` and `par2` for a full restore",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.absent != "" && strings.Contains(out, tt.absent) {
				t.Errorf("README still contains %q", tt.absent)
			}
			if !strings.Contains(out, tt.present) {
				t.Errorf("README missing %q", tt.present)
			}
		})
	}
}

// TestRenderDiscREADMEShortWayIsRunnable walks the "short way" block the way
// a restorer would, line by line, and checks that every file a step reads was
// put there by an earlier step. The block is the one restore path a stranger
// with a disc and no brb is promised; a recipe that fails at step 2 on a
// perfectly good disc is the worst thing this package can print.
func TestRenderDiscREADMEShortWayIsRunnable(t *testing.T) {
	out := RenderDiscREADME(sampleDisc())
	block := codeBlockAfter(t, out, "## Restoring, the short way")

	// Step 1 must be a copy whose glob covers the sidecar and the parity.
	// "disc03.squashfs.age*" matches disc03.squashfs.age, .age.sha512, .age.par2
	// and .age.vol*.par2 — exactly what steps 2 and 3 read.
	iCopy := strings.Index(block, "cp /mnt/data/disc03.squashfs.age* .")
	iCheck := strings.Index(block, "sha512sum -c disc03.squashfs.age.sha512")
	iRepair := strings.Index(block, "par2 repair -- disc03.squashfs.age.par2")
	iDecrypt := strings.Index(block, "-o disc03.squashfs disc03.squashfs.age")
	if iCopy < 0 || iCheck < 0 || iRepair < 0 || iDecrypt < 0 {
		t.Fatalf("short-way block is missing a step:\n%s", block)
	}
	if !(iCopy < iCheck && iCheck < iRepair && iRepair < iDecrypt) {
		t.Errorf("short-way steps are out of order (copy=%d check=%d repair=%d decrypt=%d)",
			iCopy, iCheck, iRepair, iDecrypt)
	}
	// And the reason the glob is there is written next to it, so a future
	// edit does not "tidy" it away again.
	if !strings.Contains(block, "the glob is deliberate") {
		t.Errorf("the short way does not say why the glob is there:\n%s", block)
	}
}

// TestRenderDiscREADMEDdrescueRecipeHasParityToRepairWith is the same audit
// applied to the salvage section: ddrescue produces the image alone, so a
// `par2 repair` straight after it had no recovery files to work with. The
// recipe has to copy the sidecar and the .par2 set over between the two, and
// with a glob that does not touch the image ddrescue just fought for.
func TestRenderDiscREADMEDdrescueRecipeHasParityToRepairWith(t *testing.T) {
	out := RenderDiscREADME(sampleDisc())
	block := codeBlockAfter(t, out, "## If a disc will not read")

	iRescue := strings.Index(block, "ddrescue -d -r3 /mnt/data/disc03.squashfs.age")
	iCopy := strings.Index(block, "cp /mnt/data/disc03.squashfs.age.* .")
	iRepair := strings.Index(block, "par2 repair -- disc03.squashfs.age.par2")
	if iRescue < 0 || iCopy < 0 || iRepair < 0 {
		t.Fatalf("salvage block is missing a step:\n%s", block)
	}
	if !(iRescue < iCopy && iCopy < iRepair) {
		t.Errorf("salvage steps are out of order (ddrescue=%d copy=%d repair=%d)", iRescue, iCopy, iRepair)
	}
	// ".age*" would include the image itself, and cp would stop at the very
	// I/O error the section exists to get past.
	if strings.Contains(block, "cp /mnt/data/disc03.squashfs.age* ") {
		t.Errorf("the salvage recipe copies the damaged image with cp:\n%s", block)
	}
}

// codeBlockAfter returns the first fenced code block that follows heading.
func codeBlockAfter(t *testing.T, doc, heading string) string {
	t.Helper()
	i := strings.Index(doc, heading)
	if i < 0 {
		t.Fatalf("README has no %q section", heading)
	}
	rest := doc[i+len(heading):]
	open := strings.Index(rest, "```")
	if open < 0 {
		t.Fatalf("no code block after %q", heading)
	}
	rest = rest[open+3:]
	// Skip the language tag on the opening fence.
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	}
	end := strings.Index(rest, "```")
	if end < 0 {
		t.Fatalf("unterminated code block after %q", heading)
	}
	return rest[:end]
}

// TestRenderDiscREADMEListsOnlyWhatIsOnTheDisc is the audit finding this field
// exists for: a README that tells a restorer to reach for brb-linux-aarch64 or
// to rebuild from brb-src.tar.gz, on a disc that carries neither, is worse than
// one that says nothing — it sends them looking for files that were never
// written, on the day they are already having a bad one.
func TestRenderDiscREADMEListsOnlyWhatIsOnTheDisc(t *testing.T) {
	all := []string{"brb.sh", "brb-linux-amd64", "brb-linux-aarch64", "brb-src.tar.gz"}

	tests := []struct {
		name   string
		tools  []string
		want   []string
		unwant []string
	}{
		{
			name:  "the full payload",
			tools: all,
			want: []string{
				"brb.sh", "brb-linux-amd64", "brb-linux-aarch64", "brb-src.tar.gz",
				"## Restoring with the tool on this disc",
				"uname -m          # x86_64 -> brb-linux-amd64,  aarch64 -> brb-linux-aarch64",
				"go build -mod=vendor ./cmd/brb",
				"./brb.sh ingest",
			},
		},
		{
			name:  "no payload, only the binary that ran the backup",
			tools: []string{"brb-linux-amd64"},
			want: []string{
				"brb-linux-amd64            the tool as a static binary, 64-bit Intel/AMD",
				"uname -m          # x86_64 -> brb-linux-amd64",
				"./brb-linux-amd64 ingest",
			},
			// Neither the other architecture nor the source may be promised,
			// and with no script there is nothing to say about bash.
			unwant: []string{"brb-linux-aarch64", "brb-src.tar.gz", "brb.sh", "go build"},
		},
		{
			name:  "a disc carrying no copy of the tool at all",
			tools: nil,
			unwant: []string{
				"brb.sh", "brb-linux", "brb-src.tar.gz", "uname -m",
				"## Restoring with the tool on this disc", "go build",
			},
		},
		{
			name:  "an aarch64 backup with no payload",
			tools: []string{"brb-linux-aarch64"},
			want: []string{
				"uname -m          # aarch64 -> brb-linux-aarch64",
				"cp /mnt/brb-linux-aarch64 /tmp/brb",
				"./brb-linux-aarch64 restore /dest --disc 3",
			},
			unwant: []string{"brb-linux-amd64", "brb-src.tar.gz"},
		},
		{
			name:   "the payload built without its binaries",
			tools:  []string{"brb.sh", "brb-src.tar.gz"},
			want:   []string{"./brb.sh ingest", "tar xzf /mnt/brb-src.tar.gz"},
			unwant: []string{"brb-linux", "uname -m", "chmod +x /tmp/brb"},
		},
		{
			// A writer built for a target the payload has no note for —
			// GOARCH=riscv64, a linux/386 package, anything outside the two
			// architectures build-dist.sh ships — copies itself onto every disc
			// under that name and nothing else. There is then no artifact the
			// document knows how to invoke, so the worked examples must be
			// omitted entirely rather than rendered with an empty program name.
			// assertNoPlaceholders catches "./ "; these pin that the recipe is
			// gone rather than merely mangled, and that the file listing still
			// names what the disc really carries.
			name:  "only an artifact the document cannot invoke",
			tools: []string{"brb-linux-riscv64"},
			want:  []string{"\nbrb-linux-riscv64\n"},
			unwant: []string{
				"ingest ", "index thesis", "restore /path/to/destination",
				"mount 3 /mnt/browse", "uname -m",
			},
		},
		{
			// The tarball alone is the other route to an empty Run, and it must
			// keep the one instruction that IS true of it: rebuild, then use
			// what you built.
			name:   "only the source tarball",
			tools:  []string{"brb-src.tar.gz"},
			want:   []string{"tar xzf /mnt/brb-src.tar.gz", "go build -mod=vendor ./cmd/brb"},
			unwant: []string{"ingest ", "index thesis", "mount 3 /mnt/browse"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := sampleDisc()
			d.Tools = tt.tools
			out := RenderDiscREADME(d)
			assertNoPlaceholders(t, "README", out)
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("README missing %q", w)
				}
			}
			for _, w := range tt.unwant {
				if strings.Contains(out, w) {
					t.Errorf("README promises %q, which is not on this disc:\n%s", w, out)
				}
			}
			// Whatever the disc carries, the manual restore path — the one that
			// needs no copy of brb at all — is always there.
			for _, w := range []string{
				"## Restoring, the short way",
				"unsquashfs -d /path/to/destination disc03.squashfs",
				"You do **not** need this program",
			} {
				if !strings.Contains(out, w) {
					t.Errorf("README missing %q", w)
				}
			}
		})
	}
}

// TestRenderDiscREADMEUnknownToolIsListedNotDescribed proves an artifact the
// document has no note for is still listed: it really is on the disc, and
// inventing a description for it is exactly the guessing this listing removes.
func TestRenderDiscREADMEUnknownToolIsListedNotDescribed(t *testing.T) {
	d := sampleDisc()
	d.Tools = []string{"brb-src.tar.gz", "brb-linux-riscv64", "brb.sh"}
	out := RenderDiscREADME(d)
	assertNoPlaceholders(t, "README", out)

	// Known names keep their order; the unknown one follows, bare.
	iScript := strings.Index(out, "brb.sh                     the tool as a bash script")
	iSrc := strings.Index(out, "brb-src.tar.gz             complete source")
	iOdd := strings.Index(out, "\nbrb-linux-riscv64\n")
	if iScript < 0 || iSrc < 0 || iOdd < 0 {
		t.Fatalf("listing is wrong:\n%s", out)
	}
	if !(iScript < iSrc && iSrc < iOdd) {
		t.Errorf("listing out of order: script=%d src=%d unknown=%d", iScript, iSrc, iOdd)
	}
	// It is not a binary the document knows the architecture of, so it must not
	// appear in the uname mapping.
	if strings.Contains(out, "uname -m") {
		t.Error("an unknown artifact was treated as an architecture-specific binary")
	}
}

func TestRenderDiscREADMERedundancyEverywhere(t *testing.T) {
	// Every occurrence of a percentage must move with the field it came from;
	// a stale literal would tell a restorer the wrong thing about how much
	// damage survives.
	for _, r := range []int{5, 10, 25} {
		d := sampleDisc()
		d.Redundancy = r
		d.SidecarRedundancy = 50
		out := RenderDiscREADME(d)
		assertNoPlaceholders(t, "README", out)

		// Three figures describe the image's parity and one the sidecars';
		// nothing else in the document is a percentage.
		image, sidecar := itoa(r)+"%", "50%"
		counts := map[string]int{}
		for _, g := range percentRE.FindAllString(out, -1) {
			counts[g]++
		}
		if counts[image] != 3 {
			t.Errorf("redundancy %d: the image figure appears %d time(s), want 3 (%v)", r, counts[image], counts)
		}
		if counts[sidecar] != 1 {
			t.Errorf("redundancy %d: the sidecar figure appears %d time(s), want 1 (%v)", r, counts[sidecar], counts)
		}
		if len(counts) != 2 {
			t.Errorf("redundancy %d: unexpected percentages in the README: %v", r, counts)
		}
	}

	// And the sidecar figure is genuinely rendered from the field rather than
	// hard-coded beside a comment saying it is not.
	d := sampleDisc()
	d.Redundancy = 10
	d.SidecarRedundancy = 60
	out := RenderDiscREADME(d)
	if !strings.Contains(out, "60% recovery data for them") {
		t.Error("the sidecar redundancy is not rendered from SidecarRedundancy")
	}
	if strings.Contains(out, "50%") {
		t.Error("a stale 50% survives in the README")
	}
}

func TestRenderDiscREADMEDiscNumbers(t *testing.T) {
	tests := []struct {
		disc, total int
		wantTitle   string
		wantFile    string
	}{
		{1, 1, "# Backup disc 01 of 1 —", "disc01.squashfs.age"},
		{7, 20, "# Backup disc 07 of 20 —", "disc07.squashfs.age"},
		{10, 10, "# Backup disc 10 of 10 —", "disc10.squashfs.age"},
		{100, 100, "# Backup disc 100 of 100 —", "disc100.squashfs.age"},
	}
	for _, tt := range tests {
		d := sampleDisc()
		d.Disc, d.Total = tt.disc, tt.total
		out := RenderDiscREADME(d)
		assertNoPlaceholders(t, "README", out)
		if !strings.Contains(out, tt.wantTitle) {
			t.Errorf("disc %d/%d: missing title %q", tt.disc, tt.total, tt.wantTitle)
		}
		if !strings.Contains(out, tt.wantFile) {
			t.Errorf("disc %d/%d: missing file name %q", tt.disc, tt.total, tt.wantFile)
		}
	}
}

// sampleKey is a syntactically plausible age secret key for the public-archive
// renders. It is not a real key.
const sampleKey = "AGE-SECRET-KEY-1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ"

// TestRenderDiscREADMEPublicArchive covers the passages that switch on
// PublicIdentity. A public set has to say, in the disc's own README, that it
// keeps no secret, where the key is, and that neither brb reader needs the key
// configured — ingest picks it up off the disc — while an ordinary set must
// render without a trace of any of it.
func TestRenderDiscREADMEPublicArchive(t *testing.T) {
	pub := sampleDisc()
	pub.PublicIdentity = sampleKey
	public := RenderDiscREADME(pub)
	ordinary := RenderDiscREADME(sampleDisc())
	assertNoPlaceholders(t, "public README", public)
	assertNoPlaceholders(t, "ordinary README", ordinary)

	wantPublic := []string{
		"> **This archive is deliberately NOT confidential.**",
		// Listed with the other root files, in the same column as they are.
		"identity.txt               the key to this archive — see the notice above",
		"**The age secret key is on this disc**, in `identity.txt`.",
		"## The key to this archive",
		sampleKey,
		// The worked example: nothing to export, ingest does it. Both readers
		// copy identity.txt into "$STAGING"/enc/ during ingest and use it, in
		// addition to any configured identity, for every later command.
		"# no key to configure: ingest copies identity.txt off the disc into\n" +
			"# \"$STAGING\"/enc/ and every command below picks it up from there on its own\n" +
			"\n./brb.sh ingest",
	}
	unwantPublic := []string{
		// The old worked example told the restorer to copy the key by hand
		// and export AGE_IDENTITY, because neither reader looked at the disc
		// root. Both do now, so the example must not send them on that errand.
		"export AGE_IDENTITY",
		"cp /mnt/identity.txt",
		"the tool does not read the disc root itself",
		// The ordinary set's line about the key.
		"never will be",
	}
	for _, w := range wantPublic {
		if !strings.Contains(public, w) {
			t.Errorf("public README missing %q", w)
		}
	}
	for _, w := range unwantPublic {
		if strings.Contains(public, w) {
			t.Errorf("public README still contains %q", w)
		}
	}

	wantOrdinary := []string{
		"**You do need the age secret key.**",
		"It is not on this disc and never will be.",
		"export AGE_IDENTITY=/path/to/identity.txt\n\n./brb.sh ingest",
	}
	unwantOrdinary := []string{
		"identity.txt               ",
		"NOT confidential",
		"## The key to this archive",
		// The ordinary README does say the key is "one line beginning
		// AGE-SECRET-KEY-1..."; what it must not carry is a key.
		sampleKey,
		"no key to configure",
	}
	for _, w := range wantOrdinary {
		if !strings.Contains(ordinary, w) {
			t.Errorf("ordinary README missing %q", w)
		}
	}
	for _, w := range unwantOrdinary {
		if strings.Contains(ordinary, w) {
			t.Errorf("ordinary README contains %q, which belongs to a public set only", w)
		}
	}

	// The identity file is listed in the same column as everything else in
	// the root listing; a ragged column reads as a typo in the one place a
	// restorer is told where the key is.
	col := strings.Index("brb.sh                     the tool", "the tool")
	line := lineContaining(public, "identity.txt               the key")
	if got := strings.Index(line, "the key"); got != col {
		t.Errorf("identity.txt note starts at column %d, the other notes at %d:\n%s", got, col, line)
	}
}

// TestRenderManifestPublicArchive: the manifest carries the second legible
// copy of a public archive's key, and says why, and an ordinary manifest
// carries no such section.
func TestRenderManifestPublicArchive(t *testing.T) {
	m := sampleManifest()
	m.PublicIdentity = sampleKey
	public := RenderManifest(m)
	ordinary := RenderManifest(sampleManifest())
	assertNoPlaceholders(t, "public MANIFEST", public)

	for _, w := range []string{
		"THIS SET IS PUBLIC — IT KEEPS NO SECRET",
		"  secret key: " + sampleKey,
		"identity.txt beside this file",
		// The public section sits between the recipients and the contents.
		"disc contents\n-------------\n",
	} {
		if !strings.Contains(public, w) {
			t.Errorf("public MANIFEST missing %q\n---\n%s", w, public)
		}
	}
	iKey := strings.Index(public, "THIS SET IS PUBLIC")
	iRec := strings.Index(public, "age recipients (public keys")
	iContents := strings.Index(public, "disc contents\n")
	if !(iRec < iKey && iKey < iContents) {
		t.Errorf("public section out of place: recipients=%d key=%d contents=%d", iRec, iKey, iContents)
	}
	for _, w := range []string{"THIS SET IS PUBLIC", "secret key:", "AGE-SECRET-KEY-1"} {
		if strings.Contains(ordinary, w) {
			t.Errorf("ordinary MANIFEST contains %q", w)
		}
	}
}

// TestRenderHasNoStrayBlankLines renders every shape of both documents and
// refuses a run of two blank lines anywhere. The templates lean on
// {{if}}/{{end}} placed at line starts to swallow their own newlines, and one
// misplaced end leaves a gap that reads as a missing paragraph — in the
// public-archive branches especially, which an ordinary set never renders.
func TestRenderHasNoStrayBlankLines(t *testing.T) {
	toolSets := [][]string{
		{"brb.sh", "brb-linux-amd64", "brb-linux-aarch64", "brb-src.tar.gz"},
		{"brb-linux-amd64"},
		{"brb.sh", "brb-src.tar.gz"},
		nil,
	}
	for _, key := range []string{"", sampleKey} {
		mode := "ordinary"
		if key != "" {
			mode = "public"
		}
		for _, tools := range toolSets {
			d := sampleDisc()
			d.Tools = tools
			d.PublicIdentity = key
			name := "README/" + mode + "/" + strings.Join(tools, ",")
			assertNoDoubleBlank(t, name, RenderDiscREADME(d))
		}
		m := sampleManifest()
		m.PublicIdentity = key
		assertNoDoubleBlank(t, "MANIFEST/"+mode, RenderManifest(m))
		m.ToolVersions, m.Recipients, m.PruneDirs, m.ExcludeMasks = nil, nil, nil, nil
		assertNoDoubleBlank(t, "MANIFEST/"+mode+"/empty", RenderManifest(m))
	}
}

// assertNoDoubleBlank fails on two consecutive empty lines.
func assertNoDoubleBlank(t *testing.T, name, out string) {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i := 1; i < len(lines); i++ {
		if lines[i] == "" && lines[i-1] == "" {
			t.Errorf("%s: two blank lines in a row at line %d (after %q)", name, i, lastNonBlank(lines[:i]))
		}
	}
}

// lastNonBlank returns the nearest preceding non-empty line, for messages.
func lastNonBlank(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i] != "" {
			return lines[i]
		}
	}
	return ""
}

// lineContaining returns the first line of s that contains sub, or "".
func lineContaining(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			return l
		}
	}
	return ""
}

func TestRenderManifest(t *testing.T) {
	out := RenderManifest(sampleManifest())
	assertNoPlaceholders(t, "MANIFEST", out)

	if !strings.HasPrefix(out, "brb (Blu-ray Backup) manifest\n=============================\n") {
		t.Errorf("manifest header wrong:\n%s", firstLines(out, 3))
	}

	tests := []struct {
		name string
		want string
	}{
		{"archive", "archive name    : home-2026-08-06"},
		{"created", "created         : 2026-08-06T05:21:00-04:00"},
		{"host", "host            : workstation"},
		{"source", "source          : /home/jz"},
		{"discs", "discs           : 2"},
		{"disc type", "disc type       : bd25"},
		{"image format", "image format    : SquashFS 4.0 (mountable by the Linux kernel)"},
		{"disc format says no UDF", "disc format     : ISO 9660 level 3 (multi-extent; no UDF), Rock Ridge + Joliet"},
		{"compression", "compression     : zstd level 19, block 1M"},
		{"encryption", "encryption      : age, X25519 recipients"},
		{"parity", "parity          : par2 10% over ciphertext"},
		{"version", "brb version     : 1.0.0"},
		{"independence warning", "IMPORTANT: each disc is INDEPENDENT."},
		{"tool versions section", "tool versions used to create this set\n-------------------------------------\n"},
		{"a tool version line", "  mksquashfs version 4.6.1 (2023/03/25)"},
		{"recipients section", "age recipients (public keys — harmless on their own)"},
		{"a recipient line", "  age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqsjhnrt"},
		{"disc contents section", "disc contents\n-------------\n"},
		{"disc 1 heading", "  disc 01 of 02"},
		{"disc 2 heading", "  disc 02 of 02"},
		{"a file listing", "      disc01.squashfs.age  (24000000000 bytes)"},
		{"excluded section", "excluded from this backup\n-------------------------\n"},
		{"prune entry", "  prune: .local/share/Trash"},
		{"mask entry", "  mask : *.pyc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(out, tt.want) {
				t.Errorf("manifest missing %q\n---\n%s", tt.want, out)
			}
		})
	}
}

func TestRenderManifestFileListingSortedAndScoped(t *testing.T) {
	out := RenderManifest(sampleManifest())

	// Files within a disc are listed in name order.
	iAge := strings.Index(out, "disc01.squashfs.age  (")
	iPar := strings.Index(out, "disc01.squashfs.age.vol000+100.par2")
	iIdx := strings.Index(out, "index.tsv.gz.age  (4096 bytes)")
	if iAge < 0 || iPar < 0 || iIdx < 0 {
		t.Fatalf("disc 1 listing incomplete:\n%s", out)
	}
	if !(iAge < iPar && iPar < iIdx) {
		t.Errorf("disc 1 files not name-sorted: age=%d par2=%d index=%d", iAge, iPar, iIdx)
	}

	// Disc 1's block precedes disc 2's.
	if strings.Index(out, "  disc 01 of 02") > strings.Index(out, "  disc 02 of 02") {
		t.Error("disc blocks are out of order")
	}
	// Disc 2 does not list disc 1's image.
	tail := out[strings.Index(out, "  disc 02 of 02"):]
	if strings.Contains(tail, "disc01.squashfs.age") {
		t.Error("disc 2 listing leaked disc 1's files")
	}
}

func TestRenderManifestDegenerateInputs(t *testing.T) {
	tests := []struct {
		name string
		data ManifestData
		want []string
	}{
		{
			name: "zero value renders without placeholders",
			data: ManifestData{},
			want: []string{"brb (Blu-ray Backup) manifest", "discs           : 0"},
		},
		{
			name: "no tools or recipients recorded",
			data: ManifestData{Archive: "a", Total: 1},
			want: []string{"  (not recorded)", "  (none recorded"},
		},
		{
			name: "disc with no recorded files still appears",
			data: ManifestData{Archive: "a", Total: 3, DiscFiles: map[int][]FileEntry{2: {{Name: "x", Size: 1}}}},
			want: []string{"  disc 01 of 03", "  disc 02 of 03", "  disc 03 of 03", "      x  (1 bytes)"},
		},
		{
			name: "disc number beyond Total is not dropped",
			data: ManifestData{Archive: "a", Total: 1, DiscFiles: map[int][]FileEntry{4: {{Name: "y", Size: 2}}}},
			want: []string{"  disc 01 of 01", "  disc 04 of 01", "      y  (2 bytes)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := RenderManifest(tt.data)
			assertNoPlaceholders(t, "MANIFEST", out)
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in:\n%s", w, out)
				}
			}
		})
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	// The manifest ranges over a map; two renders of the same input must be
	// byte-identical or SHA512SUMS would differ between discs of one set.
	for i := 0; i < 8; i++ {
		if got, want := RenderManifest(sampleManifest()), RenderManifest(sampleManifest()); got != want {
			t.Fatalf("manifest render is not deterministic")
		}
	}
	if got, want := RenderDiscREADME(sampleDisc()), RenderDiscREADME(sampleDisc()); got != want {
		t.Fatal("README render is not deterministic")
	}
}

func TestTemplatesParse(t *testing.T) {
	if _, err := parsedTemplates(); err != nil {
		t.Fatalf("embedded templates do not parse: %v", err)
	}
}

// itoa avoids pulling strconv into the test's import list for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// discDataFileRE matches the names of the files that live in a disc's data/
// subdirectory, with whatever suffix or glob a recipe hangs off them.
var discDataFileRE = regexp.MustCompile(`(disc\d\d\.squashfs\.age|index\.tsv\.gz\.age)[A-Za-z0-9.*+]*`)

// TestRenderDiscREADMERecipesNameFilesWhereTheyActuallyAre holds every shell
// recipe in the rendered README to the disc's real layout.
//
// The image, its sidecars and the encrypted index are written to <disc>/data/,
// not to the disc root, and a recipe that leaves the directory off does not run:
// `age -d -i key index.tsv.gz.age` from a mounted disc exits with "no such file
// or directory". That is how the "if a disc is gone entirely" section shipped —
// the one section reached only when a disc has already been lost, whose single
// command is the only way to find out what went with it.
//
// So this is a layout check rather than a string match: in every ```sh block,
// a data/ file must either be named under /mnt/data/, or have been brought into
// the working directory earlier in the same block by a cp or a ddrescue. Adding
// a recipe that reads off the disc by bare name fails here.
func TestRenderDiscREADMERecipesNameFilesWhereTheyActuallyAre(t *testing.T) {
	for _, tc := range []struct {
		name string
		data DiscData
	}{
		{"an ordinary set", sampleDisc()},
		{"a public archive", func() DiscData {
			d := sampleDisc()
			d.PublicIdentity = sampleKey
			return d
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := RenderDiscREADME(tc.data)

			inBlock := false
			blocks, operands := 0, 0
			// local holds the name prefixes a preceding cp or ddrescue has put
			// in the working directory, e.g. "disc03.squashfs.age" after
			// `cp /mnt/data/disc03.squashfs.age* .`. It is per block: each
			// fenced recipe stands on its own.
			var local []string
			isLocal := func(name string) bool {
				for _, p := range local {
					if strings.HasPrefix(name, p) {
						return true
					}
				}
				return false
			}

			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(line, "```") {
					if !inBlock && strings.TrimSpace(line) == "```sh" {
						inBlock, local, blocks = true, nil, blocks+1
					} else {
						inBlock = false
					}
					continue
				}
				if !inBlock {
					continue
				}
				brings := strings.HasPrefix(line, "cp ") || strings.HasPrefix(line, "ddrescue ")
				for _, loc := range discDataFileRE.FindAllStringIndex(line, -1) {
					operands++
					name, pre := line[loc[0]:loc[1]], line[:loc[0]]
					switch {
					case strings.HasSuffix(pre, "/mnt/data/"):
						if brings {
							local = append(local, strings.TrimSuffix(name, "*"))
						}
					case strings.HasSuffix(pre, "./"):
						local = append(local, name) // an explicit destination
					case strings.HasSuffix(pre, "/"):
						t.Errorf("recipe reads %s%s, but that file is in data/:\n  %s", pre, name, line)
					case !isLocal(name):
						t.Errorf("recipe names %q with no path and nothing copied it "+
							"into the working directory first; it is at /mnt/data/%s:\n  %s",
							name, name, line)
					}
				}
			}
			// Without these the whole check would pass on a document whose
			// fences had been renamed, having examined nothing.
			if blocks < 4 {
				t.Errorf("found only %d ```sh block(s); the fence scan is broken", blocks)
			}
			if operands < 8 {
				t.Errorf("found only %d data/ operand(s) across the recipes", operands)
			}
		})
	}
}
