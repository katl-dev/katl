package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/katl-dev/katl/internal/operatorconsole"
	"golang.org/x/sys/unix"
)

var (
	version = "0.0.0-dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "katl-console: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("katl-console", flag.ContinueOnError)
	modeValue := flags.String("mode", "", "console mode: installer or runtime")
	ttyPath := flags.String("tty", "/dev/tty1", "virtual terminal to own")
	statusPath := flags.String("status", "", "override install status path")
	handoffPath := flags.String("handoff", operatorconsole.HandoffPath, "installer handoff projection path")
	snapshotPath := flags.String("snapshot", "/run/katl/console/rendered.txt", "plain-text rendered dashboard snapshot")
	refresh := flags.Duration("refresh", time.Second, "dashboard refresh interval")
	journalLines := flags.Int("journal-lines", 200, "maximum buffered journal lines")
	if err := flags.Parse(args); err != nil {
		return err
	}
	mode := operatorconsole.Mode(strings.TrimSpace(*modeValue))
	if mode != operatorconsole.ModeInstaller && mode != operatorconsole.ModeRuntime {
		return fmt.Errorf("mode must be installer or runtime")
	}
	if *refresh <= 0 {
		return fmt.Errorf("refresh interval must be positive")
	}
	if *journalLines < 1 {
		return fmt.Errorf("journal-lines must be positive")
	}

	tty, err := os.OpenFile(*ttyPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open dashboard tty %s: %w", *ttyPath, err)
	}
	defer tty.Close()
	_, _ = io.WriteString(tty, "\x1b[?25l\x1b[2J")
	defer io.WriteString(tty, "\x1b[?25h\n")

	ring := newJournalRing(*journalLines)
	go followJournal(ctx, ring)
	collector := operatorconsole.Collector{
		Mode:        mode,
		Version:     version,
		StatusPath:  strings.TrimSpace(*statusPath),
		HandoffPath: strings.TrimSpace(*handoffPath),
	}
	snapshotPathValue := strings.TrimSpace(*snapshotPath)
	var snapshot operatorconsole.Snapshot
	var dashboard operatorconsole.Renderer
	var plainDashboard operatorconsole.Renderer
	ticker := time.NewTicker(*refresh)
	defer ticker.Stop()
	for {
		collector.Collect(&snapshot)
		if err := render(tty, snapshotPathValue, &snapshot, ring, &dashboard, &plainDashboard); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func render(tty *os.File, snapshotPath string, snapshot *operatorconsole.Snapshot, journal operatorconsole.Journal, dashboard, plainDashboard *operatorconsole.Renderer) error {
	width, height := terminalSize(tty)
	if !dashboard.MatchesDimensions(width, height) {
		target := operatorconsole.NewRenderTarget(make([]byte, operatorconsole.RenderCapacity(width, height)), width, height)
		*dashboard = operatorconsole.NewRenderer(target, true)
	}
	data := dashboard.Render(snapshot, journal)
	if written, err := tty.Write(data); err != nil {
		return fmt.Errorf("render dashboard: %w", err)
	} else if written != len(data) {
		return fmt.Errorf("render dashboard: %w", io.ErrShortWrite)
	}
	if snapshotPath != "" {
		if !plainDashboard.MatchesDimensions(width, height) {
			target := operatorconsole.NewRenderTarget(make([]byte, operatorconsole.RenderCapacity(width, height)), width, height)
			*plainDashboard = operatorconsole.NewRenderer(target, false)
		}
		plain := plainDashboard.Render(snapshot, journal)
		if err := writeSnapshot(snapshotPath, plain); err != nil {
			return err
		}
	}
	return nil
}

func terminalSize(tty *os.File) (int, int) {
	size, err := unix.IoctlGetWinsize(int(tty.Fd()), unix.TIOCGWINSZ)
	if err != nil || size.Col == 0 || size.Row == 0 {
		return 80, 25
	}
	return int(size.Col), int(size.Row)
}

func writeSnapshot(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create dashboard snapshot directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".rendered-*")
	if err != nil {
		return fmt.Errorf("create dashboard snapshot: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish dashboard snapshot: %w", err)
	}
	return nil
}

type journalRing struct {
	mu    sync.Mutex
	lines [][]byte
	start int
	count int
}

const (
	journalLineCapacity = 1024
	journalScanCapacity = 256 << 10
	journalTruncated    = "… [truncated]"
)

func newJournalRing(limit int) *journalRing {
	storage := make([]byte, limit*journalLineCapacity)
	lines := make([][]byte, limit)
	for index := range lines {
		start := index * journalLineCapacity
		lines[index] = storage[start : start : start+journalLineCapacity]
	}
	return &journalRing{lines: lines}
}

func (r *journalRing) Add(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	r.mu.Lock()
	index, target := r.nextLineLocked(min(len(line), journalLineCapacity))
	copyBoundedJournalLine(target, line)
	r.lines[index] = compactJournalTimestamp(target)
	r.mu.Unlock()
}

func (r *journalRing) AddString(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	r.mu.Lock()
	_, target := r.nextLineLocked(min(len(line), journalLineCapacity))
	copyBoundedJournalLine(target, []byte(line))
	r.mu.Unlock()
}

func (r *journalRing) nextLineLocked(length int) (int, []byte) {
	index := (r.start + r.count) % len(r.lines)
	if r.count == len(r.lines) {
		index = r.start
		r.start = (r.start + 1) % len(r.lines)
	} else {
		r.count++
	}
	r.lines[index] = r.lines[index][:min(length, journalLineCapacity)]
	return index, r.lines[index]
}

func copyBoundedJournalLine(target, line []byte) {
	if len(line) <= len(target) {
		copy(target, line)
		return
	}
	prefix := max(len(target)-len(journalTruncated), 0)
	for prefix > 0 && prefix < len(line) && line[prefix]&0xc0 == 0x80 {
		prefix--
	}
	copied := copy(target, line[:prefix])
	copy(target[copied:], journalTruncated)
}

func compactJournalTimestamp(line []byte) []byte {
	if !journalDateTimePrefix(line) {
		return line
	}

	position := len("2006-01-02T15:04:05")
	fractionEnd := position
	if position < len(line) && line[position] == '.' {
		fractionEnd++
		for fractionEnd < len(line) && isDigit(line[fractionEnd]) {
			fractionEnd++
		}
		if fractionEnd == position+1 {
			return line
		}
		position = fractionEnd
	}

	timestampEnd := position
	switch {
	case position < len(line) && line[position] == 'Z':
		timestampEnd++
	case validJournalTimezone(line, position):
		if position+3 < len(line) && line[position+3] == ':' {
			timestampEnd += len("+00:00")
		} else {
			timestampEnd += len("+0000")
		}
	}
	if timestampEnd >= len(line) || line[timestampEnd] != ' ' {
		return line
	}

	line[10] = ' '
	keepEnd := len("2006-01-02T15:04:05")
	if fractionEnd > keepEnd {
		fractionDigits := fractionEnd - keepEnd - 1
		if fractionDigits > 3 {
			fractionDigits = 3
		}
		keepEnd += 1 + fractionDigits
	}
	return line[:keepEnd+copy(line[keepEnd:], line[timestampEnd:])]
}

func journalDateTimePrefix(line []byte) bool {
	if len(line) < len("2006-01-02T15:04:05") {
		return false
	}
	for _, position := range [...]int{0, 1, 2, 3, 5, 6, 8, 9, 11, 12, 14, 15, 17, 18} {
		if !isDigit(line[position]) {
			return false
		}
	}
	return line[4] == '-' && line[7] == '-' && line[10] == 'T' && line[13] == ':' && line[16] == ':'
}

func validJournalTimezone(line []byte, position int) bool {
	if position >= len(line) || (line[position] != '+' && line[position] != '-') {
		return false
	}
	if position+5 < len(line) && line[position+3] == ':' {
		return isDigit(line[position+1]) && isDigit(line[position+2]) &&
			isDigit(line[position+4]) && isDigit(line[position+5])
	}
	return position+4 < len(line) && isDigit(line[position+1]) && isDigit(line[position+2]) &&
		isDigit(line[position+3]) && isDigit(line[position+4])
}

func isDigit(value byte) bool {
	return value >= '0' && value <= '9'
}

func (r *journalRing) WriteTail(writer *operatorconsole.JournalWriter) {
	r.mu.Lock()
	selected := 0
	selectedRows := 0
	for selected < r.count && selectedRows < writer.RowsRemaining() {
		index := (r.start + r.count - selected - 1) % len(r.lines)
		lineRows := writer.LineRows(r.lines[index])
		if selected > 0 && selectedRows+lineRows > writer.RowsRemaining() {
			break
		}
		selected++
		selectedRows += lineRows
	}
	first := r.count - selected
	for offset := range selected {
		index := (r.start + first + offset) % len(r.lines)
		if !writer.WriteLine(r.lines[index]) {
			break
		}
	}
	r.mu.Unlock()
}

func followJournal(ctx context.Context, ring *journalRing) {
	for ctx.Err() == nil {
		err := streamJournal(ctx, ring, exec.CommandContext)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			ring.AddString("journal stream unavailable: " + err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

type commandContext func(context.Context, string, ...string) *exec.Cmd

func streamJournal(ctx context.Context, ring *journalRing, command commandContext) error {
	streamContext, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := command(streamContext, "journalctl", journalctlArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var stderrText bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&stderrText, stderr)
		close(done)
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), journalScanCapacity)
	for scanner.Scan() {
		ring.Add(scanner.Bytes())
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		cancel()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	waitErr := cmd.Wait()
	<-done
	if scanErr != nil {
		return scanErr
	}
	if waitErr != nil && !errors.Is(ctx.Err(), context.Canceled) {
		if detail := bytes.TrimSpace(stderrText.Bytes()); len(detail) > 0 {
			return fmt.Errorf("%w: %s", waitErr, detail)
		}
		return waitErr
	}
	return nil
}

var journalctlArgs = []string{"--boot", "--follow", "--lines=100", "--output=short-iso", "--no-pager"}
