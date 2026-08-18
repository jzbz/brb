// Package doc renders the human-readable documents that are written onto
// every backup disc: README.md and MANIFEST.txt.
//
// These are the files a stranger reads in fifteen years with no access to
// anything else — no network, no source repository, and quite possibly no
// working copy of brb. The text is therefore treated as part of the archive
// format: it must be complete, self-contained and factually accurate about
// what is actually on the disc.
//
// Rendering is pure: nothing here touches the filesystem, runs a subprocess
// or consults the environment. Callers supply every fact.
package doc

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"
	"sync"
	"text/template"
)

//go:embed readme.md.tmpl
var readmeTemplate string

//go:embed manifest.txt.tmpl
var manifestTemplate string

// DiscData is everything the per-disc README needs to know.
type DiscData struct {
	// Archive is the archive name, e.g. "home-2026-08-06".
	Archive string
	// Disc is this disc's 1-based number.
	Disc int
	// Total is the number of discs in the set.
	Total int
	// Date is the creation timestamp, RFC 3339.
	Date string
	// Source is the absolute path of the directory that was backed up.
	Source string
	// Redundancy is the par2 recovery percentage over the image, e.g. 10.
	Redundancy int
	// SidecarRedundancy is the par2 recovery percentage over the small files
	// covered by sidecars.par2 — the .sha512 sidecars and the encrypted index.
	// It is a separate, much higher figure because those files are kilobytes.
	SidecarRedundancy int
	// Version is the brb version that produced the set.
	Version string
	// Tools names the copies of brb that are actually in the root of this
	// disc, e.g. "brb.sh", "brb-linux-amd64", "brb-src.tar.gz". The file
	// listing and the "restoring with the tool on this disc" section are
	// rendered from it and from nothing else: a README that promises a
	// restorer a file the disc does not carry is worse than one that says
	// nothing at all. Empty is fine and means the disc carries no copy of the
	// tool; the manual restore recipe never needed one.
	Tools []string
	// PublicIdentity is the archive's secret key, verbatim, when the set was
	// made with PUBLIC_ARCHIVE — and empty otherwise, which is what every
	// public-archive passage in the README is conditioned on.
	//
	// Writing a secret key into a document is the point rather than an
	// oversight: the same key is in identity.txt beside this file, and a
	// second legible copy is what lets a restorer recover from the first one
	// rotting. It is only ever set for archives that are deliberately not
	// confidential.
	PublicIdentity string
}

// PublicIdentityName is the file a public archive's secret key is written to,
// at the root of every disc. It lives here, with the other on-disc format
// facts, because both the README that points a restorer at it and the backup
// code that writes it have to agree on the name.
const PublicIdentityName = "identity.txt"

// FileEntry is one file in a disc's data/ directory, as listed by the manifest.
type FileEntry struct {
	// Name is the base name of the file.
	Name string
	// Size is the file's size in bytes.
	Size int64
}

// ManifestData is everything MANIFEST.txt needs to know. The same manifest is
// copied onto every disc of the set, so it describes the whole archive.
type ManifestData struct {
	// Archive is the archive name.
	Archive string
	// Created is the creation timestamp, RFC 3339.
	Created string
	// Host is the machine the backup was taken on ("unknown" if not known).
	Host string
	// Source is the absolute path of the directory that was backed up.
	Source string
	// Total is the number of discs in the set.
	Total int
	// DiscType is the media type, e.g. "bd25".
	DiscType string
	// Compression is the squashfs compressor, e.g. "zstd" or "none".
	Compression string
	// Level is the configured compression level.
	Level int
	// BlockSize is the squashfs data block size, e.g. "1M".
	BlockSize string
	// Redundancy is the par2 recovery percentage.
	Redundancy int
	// Version is the brb version that produced the set.
	Version string
	// ToolVersions holds one version banner per external tool, already
	// reduced to a single line each.
	ToolVersions []string
	// Recipients holds the age public keys the images were encrypted to.
	Recipients []string
	// PublicIdentity is the archive's secret key when the set was made with
	// PUBLIC_ARCHIVE, else "". See DiscData.PublicIdentity: the manifest holds
	// a second legible copy of the key on purpose.
	PublicIdentity string
	// DiscFiles maps a 1-based disc number to the files in that disc's
	// data/ directory.
	DiscFiles map[int][]FileEntry
	// PruneDirs holds the directories that were pruned from the scan.
	PruneDirs []string
	// ExcludeMasks holds the file name patterns that were excluded.
	ExcludeMasks []string
}

// readmeView is the value handed to the README template. It exists so the
// template can use pre-formatted fields (a zero-padded disc number, for
// instance) without the template needing logic.
type readmeView struct {
	Archive           string
	Disc              int    // unpadded, for prose and awk examples
	Disc02            string // zero-padded, matches the on-disc file names
	Total             int
	Date              string
	Source            string
	Redundancy        int
	SidecarRedundancy int
	Version           string

	// Tools are the copies of brb in the root of this disc, in listing order.
	Tools []toolFile
	// Binaries are the architecture-specific ones among Tools.
	Binaries []toolFile
	// UnameHint is the `uname -m` line mapping machines to the binaries that
	// are here, empty when none are.
	UnameHint string
	// Script is the bash script's name when it is here, else "".
	Script string
	// Src is the source tarball's name when it is here, else "".
	Src string
	// Run is the artifact the worked examples invoke, else "". The portable
	// script is preferred over a binary that only runs on one architecture.
	Run string

	// PublicIdentity is the archive's secret key when the set is a public
	// archive, else "". Every public-archive passage in the template is
	// guarded on it, so an ordinary set renders exactly as it did before.
	PublicIdentity string
	// PublicIdentityFile is the name that key is stored under on the disc.
	PublicIdentityFile string
}

// toolFile is one copy of brb on a disc, as the README renders it.
type toolFile struct {
	// Name is the file name, exactly as it appears on the disc.
	Name string
	// Line is the name padded out to the listing column with its note after
	// it, ready to drop into the "what is on this disc" block.
	Line string
	// Machine is the `uname -m` value the file runs on, empty when it is not
	// an architecture-specific binary.
	Machine string
}

// toolInfo is what is known about one artifact name.
type toolInfo struct {
	note    string
	machine string
}

// toolListColumn is the column the notes in the file listing start at.
const toolListColumn = 27

// toolOrder is the order the README lists the artifacts in, and toolNotes says
// what each one is. A name that is not here is still listed — the disc really
// does carry it — but without a description, because inventing one would be
// exactly the kind of claim this listing exists to avoid.
var (
	toolOrder = []string{"brb.sh", "brb-linux-amd64", "brb-linux-aarch64", "brb-src.tar.gz"}
	toolNotes = map[string]toolInfo{
		"brb.sh":            {note: "the tool as a bash script; it can also restore this"},
		"brb-linux-amd64":   {note: "the tool as a static binary, 64-bit Intel/AMD", machine: "x86_64"},
		"brb-linux-aarch64": {note: "the tool as a static binary, 64-bit ARM", machine: "aarch64"},
		"brb-src.tar.gz":    {note: "complete source for both, dependencies vendored"},
	}
)

// toolFiles turns the names of the artifacts on a disc into the listing the
// README renders, known names first in toolOrder, anything else after them in
// name order. Duplicates and empty names are dropped.
func toolFiles(names []string) []toolFile {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		if n != "" {
			want[n] = true
		}
	}

	ordered := make([]string, 0, len(want))
	for _, n := range toolOrder {
		if want[n] {
			ordered = append(ordered, n)
			delete(want, n)
		}
	}
	rest := make([]string, 0, len(want))
	for n := range want {
		rest = append(rest, n)
	}
	sort.Strings(rest)
	ordered = append(ordered, rest...)

	out := make([]toolFile, 0, len(ordered))
	for _, n := range ordered {
		info := toolNotes[n]
		line := n
		if info.note != "" {
			pad := toolListColumn - len(n)
			if pad < 1 {
				pad = 1
			}
			line = n + strings.Repeat(" ", pad) + info.note
		}
		out = append(out, toolFile{Name: n, Line: line, Machine: info.machine})
	}
	return out
}

// toolsView fills in the parts of the README that depend on which copies of brb
// the disc carries.
func toolsView(names []string) readmeView {
	var v readmeView
	v.Tools = toolFiles(names)

	var hints []string
	for _, t := range v.Tools {
		switch {
		case t.Machine != "":
			v.Binaries = append(v.Binaries, t)
			hints = append(hints, t.Machine+" -> "+t.Name)
		case t.Name == "brb.sh":
			v.Script = t.Name
		case strings.HasSuffix(t.Name, ".tar.gz"):
			v.Src = t.Name
		}
	}
	if len(hints) > 0 {
		v.UnameHint = "uname -m          # " + strings.Join(hints, ",  ")
	}

	// The examples have to name a file that is on the disc and that runs on
	// whatever machine is reading it. Only the script is true of on every
	// architecture, so it wins whenever the disc carries one; the uname line
	// above has already explained how to pick between the binaries.
	switch {
	case v.Script != "":
		v.Run = v.Script
	case len(v.Binaries) > 0:
		v.Run = v.Binaries[0].Name
	}
	return v
}

// manifestDisc is one disc's entry in the manifest's "disc contents" section.
type manifestDisc struct {
	Number int
	Files  []FileEntry
}

// manifestView is the value handed to the manifest template.
type manifestView struct {
	ManifestData
	Discs []manifestDisc
}

// parsedTemplates parses both templates once, on first use. It is a
// sync.OnceValues rather than a package-level template.Must so that a broken
// template surfaces as a rendered diagnostic instead of a panic, and so that
// importing this package has no side effects.
var parsedTemplates = sync.OnceValues(func() (*template.Template, error) {
	root := template.New("doc")
	if _, err := root.New("readme").Parse(readmeTemplate); err != nil {
		return nil, fmt.Errorf("parse readme template: %w", err)
	}
	if _, err := root.New("manifest").Parse(manifestTemplate); err != nil {
		return nil, fmt.Errorf("parse manifest template: %w", err)
	}
	return root, nil
})

// render executes one of the embedded templates. Any failure is reported in
// the returned string: these documents are written by code paths that have no
// error channel, and a visible marker in the output is far better than a
// silently truncated README.
func render(name string, data any) string {
	root, err := parsedTemplates()
	if err != nil {
		return fmt.Sprintf("brb: internal error: %v\n", err)
	}
	var b strings.Builder
	if err := root.ExecuteTemplate(&b, name, data); err != nil {
		return fmt.Sprintf("brb: internal error: render %s: %v\n", name, err)
	}
	return b.String()
}

// RenderDiscREADME renders the README.md placed in the root of disc d.Disc.
//
// The document is written for someone who has one of these discs, a Linux
// machine and the age secret key, and nothing else: it explains the on-disc
// layout, gives a restore recipe that needs only sha512sum, par2, age and the
// kernel, covers salvaging a partially readable disc with ddrescue, and says
// what to do when a disc of the set is gone entirely.
//
// The file listing and the section about running brb from the disc come from
// d.Tools, so they describe that disc and not an ideal one.
func RenderDiscREADME(d DiscData) string {
	v := toolsView(d.Tools)
	v.Archive = d.Archive
	v.Disc = d.Disc
	v.Disc02 = fmt.Sprintf("%02d", d.Disc)
	v.Total = d.Total
	v.Date = d.Date
	v.Source = d.Source
	v.Redundancy = d.Redundancy
	v.SidecarRedundancy = d.SidecarRedundancy
	v.Version = d.Version
	v.PublicIdentity = d.PublicIdentity
	if d.PublicIdentity != "" {
		v.PublicIdentityFile = PublicIdentityName
	}
	return render("readme", v)
}

// RenderManifest renders MANIFEST.txt, which describes the whole disc set and
// is copied unchanged onto every disc. It records the archive metadata, the
// exact tool versions used to create the set, the age public keys the images
// were encrypted to, the per-disc file listing, and what was deliberately left
// out of the backup.
func RenderManifest(d ManifestData) string {
	return render("manifest", manifestView{
		ManifestData: d,
		Discs:        manifestDiscs(d),
	})
}

// manifestDiscs flattens DiscFiles into a deterministic, ascending list. Discs
// 1..Total always appear even when they have no recorded files, and any disc
// number present in the map but outside that range is included too rather than
// being silently dropped.
func manifestDiscs(d ManifestData) []manifestDisc {
	seen := make(map[int]bool, len(d.DiscFiles)+d.Total)
	nums := make([]int, 0, len(d.DiscFiles)+d.Total)
	for n := 1; n <= d.Total; n++ {
		seen[n] = true
		nums = append(nums, n)
	}
	for n := range d.DiscFiles {
		if !seen[n] {
			seen[n] = true
			nums = append(nums, n)
		}
	}
	sort.Ints(nums)

	out := make([]manifestDisc, 0, len(nums))
	for _, n := range nums {
		files := append([]FileEntry(nil), d.DiscFiles[n]...)
		sort.SliceStable(files, func(i, j int) bool {
			if files[i].Name != files[j].Name {
				return files[i].Name < files[j].Name
			}
			return files[i].Size < files[j].Size
		})
		out = append(out, manifestDisc{Number: n, Files: files})
	}
	return out
}
