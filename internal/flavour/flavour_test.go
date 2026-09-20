package flavour

import "testing"

func TestNormalize(t *testing.T) {
	for _, value := range []string{"", "standard", "lts"} {
		got, err := Normalize(value)
		want := value
		if value == "" {
			want = "standard"
		}
		if err != nil || got != want {
			t.Fatalf("Normalize(%q) = %q, %v", value, got, err)
		}
	}
	for _, value := range []string{"LTS", "../lts", "next"} {
		if _, err := Normalize(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}
