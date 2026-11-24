// Package s3 is the S3-compatible object storage adapter for the files
// domain (AWS S3, MinIO, Ceph, Backblaze B2, Cloudflare R2, ...). Import it for
// its side effect: it registers the "s3" adapter.
//
// An account is one bucket, optionally restricted to a key prefix. The access
// key id and secret access key live in the SecretStore (accounts.SecretUsername
// and accounts.SecretPassword); everything else is in Account.Config:
//
//	endpoint    optional URL or host:port of an S3-compatible server; empty means AWS.
//	            Without a scheme https is assumed.
//	region      default us-east-1
//	bucket      required
//	prefix      optional folder inside the bucket, no leading slash
//	path_style  "true" for path-style addressing; implied when endpoint is set
//	            unless this is "false"
//
// Folders are virtual. A key ending in "/" is a folder marker; a path is a
// folder if its marker exists or any key starts with "<key>/", and a file if
// the exact key exists. If a key is both a file and a folder prefix, the file
// wins: Stat returns it and List on it returns ErrInvalidPath. Listing a
// folder gives the modification time of a folder only when it is not a common
// prefix (Stat finds the marker's time; List returns the zero time).
//
// Writing: Mkdir creates a zero-byte marker object "<key>/" for the folder and
// every missing parent. Write needs an existing parent (a marker or any key
// below it; the root always exists) and, without Overwrite, fails with
// ErrExists; where the server honours "If-None-Match: *" (probed once per
// drive) the put itself is conditional, otherwise a concurrent writer can
// still be overwritten. Uploads stream: a source of unknown size goes up in
// 8 MiB parts. Delete removes the keys under a folder in batches of 1,000.
// Move is a server-side copy followed by a delete, so it is not atomic and a
// failure half way can leave both copies; a folder is copied key by key.
//
// SHA-256: Write records the hash of the whole object as the user metadata
// "odraz-sha256" (S3's own checksum of a multipart upload is only a
// composite), and files.Hasher returns it; an object written by another tool
// without it (and without a full-object SHA-256 checksum) gives ErrUnsupported,
// and files.SHA256 then reads the object. Objects up to 8 MiB of known size
// are hashed in memory before the single PUT; larger or unknown-size objects
// are streamed and then copied onto themselves to add the metadata.
package s3

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/accounts"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
)

// Name is the registered adapter name.
const Name = "s3"

// DriveID is the id of the single drive an S3 account has.
const DriveID = "default"

const defaultRegion = "us-east-1"

func init() {
	accounts.Register(accounts.Adapter{
		Name:  Name,
		Label: "S3",
		AuthMethods: []accounts.AuthMethod{{
			ID: "access-key", Label: "Access key", Kind: accounts.AuthForm,
			Fields: []accounts.Field{
				{Name: accounts.SecretUsername, Label: "Access key ID", Type: accounts.FieldText, Required: true, Secret: true},
				{Name: accounts.SecretPassword, Label: "Secret access key", Type: accounts.FieldSecret, Required: true, Secret: true},
			},
		}},
		Fields: append([]accounts.Field{
			{Name: "endpoint", Label: "Endpoint URL (empty for AWS)", Type: accounts.FieldURL},
			{Name: "region", Label: "Region (default us-east-1)", Type: accounts.FieldText},
			{Name: "bucket", Label: "Bucket", Type: accounts.FieldText, Required: true},
			{Name: "prefix", Label: "Folder in the bucket (optional)", Type: accounts.FieldText},
			{Name: "path_style", Label: "Path-style addressing", Type: accounts.FieldBool},
		}, accounts.ProxyFields...),
		Domains:  map[accounts.Domain]any{files.Domain: files.Factory(newClient)},
		Validate: validate,
	})
}

// settings are the parsed non-secret account options.
type settings struct {
	endpoint  string // host[:port] for the client; "s3.amazonaws.com" for AWS
	secure    bool
	region    string
	bucket    string
	prefix    string // no leading or trailing slash
	pathStyle bool
	custom    bool // endpoint was set
}

func parseSettings(acc accounts.Account) (settings, error) {
	cfg := acc.Config
	s := settings{region: cfg["region"], bucket: strings.TrimSpace(cfg["bucket"]), secure: true}
	if s.region == "" {
		s.region = defaultRegion
	}
	if s.bucket == "" {
		return s, fmt.Errorf("s3: account %q has no bucket", acc.ID)
	}
	s.prefix = strings.Trim(cfg["prefix"], "/")
	ep := strings.TrimSpace(cfg["endpoint"])
	if ep == "" {
		s.endpoint = "s3.amazonaws.com"
		s.pathStyle = cfg["path_style"] == "true"
		return s, nil
	}
	s.custom = true
	if !strings.Contains(ep, "://") {
		ep = "https://" + ep
	}
	u, err := url.Parse(ep)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return s, fmt.Errorf("s3: invalid endpoint %q", cfg["endpoint"])
	}
	if p := strings.Trim(u.Path, "/"); p != "" || u.RawQuery != "" {
		return s, fmt.Errorf("s3: endpoint %q must be a host without a path", cfg["endpoint"])
	}
	s.endpoint = u.Host
	s.secure = u.Scheme == "https"
	s.pathStyle = cfg["path_style"] != "false"
	return s, nil
}

// headerTransport adds the account's extra headers (auth proxies) to every request.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	return t.base.RoundTrip(r)
}

func newMinio(ctx context.Context, env *accounts.Env, s settings) (*minio.Client, error) {
	if env.Creds == nil {
		return nil, fmt.Errorf("s3: account %q has no credentials: %w", env.Account.ID, odraz.ErrAuth)
	}
	cr, err := env.Creds.Credentials(ctx)
	if err != nil {
		return nil, err
	}
	if cr.Username == "" || cr.Password == "" {
		return nil, fmt.Errorf("s3: account %q needs an access key ID and a secret access key: %w", env.Account.ID, odraz.ErrAuth)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = env.TLS
	// Unlike internal/dav this sets no CheckRedirect: minio handles region
	// redirects itself.
	var rt http.RoundTripper = tr
	if len(env.Headers) > 0 {
		rt = headerTransport{base: tr, headers: env.Headers}
	}
	lookup := minio.BucketLookupAuto
	switch {
	case s.pathStyle:
		lookup = minio.BucketLookupPath
	case s.custom:
		lookup = minio.BucketLookupDNS
	}
	mc, err := minio.New(s.endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cr.Username, cr.Password, ""),
		Secure:       s.secure,
		Region:       s.region,
		BucketLookup: lookup,
		Transport:    rt,
	})
	if err != nil {
		// minio's message never holds the keys, but say only what we know.
		return nil, fmt.Errorf("s3: cannot create client for endpoint %q: %w", s.endpoint, err)
	}
	return mc, nil
}

func validate(ctx context.Context, env *accounts.Env) error {
	s, err := parseSettings(env.Account)
	if err != nil {
		return err
	}
	mc, err := newMinio(ctx, env, s)
	if err != nil {
		return err
	}
	// Cancel on return: breaking out of the loop below would otherwise leave
	// the listing goroutine and its connection behind.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Listing one key proves the keys, the bucket and read access in one request.
	opts := minio.ListObjectsOptions{Prefix: s.prefix, MaxKeys: 1}
	if s.prefix != "" {
		opts.Prefix = s.prefix + "/"
	}
	for obj := range mc.ListObjects(ctx, s.bucket, opts) {
		if obj.Err != nil {
			if e := mapValidateErr(obj.Err); e != obj.Err {
				return e
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("s3: cannot reach %q: %w", s.endpoint, obj.Err)
		}
		break
	}
	return nil
}

// New returns a files.Client for an S3 account. Exposed for tests and for
// callers that already hold an accounts.Env.
func New(env *accounts.Env) (files.Client, error) {
	s, err := parseSettings(env.Account)
	if err != nil {
		return nil, err
	}
	mc, err := newMinio(context.Background(), env, s)
	if err != nil {
		return nil, err
	}
	return &client{label: env.Account.Label, s: s, mc: mc}, nil
}

func newClient(_ context.Context, env *accounts.Env) (files.Client, error) { return New(env) }

type client struct {
	label string
	s     settings
	mc    *minio.Client
}

func (c *client) Drives(context.Context) ([]files.Drive, error) {
	name := c.label
	if name == "" {
		name = c.s.bucket
	}
	return []files.Drive{{ID: DriveID, Name: name, Kind: files.DriveOther}}, nil
}

func (c *client) OpenDrive(_ context.Context, id string) (files.FS, error) {
	if id != DriveID {
		return nil, fmt.Errorf("s3: no drive %q: %w", id, odraz.ErrNotFound)
	}
	return files.Wrap(&fs{mc: c.mc, bucket: c.s.bucket, prefix: keyPrefix(c.s.prefix)}), nil
}
