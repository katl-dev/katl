package releaseversion

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  Version
	}{
		{"2026.7.0", Version{Year: 2026, Month: 7}},
		{"2026.12.10", Version{Year: 2026, Month: 12, Patch: 10}},
		{"2026.7.0-dev.0", Version{Year: 2026, Month: 7, Stage: Dev}},
		{"2026.7.0-alpha.1", Version{Year: 2026, Month: 7, Stage: Alpha, Number: 1}},
		{"2026.7.0-beta.12", Version{Year: 2026, Month: 7, Stage: Beta, Number: 12}},
		{"2026.10.3-rc.0", Version{Year: 2026, Month: 10, Patch: 3, Stage: Candidate}},
		{"2026.10.3-rc.2", Version{Year: 2026, Month: 10, Patch: 3, Stage: Candidate, Number: 2}},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got, err := Parse(tc.value)
			if err != nil || got != tc.want {
				t.Fatalf("Parse = %+v, %v, want %+v", got, err, tc.want)
			}
			if got.String() != tc.value {
				t.Fatalf("String = %q, want %q", got.String(), tc.value)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	for _, tc := range []struct{ name, value, want string }{
		{"empty", "", "is empty"},
		{"too long", "2026.7." + strings.Repeat("1", 80), "longer than 80"},
		{"tag prefix", "v2026.7.0", "not canonical"},
		{"semantic version", "0.1.0", "not canonical"},
		{"zero-padded month", "2026.07.0", "not canonical"},
		{"month zero", "2026.0.0", "not canonical"},
		{"month thirteen", "2026.13.0", "not canonical"},
		{"leading-zero patch", "2026.7.01", "not canonical"},
		{"leading-zero counter", "2026.7.0-rc.01", "not canonical"},
		{"other label", "2026.7.0-test.1", "not canonical"},
		{"missing counter", "2026.7.0-rc", "not canonical"},
		{"build metadata", "2026.7.0+build", "not canonical"},
		{"series", "2026.12.x", "not canonical"},
		{"trailing newline", "2026.7.0\n", "not canonical"},
		{"short year", "226.7.0", "not canonical"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := Parse(tc.value); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse(%q) = %+v, %v, want error containing %q", tc.value, got, err, tc.want)
			}
		})
	}
}

func TestParseTag(t *testing.T) {
	if got, err := ParseTag("v2026.12.1-rc.3"); err != nil || got.String() != "2026.12.1-rc.3" {
		t.Fatalf("ParseTag = %v, %v", got, err)
	}
	for _, tag := range []string{"2026.12.1", "v", "release/2026.12.1"} {
		if _, err := ParseTag(tag); err == nil {
			t.Fatalf("ParseTag(%q) succeeded", tag)
		}
	}
}

func TestCandidate(t *testing.T) {
	final, err := Parse("2026.12.1")
	if err != nil {
		t.Fatal(err)
	}
	if got := final.Candidate(2).Tag(); got != "v2026.12.1-rc.2" {
		t.Fatalf("Candidate(2).Tag() = %q", got)
	}
	candidate, _ := Parse("2026.12.1-rc.4")
	if candidate.IsFinal() || !candidate.Base().IsFinal() || candidate.Base() != final {
		t.Fatalf("Base of %v = %v", candidate, candidate.Base())
	}
}

func TestCompare(t *testing.T) {
	// Each version is strictly older than the next.
	ordered := []string{"2025.12.9", "2026.1.0-dev.3", "2026.1.0-alpha.1", "2026.1.0-beta.0", "2026.1.0-rc.1", "2026.1.0-rc.2", "2026.1.0", "2026.1.1", "2026.2.0", "2026.10.0", "2027.1.0"}
	for i, older := range ordered {
		a, err := Parse(older)
		if err != nil {
			t.Fatal(err)
		}
		if a.Compare(a) != 0 {
			t.Fatalf("%s.Compare(itself) != 0", older)
		}
		for _, newer := range ordered[i+1:] {
			b, err := Parse(newer)
			if err != nil {
				t.Fatal(err)
			}
			if a.Compare(b) != -1 || b.Compare(a) != 1 {
				t.Fatalf("%s should sort before %s", older, newer)
			}
		}
	}
}
