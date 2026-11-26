package nodeidentity

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

func (SystemTrialClock) SinceBoot() (time.Duration, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0, fmt.Errorf("read boot time: %w", err)
	}
	return time.Duration(ts.Nano()), nil
}
