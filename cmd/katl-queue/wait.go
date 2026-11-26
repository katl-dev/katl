package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/katl-dev/katl/internal/retry"
)

const startWatchSocket = "start it with: systemctl --user start katl-queue-watch.socket"

var errWatchReconnect = errors.New("reconnect to watchd")

func watchSocketPath() (string, error) {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return "", errors.New("XDG_RUNTIME_DIR is unset; run katl-queue in a user session")
	}
	return filepath.Join(dir, "katl-queue", "watch.sock"), nil
}

func dialWatch(ctx context.Context, path string) (net.Conn, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "unix", path)
}

func watchUnavailable(err error) error {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("watch socket is unavailable; %s", startWatchSocket)
	}
	return err
}

func exchangeWatch(ctx context.Context, conn net.Conn, req watchRequest, accepted func(string)) (watchReply, error) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	var reply watchReply
	if err := retry.ContextError(ctx); err != nil {
		return reply, err
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return reply, err
	}
	decoder := json.NewDecoder(conn)
	if err := decoder.Decode(&reply); err != nil {
		return watchReply{}, err
	}
	if !req.List && reply.Error == "" && reply.Retry == "" {
		if reply.ID != req.ID && reply.ID != req.Resume || reply.ID == "" {
			return watchReply{}, errors.New("watchd returned an unexpected watch ID")
		}
		if err := retry.ContextError(ctx); err != nil {
			return watchReply{}, err
		}
		conn.SetDeadline(time.Time{})
		accepted(reply.ID)
		reply = watchReply{}
		if err := decoder.Decode(&reply); err != nil {
			return watchReply{}, err
		}
	}
	return reply, nil
}

func reportWatch(reply watchReply, out io.Writer) error {
	switch {
	case reply.Error != "":
		return errors.New(reply.Error)
	case reply.Outcome == nil:
		return errors.New("watchd returned no outcome")
	}
	switch reply.Outcome.Code {
	case 0:
		fmt.Fprintln(out, reply.Outcome.Message)
		return nil
	case 1:
		fmt.Fprintln(out, reply.Outcome.Message)
		return errConflict
	case 2:
		return errors.New(reply.Outcome.Message)
	default:
		return fmt.Errorf("watchd returned unknown exit status %d", reply.Outcome.Code)
	}
}

func listWatches(ctx context.Context, out io.Writer) error {
	path, err := watchSocketPath()
	if err != nil {
		return err
	}
	conn, err := dialWatch(ctx, path)
	if err != nil {
		return watchUnavailable(err)
	}
	reply, err := exchangeWatch(ctx, conn, watchRequest{List: true}, nil)
	if err != nil {
		return err
	}
	if reply.Error != "" {
		return errors.New(reply.Error)
	}

	if len(reply.Watches) == 0 {
		fmt.Fprintln(out, "no registered watches or recent results")
	}
	for _, watch := range reply.Watches {
		switch {
		case watch.Outcome == nil:
			fmt.Fprintf(out, "%s #%d: waiting since %s\n", watch.ID, watch.PR, watch.Registered.Format(time.RFC3339))
		default:
			message, _, _ := strings.Cut(watch.Outcome.Message, "\n")
			fmt.Fprintf(out, "%s #%d: exit %d at %s: %s\n", watch.ID, watch.PR,
				watch.Outcome.Code, watch.Fired.Format(time.RFC3339), message)
		}
	}
	return nil
}

func waitForWatch(ctx context.Context, dial func(context.Context, string) (net.Conn, error), req watchRequest, timeout time.Duration, out, errOut io.Writer) error {
	path, err := watchSocketPath()
	if err != nil {
		return err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	id := req.Resume
	if id == "" {
		id = rand.Text()
		req.ID = id
	}
	printedResume := false
	printResume := func() {
		if !printedResume {
			fmt.Fprintf(errOut, "resume: katl-queue wait --resume %s\n", id)
			printedResume = true
		}
	}

	connected := false
	backoff := retry.NewBackoff(100*time.Millisecond, 2*time.Second)
	reply, err := retry.Await(ctx, &backoff, func(ctx context.Context) (watchReply, bool, error) {
		conn, err := dial(ctx, path)
		if err != nil {
			if !connected {
				return watchReply{}, false, watchUnavailable(err)
			}
			return watchReply{}, false, fmt.Errorf("%w: %w", errWatchReconnect, err)
		}
		connected = true
		reply, err := exchangeWatch(ctx, conn, req, func(string) {
			// A lost acknowledgement can leave a durable registration behind.
			if req.Resume == "" {
				printResume()
			}
			req = watchRequest{Resume: id}
			backoff.Reset()
		})
		if err != nil {
			var connErr *net.OpError
			if errors.As(err, &connErr) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return watchReply{}, false, fmt.Errorf("%w: %w", errWatchReconnect, err)
			}
			return watchReply{}, false, fmt.Errorf("read watchd reply: %w", err)
		}
		return reply, reply.Blocked != "" || reply.Retry == "", nil
	}, func(err error) bool { return errors.Is(err, errWatchReconnect) })
	if err == nil {
		if reply.Blocked != "" {
			printResume()
			return fmt.Errorf("%s; repair GitHub access, then resume the watch", reply.Blocked)
		}
		return reportWatch(reply, out)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if req.Resume != "" {
			return fmt.Errorf("timed out waiting for watch %s", id)
		}
		return fmt.Errorf("timed out registering a watch for #%d", req.PR)
	}
	if errors.Is(err, retry.ErrInconclusive) && errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return err
}
