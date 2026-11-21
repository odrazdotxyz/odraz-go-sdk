package files

import (
	"context"

	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
)

// Domain is the files domain identifier used in accounts.Adapter.Domains.
const Domain accounts.Domain = "files"

// Factory builds a Client for an account. Adapters register one under Domain.
type Factory func(ctx context.Context, env *accounts.Env) (Client, error)

// ForAccount returns the files Client of an opened account, or an error
// matching odraz.ErrUnsupported if the adapter has no files support.
func ForAccount(ctx context.Context, h *accounts.Handle) (Client, error) {
	f, err := h.DomainFactory(Domain)
	if err != nil {
		return nil, err
	}
	fac, ok := f.(Factory)
	if !ok {
		return nil, accounts.ErrBadFactory(Domain, f)
	}
	return fac(ctx, h.Env)
}
