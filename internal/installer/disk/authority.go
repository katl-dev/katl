package disk

import (
	"fmt"
	"strings"
)

// ValidateDestructiveVolumeAcknowledgementKeys validates the public
// NODE/VOLUME acknowledgement shape independently of a particular plan.
func ValidateDestructiveVolumeAcknowledgementKeys(values []string) error {
	return validateVolumeAuthorityKeys("destructive storage acknowledgement", values)
}

// ValidateVolumeRebindKeys validates one-shot volume rebind authority keys.
func ValidateVolumeRebindKeys(values []string) error {
	return validateVolumeAuthorityKeys("volume rebind", values)
}

func validateVolumeAuthorityKeys(kind string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for i, raw := range values {
		value := strings.TrimSpace(raw)
		parts := strings.Split(value, "/")
		if len(parts) != 2 || !validAuthoritySegment(parts[0]) || !validAuthoritySegment(parts[1]) {
			return fmt.Errorf("%s %d must be NODE/VOLUME using lowercase DNS-label names", kind, i+1)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("%s %q is duplicated", kind, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validAuthoritySegment(value string) bool {
	if value == "" || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			continue
		}
		return false
	}
	return true
}
