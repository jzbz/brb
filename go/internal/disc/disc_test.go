package disc

import (
	"errors"
	"math"
	"testing"
)

func TestCapacityMatchesBash(t *testing.T) {
	// These numbers come straight out of brb.sh's disc_capacity(). If one of
	// them changes, discs written by the two implementations stop agreeing on
	// how much fits, so the test hard-codes them rather than deriving them.
	tests := []struct {
		typ  Type
		want int64
	}{
		{BD25, 25025314816},
		{BD50, 50050629632},
		{BDXL100, 100103356416},
		{BDXL128, 128001769472},
		{Type("nonsense"), 0},
	}
	for _, tc := range tests {
		t.Run(string(tc.typ), func(t *testing.T) {
			if got := tc.typ.Capacity(); got != tc.want {
				t.Errorf("Capacity() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseType(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Type
		wantErr bool
	}{
		{"bd25", "bd25", BD25, false},
		{"bd50", "bd50", BD50, false},
		{"bdxl100", "bdxl100", BDXL100, false},
		{"bdxl128", "bdxl128", BDXL128, false},
		{"uppercase", "BD50", BD50, false},
		{"padded", "  bdxl100\t", BDXL100, false},
		{"empty", "", "", true},
		{"unknown", "dvd", "", true},
		{"almost", "bd 25", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseType(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseType(%q) = %v, want error", tc.in, got)
				}
				if !errors.Is(err, ErrUnknownType) {
					t.Errorf("error %v does not wrap ErrUnknownType", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseType(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseType(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTypes(t *testing.T) {
	got := Types()
	if len(got) != 4 {
		t.Fatalf("Types() returned %d entries, want 4", len(got))
	}
	var prev int64
	for _, tp := range got {
		c := tp.Capacity()
		if c <= prev {
			t.Errorf("Types() not ordered by ascending capacity at %q (%d after %d)", tp, c, prev)
		}
		prev = c
		if _, err := ParseType(tp.String()); err != nil {
			t.Errorf("ParseType(%q) round trip failed: %v", tp, err)
		}
	}
}

func TestCompute(t *testing.T) {
	// Expected values computed with the shell itself:
	//   usable = cap * 98 / 100
	//   image  = (usable - reserve) * 100 / (100 + red + 1)
	tests := []struct {
		name       string
		capacity   int64
		reserve    int64
		redundancy int
		wantUsable int64
		wantImage  int64
	}{
		{"bd25 default", capBD25, 104857600, 10, 24524808519, 21999955782},
		{"bd50 default", capBD50, 104857600, 10, 49049617039, 44094377872},
		{"bdxl100 default", capBDXL100, 104857600, 10, 98101289287, 88285073591},
		{"bdxl128 default", capBDXL128, 104857600, 10, 125441734082, 112916104938},
		{"no reserve, no parity", capBD25, 0, 0, 24524808519, 24281988632},
		{"heavy parity", capBD25, 104857600, 50, 24524808519, 16172152926},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Compute(tc.capacity, tc.reserve, tc.redundancy)
			if err != nil {
				t.Fatalf("Compute: %v", err)
			}
			if b.Capacity != tc.capacity {
				t.Errorf("Capacity = %d, want %d", b.Capacity, tc.capacity)
			}
			if b.Reserve != tc.reserve {
				t.Errorf("Reserve = %d, want %d", b.Reserve, tc.reserve)
			}
			if b.Usable != tc.wantUsable {
				t.Errorf("Usable = %d, want %d", b.Usable, tc.wantUsable)
			}
			if b.Image != tc.wantImage {
				t.Errorf("Image = %d, want %d", b.Image, tc.wantImage)
			}
		})
	}
}

// TestComputeOrderOfOperations pins the integer truncation. Computing
// capacity/100*98 instead of capacity*98/100, or dividing before multiplying by
// 100 in the image step, gives a different (larger) answer.
func TestComputeOrderOfOperations(t *testing.T) {
	b, err := Compute(capBD25, 104857600, 10)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if reordered := capBD25 / 100 * 98; reordered == b.Usable {
		t.Skip("capacity happens to be divisible by 100; test cannot distinguish")
	}
	if b.Usable != capBD25*98/100 {
		t.Errorf("Usable = %d, want %d", b.Usable, capBD25*98/100)
	}
	if naive := (b.Usable - b.Reserve) / 111 * 100; naive == b.Image {
		t.Errorf("Image %d matches the reordered (divide-first) formula; truncation order is wrong", b.Image)
	}
}

func TestComputeErrors(t *testing.T) {
	tests := []struct {
		name       string
		capacity   int64
		reserve    int64
		redundancy int
	}{
		{"zero capacity", 0, 104857600, 10},
		{"negative capacity", -1, 0, 10},
		{"negative reserve", capBD25, -1, 10},
		{"negative redundancy", capBD25, 104857600, -1},
		{"reserve exceeds usable", 1000000, 104857600, 10},
		{"reserve exactly usable", 100, 98, 10},
		{"overflow", math.MaxInt64, 0, 10},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Compute(tc.capacity, tc.reserve, tc.redundancy)
			if err == nil {
				t.Fatalf("Compute(%d, %d, %d) = %+v, want error",
					tc.capacity, tc.reserve, tc.redundancy, b)
			}
		})
	}
}
