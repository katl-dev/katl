// Package flavour defines the kernel track independently of the KatlOS release.
package flavour

import "fmt"

const (
	Standard = "standard"
	LTS      = "lts"
)

// Normalize treats images produced before flavour selection as standard images.
func Normalize(value string) (string, error) {
	switch value {
	case "", Standard:
		return Standard, nil
	case LTS:
		return LTS, nil
	default:
		return "", fmt.Errorf("unknown KatlOS flavour %q; choose standard or lts", value)
	}
}

// Suffix preserves the original names of standard release assets.
func Suffix(value string) string {
	if value == LTS {
		return "-lts"
	}
	return ""
}
