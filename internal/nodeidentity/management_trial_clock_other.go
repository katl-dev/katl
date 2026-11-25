//go:build !linux

package nodeidentity

import (
	"errors"
	"time"
)

// Management trials run only on KatlOS nodes; workstation builds share the
// package for its types.
func (SystemTrialClock) SinceBoot() (time.Duration, error) {
	return 0, errors.New("management trials require Linux CLOCK_BOOTTIME")
}
