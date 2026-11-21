package main

import (
	"fmt"
	"strings"
	"time"

	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
)

const (
	certificateExpiryWarning = 30 * 24 * time.Hour
	certificateRenewalHint   = "renew the control-plane certificates; see docs/operations/renew-certificates.md"
)

var certificateNow = func() time.Time { return time.Now().UTC() }

type certificateExpiryReport struct {
	Name     string `json:"name"`
	NotAfter string `json:"notAfter"`
	// State is ok, expiring (within 30 days), or expired.
	State string `json:"state"`
	at    time.Time
}

func newCertificateExpiryReport(expiry *agentapi.CertificateExpiry) *certificateExpiryReport {
	if expiry == nil {
		return nil
	}
	report := &certificateExpiryReport{Name: expiry.GetName(), NotAfter: expiry.GetNotAfter(), State: "unknown"}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(expiry.GetNotAfter()))
	if err != nil {
		return report
	}
	report.at = at
	switch remaining := at.Sub(certificateNow()); {
	case remaining <= 0:
		report.State = "expired"
	case remaining <= certificateExpiryWarning:
		report.State = "expiring"
	default:
		report.State = "ok"
	}
	return report
}

// summary renders the expiry for status tables, adding renewal guidance once
// the certificate is within the warning window.
func (r *certificateExpiryReport) summary() string {
	if r.at.IsZero() {
		return fmt.Sprintf("certificate %s reported an unreadable expiry %q", r.Name, r.NotAfter)
	}
	date := r.at.UTC().Format(time.DateOnly)
	switch r.State {
	case "expired":
		return fmt.Sprintf("certificate %s expired on %s; %s", r.Name, date, certificateRenewalHint)
	case "expiring":
		return fmt.Sprintf("certificate %s expires on %s (%s); %s", r.Name, date, daysUntil(r.at), certificateRenewalHint)
	default:
		return fmt.Sprintf("certificate %s expires on %s (%s)", r.Name, date, daysUntil(r.at))
	}
}

func (r *certificateExpiryReport) needsAttention() bool {
	return r != nil && r.State != "ok"
}

func daysUntil(at time.Time) string {
	days := int(at.Sub(certificateNow()).Hours() / 24)
	switch days {
	case 0:
		return "in less than a day"
	case 1:
		return "in 1 day"
	default:
		return fmt.Sprintf("in %d days", days)
	}
}
