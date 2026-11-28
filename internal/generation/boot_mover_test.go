package generation

import "testing"

func TestReadsBootMoves(t *testing.T) {
	for version, want := range map[string]bool{
		"2026.11.5":       false,
		"2026.11.5-rc.1":  false,
		"2026.11.6":       true,
		"2026.11.6-rc.1":  true,
		"2026.11.0-dev.0": true,
		"2026.11.5-dev.0": true,
		"0.0.0-dev":       true,
		"0000.0.0-dev.1":  true,
	} {
		if got := ReadsBootMoves(version); got != want {
			t.Errorf("ReadsBootMoves(%q) = %v, want %v", version, got, want)
		}
	}
}
