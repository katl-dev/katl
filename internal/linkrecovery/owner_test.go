package linkrecovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeLinks stands in for the grace timer and networkctl.
type fakeLinks struct {
	armed        map[string]int
	expire       map[string]context.CancelCauseFunc
	armErr       error
	reconfigured map[string]int
	reconfigure  error
	events       strings.Builder
}

func newFakeOwner(t *testing.T, dir string) (*fakeLinks, *owner) {
	t.Helper()
	fake := &fakeLinks{armed: map[string]int{}, expire: map[string]context.CancelCauseFunc{}, reconfigured: map[string]int{}}
	o := &owner{
		armGrace: func(ctx context.Context, link string) (context.Context, context.CancelFunc, error) {
			if fake.armErr != nil {
				return nil, nil, fake.armErr
			}
			fake.armed[link]++
			ctx, cancel := context.WithCancelCause(ctx)
			fake.expire[link] = cancel
			return ctx, func() { cancel(context.Canceled) }, nil
		},
		reconfigure: func(_ context.Context, link string) error {
			fake.reconfigured[link]++
			return fake.reconfigure
		},
		gracePeriod: 20 * time.Second,
		dir:         dir,
		events:      &fake.events,
		now:         func() time.Time { return time.Date(2026, 11, 27, 6, 0, 0, 0, time.UTC) },
	}
	t.Cleanup(o.stop)
	return fake, o
}

func (f *fakeLinks) elapse(link string) { f.expire[link](context.DeadlineExceeded) }

func readRecords(t *testing.T, dir string) []Record {
	t.Helper()
	records, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestReconfiguresOnceAfterGrace(t *testing.T) {
	dir := t.TempDir()
	fake, o := newFakeOwner(t, dir)

	for range 3 {
		o.observe(t.Context(), []string{"enp1s0"})
	}
	if fake.reconfigured["enp1s0"] != 0 || len(readRecords(t, dir)) != 0 {
		t.Fatal("reconfigured before the grace period elapsed")
	}

	fake.elapse("enp1s0")
	for range 3 {
		o.observe(t.Context(), []string{"enp1s0"})
	}
	if fake.reconfigured["enp1s0"] != 1 {
		t.Fatalf("reconfigured %d times after the grace period, want 1", fake.reconfigured["enp1s0"])
	}

	// A link that agrees and later disagrees again in the same boot is not
	// retried, however long the disagreement lasts.
	o.observe(t.Context(), nil)
	o.observe(t.Context(), []string{"enp1s0"})
	o.observe(t.Context(), []string{"enp1s0"})
	if fake.reconfigured["enp1s0"] != 1 || fake.armed["enp1s0"] != 1 {
		t.Fatalf("reconfigured %d times with %d grace periods, want 1 and 1", fake.reconfigured["enp1s0"], fake.armed["enp1s0"])
	}

	records := readRecords(t, dir)
	if len(records) != 1 || records[0].Link != "enp1s0" || records[0].Outcome != Reconfigured || records[0].Error != "" {
		t.Fatalf("records = %+v, want one successful reconfiguration of enp1s0", records)
	}
	lines := strings.Split(strings.TrimSpace(fake.events.String()), "\n")
	const graceStarted = "required link enp1s0: systemd-networkd reports no carrier while sysfs reports carrier up; reconfiguring once if this persists for 20s"
	if len(lines) != 2 || lines[0] != graceStarted || lines[1] != records[0].String() {
		t.Fatalf("events = %q, want the grace period starting, then the record", lines)
	}
}

func TestRestartsGraceWhenLinkAgrees(t *testing.T) {
	fake, o := newFakeOwner(t, t.TempDir())

	o.observe(t.Context(), []string{"enp1s0"})
	first := fake.expire["enp1s0"]
	// networkd caught up before the grace period elapsed.
	o.observe(t.Context(), nil)
	first(context.DeadlineExceeded)
	o.observe(t.Context(), nil)
	if fake.reconfigured["enp1s0"] != 0 {
		t.Fatal("reconfigured a link that networkd reported with carrier")
	}

	o.observe(t.Context(), []string{"enp1s0"})
	o.observe(t.Context(), []string{"enp1s0"})
	if fake.reconfigured["enp1s0"] != 0 {
		t.Fatal("reconfigured before the new grace period elapsed")
	}
	fake.elapse("enp1s0")
	o.observe(t.Context(), []string{"enp1s0"})
	if fake.reconfigured["enp1s0"] != 1 {
		t.Fatalf("reconfigured %d times, want 1", fake.reconfigured["enp1s0"])
	}
}

func TestLinksHaveIndependentGraces(t *testing.T) {
	dir := t.TempDir()
	fake, o := newFakeOwner(t, dir)

	o.observe(t.Context(), []string{"enp1s0"})
	fake.elapse("enp1s0")
	o.observe(t.Context(), []string{"enp1s0"})
	if fake.reconfigured["enp1s0"] != 1 {
		t.Fatalf("enp1s0 reconfigured %d times, want 1", fake.reconfigured["enp1s0"])
	}

	// enp2s0 begins to disagree later, while enp1s0 still does.
	o.observe(t.Context(), []string{"enp1s0", "enp2s0"})
	o.observe(t.Context(), []string{"enp1s0", "enp2s0"})
	if fake.reconfigured["enp2s0"] != 0 {
		t.Fatal("enp2s0 reconfigured on enp1s0's grace period")
	}
	fake.elapse("enp2s0")
	o.observe(t.Context(), []string{"enp1s0", "enp2s0"})
	if fake.reconfigured["enp1s0"] != 1 || fake.reconfigured["enp2s0"] != 1 {
		t.Fatalf("reconfigured %v, want each link once", fake.reconfigured)
	}

	records := readRecords(t, dir)
	if len(records) != 2 || records[0].Link != "enp1s0" || records[1].Link != "enp2s0" {
		t.Fatalf("records = %+v, want one per link", records)
	}
	events := fake.events.String()
	if strings.Count(events, "enp1s0: ") != 1 || strings.Count(events, "enp2s0: ") != 1 || !strings.Contains(events, records[0].String()) || !strings.Contains(events, records[1].String()) {
		t.Fatalf("events = %q, want one grace period and one record per link", events)
	}
}

func TestArmFailureMakesNoAttempt(t *testing.T) {
	dir := t.TempDir()
	fake, o := newFakeOwner(t, dir)
	fake.armErr = errors.New("systemctl start failed")

	for range 3 {
		o.observe(t.Context(), []string{"enp1s0"})
	}
	// The timer works again, but the link already settled for this run.
	fake.armErr = nil
	o.observe(t.Context(), []string{"enp1s0"})

	if fake.reconfigured["enp1s0"] != 0 || fake.armed["enp1s0"] != 0 || len(readRecords(t, dir)) != 0 {
		t.Fatalf("reconfigured %d times with %d grace periods after the arm failed, want none", fake.reconfigured["enp1s0"], fake.armed["enp1s0"])
	}
	events := fake.events.String()
	if strings.Count(events, "enp1s0") != 1 || !strings.Contains(events, "systemctl start failed") || strings.Contains(events, "persists for") {
		t.Fatalf("events = %q, want one line naming enp1s0 and the error", events)
	}
}

func TestRecordsFailedReconfigure(t *testing.T) {
	dir := t.TempDir()
	fake, o := newFakeOwner(t, dir)
	fake.reconfigure = errors.New("networkctl reconfigure: exit status 1: access denied")

	o.observe(t.Context(), []string{"enp1s0"})
	fake.elapse("enp1s0")
	o.observe(t.Context(), []string{"enp1s0"})

	records := readRecords(t, dir)
	if len(records) != 1 || records[0].Outcome != Failed || records[0].Error != fake.reconfigure.Error() {
		t.Fatalf("records = %+v, want the failed reconfiguration", records)
	}
	summary := Summary(records)
	if strings.Contains(summary, "was reconfigured") || !strings.Contains(summary, "could not be reconfigured") || !strings.Contains(summary, "access denied") {
		t.Fatalf("summary = %q, want the failure and its error", summary)
	}
	if lines := strings.Split(strings.TrimSpace(fake.events.String()), "\n"); len(lines) != 2 || lines[1] != summary {
		t.Fatalf("events = %q, want the grace period starting, then the record", lines)
	}
}

func TestNoSecondAttemptAfterRestart(t *testing.T) {
	dir := t.TempDir()
	first, o := newFakeOwner(t, dir)
	o.observe(t.Context(), []string{"enp1s0"})
	first.elapse("enp1s0")
	o.observe(t.Context(), []string{"enp1s0"})
	o.stop()
	before, err := os.ReadFile(filepath.Join(dir, "enp1s0"))
	if err != nil {
		t.Fatal(err)
	}

	// A restarted owner shares the boot's records.
	second, restarted := newFakeOwner(t, dir)
	for range 3 {
		restarted.observe(t.Context(), []string{"enp1s0"})
	}
	if second.armed["enp1s0"] != 0 || second.reconfigured["enp1s0"] != 0 {
		t.Fatalf("restarted owner armed %d grace periods and reconfigured %d times, want none", second.armed["enp1s0"], second.reconfigured["enp1s0"])
	}
	after, err := os.ReadFile(filepath.Join(dir, "enp1s0"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("record changed across restart: %s -> %s (%v)", before, after, err)
	}
	if second.events.Len() != 0 {
		t.Fatalf("restarted owner reported %q", second.events.String())
	}
}

func TestClaimIsExclusive(t *testing.T) {
	dir := t.TempDir()
	record := Record{Link: "enp1s0", Outcome: Attempted}
	if claimed, err := claim(dir, record); !claimed || err != nil {
		t.Fatalf("first claim = %t, %v", claimed, err)
	}
	if claimed, err := claim(dir, record); claimed || err != nil {
		t.Fatalf("second claim = %t, %v; want refused", claimed, err)
	}
	if _, err := claim(dir, Record{Link: "../enp1s0"}); err == nil {
		t.Fatal("claimed a link name outside the record directory")
	}
	records := readRecords(t, dir)
	if len(records) != 1 || records[0].Outcome != Attempted || !strings.Contains(records[0].String(), "result was not recorded") {
		t.Fatalf("records = %+v, want one attempt without a result", records)
	}
}
