package cli

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseGlobals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		want    globals
		wantErr string
	}{
		{
			name: "no arguments is help",
			args: nil,
			want: globals{cmd: "help"},
		},
		{
			name: "bare command",
			args: []string{"doctor"},
			want: globals{cmd: "doctor", args: []string{}},
		},
		{
			name: "short yes",
			args: []string{"-y", "backup"},
			want: globals{assumeYes: true, cmd: "backup", args: []string{}},
		},
		{
			name: "long yes and no colour",
			args: []string{"--yes", "--no-color", "ingest", "/mnt"},
			want: globals{assumeYes: true, noColor: true, cmd: "ingest", args: []string{"/mnt"}},
		},
		{
			name: "config with a separate value",
			args: []string{"-c", "/etc/brb.conf", "plan"},
			want: globals{configPath: "/etc/brb.conf", cmd: "plan", args: []string{}},
		},
		{
			name: "config with an equals sign",
			args: []string{"--config=/etc/brb.conf", "plan"},
			want: globals{configPath: "/etc/brb.conf", cmd: "plan", args: []string{}},
		},
		{
			name: "help flag",
			args: []string{"--help"},
			want: globals{cmd: "help"},
		},
		{
			name: "version flag",
			args: []string{"--version"},
			want: globals{cmd: "version"},
		},
		{
			// The whole point of strict parsing: -y after the command belongs
			// to the command, and is not a global flag stripped from anywhere.
			name: "flags after the command are the command's",
			args: []string{"index", "-y"},
			want: globals{cmd: "index", args: []string{"-y"}},
		},
		{
			name: "double dash before the command",
			args: []string{"--", "doctor"},
			want: globals{cmd: "doctor", args: []string{}},
		},
		{
			name:    "unknown global flag",
			args:    []string{"--frobnicate", "doctor"},
			wantErr: "unknown flag --frobnicate",
		},
		{
			name:    "config without a value",
			args:    []string{"-c"},
			wantErr: "flag -c needs a path",
		},
		{
			name:    "config with an empty value",
			args:    []string{"--config=", "doctor"},
			wantErr: "flag --config needs a path",
		},
		{
			name:    "yes does not take a value",
			args:    []string{"--yes=1", "doctor"},
			wantErr: "flag --yes takes no value",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseGlobals(tc.args)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parseGlobals(%q) = %+v, want error %q", tc.args, got, tc.wantErr)
				}
				var ue *usageError
				if !errors.As(err, &ue) {
					t.Fatalf("parseGlobals(%q) error %v is not a usageError", tc.args, err)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseGlobals(%q) error = %q, want it to contain %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseGlobals(%q): %v", tc.args, err)
			}
			if got.cmd != tc.want.cmd || got.assumeYes != tc.want.assumeYes ||
				got.noColor != tc.want.noColor || got.configPath != tc.want.configPath {
				t.Errorf("parseGlobals(%q) = %+v, want %+v", tc.args, got, tc.want)
			}
			if len(got.args) != len(tc.want.args) || (len(got.args) > 0 && !reflect.DeepEqual(got.args, tc.want.args)) {
				t.Errorf("parseGlobals(%q) args = %q, want %q", tc.args, got.args, tc.want.args)
			}
		})
	}
}

func TestCmdFlagsParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		args     []string
		wantOnly []string
		wantDisc int
		wantKeep bool
		wantPos  []string
		wantErr  string
	}{
		{
			name:    "positional only",
			args:    []string{"/dest"},
			wantPos: []string{"/dest"},
		},
		{
			name:     "flags after the positional argument",
			args:     []string{"/dest", "--only", "home/jz/thesis", "--disc", "3", "--keep-images"},
			wantOnly: []string{"home/jz/thesis"},
			wantDisc: 3,
			wantKeep: true,
			wantPos:  []string{"/dest"},
		},
		{
			name:     "flags before the positional argument",
			args:     []string{"--disc=2", "/dest"},
			wantDisc: 2,
			wantPos:  []string{"/dest"},
		},
		{
			name:     "repeated --only",
			args:     []string{"/dest", "--only", "a", "--only", "b"},
			wantOnly: []string{"a", "b"},
			wantPos:  []string{"/dest"},
		},
		{
			name:     "double dash makes a dashed value positional",
			args:     []string{"--", "--disc", "3"},
			wantPos:  []string{"--disc", "3"},
			wantDisc: 0,
		},
		{
			name:     "a value may look like a flag",
			args:     []string{"/dest", "--only", "--weird-name"},
			wantOnly: []string{"--weird-name"},
			wantPos:  []string{"/dest"},
		},
		{
			name:    "a lone dash is positional",
			args:    []string{"-"},
			wantPos: []string{"-"},
		},
		{
			name:    "unknown flag",
			args:    []string{"/dest", "--onlyy", "a"},
			wantErr: "restore: unknown flag --onlyy",
		},
		{
			name:    "missing value",
			args:    []string{"/dest", "--disc"},
			wantErr: "restore: flag --disc needs a value",
		},
		{
			name:    "value on a boolean flag",
			args:    []string{"--keep-images=yes"},
			wantErr: "restore: flag --keep-images takes no value",
		},
		{
			name:    "non-numeric value",
			args:    []string{"/dest", "--disc", "seven"},
			wantErr: `restore: --disc: "seven" is not a disc number`,
		},
		{
			name:    "negative value",
			args:    []string{"/dest", "--disc", "-2"},
			wantErr: `restore: --disc: "-2" is not a disc number`,
		},
		{
			// 0 is RestoreOptions.Disc's sentinel for "every disc", so an
			// accepted --disc 0 restores the whole set over the destination
			// instead of the one disc the operator asked for.
			name:    "zero is not a disc",
			args:    []string{"/dest", "--disc", "0"},
			wantErr: `restore: --disc: "0" is not a disc number`,
		},
		{
			name:    "empty --only",
			args:    []string{"/dest", "--only", ""},
			wantErr: "restore: --only: value must not be empty",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var only []string
			var discN int
			var keep bool
			f := newFlags("restore")
			f.StringList(&only, "--only")
			f.DiscNum(&discN, "--disc")
			f.Bool(&keep, "--keep-images")

			err := f.parse(tc.args)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parse(%q) = nil, want error %q", tc.args, tc.wantErr)
				}
				var ue *usageError
				if !errors.As(err, &ue) {
					t.Fatalf("parse(%q) error %v is not a usageError", tc.args, err)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parse(%q) error = %q, want it to contain %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse(%q): %v", tc.args, err)
			}
			if !reflect.DeepEqual(only, tc.wantOnly) && !(len(only) == 0 && len(tc.wantOnly) == 0) {
				t.Errorf("--only = %q, want %q", only, tc.wantOnly)
			}
			if discN != tc.wantDisc {
				t.Errorf("--disc = %d, want %d", discN, tc.wantDisc)
			}
			if keep != tc.wantKeep {
				t.Errorf("--keep-images = %v, want %v", keep, tc.wantKeep)
			}
			if !reflect.DeepEqual(f.pos, tc.wantPos) && !(len(f.pos) == 0 && len(tc.wantPos) == 0) {
				t.Errorf("positional = %q, want %q", f.pos, tc.wantPos)
			}
		})
	}
}

func TestCmdFlagsNeed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		pos      []string
		min, max int
		wantErr  string
	}{
		{name: "exactly right", pos: []string{"a"}, min: 1, max: 1},
		{name: "optional argument omitted", pos: nil, min: 0, max: 1},
		{name: "too few", pos: nil, min: 1, max: 1, wantErr: "not enough arguments"},
		{name: "too many", pos: []string{"a", "b"}, min: 1, max: 1, wantErr: `unexpected argument "b"`},
		{name: "unlimited", pos: []string{"a", "b", "c"}, min: 0, max: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFlags("cmd")
			f.pos = tc.pos
			err := f.need(tc.min, tc.max, "cmd <arg>")
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("need: unexpected error %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("need: want error %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("need: error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestDiscNumber(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{in: "1", want: 1},
		{in: "07", want: 7},
		{in: " 3 ", want: 3},
		{in: "0", wantErr: true},
		{in: "-1", wantErr: true},
		{in: "all", wantErr: true},
		{in: "", wantErr: true},
		{in: "1.5", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := discNumber("list", tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("discNumber(%q) = %d, want an error", tc.in, got)
				}
				var ue *usageError
				if !errors.As(err, &ue) {
					t.Fatalf("discNumber(%q) error %v is not a usageError", tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("discNumber(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("discNumber(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestDoubleDashIsNotEatenAsAValue: the package comment teaches "--" as the
// escape for data that begins with a dash, and a value-taking flag written just
// before it used to swallow it — so "restore --only -- -weird.txt" failed with
// "unknown flag -weird.txt", which reads as though the file were not in the
// archive.
func TestDoubleDashIsNotEatenAsAValue(t *testing.T) {
	t.Parallel()
	var only []string
	f := newFlags("restore")
	f.StringList(&only, "--only")

	err := f.parse([]string{"/dest", "--only", "--", "-weird.txt"})
	if err == nil {
		t.Fatalf("parse accepted a bare -- as the value of --only: only=%q pos=%q", only, f.pos)
	}
	var ue *usageError
	if !errors.As(err, &ue) {
		t.Fatalf("error %v is not a usageError", err)
	}
	for _, want := range []string{"--only", "--only=VALUE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// The spelling that works keeps working, and so does "--" in its own right.
	only = nil
	f = newFlags("restore")
	f.StringList(&only, "--only")
	if err := f.parse([]string{"/dest", "--only=-weird.txt"}); err != nil {
		t.Fatalf("--only=VALUE: %v", err)
	}
	if len(only) != 1 || only[0] != "-weird.txt" {
		t.Errorf("only = %q, want [-weird.txt]", only)
	}

	var yes bool
	f = newFlags("index")
	f.Bool(&yes, "--yes")
	if err := f.parse([]string{"--", "-y"}); err != nil {
		t.Fatalf("bare --: %v", err)
	}
	if len(f.pos) != 1 || f.pos[0] != "-y" {
		t.Errorf("pos = %q, want [-y]", f.pos)
	}
}
