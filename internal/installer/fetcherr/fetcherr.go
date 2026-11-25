// Package fetcherr reports why an HTTP request made by the installer failed
// without echoing the request URL.
package fetcherr

import (
	"context"
	"errors"
	"net"
	"net/url"
	"syscall"
)

type causeError struct {
	message string
	cause   error
}

func (e *causeError) Error() string { return e.message }
func (e *causeError) Unwrap() error { return e.cause }

// Cause returns the reason an http.Client.Do error occurred, stripped of the
// request URL that *url.Error embeds. Operators supply URLs that may carry
// credentials, so callers that name the URL do so through their own redaction
// and append this cause. The returned error unwraps to the underlying
// transport error. Errors that are not *url.Error are returned unchanged.
func Cause(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	return &causeError{message: describe(urlErr.Err), cause: urlErr.Err}
}

func describe(err error) string {
	var dnsErr *net.DNSError
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		return "no such host"
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		// Other transport errors, such as TLS certificate verification
		// failures, name their cause and carry no request URL.
		return err.Error()
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
