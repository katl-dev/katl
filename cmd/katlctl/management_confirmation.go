package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/inventory"
	"github.com/katl-dev/katl/internal/katlc/transport"
)

// managementTrust names the trust a trust-changing command may have left on a
// node. An integer type, so a caller cannot pass a string literal.
type managementTrust uint8

const (
	trustOldAuthority managementTrust = iota + 1
	trustPreviousAuthority
	trustNodePreviousAuthority
	trustTrustedNetwork
	trustReplacementAuthority
)

func (t managementTrust) String() string {
	switch t {
	case trustOldAuthority:
		return "the old authority"
	case trustPreviousAuthority:
		return "the previous authority"
	case trustNodePreviousAuthority:
		return "its previous management authority"
	case trustTrustedNetwork:
		return "the trusted network"
	case trustReplacementAuthority:
		return "the replacement authority"
	default:
		panic(fmt.Sprintf("unknown management trust %d", uint8(t)))
	}
}

// managementAddress says whether a check's address is bound to the node. An
// address is bound when an earlier verified observation recorded it for this
// node: the saved context's endpoint, which 'katlctl node status' uses, or an
// endpoint this command has already confirmed. A failure at a bound address is
// about the node. At any other address, whatever answered, or did not, may not
// be the node, so the address is the first thing to check.
type managementAddress uint8

const (
	// addressBound is an address an earlier verified observation bound to the
	// node.
	addressBound managementAddress = iota + 1
	// addressUnverified is any other address: one given with --endpoint, or a
	// ClusterConfig address the saved context does not record for the node.
	addressUnverified
)

// addressBinding returns how endpoint relates to saved, the endpoint the saved
// context records for the node, which is zero when there is none.
func addressBinding(saved, endpoint inventory.HostPort) managementAddress {
	if !saved.IsZero() && saved.Equal(endpoint) {
		return addressBound
	}
	return addressUnverified
}

// managementCheck is a check of one node's management endpoint.
type managementCheck struct {
	// claim is what the check confirms when it passes.
	claim string
	// address says whether endpoint is bound to the node. It is zero when the
	// caller names its own next step, and the explanation then gives no advice.
	address  managementAddress
	endpoint inventory.HostPort
	// rerun is the command that repeats the check, if the reader can rerun it.
	rerun string
	// timeout is the command's --timeout, if a longer one could help.
	timeout time.Duration
	// authorityRemedy replaces transport.ManagementAuthorityRemedy for a
	// command whose credentials that remedy does not describe.
	authorityRemedy string
}

// managementReadError explains a failed check that changed nothing. Its cause
// comes from the call's evidence alone.
func managementReadError(check managementCheck, err error) error {
	return explainManagementFailure(check, nil, err)
}

// managementTrustChangeError explains an unconfirmed step of a command that
// changes management trust. prior is the trust the node may still have; the
// step's outcome is unknown whatever the evidence shows, so the explanation
// does not depend on it. The advice still does.
func managementTrustChangeError(check managementCheck, prior managementTrust, err error) error {
	return explainManagementFailure(check, &prior, err)
}

func explainManagementFailure(check managementCheck, prior *managementTrust, err error) error {
	evidence := transport.ClassifyManagementEvidence(err, nil, nil)
	unverified := check.address == addressUnverified
	explanation := "could not confirm " + check.claim
	switch {
	case prior != nil:
		explanation += "; the node may still trust " + prior.String()
	case evidence == transport.ManagementUnreachable && unverified:
		explanation += fmt.Sprintf(": the node did not answer at %s", check.endpoint)
	case evidence == transport.ManagementUnreachable:
		explanation += ": the node did not answer"
	case evidence == transport.ManagementAuthorityMismatch && unverified:
		explanation += fmt.Sprintf(": the endpoint at %s presented a certificate the configured authority did not sign", check.endpoint)
	case evidence == transport.ManagementAuthorityMismatch:
		explanation += "; the node may still trust a different management authority"
	case evidence == transport.ManagementRequiresTLS:
		explanation += "; the node may still trust an mTLS authority"
	}
	if err == nil {
		return errors.New(explanation)
	}
	if advice := check.advice(evidence, err); advice != "" {
		return fmt.Errorf("%s: %w; %s", explanation, err, advice)
	}
	return fmt.Errorf("%s: %w", explanation, err)
}

// advice is the one next step the evidence supports.
func (c managementCheck) advice(evidence transport.ManagementEvidence, err error) string {
	switch {
	case c.address == 0:
		return ""
	case evidence == transport.ManagementAuthorityMismatch && c.address == addressUnverified:
		// The certificate shows only that whatever answered is not signed by
		// the configured authority. Trust material that made it verify would
		// bind the node's credentials to whatever answers at this address.
		return fmt.Sprintf("check that %s is the node's management address; if it is, %s", c.endpoint, c.remedy())
	case evidence == transport.ManagementAuthorityMismatch:
		// A verified observation already bound this address to the node, so
		// neither the address nor a rerun can help.
		return c.remedy()
	case evidence == transport.ManagementUnreachable && c.address == addressUnverified:
		return c.thenRerun(fmt.Sprintf("check that %s is the node's management address", c.endpoint))
	case evidence == transport.ManagementUnreachable && c.rerun == "":
		return "retry once 'katlctl node status' shows it up"
	case evidence == transport.ManagementUnreachable:
		return fmt.Sprintf("rerun %s once 'katlctl node status' shows it up", c.rerun)
	case c.timeout > 0 && managementVerificationTimedOut(err):
		return c.thenRerun(fmt.Sprintf("the check timed out after %s; try to increase --timeout, or check the address and node reachability", c.timeout))
	default:
		return c.thenRerun("check the address and node reachability")
	}
}

func (c managementCheck) remedy() string {
	if c.authorityRemedy != "" {
		return c.authorityRemedy
	}
	return transport.ManagementAuthorityRemedy
}

func (c managementCheck) thenRerun(step string) string {
	if c.rerun == "" {
		return step
	}
	return step + ", then rerun " + c.rerun
}
