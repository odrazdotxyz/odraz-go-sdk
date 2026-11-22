// Package nextcloud is the Nextcloud adapter for the files domain. Import it
// for its side effect: it registers the "nextcloud" adapter.
//
// Files are plain WebDAV under /remote.php/dav/files/<user-id>. On top of the
// generic WebDAV driver it adds what Nextcloud knows and WebDAV does not:
//
//   - the user's id (WebDAV paths use the id, which can differ from the login
//     name), found with the OCS API;
//   - Team folders (the "Group folders" app): a team folder shows up as a
//     top-level folder of every member's tree, flagged nc:mount-type=group in
//     PROPFIND. Each one is reported as its own DriveProject drive, with its
//     own quota, and read-only when the user has no write permission.
//
// Sign-in is HTTP Basic with a username and an app password (Settings >
// Security > Devices & sessions). Browser (OIDC) sign-in needs Nextcloud's
// user_oidc app and is not part of this adapter yet.
package nextcloud

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
	"github.com/odrazdotxyz/odraz-go-sdk/internal/dav"
	"github.com/odrazdotxyz/odraz-go-sdk/internal/davfs"
)

// Name is the registered adapter name.
const Name = "nextcloud"

// PersonalDriveID is the id of the user's own files; team folders use "team:<name>".
const PersonalDriveID = "personal"

const teamPrefix = "team:"

func init() {
	accounts.Register(accounts.Adapter{
		Name:  Name,
		Label: "Nextcloud",
		AuthMethods: []accounts.AuthMethod{{
			ID: "app-password", Label: "Username and app password", Kind: accounts.AuthForm,
			Fields: []accounts.Field{
				{Name: accounts.SecretUsername, Label: "Username", Type: accounts.FieldText, Required: true},
				{Name: accounts.SecretPassword, Label: "App password", Type: accounts.FieldSecret, Required: true, Secret: true},
			},
		}},
		Fields:   append([]accounts.Field{{Name: "url", Label: "Server URL", Type: accounts.FieldURL, Required: true}}, accounts.ProxyFields...),
		Domains:  map[accounts.Domain]any{files.Domain: files.Factory(newClient)},
		Validate: validate,
	})
}

func serverClient(env *accounts.Env) (*dav.Client, error) {
	u := env.Account.Config["url"]
	if u == "" {
		return nil, fmt.Errorf("nextcloud: account %q has no url", env.Account.ID)
	}
	return dav.New(dav.Config{BaseURL: u, Creds: env.Creds, Headers: env.Headers, TLS: env.TLS})
}

type ocsUser struct {
	OCS struct {
		Data struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayname"`
		} `json:"data"`
	} `json:"ocs"`
}

// whoami asks Nextcloud who the credentials belong to. It doubles as the
// "is this a Nextcloud server and do the credentials work" check.
func whoami(ctx context.Context, c *dav.Client, serverURL string) (id, display string, err error) {
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("OCS-APIRequest", "true")
	resp, err := c.Do(ctx, dav.Request{Method: "GET", Path: "/ocs/v2.php/cloud/user", Header: h, Expect: dav.ExpectJSON})
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var u ocsUser
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil || u.OCS.Data.ID == "" {
		return "", "", fmt.Errorf("nextcloud: %s did not answer like a Nextcloud server: %w", serverURL, odraz.ErrNotFound)
	}
	return u.OCS.Data.ID, u.OCS.Data.DisplayName, nil
}

func validate(ctx context.Context, env *accounts.Env) error {
	c, err := serverClient(env)
	if err != nil {
		return err
	}
	_, _, err = whoami(ctx, c, env.Account.Config["url"])
	return err
}

type client struct {
	api *dav.Client
	env *accounts.Env

	mu       sync.Mutex
	id, name string
}

func newClient(_ context.Context, env *accounts.Env) (files.Client, error) {
	api, err := serverClient(env)
	if err != nil {
		return nil, err
	}
	return &client{api: api, env: env}, nil
}

// user resolves (once) the Nextcloud user id and display name.
func (c *client) user(ctx context.Context) (id, name string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.id == "" {
		id, name, err := whoami(ctx, c.api, c.env.Account.Config["url"])
		if err != nil {
			return "", "", err
		}
		c.id, c.name = id, name
	}
	return c.id, c.name, nil
}

// davBase is the WebDAV root of the user's files, optionally below a folder.
func (c *client) davBase(id, folder string) (string, error) {
	u, err := url.Parse(c.env.Account.Config["url"])
	if err != nil {
		return "", err
	}
	// u.Path holds the decoded path; url.URL escapes it once when it is rendered.
	p := strings.TrimRight(u.Path, "/") + "/remote.php/dav/files/" + id
	if folder != "" {
		p += "/" + folder
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = p, "", "", ""
	return u.String(), nil
}

func (c *client) open(base string) (*dav.Client, error) {
	return dav.New(dav.Config{BaseURL: base, Creds: c.env.Creds, Headers: c.env.Headers, TLS: c.env.TLS})
}

func (c *client) Drives(ctx context.Context) ([]files.Drive, error) {
	id, display, err := c.user(ctx)
	if err != nil {
		return nil, err
	}
	base, err := c.davBase(id, "")
	if err != nil {
		return nil, err
	}
	root, err := c.open(base)
	if err != nil {
		return nil, err
	}
	rs, err := root.Propfind(ctx, "/", 1)
	if err != nil {
		return nil, err
	}
	name := display
	if name == "" {
		name = id
	}
	out := []files.Drive{{ID: PersonalDriveID, Name: name, Kind: files.DrivePersonal}}
	for _, r := range rs {
		switch {
		case r.Path == "/":
			out[0].Quota = quota(r)
		case r.IsDir && r.MountType == "group" && strings.Count(r.Path, "/") == 1:
			out = append(out, files.Drive{
				ID: teamPrefix + strings.TrimPrefix(r.Path, "/"), Name: strings.TrimPrefix(r.Path, "/"),
				Kind: files.DriveProject, Quota: quota(r), ReadOnly: readOnly(r.Permissions),
			})
		}
	}
	return out, nil
}

// quota maps WebDAV quota properties. Nextcloud reports a negative
// quota-available-bytes (-3 unlimited, -2 unknown) when there is no limit.
func quota(r dav.Resource) *files.Quota {
	if r.QuotaUsed < 0 {
		return nil
	}
	if r.QuotaAvailable < 0 {
		return &files.Quota{Used: r.QuotaUsed, Total: -1}
	}
	return &files.Quota{Used: r.QuotaUsed, Total: r.QuotaUsed + r.QuotaAvailable}
}

// readOnly reads Nextcloud's permission flags: W update, C create file,
// K create folder, D delete, N rename, V move. A folder offering none of them
// can only be read.
func readOnly(perms string) bool {
	return perms != "" && !strings.ContainsAny(perms, "WCKDNV")
}

func (c *client) OpenDrive(ctx context.Context, driveID string) (files.FS, error) {
	id, _, err := c.user(ctx)
	if err != nil {
		return nil, err
	}
	var folder string
	ro := false
	switch {
	case driveID == PersonalDriveID:
	case strings.HasPrefix(driveID, teamPrefix) && len(driveID) > len(teamPrefix) && !strings.Contains(driveID[len(teamPrefix):], "/"):
		folder = driveID[len(teamPrefix):]
		// Look the folder up again so a team folder removed (or made read-only) since Drives is noticed.
		ds, err := c.Drives(ctx)
		if err != nil {
			return nil, err
		}
		found := false
		for _, d := range ds {
			if d.ID == driveID {
				found, ro = true, d.ReadOnly
			}
		}
		if !found {
			return nil, fmt.Errorf("nextcloud: no drive %q: %w", driveID, odraz.ErrNotFound)
		}
	default:
		return nil, fmt.Errorf("nextcloud: no drive %q: %w", driveID, odraz.ErrNotFound)
	}
	base, err := c.davBase(id, folder)
	if err != nil {
		return nil, err
	}
	d, err := c.open(base)
	if err != nil {
		return nil, err
	}
	fs := files.Wrap(&davfs.FS{C: d})
	if ro {
		fs = files.ReadOnly(fs)
	}
	return fs, nil
}
