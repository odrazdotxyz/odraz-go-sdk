package accounts

import (
	"context"
	"fmt"
	"os"
	"time"

	odraz "github.com/odraz/sdk"
)

// CredKind says how Credentials are applied to requests.
type CredKind string

const (
	CredBasic  CredKind = "basic"
	CredBearer CredKind = "bearer"
)

// Credentials are username/password or a bearer token. OpenCloud app tokens
// are sent as Basic auth, so they are CredBasic with the token as Password.
type Credentials struct {
	Kind         CredKind
	Username     string
	Password     string
	AccessToken  string
	RefreshToken string
	Expiry       time.Time // zero means no expiry
}

// Expired reports whether the credentials expire within the given skew.
func (c Credentials) Expired(now time.Time, skew time.Duration) bool {
	return !c.Expiry.IsZero() && !now.Add(skew).Before(c.Expiry)
}

// CredentialProvider supplies current credentials for each request.
// Adapters never store secrets; they ask the provider every time.
type CredentialProvider interface {
	Credentials(ctx context.Context) (Credentials, error)
}

// StaticProvider always returns the same credentials.
type StaticProvider struct{ Creds Credentials }

func (p StaticProvider) Credentials(context.Context) (Credentials, error) { return p.Creds, nil }

// EnvProvider reads credentials from environment variables:
// <Prefix>_USERNAME + <Prefix>_PASSWORD, or <Prefix>_TOKEN for bearer.
type EnvProvider struct{ Prefix string }

func (p EnvProvider) Credentials(context.Context) (Credentials, error) {
	if t := os.Getenv(p.Prefix + "_TOKEN"); t != "" {
		return Credentials{Kind: CredBearer, AccessToken: t}, nil
	}
	u, pw := os.Getenv(p.Prefix+"_USERNAME"), os.Getenv(p.Prefix+"_PASSWORD")
	if u == "" {
		return Credentials{}, fmt.Errorf("env %s_USERNAME or %s_TOKEN not set: %w", p.Prefix, p.Prefix, odraz.ErrAuth)
	}
	return Credentials{Kind: CredBasic, Username: u, Password: pw}, nil
}
