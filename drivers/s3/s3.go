// Package s3 is the S3-compatible object storage adapter for the files
// domain. Import it for its side effect: it registers the "s3" adapter.
//
// # Account
//
// An account is one bucket, optionally restricted to a key prefix, and has one
// drive (id [DriveID]). The access key id and secret access key live in the
// SecretStore (accounts.SecretUsername and accounts.SecretPassword); everything
// else is in Account.Config:
//
//	endpoint    optional URL of an S3-compatible server; empty means AWS.
//	            accounts.Configure requires http:// or https://; a bare
//	            host:port set directly in Config is taken as https.
//	            No credentials, path, query or fragment; the errors about such an
//	            endpoint do not repeat it.
//	region      default us-east-1
//	bucket      required
//	prefix      optional folder inside the bucket (outer slashes are ignored)
//	path_style  "true" for path-style addressing; implied when endpoint is set
//	            unless this is "false"
//
// The common proxy fields work too (extra headers, client certificate,
// insecure_skip_verify). Extra headers are added after the request is signed, so
// they must not replace headers S3 signs (such as Authorization or Host). Unlike
// the WebDAV based adapters this one does not detect auth proxies: it never
// returns ErrBlockedByProxy. Redirects are not followed by the HTTP client.
//
// Validate lists one key under the prefix, which proves the keys, the bucket and
// list permission (AccessDenied there is ErrAuth). It does not prove read, write
// or delete permission. The driver needs to list the bucket and to get, put and
// delete objects; aborting failed uploads also needs the multipart list and abort
// permissions.
//
// # Providers
//
// Only MinIO has been tested (the image pinned in testenv/minio, over plain HTTP
// with path-style addressing and an unversioned bucket). Everything else below is
// expected to work but UNTESTED; check endpoints and regions in the provider's
// documentation.
//
//	MinIO           endpoint http(s)://host:port, path-style (the default)
//	AWS S3          no endpoint, the bucket's region; untested
//	Backblaze B2    endpoint https://s3.<region>.backblazeb2.com; untested
//	Wasabi          endpoint https://s3.<region>.wasabisys.com; untested
//	Cloudflare R2   endpoint https://<account id>.r2.cloudflarestorage.com,
//	                region auto; untested
//	Ceph and others their S3 endpoint; untested
//
// Points to check on an untested provider: multipart uploads ask for CRC32C
// checksums; objects above 5 GiB are copied in parts whose sizes can differ by
// one byte, while some services (R2 documents this) want equal parts; and the
// conditional put described below may be ignored. On a versioned bucket (B2
// buckets keep versions) deleted and replaced objects stay as old versions.
//
// # Folders
//
// Folders are virtual. A key ending in "/" is a folder marker; a path is a folder
// if its marker exists or any key starts with "<key>/", and a file if the exact
// key exists. If a key is both a file and a folder prefix, the file wins: Stat
// returns it and List on it returns ErrInvalidPath. Mkdir creates a zero-byte
// marker object "<key>/" for the folder and every missing parent, so empty
// folders persist (Capabilities still report no RealDirectories). A folder that
// exists only because of the keys below it disappears with the last of them. List
// returns the zero modification time for folders; Stat returns the marker's time
// when the folder has a marker. List entries of files have no MimeType (Stat has
// it). Keys with empty, "." or ".." segments, written by other tools, cannot be
// addressed. Copy and Move recreate every key ending in "/" as an empty marker,
// so data another tool stored under such a key is not carried over.
//
// # Reading
//
// Read supports ranges. A range that starts at an offset above 0 and at or past
// the end of the file fails, and so does asking an empty file for a positive
// length: the returned reader gives the server's error (MinIO answers
// InvalidRange). Reading a whole file (offset 0, negative length) works, also an
// empty one. A range that only ends past the end is cut off there, and length 0
// gives an empty result.
//
// # Writing
//
// Write needs an existing parent (a marker or any key below it; the root always
// exists) and, without Overwrite, fails with ErrExists after a Stat. A single-PUT
// upload (known size up to 16 MiB) also sends "If-None-Match: *" where the
// server honours it, and the server then enforces it on the put, which closes the
// race with a concurrent writer. Whether it does is found out once per drive by
// writing and deleting a small ".odraz/probe-*" object under the prefix (it, and
// with it a folder ".odraz", remains if deleting is not permitted). Multipart
// uploads (known size above 16 MiB, and every upload of unknown size) do not
// send the header: the high-level PutObject can only send it when the upload is
// initiated, not on completion. Only the Stat guards them, so two writers whose
// uploads overlap in time can both succeed. On servers that ignore the header
// only the Stat guards any upload.
//
// Uploads stream. A source of unknown size goes up in 8 MiB parts and may not
// exceed 8 MiB x 10,000 parts (about 78 GiB); a longer one fails with
// ErrUnsupported. A source longer than the declared Size fails with
// ErrInvalidPath, and one that fails or ends before the declared Size fails the
// Write too. In all these cases the upload is never completed: nothing of the new
// content is stored as the object, and with Overwrite the previous object stays
// as it was (the parts of a multipart upload are aborted best effort, below).
// Progress counts bytes read from the source; it can reach the total while the
// upload is still running.
//
// WriteOptions.Metadata becomes user metadata (x-amz-meta-*), except that the
// keys Cache-Control, Content-Disposition, Content-Encoding, Content-Language,
// Content-Type and Expires (in any case) set those headers. Without Content-Type
// the one of the file name's extension is used; Expires must be an HTTP date.
// Keys starting with "x-amz-" (after removing "x-amz-meta-") are dropped. A name
// that is not a valid header name or starts with "x-minio-", a value with CR, LF
// or another control character (tab is allowed) and an unreadable Expires fail
// with ErrInvalidPath before anything is sent. Values that are not ASCII are
// stored RFC 2047-encoded ("=?UTF-8?q?...?="): the client library used here
// decodes them when reading, other tools may show the encoded form
// (mime.WordDecoder decodes it). The files API cannot read metadata back.
//
// Failed or cancelled uploads are aborted best effort (RemoveIncompleteUpload
// with a context that is not cancelled), but a crash can leave incomplete
// multipart uploads behind, which are billed: give the bucket an
// AbortIncompleteMultipartUpload lifecycle rule.
//
// # Delete, Move and Copy
//
// Delete removes the keys under a folder in batches of 1,000. Copy is a
// server-side copy, key by key for a folder: one CopyObject request for an object
// up to 5 GiB, a multipart copy above that (tested only with that limit lowered).
// Move is that copy followed by a delete, so it is not atomic and a failure half
// way can leave both copies. When a folder replaces or is replaced (overwrite),
// the target is deleted before the copy starts. Copy and Move keep the
// Content-Type, the other headers listed above and the user metadata, including
// the stored SHA-256; the ETag can change. Each file is asked for its metadata
// first, because MinIO loses Expires in a plain copy: such an object, like one
// above 5 GiB, is copied with its headers and metadata set again.
//
// # SHA-256
//
// Write records the whole object's hash as the user metadata "odraz-sha256" with
// the value "<64 lowercase hex>:<size in bytes>" (S3's own checksum of a
// multipart upload is only a composite), and files.Hasher returns it. Only this
// adapter sets it; a full-object S3 SHA-256 checksum stored by another tool is
// accepted too. Callers cannot set the key through WriteOptions.Metadata; it is
// dropped, also when spelled "X-Amz-Meta-Odraz-Sha256". SHA256 gives
// ErrUnsupported when there is neither, when the value is malformed, or when the
// size in it differs from the object's current size (another tool rewrote the
// content and kept the metadata); files.SHA256 then reads the object.
// LIMITATION: a rewrite of the same length is not detected. The hash vouches for
// what this driver was given to write, and whoever must be sure has to verify by
// reading.
//
// A hash is stored only when it costs nothing: objects up to 8 MiB of known size
// are hashed in memory before the single PUT, and for a larger object of known
// size the caller passes it as WriteOptions.SHA256. A supplied hash is checked
// against the data as it streams, for any size: a mismatch fails with
// ErrChecksum and, as above, stores and replaces nothing. Other uploads store no
// hash (one of unknown size cannot, as the size is part of the value), and
// SHA256 gives ErrUnsupported for them. Every object is uploaded once.
//
// # Testing
//
// The in-process fake cannot store folder markers, so there is no always-on
// conformance test: the conformance suite (TestConformanceS3) and the other
// *Real tests need a real server. Run eval "$(testenv/minio/seed.sh)" and then
// go test ./drivers/s3/ -run Conformance; see testenv/minio/README.md for the behaviour
// observed there. Without the ODRAZ_TEST_S3_* variables those tests skip.
package s3

import (
	"context"
	"errors"
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
	// No message here repeats the endpoint or any part of it: someone may have
	// put credentials in it, and a secret with "/", "?" or "#" in it ends up in
	// what parses as the host, path, query or fragment.
	u, err := url.Parse(ep)
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https"):
		return s, errors.New("s3: invalid endpoint: want http(s)://host[:port]")
	case u.User != nil:
		return s, errors.New("s3: invalid endpoint: it must not contain credentials (the access key ID and secret access key are separate fields)")
	case strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(ep, "#"):
		return s, errors.New("s3: invalid endpoint: it must be a host without a path, query or fragment")
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
