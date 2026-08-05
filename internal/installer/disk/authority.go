package disk

import (
	"fmt"
	"sort"
	"strings"
)

// DestructiveVolumeAuthorityError reports the concrete node volumes whose
// existing contents would be overwritten without operation-level authority.
type DestructiveVolumeAuthorityError struct {
	Required []string
}

func (e *DestructiveVolumeAuthorityError) Error() string {
	flags := make([]string, 0, len(e.Required))
	for _, target := range e.Required {
		flags = append(flags, "--acknowledge-storage-wipe "+target)
	}
	return fmt.Sprintf(
		"destructive storage acknowledgement required for non-blank node volumes: %s; inspect the selected devices, then retry with %s",
		strings.Join(e.Required, ", "), strings.Join(flags, " "),
	)
}

// RequiredDestructiveVolumeAcknowledgements returns stable NODE/VOLUME keys
// for destructive plans whose discovered target already contains signatures.
// A destructive plan against a blank target needs no acknowledgement.
func RequiredDestructiveVolumeAcknowledgements(node string, plans []VolumePlan) []string {
	node = strings.TrimSpace(node)
	seen := make(map[string]struct{})
	for _, plan := range plans {
		if (!plan.Wipe && !plan.Repartition) || len(plan.Signatures) == 0 {
			continue
		}
		key := node + "/" + strings.TrimSpace(plan.Name)
		if node == "" || strings.TrimSpace(plan.Name) == "" {
			continue
		}
		seen[key] = struct{}{}
	}
	required := make([]string, 0, len(seen))
	for key := range seen {
		required = append(required, key)
	}
	sort.Strings(required)
	return required
}

// ValidateDestructiveVolumeAcknowledgements enforces operation-level
// authority for every non-blank destructive volume plan.
func ValidateDestructiveVolumeAcknowledgements(node string, plans []VolumePlan, acknowledged []string) error {
	provided := make(map[string]struct{}, len(acknowledged))
	for _, value := range acknowledged {
		provided[strings.TrimSpace(value)] = struct{}{}
	}
	var missing []string
	for _, required := range RequiredDestructiveVolumeAcknowledgements(node, plans) {
		if _, ok := provided[required]; !ok {
			missing = append(missing, required)
		}
	}
	if len(missing) > 0 {
		return &DestructiveVolumeAuthorityError{Required: missing}
	}
	return nil
}

// ValidateDestructiveVolumeAcknowledgementKeys validates the public
// NODE/VOLUME acknowledgement shape independently of a particular plan.
func ValidateDestructiveVolumeAcknowledgementKeys(values []string) error {
	seen := make(map[string]struct{}, len(values))
	for i, raw := range values {
		value := strings.TrimSpace(raw)
		parts := strings.Split(value, "/")
		if len(parts) != 2 || !validAuthoritySegment(parts[0]) || !validAuthoritySegment(parts[1]) {
			return fmt.Errorf("destructive storage acknowledgement %d must be NODE/VOLUME using lowercase DNS-label names", i+1)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("destructive storage acknowledgement %q is duplicated", value)
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
