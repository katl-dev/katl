package generation

import "testing"

func TestReadsBootMoves(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		want    bool
	}{
		{"final below reader", "2026.11.5", false},
		{"rc below reader", "2026.11.5-rc.1", false},
		{"beta below reader", "2026.11.0-beta.1", false},
		{"alpha below reader", "2026.11.5-alpha.1", false},
		{"final reader", "2026.11.6", true},
		{"rc of reader", "2026.11.6-rc.1", true},
		{"alpha above reader", "2026.12.0-alpha.1", true},
		{"dev stamp below reader", "2026.11.0-dev.0", true},
		{"dev stamp of patch below reader", "2026.11.5-dev.0", true},
		{"outside grammar without year", "0.0.0-dev", true},
		{"outside grammar with month 0", "0000.0.0-dev.1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReadsBootMoves(tc.version); got != tc.want {
				t.Errorf("ReadsBootMoves(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}
