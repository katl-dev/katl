package managementidentity

import "fmt"

// Authentication identifies the management network's authorization boundary.
// The empty value preserves mTLS for existing credentials and installed nodes.
type Authentication string

const (
	TrustedNetwork Authentication = "trusted-network"
	MutualTLS      Authentication = "mtls"
)

func (mode Authentication) Validate() error {
	switch mode {
	case "", TrustedNetwork, MutualTLS:
		return nil
	default:
		return fmt.Errorf("management authentication %q must be trusted-network or mtls", mode)
	}
}
