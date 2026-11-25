package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/katl-dev/katl/internal/persistedrecord"
	"golang.org/x/sys/unix"
)

const watchRetention = 7 * 24 * time.Hour

type watchRecord struct {
	ID         string        `json:"id"`
	PR         int           `json:"pr"`
	Registered time.Time     `json:"registered"`
	SeenQueued bool          `json:"seenQueued,omitempty"`
	Head       string        `json:"head,omitempty"`
	HeadSince  time.Time     `json:"headSince,omitzero"`
	Fired      time.Time     `json:"fired,omitzero"`
	Outcome    *watchOutcome `json:"outcome,omitempty"`
}

func (r watchRecord) expired(now time.Time) bool {
	return r.Outcome != nil && !now.Before(r.Fired.Add(watchRetention))
}

type watchRequest struct {
	ID     string `json:"id,omitempty"`
	PR     int    `json:"pr,omitempty"`
	Resume string `json:"resume,omitempty"`
	List   bool   `json:"list,omitempty"`
}

type watchReply struct {
	ID      string        `json:"id,omitempty"`
	Watches []watchRecord `json:"watches,omitempty"`
	Outcome *watchOutcome `json:"outcome,omitempty"`
	Error   string        `json:"error,omitempty"`
	Blocked string        `json:"blocked,omitempty"`
	Retry   string        `json:"retry,omitempty"`
}

type watchRetry struct {
	at       time.Time
	failures int
}

type watchDaemon struct {
	query         watchQuery
	notify        func(context.Context, string)
	log           io.Writer
	path          string
	write         func(string, []byte, os.FileMode) error
	now           func() time.Time
	interval      time.Duration
	notifications chan string

	mu      sync.Mutex
	records map[string]watchRecord
	changed chan struct{}
	blocked string
	denied  map[int]string

	// Only the poll loop reads or writes staged observations and retry state.
	staged       []watchRecord
	retries      map[int]watchRetry
	lastPolled   int
	ratePath     string
	rateUntil    time.Time
	rateUnsaved  bool
	rateFailures int
}

func loadWatchDaemon(dir string, interval time.Duration, log io.Writer) (*watchDaemon, error) {
	d := &watchDaemon{
		log: log, path: filepath.Join(dir, "watches.json"), ratePath: filepath.Join(dir, "rate-limit.json"), now: time.Now,
		write: persistedrecord.WriteFileAtomic, interval: interval, notifications: make(chan string, 8),
		records: map[string]watchRecord{}, changed: make(chan struct{}), denied: map[int]string{},
		retries: map[int]watchRetry{},
	}
	data, err := os.ReadFile(d.ratePath)
	if err == nil {
		if err := d.rateUntil.UnmarshalJSON(data); err != nil {
			return nil, fmt.Errorf("read watch throttle %s: %w", d.ratePath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	data, err = os.ReadFile(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return nil, err
	}
	var records []watchRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("read watch state %s: %w", d.path, err)
	}
	for _, record := range records {
		if record.ID == "" || !validPRNumber(record.PR) || record.Registered.IsZero() ||
			(record.Outcome == nil) != record.Fired.IsZero() {
			return nil, fmt.Errorf("invalid watch in %s", d.path)
		}
		d.records[record.ID] = record
	}
	return d, nil
}

func sortedWatches(records map[string]watchRecord) []watchRecord {
	return slices.SortedFunc(maps.Values(records), func(a, b watchRecord) int {
		if diff := a.PR - b.PR; diff != 0 {
			return diff
		}
		if cmp := a.Registered.Compare(b.Registered); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// save runs under mu. Readers and notifications only use the replacement
// snapshot after the atomic write succeeds.
func (d *watchDaemon) save(records map[string]watchRecord, outcomes []watchRecord) error {
	data, err := json.Marshal(sortedWatches(records))
	if err != nil {
		return err
	}
	if err := d.write(d.path, data, 0o600); err != nil {
		return err
	}
	d.records = records
	if len(outcomes) == 0 {
		return nil
	}
	d.signalChange()
	notified := map[string]bool{}
	for _, record := range outcomes {
		outcome := record.Outcome
		fmt.Fprintf(d.log, "%s (exit %d)\n", outcome.Message, outcome.Code)
		if d.notify != nil && outcome.Code != 2 && !notified[outcome.Message] {
			notified[outcome.Message] = true
			select {
			case d.notifications <- outcome.Message:
			default:
				fmt.Fprintf(d.log, "notification queue full; dropping %q\n", outcome.Message)
			}
		}
	}
	return nil
}

func (d *watchDaemon) signalChange() {
	close(d.changed)
	d.changed = make(chan struct{})
}

func (d *watchDaemon) register(req watchRequest) (ack watchReply, replay bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if record, exists := d.records[req.ID]; exists {
		if record.expired(d.now()) || record.PR != req.PR {
			return watchReply{Error: fmt.Sprintf("watch ID %q is expired or already registered for another PR", req.ID)}, false
		}
		return watchReply{ID: record.ID}, true
	}
	record := watchRecord{PR: req.PR, ID: req.ID, Registered: d.now()}
	records := maps.Clone(d.records)
	records[record.ID] = record
	if err := d.save(records, nil); err != nil {
		return watchReply{Retry: fmt.Sprintf("persist watch registration: %v", err)}, false
	}
	return watchReply{ID: record.ID}, false
}

func (d *watchDaemon) list() []watchRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	return slices.DeleteFunc(sortedWatches(d.records), func(r watchRecord) bool { return r.expired(now) })
}

func (d *watchDaemon) lookup(id string, replay bool) (watchReply, <-chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	record, exists := d.records[id]
	switch {
	case !exists || record.expired(d.now()):
		return watchReply{Error: fmt.Sprintf("watch %q is unknown or expired", id)}, nil
	case record.Outcome != nil:
		if replay {
			outcome := *record.Outcome
			head := record.Head
			if head == "" {
				head = "unknown"
			}
			outcome.Message = fmt.Sprintf("recorded %s for head %s: %s; after a push, start a fresh `katl-queue wait PR`",
				record.Fired.Format(time.RFC3339), head, strings.TrimSuffix(outcome.Message, "."))
			return watchReply{Outcome: &outcome}, nil
		}
		return watchReply{Outcome: record.Outcome}, nil
	case d.blocked != "":
		return watchReply{Blocked: d.blocked}, nil
	case d.denied[record.PR] != "":
		return watchReply{Blocked: d.denied[record.PR]}, nil
	default:
		return watchReply{}, d.changed
	}
}

func (d *watchDaemon) updateAccess(blocked string, numbers []int, denied map[int]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	changed := d.blocked != blocked
	d.blocked = blocked
	for _, n := range numbers {
		if d.denied[n] != denied[n] {
			changed = true
			if denied[n] == "" {
				delete(d.denied, n)
			} else {
				d.denied[n] = denied[n]
			}
		}
	}
	if changed {
		d.signalChange()
	}
}

func (d *watchDaemon) flush() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	records := maps.Clone(d.records)
	var outcomes []watchRecord
	for _, record := range d.staged {
		if record.Outcome != nil {
			record.Fired = now
			outcomes = append(outcomes, record)
		}
		records[record.ID] = record
	}
	maps.DeleteFunc(records, func(_ string, r watchRecord) bool { return r.expired(now) })
	if len(d.staged) == 0 && len(records) == len(d.records) {
		return true
	}
	if err := d.save(records, outcomes); err != nil {
		fmt.Fprintf(d.log, "could not persist watches: %v; retrying\n", err)
		return false
	}
	d.staged = nil
	return true
}

func (d *watchDaemon) activeWatches() map[int][]watchRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	active := map[int][]watchRecord{}
	for _, record := range d.records {
		if record.Outcome == nil {
			active[record.PR] = append(active[record.PR], record)
		}
	}
	// Alias-level access failures matter only while a PR has pending waits.
	maps.DeleteFunc(d.denied, func(pr int, _ string) bool { return len(active[pr]) == 0 })
	return active
}

func watchBackoff(interval time.Duration, failures int) time.Duration {
	delay := interval
	for i := 1; i < failures && delay < 15*time.Minute; i++ {
		delay = min(2*delay, 15*time.Minute)
	}
	return delay
}

func (d *watchDaemon) pollOnce(ctx context.Context) {
	if !d.flush() || !d.saveRateLimit() || d.now().Before(d.rateUntil) {
		return
	}
	active := d.activeWatches()
	maps.DeleteFunc(d.retries, func(pr int, _ watchRetry) bool { return len(active[pr]) == 0 })
	now := d.now()
	maps.DeleteFunc(active, func(pr int, _ []watchRecord) bool { return now.Before(d.retries[pr].at) })

	numbers := slices.Sorted(maps.Keys(active))
	// Start after the last batch GitHub served, even when it rejected the
	// next batch or exhausted its quota in the successful response.
	start, found := slices.BinarySearch(numbers, d.lastPolled)
	if found {
		start++
	}
	if start != 0 && start != len(numbers) {
		numbers = slices.Concat(numbers[start:], numbers[:start])
	}

	limited := false
	for numbers := range slices.Chunk(numbers, watchBatchSize) {
		if ctx.Err() != nil {
			return
		}
		retry, issue := d.pollBatch(ctx, numbers, active)
		if ctx.Err() != nil {
			return
		}
		limited = issue.limited
		for _, n := range numbers {
			if !retry[n] {
				delete(d.retries, n)
				continue
			}
			failures := min(d.retries[n].failures+1, 16)
			d.retries[n] = watchRetry{failures: failures, at: d.now().Add(watchBackoff(d.interval, failures))}
		}
		saved := d.flush()
		if issue.operator {
			d.updateAccess(issue.cause, nil, nil)
		}
		if !saved || issue.operator || limited {
			break
		}
	}
	if !limited && len(active) != 0 {
		d.rateFailures = 0
	}
}

func (d *watchDaemon) saveRateLimit() bool {
	if !d.rateUnsaved {
		return true
	}
	data, err := d.rateUntil.MarshalJSON()
	if err == nil {
		err = d.write(d.ratePath, data, 0o600)
	}
	if err != nil {
		fmt.Fprintf(d.log, "could not persist GitHub throttle deadline: %v; retrying\n", err)
		return false
	}
	d.rateUnsaved = false
	return true
}

func (d *watchDaemon) fetch(ctx context.Context, query string) (watchGraphQL, watchQueryIssue) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	response, err := d.query(ctx, query)
	now := d.now()
	envelope, issue := readWatchQuery(response, err, now)
	if issue.limited {
		d.rateFailures = min(d.rateFailures+1, 16)
		d.rateUntil = issue.retryAt
		if d.rateUntil.IsZero() {
			d.rateUntil = now.Add(watchBackoff(max(d.interval, time.Minute), d.rateFailures))
		}
		d.rateUnsaved = true
		d.saveRateLimit()
	}
	if issue.cause != "" && ctx.Err() != context.Canceled {
		fmt.Fprintln(d.log, issue.cause)
	}
	return envelope, issue
}

func (d *watchDaemon) pollBatch(ctx context.Context, numbers []int, active map[int][]watchRecord) (map[int]bool, watchQueryIssue) {
	now := d.now()
	envelope, issue := d.fetch(ctx, pullWatchQuery(numbers))
	if ctx.Err() != nil || (issue.operator && envelope.Data.Repository == nil) {
		return nil, issue
	}
	// A response that actually saw the repository clears a prior global
	// access failure. A transport failure or throttle provides no evidence.
	canObserve := envelope.Data.Repository != nil
	denied := map[int]string{}
	retry := map[int]bool{}
	for _, n := range numbers {
		evidence := envelope.evidence(n)
		for _, record := range active[n] {
			previous := record
			head := watchString(evidence.pr["headRefOid"])
			if head != "" && head != record.Head {
				// GitHub's commit push time is unavailable. The registration
				// starts the grace period, and observing a new head resets it.
				record.HeadSince = record.Registered
				if record.Head != "" {
					record.HeadSince = now
				}
				record.Head = head
			}
			switch {
			case evidence.absent:
				record.Outcome = &watchOutcome{Code: 2, Message: fmt.Sprintf("pull request %s/%s#%d not found", katlOwner, katlRepo, n)}
			default:
				outcome, conclusive := evaluateWatch(&record, evidence, now, d.interval)
				record.Outcome = outcome
				if outcome == nil {
					denied[n] = evidence.operator
					retry[n] = retry[n] || (!conclusive || evidence.failed) && evidence.operator == ""
				}
			}
			if record.Outcome != nil {
				record.Head = head
			}
			if record != previous {
				d.staged = append(d.staged, record)
			}
		}
	}
	if canObserve {
		d.lastPolled = numbers[len(numbers)-1]
		d.updateAccess("", numbers, denied)
	}
	if issue.limited || issue.operator {
		return nil, issue
	}
	return retry, issue
}

func (d *watchDaemon) pollLoop(ctx context.Context, tick func(context.Context) bool) {
	for tick(ctx) {
		d.pollOnce(ctx)
	}
}
func (d *watchDaemon) serve(ctx context.Context, listener net.Listener, tick func(context.Context) bool) error {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer func() {
		cancel()
		listener.Close()
		stop()
		workers.Wait()
	}()

	workers.Go(func() { d.pollLoop(ctx, tick) })
	if d.notify != nil {
		workers.Go(func() {
			for {
				select {
				case message := <-d.notifications:
					d.notify(ctx, message)
				case <-ctx.Done():
					return
				}
			}
		})
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		workers.Go(func() { d.serveClient(ctx, conn) })
	}
}

func (d *watchDaemon) serveClient(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	request := bufio.NewScanner(conn)
	if !request.Scan() {
		return
	}
	conn.SetReadDeadline(time.Time{})
	replies := json.NewEncoder(conn)

	var req watchRequest
	if json.Unmarshal(request.Bytes(), &req) != nil {
		replies.Encode(watchReply{Error: "invalid watch request"})
		return
	}
	var ack watchReply
	replay := req.Resume != ""
	switch {
	case req.List && req.ID == "" && req.PR == 0 && req.Resume == "":
		replies.Encode(watchReply{Watches: d.list()})
		return
	case req.Resume != "" && req.ID == "" && req.PR == 0 && !req.List:
		initial, _ := d.lookup(req.Resume, false)
		if initial.Error != "" {
			replies.Encode(initial)
			return
		}
		ack.ID = req.Resume
	case !req.List && req.Resume == "" && req.ID != "" && validPRNumber(req.PR):
		ack, replay = d.register(req)
	default:
		replies.Encode(watchReply{Error: "invalid watch request"})
		return
	}
	if replies.Encode(ack) != nil || ack.ID == "" {
		return
	}

	disconnected := make(chan struct{})
	go func() {
		var b [1]byte
		conn.Read(b[:])
		close(disconnected)
	}()
	for {
		reply, changed := d.lookup(ack.ID, replay)
		if changed == nil {
			replies.Encode(reply)
			return
		}
		select {
		case <-changed:
		case <-disconnected:
			return
		case <-ctx.Done():
			return
		}
	}
}

func watchStateDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "katl-queue"), nil
}

func lockWatchState(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "watch.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("lock watch state in %s (another watchd may be running): %w", dir, err)
	}
	return lock, nil
}

func listenWatchSocket() (net.Listener, error) {
	if os.Getenv("LISTEN_PID") == strconv.Itoa(os.Getpid()) {
		count := os.Getenv("LISTEN_FDS")
		os.Unsetenv("LISTEN_PID")
		os.Unsetenv("LISTEN_FDS")
		os.Unsetenv("LISTEN_FDNAMES")
		if count != "1" {
			return nil, errors.New("watchd expects one systemd socket descriptor")
		}
		// systemd starts passing descriptors at fd 3. FileListener takes
		// its own copy so the inherited descriptor can be closed here.
		socket := os.NewFile(3, "systemd watch socket")
		defer socket.Close()
		listener, err := net.FileListener(socket)
		if err != nil {
			return nil, fmt.Errorf("open systemd watch socket: %w", err)
		}
		if listener.Addr().Network() != "unix" {
			listener.Close()
			return nil, errors.New("watchd expects a systemd Unix stream socket")
		}
		return listener, nil
	}

	path, err := watchSocketPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if errors.Is(err, unix.EADDRINUSE) {
		// A killed standalone daemon can leave its socket pathname behind.
		info, statErr := os.Lstat(path)
		if statErr == nil && info.Mode()&os.ModeSocket != 0 {
			conn, dialErr := net.DialTimeout("unix", path, time.Second)
			if dialErr == nil {
				conn.Close()
			} else if errors.Is(dialErr, unix.ECONNREFUSED) {
				if err := os.Remove(path); err != nil {
					return nil, err
				}
				listener, err = net.Listen("unix", path)
			}
		}
	}
	return listener, err
}

func watchd(ctx context.Context, interval time.Duration, notify bool, out io.Writer) error {
	dir, err := watchStateDir()
	if err != nil {
		return err
	}
	lock, err := lockWatchState(dir)
	if err != nil {
		return err
	}
	defer lock.Close()

	d, err := loadWatchDaemon(dir, interval, out)
	if err != nil {
		return err
	}
	listener, err := listenWatchSocket()
	if err != nil {
		return err
	}

	// The service runs outside a checkout. PR numbers and persisted watches
	// always refer to Katl's repository, regardless of the caller's directory.
	d.query = queryWatchGitHub
	if notify {
		path, err := exec.LookPath("notify-send")
		if err != nil {
			fmt.Fprintln(out, "notify-send unavailable; desktop notifications disabled")
		} else {
			d.notify = func(ctx context.Context, message string) {
				ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				if output, err := exec.CommandContext(ctx, path, "--", "katl-queue", message).CombinedOutput(); err != nil {
					fmt.Fprintf(out, "notify-send: %v %s\n", err, strings.TrimSpace(string(output)))
				}
			}
		}
	}
	fmt.Fprintf(out, "watchd listening on %s, interval %s\n", listener.Addr(), interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	return d.serve(ctx, listener, func(ctx context.Context) bool {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			return true
		}
	})
}
