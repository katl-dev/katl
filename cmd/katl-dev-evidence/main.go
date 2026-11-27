package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/pflag"
)

const defaultBudget = 50 << 30

const usage = `Usage:
  katl-dev-evidence new <owner>/<name>
  katl-dev-evidence list
  katl-dev-evidence keep <entry> --until <YYYY-MM-DD>
  katl-dev-evidence release <entry> [--force]
  katl-dev-evidence prune [--budget <size>]

Entries can be addressed by owner/name or their current absolute path.
Keep prints the new path and protects the entry through the given local date.
Release requires --force to remove an entry before its pin expires.
Prune returns expired pins to ordinary aging and removes the oldest unpinned
entries as needed to meet the budget (default 50GiB). Sizes are whole bytes
or use B, KiB, MiB, GiB, or TiB. Accounting uses logical file sizes.
The store is in .local/state/katl-dev/evidence under your passwd home directory.
`

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "katl-dev-evidence: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out, diagnostics io.Writer, now time.Time) error {
	return runWithStore(args, out, diagnostics, now, func() (*store, error) {
		location, err := evidenceRoot()
		if err != nil {
			return nil, err
		}
		return openStore(location)
	})
}

func runWithStore(args []string, out, diagnostics io.Writer, now time.Time, open func() (*store, error)) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(out, usage)
		return err
	}

	command := args[0]
	flags := pflag.NewFlagSet(command, pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	wantArgs := 0
	var until, budgetText string
	var force bool
	switch command {
	case "new":
		wantArgs = 1
	case "list":
	case "keep":
		wantArgs = 1
		flags.StringVar(&until, "until", "", "local date (YYYY-MM-DD)")
	case "release":
		wantArgs = 1
		flags.BoolVar(&force, "force", false, "allow releasing an unexpired pin")
	case "prune":
		flags.StringVar(&budgetText, "budget", "", "maximum total size")
	default:
		return fmt.Errorf("unknown command %q; see --help", command)
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			_, err = io.WriteString(out, usage)
		}
		return err
	}
	if flags.NArg() != wantArgs {
		return fmt.Errorf("wrong number of arguments to %s; see --help", command)
	}

	budget := int64(defaultBudget)
	switch command {
	case "keep":
		date, err := time.ParseInLocation(time.DateOnly, until, now.Location())
		if err != nil {
			return errors.New("keep requires --until YYYY-MM-DD")
		}
		if !now.Before(date.AddDate(0, 0, 1)) {
			return errors.New("--until date has passed")
		}
	case "prune":
		if flags.Changed("budget") {
			var err error
			budget, err = parseBudget(budgetText)
			if err != nil {
				return err
			}
		}
	}

	store, err := open()
	if err != nil {
		return err
	}
	defer store.close()

	entries, scanErr := store.entries(now.Location())
	if scanErr != nil && command != "prune" {
		return scanErr
	}
	var selected entry
	switch command {
	case "keep", "release":
		selected, err = store.find(entries, flags.Arg(0))
		if err != nil {
			return err
		}
	}

	switch command {
	case "new":
		name, err := store.create(entries, flags.Arg(0), diagnostics)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, name)
		return err
	case "list":
		return store.list(out, diagnostics, entries, now)
	case "keep":
		// Store the date itself in the path so one rename commits the payload
		// and its deadline together.
		destination := filepath.Join(pinDir, until, selected.id)
		if _, err := store.move(selected, destination); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, filepath.Join(store.location, destination))
		return err
	case "release":
		if now.Before(selected.deadline) && !force {
			return fmt.Errorf("evidence entry %q is pinned until %s. Use --force to release it early", selected.id, selected.deadline.Format(time.RFC3339))
		}
		return store.remove(selected)
	case "prune":
		return errors.Join(scanErr, store.prune(out, entries, now, budget))
	}
	return nil
}

func evidenceRoot() (string, error) {
	home, err := passwdHome()
	if err != nil {
		return "", fmt.Errorf("locate passwd home for evidence store: %w", err)
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("passwd home for evidence store is not absolute: %q", home)
	}
	return filepath.Join(home, ".local", "state", "katl-dev", "evidence"), nil
}

func parseBudget(value string) (int64, error) {
	number := strings.TrimRight(value, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
	var units int64
	switch value[len(number):] {
	case "", "B":
		units = 1
	case "KiB":
		units = 1 << 10
	case "MiB":
		units = 1 << 20
	case "GiB":
		units = 1 << 30
	case "TiB":
		units = 1 << 40
	}
	size, err := strconv.ParseInt(number, 10, 64)
	if err != nil || size < 0 || units == 0 || size > math.MaxInt64/units {
		return 0, fmt.Errorf("invalid budget %q; use whole bytes or B, KiB, MiB, GiB or TiB", value)
	}
	return size * units, nil
}

func (s *store) list(out, diagnostics io.Writer, entries []entry, now time.Time) error {
	table := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "OWNER\tENTRY\tBYTES\tAGE\tPINNED THROUGH\tPATH")
	for _, e := range entries {
		bytes := "unknown"
		if size, ok := s.measure(e, diagnostics); ok {
			bytes = strconv.FormatInt(size, 10)
		}
		date := "-"
		if now.Before(e.deadline) {
			date = e.deadline.AddDate(0, 0, -1).Format(time.DateOnly)
		}
		age := max(now.Sub(e.modified), 0).Truncate(time.Second)
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n",
			filepath.Dir(e.id), filepath.Base(e.id), bytes, age, date, filepath.Join(s.location, e.path))
	}
	return table.Flush()
}
