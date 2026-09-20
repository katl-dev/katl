package transport

import (
	"context"

	"github.com/katl-dev/katl/internal/managementidentity"
)

type clientCredentialsKey struct{}

// WithClientCredentials carries the selected cluster's authentication through
// multi-node operations, including connections made for dependent maintenance.
func WithClientCredentials(ctx context.Context, credentials managementidentity.ClientCredentials) context.Context {
	return context.WithValue(ctx, clientCredentialsKey{}, credentials)
}

func ClientCredentialsFromContext(ctx context.Context) *managementidentity.ClientCredentials {
	credentials, ok := ctx.Value(clientCredentialsKey{}).(managementidentity.ClientCredentials)
	if !ok {
		return nil
	}
	return &credentials
}
