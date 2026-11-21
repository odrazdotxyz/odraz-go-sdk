// Package webdav is the generic WebDAV adapter for the files domain.
// Import it for its side effect: it registers the "webdav" adapter.
package webdav

import (
	"context"
	"fmt"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	"github.com/odrazdotxyz/odraz-go-sdk/internal/dav"
	"github.com/odrazdotxyz/odraz-go-sdk/internal/davfs"
)

// Name is the registered adapter name.
const Name = "webdav"

// DefaultDriveID is the id of the single drive a generic WebDAV server has.
const DefaultDriveID = "default"

func init() {
	accounts.Register(accounts.Adapter{
		Name:  Name,
		Label: "WebDAV",
		AuthMethods: []accounts.AuthMethod{{
			ID: "basic", Label: "Username and password", Kind: accounts.AuthForm,
			Fields: []accounts.Field{
				{Name: accounts.SecretUsername, Label: "Username", Type: accounts.FieldText, Required: true},
				{Name: accounts.SecretPassword, Label: "Password", Type: accounts.FieldSecret, Required: true, Secret: true},
			},
		}},
		Fields:   append([]accounts.Field{{Name: "url", Label: "WebDAV URL", Type: accounts.FieldURL, Required: true}}, accounts.ProxyFields...),
		Domains:  map[accounts.Domain]any{files.Domain: files.Factory(newClient)},
		Validate: validate,
	})
}

func davClient(env *accounts.Env, baseURL string) (*dav.Client, error) {
	return dav.New(dav.Config{BaseURL: baseURL, Creds: env.Creds, Headers: env.Headers, TLS: env.TLS})
}

func baseURL(env *accounts.Env) (string, error) {
	u := env.Account.Config["url"]
	if u == "" {
		return "", fmt.Errorf("webdav: account %q has no url", env.Account.ID)
	}
	return u, nil
}

func validate(ctx context.Context, env *accounts.Env) error {
	u, err := baseURL(env)
	if err != nil {
		return err
	}
	c, err := davClient(env, u)
	if err != nil {
		return err
	}
	_, err = c.Propfind(ctx, "/", 0)
	return err
}

// New returns a files.Client for a WebDAV base URL. Exposed for tests and
// for callers that already hold an accounts.Env.
func New(env *accounts.Env, url string) (files.Client, error) {
	c, err := davClient(env, url)
	if err != nil {
		return nil, err
	}
	return &client{label: env.Account.Label, fs: files.Wrap(&davfs.FS{C: c}), dav: c}, nil
}

func newClient(_ context.Context, env *accounts.Env) (files.Client, error) {
	u, err := baseURL(env)
	if err != nil {
		return nil, err
	}
	return New(env, u)
}

type client struct {
	label string
	fs    files.FS
	dav   *dav.Client
}

func (c *client) Drives(ctx context.Context) ([]files.Drive, error) {
	d := files.Drive{ID: DefaultDriveID, Name: c.label, Kind: files.DrivePersonal}
	if d.Name == "" {
		d.Name = "WebDAV"
	}
	rs, err := c.dav.Propfind(ctx, "/", 0)
	if err != nil {
		return nil, err
	}
	if len(rs) > 0 && rs[0].QuotaUsed >= 0 && rs[0].QuotaAvailable >= 0 {
		d.Quota = &files.Quota{Used: rs[0].QuotaUsed, Total: rs[0].QuotaUsed + rs[0].QuotaAvailable}
	}
	return []files.Drive{d}, nil
}

func (c *client) OpenDrive(_ context.Context, id string) (files.FS, error) {
	if id != DefaultDriveID {
		return nil, fmt.Errorf("webdav: no drive %q: %w", id, odraz.ErrNotFound)
	}
	return c.fs, nil
}
