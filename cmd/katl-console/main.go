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
	var renderBuffer []byte
	var dashboard operatorconsole.Renderer
	ticker := time.NewTicker(*refresh)
	defer ticker.Stop()
	for {
		collector.Collect(&snapshot)
		if err := render(tty, snapshotPathValue, &snapshot, ring, &dashboard, &renderBuffer); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func render(tty *os.File, snapshotPath string, snapshot *operatorconsole.Snapshot, journal operatorconsole.Journal, dashboard *operatorconsole.Renderer, buffer *[]byte) error {
	width, height := terminalSize(tty)
	required := len("\x1b[H\x1b[2J") + operatorconsole.RenderCapacity(width, height)
	if cap(*buffer) < required {
		*buffer = make([]byte, 0, required)
	}
	data := (*buffer)[:0]
	data = append(data, "\x1b[H\x1b[2J"...)
	plainStart := len(data)
	data = dashboard.Append(data, snapshot, journal, width, height)
	*buffer = data
	if written, err := tty.Write(data); err != nil {
		return fmt.Errorf("render dashboard: %w", err)
	} else if written != len(data) {
		return fmt.Errorf("render dashboard: %w", io.ErrShortWrite)
	}
	if snapshotPath != "" {
		if err := writeSnapshot(snapshotPath, data[plainStart:]); err != nil {
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

const journalLineCapacity = 1024

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
	copy(r.nextLineLocked(len(line)), line)
	r.mu.Unlock()
}

func (r *journalRing) AddString(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	r.mu.Lock()
	copy(r.nextLineLocked(len(line)), line)
	r.mu.Unlock()
}

func (r *journalRing) nextLineLocked(length int) []byte {
	index := (r.start + r.count) % len(r.lines)
	if r.count == len(r.lines) {
		index = r.start
		r.start = (r.start + 1) % len(r.lines)
	} else {
		r.count++
	}
	if cap(r.lines[index]) < length {
		r.lines[index] = make([]byte, length)
	} else {
		r.lines[index] = r.lines[index][:length]
	}
	return r.lines[index]
}

func (r *journalRing) AppendTail(dst []byte, rows, width int) ([]byte, int) {
	r.mu.Lock()
	rows = min(rows, r.count)
	first := r.count - rows
	for offset := range rows {
		index := (r.start + first + offset) % len(r.lines)
		dst = operatorconsole.AppendJournalLine(dst, r.lines[index], width)
	}
	r.mu.Unlock()
	return dst, rows
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
	cmd := command(ctx, "journalctl", "--boot", "--follow", "--lines=100", "--output=short-monotonic", "--no-pager")
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
	scanner.Buffer(make([]byte, 4096), 256<<10)
	for scanner.Scan() {
		ring.Add(scanner.Bytes())
	}
	scanErr := scanner.Err()
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
