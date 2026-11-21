package accounts

import (
	"context"
	"fmt"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

// RefreshFunc exchanges expiring credentials for fresh ones (typically with
// the refresh token). The result is saved through the SecretStore by the
// provider; the hook itself must not persist anything.
type RefreshFunc func(ctx context.Context, old Credentials) (Credentials, error)

// refreshSkew is how long before expiry credentials are refreshed.
const refreshSkew = 30 * time.Second

// refreshingProvider wraps the store-backed provider: credentials that are
// expired (or about to be) are refreshed first. Concurrent callers share one
// refresh: the first takes the lock, the others wait and then reuse its result.
type refreshingProvider struct {
	base    storeProvider
	refresh RefreshFunc
	now     func() time.Time
	lock    chan struct{} // 1-slot semaphore; unlike a mutex, waiting honours ctx
}

func newRefreshingProvider(base storeProvider, f RefreshFunc) *refreshingProvider {
	return &refreshingProvider{base: base, refresh: f, now: time.Now, lock: make(chan struct{}, 1)}
}

func (p *refreshingProvider) Credentials(ctx context.Context) (Credentials, error) {
	c, err := p.base.Credentials(ctx)
	if err != nil || !c.Expired(p.now(), refreshSkew) {
		return c, err
	}
	select {
	case p.lock <- struct{}{}:
		defer func() { <-p.lock }()
	case <-ctx.Done():
		return Credentials{}, ctx.Err()
	}
	// Another caller may have refreshed while we waited.
	if c, err = p.base.Credentials(ctx); err != nil || !c.Expired(p.now(), refreshSkew) {
		return c, err
	}
	if c.RefreshToken == "" && c.Kind == CredBearer {
		return Credentials{}, fmt.Errorf("accounts: credentials expired and no refresh token: %w", odraz.ErrAuth)
	}
	nc, err := p.refresh(ctx, c)
	if err != nil {
		if ctx.Err() != nil {
			return Credentials{}, ctx.Err()
		}
		return Credentials{}, fmt.Errorf("accounts: refreshing credentials: %w: %w", odraz.ErrAuth, err)
	}
	if nc.RefreshToken == "" {
		nc.RefreshToken = c.RefreshToken // providers may omit it when unchanged
	}
	if err := p.save(ctx, nc); err != nil {
		return Credentials{}, err
	}
	return nc, nil
}

func (p *refreshingProvider) save(ctx context.Context, c Credentials) error {
	id, s := p.base.id, p.base.store
	if err := s.Put(ctx, id, SecretAccessToken, []byte(c.AccessToken)); err != nil {
		return fmt.Errorf("accounts: saving refreshed credentials: %w", err)
	}
	if c.RefreshToken != "" {
		if err := s.Put(ctx, id, SecretRefreshToken, []byte(c.RefreshToken)); err != nil {
			return fmt.Errorf("accounts: saving refreshed credentials: %w", err)
		}
	}
	exp := ""
	if !c.Expiry.IsZero() {
		exp = c.Expiry.UTC().Format(time.RFC3339)
	}
	if err := s.Put(ctx, id, SecretExpiry, []byte(exp)); err != nil {
		return fmt.Errorf("accounts: saving refreshed credentials: %w", err)
	}
	return nil
}
