package systemdunit

import "strings"

const TargetName = "katl-configured-units.target"

type Activation struct {
	Enabled  []string
	Required []string
}

func (a Activation) Target() string {
	// Waiting for ordinary wanted units would deadlock units ordered after
	// multi-user.target. Only explicit boot-health requirements hold the target.
	return "[Unit]\nDescription=Configured Katl systemd units\nDefaultDependencies=no\n" +
		"Wants=" + strings.Join(a.Enabled, " ") + "\n" +
		"Requires=" + strings.Join(a.Required, " ") + "\n" +
		"After=" + strings.Join(a.Required, " ") + "\n"
}
