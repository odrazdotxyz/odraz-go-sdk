// Package opencloud is the OpenCloud adapter for the files domain. Import it
// for its side effect: it registers the "opencloud" adapter.
//
// Drives come from the Graph API (GET /graph/v1.0/me/drives); each one is
// opened as a WebDAV filesystem on the space URL the API returns. This is the
// reference implementation for new vendor adapters (see CONTRIBUTING.md).
package opencloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts/oidc"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	"github.com/odrazdotxyz/odraz-go-sdk/internal/dav"
	"github.com/odrazdotxyz/odraz-go-sdk/internal/davfs"
)

// Name is the registered adapter name.
const Name = "opencloud"

// AuthOIDC is the ID of the browser login method. The app drives it with
// accounts/oidc (Begin, open the URL, Complete, Save), then stores the client
// ID in Account.Config[accounts.ConfigOIDCClientID].
const AuthOIDC = "oidc"

// AuthAppToken is the ID of the v1 login method (Basic auth with an app token).
// A browser-based OIDC method will be added next to it, without changing this one.
const AuthAppToken = "app-token"

func init() {
	accounts.Register(accounts.Adapter{
		Name:  Name,
		Label: "OpenCloud",
		AuthMethods: []accounts.AuthMethod{{
			ID: AuthOIDC, Label: "Sign in with your browser (OIDC)", Kind: accounts.AuthBrowser,
			// Supplied by the app, not typed by the user: the client ID the
			// server's admin registered for the app with the identity provider.
			Fields: []accounts.Field{{Name: accounts.ConfigOIDCClientID, Label: "OIDC client ID", Type: accounts.FieldText, Required: true}},
		}, {
			ID: AuthAppToken, Label: "Username and app token", Kind: accounts.AuthForm,
			Fields: []accounts.Field{
				{Name: accounts.SecretUsername, Label: "Username", Type: accounts.FieldText, Required: true},
				// Sent as the Basic-auth password, so it uses the password secret key.
				{Name: accounts.SecretPassword, Label: "App token", Type: accounts.FieldSecret, Required: true, Secret: true},
			},
		}},
		Fields:   append([]accounts.Field{{Name: "url", Label: "Server URL", Type: accounts.FieldURL, Required: true}}, accounts.ProxyFields...),
		Domains:  map[accounts.Domain]any{files.Domain: files.Factory(newClient)},
		Validate: validate,
		Refresh:  refresher,
	})
}

// refresher renews OIDC tokens; accounts using an app token have none.
func refresher(env *accounts.Env) accounts.RefreshFunc {
	id := env.Account.Config[accounts.ConfigOIDCClientID]
	if id == "" {
		return nil
	}
	return oidc.RefreshFunc(oidc.HTTPClient(env), env.Account.Config["url"], id)
}

func serverClient(env *accounts.Env) (*dav.Client, error) {
	u := env.Account.Config["url"]
	if u == "" {
		return nil, fmt.Errorf("opencloud: account %q has no url", env.Account.ID)
	}
	return dav.New(dav.Config{BaseURL: u, Creds: env.Creds, Headers: env.Headers, TLS: env.TLS})
}

// validate checks reachability and credentials with GET /graph/v1.0/me.
func validate(ctx context.Context, env *accounts.Env) error {
	c, err := serverClient(env)
	if err != nil {
		return err
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := getJSON(ctx, c, "/graph/v1.0/me", &me); err != nil {
		return err
	}
	if me.ID == "" {
		return fmt.Errorf("opencloud: %s did not answer like an OpenCloud server: %w", env.Account.Config["url"], odraz.ErrNotFound)
	}
	return nil
}

func getJSON(ctx context.Context, c *dav.Client, path string, out any) error {
	h := http.Header{}
	h.Set("Accept", "application/json")
	resp, err := c.Do(ctx, dav.Request{Method: "GET", Path: path, Header: h, Expect: dav.ExpectJSON})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("opencloud: decoding %s: %w", path, err)
	}
	return nil
}

type graphDrive struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	DriveType string `json:"driveType"`
	Quota     *struct {
		Total int64 `json:"total"`
		Used  int64 `json:"used"`
	} `json:"quota"`
	Root struct {
		WebDavURL string `json:"webDavUrl"`
	} `json:"root"`
}

type client struct {
	api *dav.Client
	env *accounts.Env

	mu     sync.Mutex
	spaces map[string]string // drive ID -> WebDAV URL
	ro     map[string]bool   // drive ID -> read-only
}

func newClient(_ context.Context, env *accounts.Env) (files.Client, error) {
	api, err := serverClient(env)
	if err != nil {
		return nil, err
	}
	return &client{api: api, env: env, spaces: map[string]string{}, ro: map[string]bool{}}, nil
}

func (c *client) list(ctx context.Context) ([]graphDrive, error) {
	var res struct {
		Value []graphDrive `json:"value"`
	}
	if err := getJSON(ctx, c.api, "/graph/v1.0/me/drives", &res); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range res.Value {
		if d.Root.WebDavURL != "" {
			c.spaces[d.ID] = d.Root.WebDavURL
			c.ro[d.ID] = readOnly(d)
		}
	}
	return res.Value, nil
}

func (c *client) Drives(ctx context.Context) ([]files.Drive, error) {
	gd, err := c.list(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]files.Drive, 0, len(gd))
	for _, d := range gd {
		out = append(out, files.Drive{ID: d.ID, Name: d.Name, Kind: kind(d.DriveType), Quota: quota(d), ReadOnly: readOnly(d)})
	}
	return out, nil
}

func kind(t string) files.DriveKind {
	switch t {
	case "personal":
		return files.DrivePersonal
	case "project":
		return files.DriveProject
	case "virtual", "mountpoint", "share":
		return files.DriveShared // "virtual" is the Shares space
	}
	return files.DriveOther
}

// readOnly is true for the virtual Shares space: it only lists mounted shares,
// so creating, moving or deleting at its root does not work. Writing into a
// share belongs to the sharing domain (v1 does not open shares as spaces).
func readOnly(d graphDrive) bool { return d.DriveType == "virtual" }

// quota maps Graph quota: total 0 means unlimited, reported as Total -1.
func quota(d graphDrive) *files.Quota {
	if d.Quota == nil {
		return nil
	}
	q := &files.Quota{Used: d.Quota.Used, Total: d.Quota.Total}
	if d.Quota.Total <= 0 {
		q.Total = -1
	}
	return q
}

func (c *client) OpenDrive(ctx context.Context, id string) (files.FS, error) {
	c.mu.Lock()
	u, ok := c.spaces[id]
	c.mu.Unlock()
	if !ok {
		if _, err := c.list(ctx); err != nil {
			return nil, err
		}
		c.mu.Lock()
		u, ok = c.spaces[id]
		c.mu.Unlock()
	}
	if !ok {
		return nil, fmt.Errorf("opencloud: no drive %q: %w", id, odraz.ErrNotFound)
	}
	base, err := c.spaceURL(u)
	if err != nil {
		return nil, err
	}
	d, err := dav.New(dav.Config{BaseURL: base, Creds: c.env.Creds, Headers: c.env.Headers, TLS: c.env.TLS})
	if err != nil {
		return nil, err
	}
	fs := files.Wrap(&davfs.FS{C: d})
	if c.readOnly(id) {
		fs = files.ReadOnly(fs)
	}
	return fs, nil
}

func (c *client) readOnly(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ro[id]
}

// spaceURL takes the path from the URL the API returned but keeps the scheme
// and host the user configured. The API reports the server's own idea of its
// public URL (OC_URL), which differs from the address the client uses behind
// a reverse proxy or auth proxy, and a foreign host would also trip the
// cross-host redirect protection.
func (c *client) spaceURL(apiURL string) (string, error) {
	au, err := url.Parse(apiURL)
	if err != nil || au.Path == "" {
		return "", fmt.Errorf("opencloud: bad space URL %q", apiURL)
	}
	su, err := url.Parse(c.env.Account.Config["url"])
	if err != nil {
		return "", err
	}
	su.Path, su.RawPath, su.RawQuery, su.Fragment = au.Path, "", "", ""
	return su.String(), nil
}
