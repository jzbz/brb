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
	for _, bad := range []string{"@@", "<no value>", "{{", "}}", "internal error"} {
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
		{"restore recipe copies the right image", "cp /mnt/data/disc03.squashfs.age ."},
		{"kernel mount recipe", "sudo mount -o loop,ro disc03.squashfs /mnt"},
		{"ddrescue section", "ddrescue -d -r3 /mnt/data/disc03.squashfs.age"},
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
