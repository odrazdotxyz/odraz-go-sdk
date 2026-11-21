package accounts

import (
	"context"
	"fmt"
	"sort"
	"sync"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

// Domain names a capability area of an adapter ("files", later "calendar"...).
// Each domain package defines its own constant, so adding a domain never
// touches this package.
type Domain string

// Adapter is what a vendor/driver package registers from init().
type Adapter struct {
	Name        string
	Label       string
	AuthMethods []AuthMethod
	Fields      []Field // connection fields (URL, proxy options...)
	// Domains maps each supported domain to its typed factory (e.g.
	// files.Factory). Values are interpreted by the domain package.
	Domains map[Domain]any
	// Refresh, if set, returns the hook that renews expiring or rejected
	// bearer credentials for an account (OIDC). It may return nil when the
	// account does not use refreshable credentials.
	Refresh func(env *Env) RefreshFunc
	// Validate checks that the account works (reachable, credentials ok) and
	// returns ErrAuth, ErrNotFound or ErrBlockedByProxy with a clear message.
	Validate func(ctx context.Context, env *Env) error
}

var (
	regMu    sync.RWMutex
	registry = map[string]Adapter{}
)

// Register adds an adapter. It panics on an empty or duplicate name, like
// database/sql.Register. Call it from init().
func Register(a Adapter) {
	regMu.Lock()
	defer regMu.Unlock()
	if a.Name == "" {
		panic("accounts: Register with empty adapter name")
	}
	if _, dup := registry[a.Name]; dup {
		panic("accounts: Register called twice for adapter " + a.Name)
	}
	registry[a.Name] = a
}

// Lookup returns a registered adapter.
func Lookup(name string) (Adapter, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	a, ok := registry[name]
	return a, ok
}

// Adapters lists registered adapters sorted by name.
func Adapters() []Adapter {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Adapter, 0, len(registry))
	for _, a := range registry {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Handle is an opened account. Domain packages obtain their client from it,
// e.g. files.ForAccount(ctx, handle).
type Handle struct {
	Account Account
	Env     *Env
	adapter Adapter
}

// Open resolves the adapter and builds its environment (credentials, extra
// headers, client certificate) from the account and the secret store.
func Open(ctx context.Context, acc Account, store SecretStore, opts ...OpenOption) (*Handle, error) {
	a, ok := Lookup(acc.Adapter)
	if !ok {
		return nil, fmt.Errorf("accounts: adapter %q is not registered: %w", acc.Adapter, odraz.ErrUnsupported)
	}
	env, err := newEnv(ctx, acc, store)
	if err != nil {
		return nil, err
	}
	var o openOptions
	for _, f := range opts {
		f(&o)
	}
	var refresh RefreshFunc
	if a.Refresh != nil {
		refresh = a.Refresh(env)
	}
	if o.refresh != nil {
		refresh = o.refresh
	}
	if sp, ok := env.Creds.(storeProvider); ok && refresh != nil {
		env.Creds = newRefreshingProvider(sp, refresh)
	}
	return &Handle{Account: acc, Env: env, adapter: a}, nil
}

type openOptions struct{ refresh RefreshFunc }

// OpenOption customizes Open.
type OpenOption func(*openOptions)

// WithRefresh overrides the adapter's credential refresh hook.
func WithRefresh(f RefreshFunc) OpenOption { return func(o *openOptions) { o.refresh = f } }

// Adapter returns the adapter description.
func (h *Handle) Adapter() Adapter { return h.adapter }

// Supports reports whether the adapter implements the domain.
func (h *Handle) Supports(d Domain) bool { _, ok := h.adapter.Domains[d]; return ok }

// DomainFactory returns the raw factory registered for d, or an error
// wrapping odraz.ErrUnsupported. Domain packages type-assert it.
func (h *Handle) DomainFactory(d Domain) (any, error) {
	f, ok := h.adapter.Domains[d]
	if !ok {
		return nil, fmt.Errorf("accounts: adapter %q does not support %s: %w", h.adapter.Name, d, odraz.ErrUnsupported)
	}
	return f, nil
}

// Validate opens the account and runs the adapter's check.
func Validate(ctx context.Context, acc Account, store SecretStore) error {
	h, err := Open(ctx, acc, store)
	if err != nil {
		return err
	}
	if h.adapter.Validate == nil {
		return nil
	}
	return h.adapter.Validate(ctx, h.Env)
}

// ErrBadFactory reports an adapter that registered a wrong factory type.
func ErrBadFactory(d Domain, got any) error {
	return fmt.Errorf("accounts: factory for %s has unexpected type %T", d, got)
}
