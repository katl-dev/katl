package manifest

import (
	"strings"
	"testing"
)

func TestEnabledUnitsValidation(t *testing.T) {
	for _, test := range []struct {
		name  string
		units []string
		masks []string
		want  string
	}{
		{name: "native", units: []string{"systemd-timesyncd.service", "backup.timer", "worker@one.service"}},
		{name: "template", units: []string{"worker@.service"}, want: "concrete"},
		{name: "option", units: []string{"--all"}, want: "concrete"},
		{name: "duplicate", units: []string{"backup.timer", "backup.timer"}, want: "duplicates"},
		{name: "protected", units: []string{"katlc-agent.service"}, want: "release-critical"},
		{name: "masked", units: []string{"backup.timer"}, masks: []string{"backup.timer"}, want: "conflicts"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateHostConfiguration(HostConfiguration{EnabledUnits: test.units, MaskedUnits: test.masks}, false)
			if test.want == "" && err != nil || test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}
