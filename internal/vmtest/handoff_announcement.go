package vmtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/katl-dev/katl/internal/installer/handoff"
)

// completeLines returns the newline-terminated prefix of serialText. Serial
// output arrives in arbitrary chunks, so the unterminated tail may be a line
// that is still being written.
func completeLines(serialText string) string {
	return serialText[:strings.LastIndex(serialText, "\n")+1]
}

// LastHandoffAnnouncement returns the last installer handoff announcement in
// serialText, which must hold only complete lines, as SerialHookEvent.SerialText
// does. ok is false when there is none.
func LastHandoffAnnouncement(serialText string) (endpoint handoff.Endpoint, ok bool, err error) {
	lines := strings.Split(serialText, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if endpoint, ok, err = handoff.ParseAnnouncement(lines[i]); ok || err != nil {
			return endpoint, ok, err
		}
	}
	return handoff.Endpoint{}, false, nil
}

// WaitForHandoffAnnouncement blocks until the installer has announced its
// handoff endpoint on the serial console.
func (h *VMHandle) WaitForHandoffAnnouncement(interval time.Duration) (handoff.Endpoint, error) {
	if interval == 0 {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		endpoint, ok, err := LastHandoffAnnouncement(completeLines(readSerial(h.Plan.SerialLog)))
		if err != nil || ok {
			return endpoint, err
		}
		select {
		case <-h.done:
			waitErr := h.Wait()
			endpoint, ok, err := LastHandoffAnnouncement(completeLines(readSerial(h.Plan.SerialLog)))
			if err != nil || ok {
				return endpoint, err
			}
			if waitErr == nil {
				waitErr = errors.New("libvirt domain exited")
			}
			return handoff.Endpoint{}, fmt.Errorf("installer did not announce its handoff endpoint: %w", waitErr)
		case <-h.ctx.Done():
			return handoff.Endpoint{}, context.Cause(h.ctx)
		case <-ticker.C:
		}
	}
}
