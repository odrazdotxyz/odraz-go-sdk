package accounts

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

// Secret keys used in the SecretStore.
const (
	SecretUsername     = "username"
	SecretPassword     = "password"
	SecretAccessToken  = "access_token"
	SecretRefreshToken = "refresh_token"
	SecretExpiry       = "expiry" // RFC 3339
	SecretClientCert   = "client_cert"
	SecretClientKey    = "client_key"
	secretHeaderPrefix = "header."
)

// Config keys for connection options shared by all adapters.
const (
	ConfigHeaderPrefix  = "header."        // header.<Name> = plain value
	ConfigSecretHeaders = "secret_headers" // comma-separated names whose values are in the SecretStore as header.<Name>
	// ConfigInsecure = "true" disables TLS certificate verification, for
	// self-hosted servers with self-signed certificates. Insecure: the
	// connection can be intercepted. UIs should make the user opt in explicitly.
	ConfigInsecure = "insecure_skip_verify"
)

// ProxyFields describe the optional mTLS settings every adapter may expose.
// Extra headers are open-ended and use the header.<Name> convention instead.
var ProxyFields = []Field{
	{Name: ConfigInsecure, Label: "Skip TLS certificate verification (insecure)", Type: FieldBool},
	{Name: SecretClientCert, Label: "Client certificate (PEM)", Type: FieldSecret, Secret: true},
	{Name: SecretClientKey, Label: "Client key (PEM)", Type: FieldSecret, Secret: true},
}

// Env is what an adapter factory receives: everything needed to talk to the
// server, with secrets already resolved. Adapters never store it.
type Env struct {
	Account Account
	Creds   CredentialProvider
	Headers map[string]string // extra headers for every request
	TLS     *tls.Config       // nil unless a client certificate is configured
}

func newEnv(ctx context.Context, acc Account, store SecretStore) (*Env, error) {
	env := &Env{Account: acc, Headers: map[string]string{}}
	if store != nil {
		env.Creds = storeProvider{id: acc.ID, store: store, cfg: acc.Config}
	}
	for k, v := range acc.Config {
		if name, ok := strings.CutPrefix(k, ConfigHeaderPrefix); ok && name != "" {
			env.Headers[name] = v
		}
	}
	for _, name := range strings.Split(acc.Config[ConfigSecretHeaders], ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if store == nil {
			return nil, fmt.Errorf("accounts: secret header %q needs a SecretStore", name)
		}
		v, err := store.Get(ctx, acc.ID, secretHeaderPrefix+name)
		if err != nil {
			return nil, fmt.Errorf("accounts: secret header %q: %w", name, err)
		}
		env.Headers[name] = string(v)
	}
	if store != nil {
		cert, errC := store.Get(ctx, acc.ID, SecretClientCert)
		key, errK := store.Get(ctx, acc.ID, SecretClientKey)
		switch {
		case errC == nil && errK == nil:
			pair, err := tls.X509KeyPair(cert, key)
			if err != nil {
				return nil, fmt.Errorf("accounts: client certificate: %w", err)
			}
			env.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
		case errC == nil || errK == nil:
			return nil, errors.New("accounts: client_cert and client_key must both be set")
		}
	}
	if acc.Config[ConfigInsecure] == "true" {
		if env.TLS == nil {
			env.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		env.TLS.InsecureSkipVerify = true //nolint:gosec // explicit user opt-in
	}
	return env, nil
}

// storeProvider reads credentials from the SecretStore on every call:
// a bearer token if access_token exists, otherwise username/password.
// (Refresh of expiring tokens is layered on top of this.)
type storeProvider struct {
	id    string
	store SecretStore
	cfg   map[string]string // the username is not secret and lives in Account.Config
}

func (p storeProvider) Credentials(ctx context.Context) (Credentials, error) {
	if tok, err := p.store.Get(ctx, p.id, SecretAccessToken); err == nil {
		c := Credentials{Kind: CredBearer, AccessToken: string(tok)}
		if rt, err := p.store.Get(ctx, p.id, SecretRefreshToken); err == nil {
			c.RefreshToken = string(rt)
		}
		if ex, err := p.store.Get(ctx, p.id, SecretExpiry); err == nil {
			c.Expiry, _ = time.Parse(time.RFC3339, string(ex))
		}
		return c, nil
	}
	user := p.cfg[SecretUsername]
	if b, err := p.store.Get(ctx, p.id, SecretUsername); err == nil {
		user = string(b) // an adapter may declare the username secret
	}
	if user == "" {
		return Credentials{}, fmt.Errorf("no credentials stored for account %s: %w", p.id, odraz.ErrAuth)
	}
	pw, _ := p.store.Get(ctx, p.id, SecretPassword)
	return Credentials{Kind: CredBasic, Username: user, Password: string(pw)}, nil
}
