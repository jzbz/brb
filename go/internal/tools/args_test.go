package tools

import (
	"slices"
	"strings"
	"testing"
)

func TestLevelApplies(t *testing.T) {
	tests := []struct {
		comp string
		want bool
	}{
		{"zstd", true},
		{"gzip", true},
		{"lzo", true},
		{"ZSTD", true},
		{"  gzip  ", true},
		{"xz", false},
		{"lz4", false},
		{"lzma", false},
		{"none", false},
		{"", false},
		{"brotli", false},
	}
	for _, tc := range tests {
		if got := LevelApplies(tc.comp); got != tc.want {
			t.Errorf("LevelApplies(%q) = %v, want %v", tc.comp, got, tc.want)
		}
	}
}

func TestNoCompression(t *testing.T) {
	tests := []struct {
		comp string
		want bool
	}{
		{"", true},
		{"none", true},
		{"NONE", true},
		{" none ", true},
		{"zstd", false},
		{"xz", false},
	}
	for _, tc := range tests {
		if got := NoCompression(tc.comp); got != tc.want {
			t.Errorf("NoCompression(%q) = %v, want %v", tc.comp, got, tc.want)
		}
	}
}

func TestMksquashfsArgs(t *testing.T) {
	base := MkOptions{SourceDir: "/src", Out: "/img/disc01.squashfs"}

	tests := []struct {
		name string
		mod  func(*MkOptions)
		want []string
	}{
		{
			name: "defaults, no compression named",
			want: []string{"-", "/img/disc01.squashfs", "-cpiostyle0", "-no-progress",
				"-quiet", "-no-xattrs", "-no-exports", "-no-compression"},
		},
		{
			name: "explicit none",
			mod:  func(o *MkOptions) { o.Compression = "none"; o.Level = 19 },
			want: []string{"-", "/img/disc01.squashfs", "-cpiostyle0", "-no-progress",
				"-quiet", "-no-xattrs", "-no-exports", "-no-compression"},
		},
		{
			name: "zstd with level and the brb.sh defaults",
			mod: func(o *MkOptions) {
				o.Compression, o.Level, o.BlockSize, o.Xattrs = "zstd", 19, "1M", true
			},
			want: []string{"-", "/img/disc01.squashfs", "-cpiostyle0", "-no-progress",
				"-quiet", "-b", "1M", "-xattrs", "-no-exports",
				"-comp", "zstd", "-Xcompression-level", "19"},
		},
		{
			name: "gzip takes a level",
			mod:  func(o *MkOptions) { o.Compression, o.Level = "gzip", 9 },
			want: []string{"-", "/img/disc01.squashfs", "-cpiostyle0", "-no-progress",
				"-quiet", "-no-xattrs", "-no-exports", "-comp", "gzip",
				"-Xcompression-level", "9"},
		},
		{
			name: "lzo takes a level",
			mod:  func(o *MkOptions) { o.Compression, o.Level = "lzo", 8 },
			want: []string{"-", "/img/disc01.squashfs", "-cpiostyle0", "-no-progress",
				"-quiet", "-no-xattrs", "-no-exports", "-comp", "lzo",
				"-Xcompression-level", "8"},
		},
		{
			name: "xz drops the level",
			mod:  func(o *MkOptions) { o.Compression, o.Level = "xz", 9 },
			want: []string{"-", "/img/disc01.squashfs", "-cpiostyle0", "-no-progress",
				"-quiet", "-no-xattrs", "-no-exports", "-comp", "xz"},
		},
		{
			name: "lz4 drops the level",
			mod:  func(o *MkOptions) { o.Compression, o.Level = "lz4", 9 },
			want: []string{"-", "/img/disc01.squashfs", "-cpiostyle0", "-no-progress",
				"-quiet", "-no-xattrs", "-no-exports", "-comp", "lz4"},
		},
		{
			name: "zero level is never passed",
			mod:  func(o *MkOptions) { o.Compression, o.Level = "zstd", 0 },
			want: []string{"-", "/img/disc01.squashfs", "-cpiostyle0", "-no-progress",
				"-quiet", "-no-xattrs", "-no-exports", "-comp", "zstd"},
		},
		{
			name: "compressor name is normalised",
			mod:  func(o *MkOptions) { o.Compression, o.Level = " ZSTD ", 3 },
			want: []string{"-", "/img/disc01.squashfs", "-cpiostyle0", "-no-progress",
				"-quiet", "-no-xattrs", "-no-exports", "-comp", "zstd",
				"-Xcompression-level", "3"},
		},
		{
			name: "processors and memory",
			mod: func(o *MkOptions) {
				o.Compression, o.Processors, o.MemMB = "zstd", 4, 2048
			},
			want: []string{"-", "/img/disc01.squashfs", "-cpiostyle0", "-no-progress",
				"-quiet", "-no-xattrs", "-no-exports", "-processors", "4",
				"-mem", "2048M", "-comp", "zstd"},
		},
		{
			name: "zero processors and memory are omitted",
			mod: func(o *MkOptions) {
				o.Compression, o.Processors, o.MemMB = "zstd", 0, 0
			},
			want: []string{"-", "/img/disc01.squashfs", "-cpiostyle0", "-no-progress",
				"-quiet", "-no-xattrs", "-no-exports", "-comp", "zstd"},
		},
		{
			name: "unknown compressor is passed through without a level",
			mod:  func(o *MkOptions) { o.Compression, o.Level = "brotli", 5 },
			want: []string{"-", "/img/disc01.squashfs", "-cpiostyle0", "-no-progress",
				"-quiet", "-no-xattrs", "-no-exports", "-comp", "brotli"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := base
			if tc.mod != nil {
				tc.mod(&o)
			}
			got := MksquashfsArgs(o)
			if !slices.Equal(got, tc.want) {
				t.Errorf("MksquashfsArgs()\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestMksquashfsArgsFileListIsNotOnTheCommandLine(t *testing.T) {
	// The list must travel over stdin; a million paths would blow past ARG_MAX.
	got := MksquashfsArgs(MkOptions{
		SourceDir: "/src",
		Out:       "/img/disc01.squashfs",
		Files:     []string{"a", "b/c"},
	})
	for _, a := range got {
		if a == "a" || a == "b/c" {
			t.Fatalf("file list leaked onto the command line: %q", got)
		}
	}
	if !slices.Contains(got, "-cpiostyle0") {
		t.Errorf("missing -cpiostyle0 in %q", got)
	}
}

func TestPar2CreateArgs(t *testing.T) {
	tests := []struct {
		name string
		o    Par2Options
		want []string
	}{
		{
			name: "brb.sh defaults",
			o:    Par2Options{Dir: "/enc", File: "disc01.squashfs.age", Redundancy: 10, Blocks: 3000, MemoryMB: 1024},
			want: []string{"create", "-q", "-r10", "-n1", "-b3000", "-m1024", "--", "disc01.squashfs.age"},
		},
		{
			name: "everything defaulted away",
			o:    Par2Options{Dir: "/enc", File: "x.age"},
			want: []string{"create", "-q", "-n1", "--", "x.age"},
		},
		{
			name: "threads",
			o:    Par2Options{Dir: "/enc", File: "x.age", Redundancy: 5, Threads: 8},
			want: []string{"create", "-q", "-r5", "-n1", "-t8", "--", "x.age"},
		},
		{
			name: "a file starting with a dash is protected by --",
			o:    Par2Options{Dir: "/enc", File: "-weird.age"},
			want: []string{"create", "-q", "-n1", "--", "-weird.age"},
		},
		{
			// brb.sh: par2 create -q -r50 -n1 -b100 -- sidecars.par2 *.sha512 index.tsv.gz.age
			name: "the sidecar set names itself first and its members after",
			o: Par2Options{
				Dir: "/discs/disc01/data", File: "sidecars.par2", Redundancy: 50, Blocks: 100,
				Inputs: []string{"disc01.squashfs.age.sha512", "disc01.squashfs.sha512", "index.tsv.gz.age"},
			},
			want: []string{"create", "-q", "-r50", "-n1", "-b100", "--", "sidecars.par2",
				"disc01.squashfs.age.sha512", "disc01.squashfs.sha512", "index.tsv.gz.age"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Par2CreateArgs(tc.o); !slices.Equal(got, tc.want) {
				t.Errorf("Par2CreateArgs()\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestPar2VerifyRepairArgs(t *testing.T) {
	if got, want := Par2VerifyArgs("x.age.par2"), []string{"verify", "--", "x.age.par2"}; !slices.Equal(got, want) {
		t.Errorf("Par2VerifyArgs() = %q, want %q", got, want)
	}
	if got, want := Par2RepairArgs("x.age.par2"), []string{"repair", "--", "x.age.par2"}; !slices.Equal(got, want) {
		t.Errorf("Par2RepairArgs() = %q, want %q", got, want)
	}
	// A second ingest of the same disc leaves alternate damaged copies beside
	// the image; par2 combines them only when they are named as operands.
	if got, want := Par2RepairArgs("x.age.par2", "x.age.copy1", "x.age.copy2"),
		[]string{"repair", "--", "x.age.par2", "x.age.copy1", "x.age.copy2"}; !slices.Equal(got, want) {
		t.Errorf("Par2RepairArgs(extras) = %q, want %q", got, want)
	}
}

func TestMkisofsArgs(t *testing.T) {
	tests := []struct {
		name string
		o    ISOOptions
		want []string
	}{
		{
			name: "full",
			o:    ISOOptions{Dir: "/discs/disc01", Out: "/iso/disc01.iso", Label: "BACKUP_01_OF_03", AppID: "brb 1.0.0", Publish: "home-2026-01-01"},
			want: []string{"-as", "mkisofs", "-quiet", "-iso-level", "3", "-r", "-J",
				"-joliet-long", "-V", "BACKUP_01_OF_03", "-A", "brb 1.0.0",
				"-p", "home-2026-01-01", "-o", "/iso/disc01.iso", "/discs/disc01"},
		},
		{
			name: "minimal",
			o:    ISOOptions{Dir: "/d", Out: "/o.iso"},
			want: []string{"-as", "mkisofs", "-quiet", "-iso-level", "3", "-r", "-J",
				"-joliet-long", "-o", "/o.iso", "/d"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MkisofsArgs(tc.o)
			if !slices.Equal(got, tc.want) {
				t.Errorf("MkisofsArgs()\n got %q\nwant %q", got, tc.want)
			}
			if slices.Contains(got, "-udf") {
				t.Error("-udf must not be passed: xorriso cannot write UDF")
			}
		})
	}
}

func TestCdrecordArgs(t *testing.T) {
	tests := []struct {
		name  string
		dev   string
		iso   string
		speed int
		want  []string
	}{
		{"with speed", "/dev/sr0", "/iso/disc01.iso", 4,
			[]string{"-as", "cdrecord", "-v", "dev=/dev/sr0", "speed=4", "-eject", "/iso/disc01.iso"}},
		{"speed left to the drive", "/dev/sr0", "/iso/disc01.iso", 0,
			[]string{"-as", "cdrecord", "-v", "dev=/dev/sr0", "-eject", "/iso/disc01.iso"}},
		{"negative speed is ignored", "/dev/sr0", "/i.iso", -1,
			[]string{"-as", "cdrecord", "-v", "dev=/dev/sr0", "-eject", "/i.iso"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CdrecordArgs(tc.dev, tc.iso, tc.speed); !slices.Equal(got, tc.want) {
				t.Errorf("CdrecordArgs()\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestUnsquashfsArgs(t *testing.T) {
	tests := []struct {
		name string
		o    UnsqOptions
		want []string
	}{
		{
			name: "plain",
			o:    UnsqOptions{Image: "/r/disc01.squashfs", Dest: "/dest"},
			want: []string{"-d", "/dest", "-no-progress", "-no-wildcards", "-no-xattrs", "/r/disc01.squashfs"},
		},
		{
			name: "force and xattrs",
			o:    UnsqOptions{Image: "/i.sq", Dest: "/dest", Force: true, Xattrs: true},
			want: []string{"-d", "/dest", "-no-progress", "-no-wildcards", "-f", "-xattrs", "/i.sq"},
		},
		{
			name: "path filters follow the image",
			o:    UnsqOptions{Image: "/i.sq", Dest: "/dest", Force: true, Xattrs: true, Only: []string{"docs/thesis.pdf", "photos"}},
			want: []string{"-d", "/dest", "-no-progress", "-no-wildcards", "-f", "-xattrs", "/i.sq",
				"docs/thesis.pdf", "photos"},
		},
		{
			// An unprivileged restore limits itself to the namespace it can
			// write; the regex must follow -xattrs and precede the image.
			name: "xattrs restricted to a namespace",
			o: UnsqOptions{Image: "/i.sq", Dest: "/dest", Force: true, Xattrs: true,
				XattrsInclude: "^user."},
			want: []string{"-d", "/dest", "-no-progress", "-no-wildcards", "-f", "-xattrs",
				"-xattrs-include", "^user.", "/i.sq"},
		},
		{
			name: "xattrs-include is ignored when xattrs are off",
			o:    UnsqOptions{Image: "/i.sq", Dest: "/dest", XattrsInclude: "^user."},
			want: []string{"-d", "/dest", "-no-progress", "-no-wildcards", "-no-xattrs", "/i.sq"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := UnsquashfsArgs(tc.o)
			if !slices.Equal(got, tc.want) {
				t.Errorf("UnsquashfsArgs()\n got %q\nwant %q", got, tc.want)
			}
			img := slices.Index(got, tc.o.Image)
			for _, only := range tc.o.Only {
				if slices.Index(got, only) < img {
					t.Errorf("extraction path %q precedes the image in %q", only, got)
				}
			}
		})
	}
}

func TestSanitiseLabel(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"already legal", "BACKUP_01_OF_03", "BACKUP_01_OF_03"},
		{"lowercased", "backup_01_of_03", "BACKUP_01_OF_03"},
		{"mixed case", "BaCkUp", "BACKUP"},
		{"spaces and punctuation", "my backup: disc 1/3", "MY_BACKUP__DISC_1_3"},
		{"all invalid", "!@#$%^&*()", "__________"},
		{"digits and underscores survive", "_09_", "_09_"},
		{"exactly 32", strings.Repeat("A", 32), strings.Repeat("A", 32)},
		{"40 chars truncated to 32", strings.Repeat("A", 40), strings.Repeat("A", 32)},
		{"40 mixed chars truncated to 32", strings.Repeat("ab-", 14), strings.Repeat("AB_", 10) + "AB"},
		{"unicode becomes one underscore per byte", "café", "CAF__"},
		{"unicode truncation stays at 32 bytes", strings.Repeat("é", 40), strings.Repeat("_", 32)},
		{"newline and tab", "a\nb\tc", "A_B_C"},
		{"nul byte", "a\x00b", "A_B"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitiseLabel(tc.in)
			if got != tc.want {
				t.Errorf("SanitiseLabel(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) > maxLabel {
				t.Errorf("SanitiseLabel(%q) is %d bytes, over the 32 byte limit", tc.in, len(got))
			}
			for _, c := range []byte(got) {
				legal := (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
				if !legal {
					t.Errorf("SanitiseLabel(%q) = %q contains illegal byte %q", tc.in, got, c)
				}
			}
			if again := SanitiseLabel(got); again != got {
				t.Errorf("SanitiseLabel is not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestDiscLabel(t *testing.T) {
	tests := []struct {
		prefix     string
		n, total   int
		want       string
		wantLength int
	}{
		{"BACKUP", 1, 3, "BACKUP_01_OF_03", 15},
		{"my photos", 7, 12, "MY_PHOTOS_07_OF_12", 18},
		{strings.Repeat("X", 40), 1, 2, strings.Repeat("X", 32), 32},
	}
	for _, tc := range tests {
		got := DiscLabel(tc.prefix, tc.n, tc.total)
		if got != tc.want {
			t.Errorf("DiscLabel(%q, %d, %d) = %q, want %q", tc.prefix, tc.n, tc.total, got, tc.want)
		}
		if len(got) != tc.wantLength {
			t.Errorf("DiscLabel(%q, ...) length = %d, want %d", tc.prefix, len(got), tc.wantLength)
		}
	}
}

func TestKeepISOLine(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"xorriso : UPDATE : 12 files added in 1 seconds", false},
		{"xorriso : NOTE : Copying to System Area", false},
		{"Media current: stdio file, overwriteable", false},
		{"Added to ISO image: directory '/'='/discs/disc01'", false},
		{"ISO image produced: 1234 sectors", false},
		{"Written to medium : 1234 sectors at LBA 0", false},
		{"xorriso : Command completed successfully", false},
		{"xorriso : FAILURE : Cannot open source file", true},
		{"xorriso : SORRY : -as mkisofs: Unrecognized option", true},
		{"libisofs: something went wrong", true},
	}
	for _, tc := range tests {
		if got := KeepISOLine(tc.line); got != tc.want {
			t.Errorf("KeepISOLine(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}

func TestParseCompressors(t *testing.T) {
	tests := []struct {
		name string
		help string
		want []string
	}{
		{
			name: "nothing",
			help: "",
			want: nil,
		},
		{
			name: "squashfs-tools 4.7 summary block",
			help: "-comp <comp>\t\tselect <comp> compression\n" +
				"\t\t\tCompressors available:\n" +
				"\t\t\t\tgzip (default)\n" +
				"\t\t\t\tlzo\n" +
				"\t\t\t\tlz4\n" +
				"\t\t\t\txz\n" +
				"\t\t\t\tzstd\n" +
				"\t\t\t\tlzma\n" +
				"-noI\t\t\tdo not compress inode table\n",
			want: []string{"gzip", "lzo", "lz4", "xz", "zstd", "lzma"},
		},
		{
			name: "detailed block with per-compressor options",
			help: "Compressors available and compressor specific options:\n" +
				"\tgzip (default)\n" +
				"\t  -Xcompression-level <compression-level>\n" +
				"\t\t<compression-level> should be 1 .. 9 (default 9)\n" +
				"\tlzo\n" +
				"\t  -Xalgorithm <algorithm>\n" +
				"\t\tWhere <algorithm> is one of:\n" +
				"\t\t\tlzo1x_1\n" +
				"\t\t\tlzo1x_999 (default)\n" +
				"\tzstd\n" +
				"\t  -Xcompression-level <compression-level>\n",
			want: []string{"gzip", "lzo", "zstd"},
		},
		{
			name: "both blocks, de-duplicated in first-seen order",
			help: "\t\t\tCompressors available:\n\t\t\t\tzstd\n\t\t\t\tgzip\n" +
				"-noI\tdo not compress\n" +
				"Compressors available and compressor specific options:\n\tgzip (default)\n\tzstd\n\tlz4\n",
			want: []string{"zstd", "gzip", "lz4"},
		},
		{
			name: "heading with no entries",
			help: "Compressors available:\n-noI\tsomething\n",
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseCompressors(tc.help); !slices.Equal(got, tc.want) {
				t.Errorf("ParseCompressors()\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}
