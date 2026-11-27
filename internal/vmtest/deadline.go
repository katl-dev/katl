package vmtest

// DeadlineTimerDisabled skips timer activation while leaving its expiry service
// available. An empty [Timer] schedule would cause systemd to reject the unit.
const DeadlineTimerDisabled = "[Unit]\nConditionPathExists=!/\n"

func DeadlineExpiryUnit(instance string) string {
	return "katl-deadline-expire@" + instance + ".service"
}
