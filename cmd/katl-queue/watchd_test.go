package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

const (
	testInterval = time.Minute
	pr488URL     = "https://github.com/katl-dev/katl/pull/488"
	pr488Head    = "52acd0a3f00efc888cab6dcf520b870c75611d86"

	merged488 = `{"data":{"repository":{"pr488":{"state":"MERGED","merged":true,
		"mergeCommit":{"oid":"b8c94940df9e23c58616cb698df0215277ea326d"}}}}}`
	closed488 = `{"data":{"repository":{"pr488":{"state":"CLOSED","merged":false}}}}`
	idle488   = `{"data":{"repository":{"pr488":{"state":"OPEN","merged":false,
		"autoMergeRequest":null,"mergeQueueEntry":null}}}}`
	pending488 = `{"data":{"repository":{"pr488":{"state":"OPEN","merged":false,
		"mergeable":"MERGEABLE","reviewDecision":null,"mergeQueueEntry":null,
		"autoMergeRequest":{"enabledAt":"2026-11-24T00:39:53Z"},
		"headRefOid":"` + pr488Head + `","commits":{"nodes":[
		{"commit":{"oid":"` + pr488Head + `","statusCheckRollup":{"state":"PENDING"}}}
		]}}}}}`
	failedContexts = `{"contexts":{
		"pageInfo":{"hasNextPage":true},"nodes":[
		{"name":"CI","conclusion":"FAILURE"},{"name":"Lint","conclusion":"SUCCESS"},
		{"context":"build/status","state":"ERROR"}
		]}}`
	checkDetailsError = `{"type":"INTERNAL","path":["repository","pr488","commits","nodes",0,"commit","checkDetails","contexts"],"message":"resolver failed"}`
)

func watchRollup(state, details, issue string) string {
	response := strings.Replace(pending488, `"statusCheckRollup":{"state":"PENDING"}`,
		fmt.Sprintf(`"statusCheckRollup":{"state":%q},"checkDetails":%s`, state, details), 1)
	if issue != "" {
		response = strings.TrimSuffix(response, "}") + `,"errors":[` + issue + `]}`
	}
	return response
}

// Drive the daemon through its clock, socket, and GitHub query boundary.
type pendingWatchQuery struct {
	query string
	reply chan watchHTTP
}

type watchTestTick struct {
	now   time.Time
	ready chan struct{}
}

type watchTest struct {
	t         *testing.T
	path, dir string
	now       atomic.Pointer[time.Time]
	ticks     chan watchTestTick
	queries   chan pendingWatchQuery
	wrap      func(net.Conn) net.Conn
	write     func(string, []byte, os.FileMode) error
}

func newWatchTest(t *testing.T) *watchTest {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path, err := watchSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	w := &watchTest{
		t: t, path: path, dir: t.TempDir(), ticks: make(chan watchTestTick),
		queries: make(chan pendingWatchQuery),
	}
	now := time.Date(2026, 11, 24, 0, 38, 0, 0, time.UTC)
	w.now.Store(&now)
	return w
}

func (w *watchTest) start() func() {
	w.t.Helper()
	d, err := loadWatchDaemon(w.dir, testInterval, io.Discard)
	if err != nil {
		w.t.Fatal(err)
	}
	d.now = func() time.Time { return *w.now.Load() }
	if w.write != nil {
		realWrite := d.write
		d.write = func(path string, data []byte, mode os.FileMode) error {
			if err := w.write(path, data, mode); err != nil {
				return err
			}
			return realWrite(path, data, mode)
		}
	}
	d.query = func(ctx context.Context, query string) (watchHTTP, error) {
		call := pendingWatchQuery{query, make(chan watchHTTP, 1)}
		select {
		case w.queries <- call:
		case <-ctx.Done():
			return watchHTTP{}, ctx.Err()
		}
		select {
		case raw := <-call.reply:
			return raw, nil
		case <-ctx.Done():
			return watchHTTP{}, ctx.Err()
		}
	}
	listener, err := net.Listen("unix", w.path)
	if err != nil {
		w.t.Fatal(err)
	}
	var socket net.Listener = listener
	if w.wrap != nil {
		socket = watchListener{listener, w.wrap}
	}
	ctx, cancel := context.WithCancel(w.t.Context())
	done := make(chan error, 1)
	go func() {
		done <- d.serve(ctx, socket, func(ctx context.Context) bool {
			select {
			case <-ctx.Done():
				return false
			case tick := <-w.ticks:
				w.now.Store(&tick.now)
				close(tick.ready)
				return true
			}
		})
	}()
	stop := sync.OnceFunc(func() {
		cancel()
		if err := receiveWatch(w.t, done); err != nil {
			w.t.Error(err)
		}
	})
	w.t.Cleanup(stop)
	return stop
}

func receiveWatch[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the watcher")
		var zero T
		return zero
	}
}

func (w *watchTest) tick(by time.Duration) {
	w.t.Helper()
	tick := watchTestTick{now: w.now.Load().Add(by), ready: make(chan struct{})}
	select {
	case w.ticks <- tick:
		receiveWatch(w.t, tick.ready)
	case call := <-w.queries:
		w.t.Fatalf("unexpected GitHub query before next tick: %s", call.query)
	case <-time.After(5 * time.Second):
		w.t.Fatal("watch daemon did not take its next tick")
	}
}

func (call pendingWatchQuery) numbers(t *testing.T) []int {
	t.Helper()
	aliases := regexp.MustCompile(`pr([0-9]+): pullRequest\(number: ([0-9]+)\)`).FindAllStringSubmatch(call.query, -1)
	var got []int
	for _, alias := range aliases {
		n, _ := strconv.Atoi(alias[1])
		got = append(got, n)
		if alias[1] != alias[2] {
			t.Errorf("mismatched PR alias and number: %s", alias[0])
		}
	}
	if strings.Contains(call.query, "REMOVED_FROM_MERGE_QUEUE_EVENT") {
		t.Fatalf("poll fetched removal history: %s", call.query)
	}
	return got
}

func (w *watchTest) query(want ...int) pendingWatchQuery {
	w.t.Helper()
	call := receiveWatch(w.t, w.queries)
	got := call.numbers(w.t)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		w.t.Fatalf("polled PRs %v, want %v\n%s", got, want, call.query)
	}
	return call
}

func (w *watchTest) poll(raw string, want ...int) {
	w.t.Helper()
	w.query(want...).reply <- watchHTTP{status: http.StatusOK, body: []byte(raw)}
}

func recordedWatch(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "watch", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

type watchCommandResult struct {
	code int
	text string
}

type watchCommand struct {
	cancel context.CancelFunc
	id     chan string
	result chan watchCommandResult
}

type watchStderr struct {
	bytes.Buffer
	ids chan<- string
}

func (w *watchStderr) Write(p []byte) (int, error) {
	if id, ok := strings.CutPrefix(strings.TrimSpace(string(p)), "resume: katl-queue wait --resume "); ok {
		w.ids <- id
	}
	return w.Buffer.Write(p)
}

func (w *watchTest) command(args ...string) watchCommand {
	w.t.Helper()
	ctx, cancel := context.WithCancel(w.t.Context())
	w.t.Cleanup(cancel)
	cmd := watchCommand{cancel: cancel, id: make(chan string, 1), result: make(chan watchCommandResult, 1)}
	go func() {
		var out bytes.Buffer
		stderr := &watchStderr{ids: cmd.id}
		err := run(ctx, args, &out, stderr)
		code := commandStatus(err, stderr)
		cmd.result <- watchCommandResult{code, out.String() + stderr.String()}
	}()
	return cmd
}

func (w *watchTest) expect(cmd watchCommand, code int, messages ...string) string {
	w.t.Helper()
	got := receiveWatch(w.t, cmd.result)
	if got.code != code {
		w.t.Fatalf("command exited %d with %q, want %d", got.code, got.text, code)
	}
	for _, message := range messages {
		if !strings.Contains(got.text, message) {
			w.t.Fatalf("output %q does not contain %q", got.text, message)
		}
	}
	return got.text
}

func (w *watchTest) expectBlocker(cmd watchCommand, message string) string {
	w.t.Helper()
	got := w.expect(cmd, 1, pr488URL+" "+message)
	if strings.Contains(got, "katl-queue why") {
		w.t.Fatalf("blocker referred to removal history: %s", got)
	}
	return got
}

func (w *watchTest) waiting() (watchCommand, string) {
	w.t.Helper()
	cmd := w.command("wait", "488")
	return cmd, receiveWatch(w.t, cmd.id)
}

func (w *watchTest) persisted() []watchRecord {
	w.t.Helper()
	raw, err := os.ReadFile(filepath.Join(w.dir, "watches.json"))
	if err != nil {
		w.t.Fatal(err)
	}
	var state []watchRecord
	if err := json.Unmarshal(raw, &state); err != nil {
		w.t.Fatal(err)
	}
	return state
}

func TestWaitMergedAfterReenqueue(t *testing.T) {
	w := newWatchTest(t)
	w.start()
	cmd, _ := w.waiting()

	w.tick(testInterval)
	w.poll(recordedWatch(t, "queued.json"), 488)
	w.tick(testInterval)
	// The removal and re-enqueue fell between polls. Only the merge matters.
	w.poll(merged488, 488)
	w.expect(cmd, 0, "#488 merged")
}

func TestWaitQueueDeparture(t *testing.T) {
	w := newWatchTest(t)
	w.start()
	cmd, _ := w.waiting()

	w.tick(testInterval)
	w.poll(recordedWatch(t, "queued.json"), 488)
	w.tick(testInterval)
	w.poll(pending488, 488)
	w.expect(cmd, 1, pr488URL+" left the merge queue (observed outside the queue)", "katl-queue why 488")
}

func TestWaitFailingRollup(t *testing.T) {
	for _, tc := range []struct {
		name, state, details, issue, message string
	}{
		{"complete page", "FAILURE", strings.Replace(failedContexts, "true", "false", 1), "", "failing checks: CI, build/status."},
		{"partial page", "FAILURE", failedContexts, "", "failing checks include: CI, build/status."},
		{"no failures on page", "FAILURE", `{"contexts":{"pageInfo":{"hasNextPage":true},
			"nodes":[{"name":"Lint","conclusion":"SUCCESS"}]}}`, "", "a check failed."},
		{"details resolver failed", "FAILURE", "null", checkDetailsError, "a check failed."},
		{"error rollup", "ERROR", "null", "", "a check failed."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWatchTest(t)
			w.start()
			cmd, _ := w.waiting()

			w.tick(testInterval)
			w.poll(pending488, 488)
			w.tick(testInterval)
			call := w.query(488)
			call.reply <- watchHTTP{status: http.StatusOK, body: []byte(watchRollup(tc.state, tc.details, tc.issue))}
			got := w.expectBlocker(cmd, tc.message)
			if strings.Contains(got, "Lint") {
				t.Fatalf("named a passing check: %s", got)
			}
			for _, field := range []string{"headRefOid", "commits(last: 1)", "statusCheckRollup { state }", "contexts(first: 50)", "hasNextPage"} {
				if !strings.Contains(call.query, field) {
					t.Errorf("poll query omitted %q: %s", field, call.query)
				}
			}
			w.tick(testInterval)
			w.tick(testInterval) // The failing rollup was committed without another query.
		})
	}
}

func TestWaitQueuedFailingRollup(t *testing.T) {
	w := newWatchTest(t)
	w.start()
	cmd, _ := w.waiting()

	w.tick(testInterval)
	w.poll(strings.Replace(watchRollup("FAILURE", failedContexts, ""), `"mergeQueueEntry":null`,
		`"mergeQueueEntry":{"state":"AWAITING_CHECKS"}`, 1), 488)
	w.tick(testInterval)
	w.poll(merged488, 488)
	w.expect(cmd, 0, "#488 merged")
}

func TestWaitNoCI(t *testing.T) {
	nullRollup := strings.Replace(pending488, `"statusCheckRollup":{"state":"PENDING"}`,
		`"statusCheckRollup":null,"checkDetails":null`, 1)
	for _, tc := range []struct {
		name, response string
	}{
		{"null rollup", nullRollup},
		{"empty rollup", watchRollup("PENDING", `{"contexts":{"checkRunCount":0,"statusContextCount":0}}`, "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWatchTest(t)
			w.start()
			cmd, _ := w.waiting()

			w.tick(9 * time.Minute)
			w.poll(tc.response, 488)
			w.tick(time.Minute)
			w.poll(tc.response, 488)
			w.expectBlocker(cmd, "no CI started for head "+pr488Head+"; close and reopen the PR or push to retrigger")
		})
	}
}

func TestWaitRequiresEvidenceOfNoCI(t *testing.T) {
	for _, tc := range []struct {
		name, response string
	}{
		{"check run", watchRollup("PENDING", `{"contexts":{"checkRunCount":1,"statusContextCount":0}}`, "")},
		{"commit status", watchRollup("PENDING", `{"contexts":{"checkRunCount":0,"statusContextCount":1}}`, "")},
		{"missing fields", pending488},
		{"resolver error", strings.Replace(watchRollup("PENDING", "null", checkDetailsError),
			`"statusCheckRollup":{"state":"PENDING"}`, `"statusCheckRollup":null`, 1)},
		{"wrong commit", strings.Replace(watchRollup("PENDING", `{"contexts":{"checkRunCount":0,"statusContextCount":0}}`, ""),
			`"oid":"`+pr488Head+`"`, `"oid":"different"`, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWatchTest(t)
			w.start()
			cmd, _ := w.waiting()

			w.tick(11 * time.Minute)
			w.poll(tc.response, 488)
			w.tick(time.Minute)
			w.poll(merged488, 488)
			w.expect(cmd, 0, "#488 merged")
		})
	}
}

func TestWaitNoCIGraceRestartsOnHeadChange(t *testing.T) {
	w := newWatchTest(t)
	stop := w.start()
	cmd, _ := w.waiting()
	const newHead = "60e174815286b68cb51f9157fba1c6793e99a5ea"
	response := strings.ReplaceAll(watchRollup("PENDING", `{"contexts":{"checkRunCount":0,"statusContextCount":0}}`, ""),
		pr488Head, newHead)

	w.tick(time.Minute)
	w.poll(pending488, 488)
	w.tick(10 * time.Minute)
	w.poll(response, 488)
	w.tick(0)
	call := w.query(488)
	call.reply <- watchHTTP{status: http.StatusOK, body: []byte(response)}
	stop()

	w.start()
	w.tick(9 * time.Minute)
	w.poll(response, 488)
	w.tick(time.Minute)
	w.poll(response, 488)
	w.expectBlocker(cmd, "no CI started for head "+newHead+"; close and reopen the PR or push to retrigger")
}

func TestWaitBlockers(t *testing.T) {
	failed := watchRollup("FAILURE", "null", checkDetailsError)
	for _, tc := range []struct {
		response, message string
	}{
		{closed488, "closed without merging"},
		{strings.Replace(failed, "MERGEABLE", "CONFLICTING", 1), "has a merge conflict"},
		{strings.Replace(failed, `"reviewDecision":null`, `"reviewDecision":"CHANGES_REQUESTED"`, 1), "has changes requested in review"},
	} {
		t.Run(tc.message, func(t *testing.T) {
			w := newWatchTest(t)
			w.start()
			cmd, _ := w.waiting()

			w.tick(testInterval)
			w.poll(tc.response, 488)
			w.expectBlocker(cmd, tc.message)
		})
	}
}

func TestWaitAdmissionGrace(t *testing.T) {
	w := newWatchTest(t)
	w.start()
	cmd, _ := w.waiting()

	w.tick(testInterval + time.Second)
	w.poll(idle488, 488)
	w.tick(testInterval)
	w.poll(idle488, 488)
	w.expectBlocker(cmd, "is outside the queue and auto-merge is off")
}

func TestWaitAdmissionGraceAfterInconclusivePolls(t *testing.T) {
	w := newWatchTest(t)
	w.start()
	cmd, _ := w.waiting()

	for _, advance := range []time.Duration{0, testInterval} {
		w.tick(advance)
		w.poll(`{"data":{"repository":{"pr488":null}}}`, 488)
	}
	w.tick(testInterval)
	w.tick(testInterval) // The second inconclusive poll backs off for two intervals.
	w.poll(idle488, 488)
	w.expectBlocker(cmd, "is outside the queue and auto-merge is off")
}

func TestWaitQueueHistorySurvivesRestart(t *testing.T) {
	w := newWatchTest(t)
	stop := w.start()
	first, firstID := w.waiting()

	w.tick(testInterval)
	w.poll(recordedWatch(t, "queued.json"), 488)
	w.tick(testInterval)
	// A null queue entry from a resolver error proves neither absence nor departure.
	w.poll(`{"data":{"repository":{"pr488":{"merged":false,"state":"OPEN","mergeQueueEntry":null}}},
		"errors":[{"type":"NOT_FOUND","path":["repository","pr488","mergeQueueEntry"],"message":"resolver failed"}]}`, 488)
	w.tick(0) // This rendezvous follows the previous poll, regardless of retry delay.
	stop()

	w.start()
	second, secondID := w.waiting()
	if firstID == secondID {
		t.Fatal("two invocations received the same ID")
	}
	w.tick(testInterval)
	w.poll(pending488, 488)
	w.expect(w.command("wait", "--resume", firstID), 1, "observed outside the queue")
	w.expect(first, 1, "observed outside the queue")

	w.tick(testInterval)
	w.poll(merged488, 488)
	w.expect(second, 0, "#488 merged")
}

func TestWaitNotFound(t *testing.T) {
	w := newWatchTest(t)
	w.start()
	cmd, id := w.waiting()

	w.tick(testInterval)
	w.poll(`{"data":{"repository":{"pr488":null}},
		"errors":[{"type":"NOT_FOUND","path":["repository","pr488","mergeQueueEntry"],"message":"inner resource missing"}]}`, 488)
	w.tick(testInterval)
	w.poll(`{"data":{"repository":{"pr488":null}}}`, 488)
	w.tick(2 * testInterval)
	w.poll(`{"data":{"repository":{"pr488":null}},
		"errors":[{"type":"NOT_FOUND","path":["repository","pr488"],"message":"Could not resolve to a PullRequest with the number of 488."}]}`, 488)
	w.expect(cmd, 2, "katl-dev/katl#488", "not found")
	w.tick(testInterval)
	w.tick(testInterval) // No query occurs between these ticks.
	w.expect(w.command("wait", "--resume", id), 2, "katl-dev/katl#488", "not found")
}

func TestWaitPollEvidence(t *testing.T) {
	w := newWatchTest(t)
	w.start()
	good, _ := w.waiting()
	sibling := w.command("wait", "457")
	receiveWatch(t, sibling.id)

	w.tick(testInterval)
	w.poll(`{"data":{"repository":{"pr457":{"merged":"true","state":"CLOSED"},"pr488":{"merged":true,"state":null}}},
		"errors":[{"type":"INTERNAL","path":["repository","pr488","state"],"message":"resolver failed"}]}`, 457, 488)
	w.expect(good, 0, "#488 merged")

	w.tick(testInterval)
	w.poll(`{"data":{"repository":{"pr457":{"merged":true}}}}`, 457)
	w.expect(sibling, 0, "#457 merged")
}

func TestWatchPollDiscardsLateResult(t *testing.T) {
	d, err := loadWatchDaemon(t.TempDir(), testInterval, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		d.records["watch"] = watchRecord{ID: "watch", PR: 488, Registered: time.Now()}
		d.query = func(ctx context.Context, _ string) (watchHTTP, error) {
			<-ctx.Done()
			return watchHTTP{status: http.StatusOK, body: []byte(merged488)}, nil
		}

		d.pollOnce(t.Context())

		if outcome := d.records["watch"].Outcome; outcome != nil {
			t.Fatalf("watch settled from a reply after its query budget: %+v", outcome)
		}
	})
}

func TestWaitMalformedGraphQLError(t *testing.T) {
	w := newWatchTest(t)
	w.start()
	affected := w.command("wait", "457")
	receiveWatch(t, affected.id)
	w.tick(testInterval)
	w.poll(`{"data":{"repository":{"pr457":{"merged":false,"state":"OPEN","mergeQueueEntry":{"state":"QUEUED"}}}}}`, 457)

	merged, _ := w.waiting()
	missing := w.command("wait", "455")
	receiveWatch(t, missing.id)
	w.tick(testInterval)
	w.poll(`{"data":{"repository":{
		"pr455":null,
		"pr457":{"merged":false,"state":"OPEN","mergeQueueEntry":null},
		"pr488":{"merged":true}
	}},"errors":[
		{"extensions":{"code":12},"message":"resolver failed","path":["repository","pr457","mergeQueueEntry"]},
		{"type":"FORBIDDEN","message":"no access","extensions":{"code":12}},
		{"type":"NOT_FOUND","path":["repository","pr455"],"message":"Pull request not found"}
	]}`, 455, 457, 488)
	w.expect(merged, 0, "#488 merged")
	w.expect(missing, 2, "katl-dev/katl#455", "not found")

	w.tick(testInterval)
	w.poll(`{"data":{"repository":{"pr457":{"merged":true}}}}`, 457)
	w.expect(affected, 0, "#457 merged")
}

func TestWaitAuthenticationRecovery(t *testing.T) {
	w := newWatchTest(t)
	w.start()
	cmd, id := w.waiting()

	w.tick(testInterval)
	w.query(488).reply <- watchHTTP{status: http.StatusUnauthorized, body: []byte(`{"message":"Bad credentials"}`)}
	w.expect(cmd, 2, "Bad credentials", "resume: katl-queue wait --resume "+id)
	w.expect(w.command("wait", "--resume", id), 2, "Bad credentials", "resume: katl-queue wait --resume "+id)
	newWait, otherID := w.waiting()
	w.expect(newWait, 2, "Bad credentials", "resume: katl-queue wait --resume "+otherID)

	w.tick(testInterval)
	w.poll(merged488, 488)
	w.tick(testInterval)
	w.expect(w.command("wait", "--resume", id), 0, "#488 merged")
}

func TestWatchRateLimit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response func(time.Time) watchHTTP
		delay    time.Duration
	}{
		{"retry after", func(_ time.Time) watchHTTP {
			return watchHTTP{status: http.StatusForbidden,
				header: http.Header{"Retry-After": {"180"}},
				body:   []byte(`{"message":"You have exceeded a secondary rate limit."}`)}
		}, 3 * time.Minute},
		{"primary reset", func(now time.Time) watchHTTP {
			header := make(http.Header)
			header.Set("X-RateLimit-Remaining", "0")
			header.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(4*time.Minute).Unix(), 10))
			return watchHTTP{status: http.StatusOK, header: header, body: []byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`)}
		}, 4 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWatchTest(t)
			stop := w.start()
			committed, committedID := w.waiting()
			w.tick(testInterval)
			w.poll(merged488, 488)
			w.expect(committed, 0, "#488 merged")

			cmd, _ := w.waiting()
			w.tick(testInterval)
			w.query(488).reply <- tc.response(*w.now.Load())
			w.tick(0) // Wait for rate limiting to be processed at the original time.
			stop()
			w.start()
			w.tick(tc.delay - time.Second)
			w.tick(0) // The previous tick must not query.
			w.expect(w.command("wait", "--resume", committedID), 0, "#488 merged")

			w.tick(time.Second)
			w.poll(merged488, 488)
			w.expect(cmd, 0, "#488 merged")
		})
	}
}

func TestWatchPollsEveryPRWithOneBatchPerReset(t *testing.T) {
	w := newWatchTest(t)
	w.start()

	const pending = 26
	for n := 1; n <= pending; n++ {
		wait := w.command("wait", strconv.Itoa(n))
		receiveWatch(t, wait.id)
	}

	seen := map[int]bool{}
	for range 2 {
		w.tick(testInterval)
		call := receiveWatch(t, w.queries)
		numbers := call.numbers(t)
		if len(numbers) == 0 || len(numbers) > 25 {
			t.Fatalf("invalid batch of %d PRs", len(numbers))
		}
		var prs []string
		for _, n := range numbers {
			seen[n] = true
			prs = append(prs, fmt.Sprintf(`"pr%d":{"merged":false,"state":"OPEN","mergeQueueEntry":null,"autoMergeRequest":{"enabledAt":"2026-11-24T00:38:00Z"}}`, n))
		}
		header := make(http.Header)
		header.Set("X-RateLimit-Remaining", "0")
		header.Set("X-RateLimit-Reset", strconv.FormatInt(w.now.Load().Add(testInterval).Unix(), 10))
		call.reply <- watchHTTP{status: http.StatusOK, header: header,
			body: []byte(`{"data":{"repository":{` + strings.Join(prs, ",") + `}}}`)}
		w.tick(0) // A quota-consuming response must end the tick.
	}
	for n := 1; n <= pending; n++ {
		if !seen[n] {
			t.Errorf("PR %d was still unpolled after two successful batches", n)
		}
	}
}

func TestWatchThrottleStopsLaterBatches(t *testing.T) {
	w := newWatchTest(t)
	w.start()
	cmd, _ := w.waiting()

	var firstBatch []int
	for n := 1; n <= 25; n++ {
		other := w.command("wait", strconv.Itoa(n))
		receiveWatch(t, other.id)
		firstBatch = append(firstBatch, n)
	}

	w.tick(testInterval)
	w.query(firstBatch...).reply <- watchHTTP{status: http.StatusTooManyRequests}
	w.tick(0) // Completing the first request must not start the next batch.
	w.tick(testInterval)
	w.query(firstBatch...).reply <- watchHTTP{status: http.StatusTooManyRequests}
	w.tick(0)
	w.tick(testInterval)
	w.tick(0) // The second secondary limit doubles the fallback.
	w.tick(testInterval)
	w.poll(`{"data":{"repository":{}}}`, firstBatch...)
	w.poll(merged488, 488)
	w.expect(cmd, 0, "#488 merged")
}

func TestWatchServerErrorKeepsWaitPending(t *testing.T) {
	w := newWatchTest(t)
	w.start()
	cmd, _ := w.waiting()

	w.tick(testInterval)
	w.query(488).reply <- watchHTTP{status: http.StatusBadGateway, body: []byte(merged488)}
	w.tick(testInterval)
	w.poll(closed488, 488)
	w.expect(cmd, 1, "closed without merging")
}

func TestWaitCrashBeforeCommit(t *testing.T) {
	w := newWatchTest(t)
	failedWrite := make(chan struct{}, 1)
	w.write = func(_ string, data []byte, _ os.FileMode) error {
		if bytes.Contains(data, []byte(`"outcome"`)) {
			failedWrite <- struct{}{}
			return errors.New("injected failure before atomic write")
		}
		return nil
	}
	stop := w.start()
	cmd, id := w.waiting()

	w.tick(testInterval)
	w.poll(merged488, 488)
	receiveWatch(t, failedWrite)
	w.expect(w.command("watches"), 0, id+" #488: waiting")
	if records := w.persisted(); len(records) != 1 || records[0].Outcome != nil {
		t.Fatalf("uncommitted outcome on disk: %+v", records)
	}
	stop()

	w.write = nil
	w.start()
	w.tick(testInterval)
	w.poll(closed488, 488)
	w.expect(cmd, 1, "closed without merging")
	w.expect(w.command("wait", "--resume", id), 1, "closed without merging")
}

type watchListener struct {
	net.Listener
	wrap func(net.Conn) net.Conn
}

func (l watchListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return l.wrap(conn), nil
}

type watchReplyDrop struct {
	net.Conn
	onWrite func([]byte) error
}

func (c watchReplyDrop) Write(data []byte) (int, error) {
	if err := c.onWrite(data); err != nil {
		return 0, err
	}
	return c.Conn.Write(data)
}

func TestWaitCrashAfterCommit(t *testing.T) {
	w := newWatchTest(t)
	dropped := make(chan struct{}, 1)
	w.wrap = func(conn net.Conn) net.Conn {
		return watchReplyDrop{conn, func(data []byte) error {
			if bytes.Contains(data, []byte(`"outcome"`)) {
				dropped <- struct{}{}
				return io.ErrClosedPipe
			}
			return nil
		}}
	}
	stop := w.start()
	cmd, id := w.waiting()
	w.tick(testInterval)
	w.poll(merged488, 488)
	receiveWatch(t, dropped)
	stop()

	w.wrap = nil
	w.start()
	w.expect(w.command("wait", "--resume", id), 0, "#488 merged")
	w.expect(cmd, 0, "#488 merged")
	w.tick(testInterval)
	w.tick(testInterval) // The already committed watch was never re-polled.
}

func TestWaitRetriesRegistrationBeforePrintingResume(t *testing.T) {
	w := newWatchTest(t)
	dropped := make(chan string, 1)
	retrying := make(chan string, 1)
	release := make(chan struct{})
	var accepted int
	w.wrap = func(conn net.Conn) net.Conn {
		accepted++
		attempt := accepted
		return watchReplyDrop{conn, func(data []byte) error {
			var reply watchReply
			if json.Unmarshal(data, &reply) != nil || reply.ID == "" {
				return nil
			}
			switch attempt {
			case 1:
				dropped <- reply.ID
				return io.ErrClosedPipe
			case 2:
				retrying <- reply.ID
				select {
				case <-release:
				case <-t.Context().Done():
				}
			}
			return nil
		}}
	}
	w.start()
	cmd := w.command("wait", "488")
	firstID := receiveWatch(t, dropped)
	retryID := receiveWatch(t, retrying)
	select {
	case id := <-cmd.id:
		t.Errorf("printed resume for %s before receiving acknowledgement", id)
	default:
	}
	close(release)

	id := receiveWatch(t, cmd.id)
	if id != firstID || id != retryID {
		t.Fatalf("registration changed IDs: original %q, retry %q, resume %q", firstID, retryID, id)
	}
	if records := w.persisted(); len(records) != 1 || records[0].ID != id || records[0].PR != 488 {
		t.Fatalf("expected one persisted attempt: %+v", records)
	}
	w.tick(testInterval)
	w.poll(merged488, 488)
	w.expect(cmd, 0, "#488 merged")
}

func TestWaitRegistrationRetryDeadline(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	for _, tc := range []struct {
		name    string
		timeout time.Duration
	}{
		{"no timeout", 0},
		{"explicit timeout", 32 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				requests := make(chan watchRequest, 64)
				dial := func(_ context.Context, _ string) (net.Conn, error) {
					client, server := net.Pipe()
					acknowledge := time.Since(start) >= 35*time.Second
					go func() {
						defer server.Close()
						var req watchRequest
						if err := json.NewDecoder(server).Decode(&req); err != nil {
							t.Error(err)
							return
						}
						requests <- req
						if acknowledge {
							reply := json.NewEncoder(server)
							reply.Encode(watchReply{ID: req.ID})
							reply.Encode(watchReply{Outcome: &watchOutcome{Code: 0, Message: "#488 merged"}})
						}
					}()
					return client, nil
				}

				var out, stderr bytes.Buffer
				err := waitForWatch(t.Context(), dial, watchRequest{PR: 488}, tc.timeout, &out, &stderr)
				code := commandStatus(err, &stderr)
				id := (<-requests).ID
				if id == "" {
					t.Fatal("registration did not send an ID")
				}
				for len(requests) > 0 {
					if req := <-requests; req.ID != id || req.PR != 488 || req.Resume != "" {
						t.Fatalf("registration changed before acknowledgement: %+v", req)
					}
				}

				if tc.timeout == 0 {
					if code != 0 || !strings.Contains(stderr.String(), "resume: katl-queue wait --resume "+id) {
						t.Fatalf("wait exited %d after %s: %s%s", code, time.Since(start), &out, &stderr)
					}
				} else {
					if code != 2 || time.Since(start) != tc.timeout || !strings.Contains(stderr.String(), "timed out registering") ||
						strings.Contains(stderr.String(), id) || strings.Contains(stderr.String(), "resume:") {
						t.Fatalf("wait exited %d after %s: %s%s", code, time.Since(start), &out, &stderr)
					}
				}
			})
		})
	}
}

func TestWaitRejectsConflictingRegistration(t *testing.T) {
	w := newWatchTest(t)
	w.start()
	cmd, id := w.waiting()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	conn, err := dialWatch(ctx, w.path)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := exchangeWatch(ctx, conn, watchRequest{ID: id, PR: 457}, func(string) {
		t.Error("watchd acknowledged conflicting registration")
	})
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := commandStatus(reportWatch(reply, &out), &stderr); code != 2 || !strings.Contains(stderr.String(), "registered for another PR") {
		t.Fatalf("conflicting registration exited %d: %s%s", code, &out, &stderr)
	}
	w.tick(testInterval)
	w.poll(merged488, 488)
	w.expect(cmd, 0, "#488 merged")
}

func TestWaitNewAttemptAfterOutcome(t *testing.T) {
	w := newWatchTest(t)
	w.start()
	first, firstID := w.waiting()
	w.tick(testInterval)
	w.poll(closed488, 488)
	w.expect(first, 1, "closed without merging")

	second, secondID := w.waiting()
	if firstID == secondID {
		t.Fatal("a new wait reused the completed ID")
	}
	w.tick(testInterval)
	w.poll(merged488, 488)
	w.expect(second, 0, "#488 merged")
	w.expect(w.command("wait", "--resume", firstID), 1, "closed without merging")
}

func TestWaitReplaysRecordedHeadAfterPush(t *testing.T) {
	w := newWatchTest(t)
	stop := w.start()
	first, id := w.waiting()
	w.tick(testInterval)
	w.poll(watchRollup("FAILURE", `{"contexts":{"nodes":[{"name":"bird","conclusion":"FAILURE"}]}}`, ""), 488)
	verdict := pr488URL + " failing checks: bird"
	w.expect(first, 1, verdict+".")
	stop()

	w.start()
	second, _ := w.waiting()
	const newHead = "60e174815286b68cb51f9157fba1c6793e99a5ea"
	w.tick(testInterval)
	w.poll(strings.ReplaceAll(pending488, pr488Head, newHead), 488)
	want := "recorded 2026-11-24T00:39:00Z for head " + pr488Head + ": " + verdict +
		"; after a push, start a fresh `katl-queue wait PR`"
	w.expect(w.command("wait", "--resume", id), 1, want)

	conn, err := dialWatch(t.Context(), w.path)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := exchangeWatch(t.Context(), conn, watchRequest{ID: id, PR: 488}, func(string) {})
	if err != nil || reply.Outcome == nil || reply.Outcome.Code != 1 || reply.Outcome.Message != want {
		t.Fatalf("replayed registration returned %+v, %v", reply, err)
	}

	w.tick(testInterval)
	w.poll(merged488, 488)
	w.expect(second, 0, "#488 merged")
}

func TestWaitRetainsOutcomeForSevenDays(t *testing.T) {
	w := newWatchTest(t)
	w.start()
	cmd, id := w.waiting()
	w.tick(testInterval)
	w.poll(merged488, 488)
	w.expect(cmd, 0, "#488 merged")

	w.tick(7*24*time.Hour - time.Second)
	w.expect(w.command("wait", "--resume", id), 0, "#488 merged")
	w.tick(time.Second)
	w.expect(w.command("wait", "--resume", id), 2, "unknown or expired")
}

func TestWaitTimeoutKeepsRegistration(t *testing.T) {
	w := newWatchTest(t)
	w.start()
	cmd := w.command("wait", "488", "--timeout", "200ms")
	id := receiveWatch(t, cmd.id)
	w.expect(cmd, 2, "timed out", id)

	w.tick(testInterval)
	w.poll(merged488, 488)
	w.expect(w.command("wait", "--resume", id), 0, "#488 merged")
}

func TestWhyUsesKatlRepository(t *testing.T) {
	removed := recordedWatch(t, "removed.json")
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("PATH", dir)
	t.Setenv("GH_REPO", "other/repo")
	t.Setenv("WHY_TEST_REMOVAL", removed)
	script := `case "$1 $2" in
"repo view") printf '%s\n' 'other/repo' ;;
"api graphql")
  [ "$6 $8 $9 ${10}" = "owner=katl-dev name=katl --hostname github.com" ] || exit 90
  printf '%s' "$WHY_TEST_REMOVAL" ;;
"run view")
  [ "$5 $6" = "--repo github.com/katl-dev/katl" ] || exit 91
  printf '%s\n' 'CI	step	2026-11-24T00:39:53Z	--- FAIL: TestQueueRemoval (0.1s)' ;;
*) exit 92 ;;
esac`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!"+sh+"\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := commandStatus(run(t.Context(), []string{"why", "488"}, &out, &stderr), &stderr)
	if code != 0 || !strings.Contains(out.String(), "#488 removed from the queue") ||
		!strings.Contains(out.String(), "FAIL: TestQueueRemoval") {
		t.Fatalf("why exited %d: %s%s", code, &out, &stderr)
	}
}

func TestWatchLegacyCommand(t *testing.T) {
	dir := t.TempDir()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	script := `case "$1 $2" in
"repo view") printf '%s\n' 'katl-dev/katl' ;;
"api graphql")
  case "$4" in *REMOVED_FROM_MERGE_QUEUE_EVENT*) ;; *) exit 90 ;; esac
  if [ -e "$WATCH_TEST_MARK" ]; then
    printf '%s' "$WATCH_TEST_FINAL"
  else
    : > "$WATCH_TEST_MARK"
    printf '%s' "$WATCH_TEST_INITIAL"
  fi ;;
*) exit 91 ;;
esac`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!"+sh+"\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("WATCH_TEST_MARK", filepath.Join(dir, "polled"))
	t.Setenv("WATCH_TEST_INITIAL", recordedWatch(t, "queued.json"))
	t.Setenv("WATCH_TEST_FINAL", recordedWatch(t, "removed.json"))

	var out, stderr bytes.Buffer
	code := commandStatus(run(t.Context(), []string{"watch", "--interval", "1ms", "488"}, &out, &stderr), &stderr)
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("legacy watch exited %d: stdout %s, stderr %s", code, &out, &stderr)
	}
	for _, want := range []string{"#488 AWAITING_CHECKS at position 2", "removed from the queue: failed_checks", "VM (standard) / VM / lifecycle"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("legacy watch omitted %q: %s", want, &out)
		}
	}
}
