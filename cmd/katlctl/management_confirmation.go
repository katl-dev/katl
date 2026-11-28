package main

import (
	"fmt"

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

// managementReadError explains a failed check that changed nothing. Its cause
// comes from the call's evidence alone.
func managementReadError(check string, err error) error {
	return explainManagementFailure(check, nil, err)
}

// managementTrustChangeError explains an unconfirmed step of a command that
// changes management trust. prior is the trust the node may still have; the
// step's outcome is unknown whatever the evidence shows, so the text does not
// depend on it.
func managementTrustChangeError(check string, prior managementTrust, err error) error {
	return explainManagementFailure(check, &prior, err)
}

func explainManagementFailure(check string, prior *managementTrust, err error) error {
	if prior != nil {
		if err == nil {
			return fmt.Errorf("could not confirm %s; the node may still trust %s", check, *prior)
		}
		return fmt.Errorf("could not confirm %s; the node may still trust %s: %w", check, *prior, err)
	}
	if err == nil {
		return fmt.Errorf("could not confirm %s", check)
	}
	switch transport.ClassifyManagementEvidence(err, nil, nil) {
	case transport.ManagementUnreachable:
		return fmt.Errorf("could not confirm %s: the node did not answer; retry once 'katlctl node status' shows it up: %w", check, err)
	case transport.ManagementAuthorityMismatch:
		return fmt.Errorf("could not confirm %s; the node may still trust a different management authority: %w", check, err)
	case transport.ManagementRequiresTLS:
		return fmt.Errorf("could not confirm %s; the node may still trust an mTLS authority: %w", check, err)
	default:
		return fmt.Errorf("could not confirm %s: %w", check, err)
	}
}
