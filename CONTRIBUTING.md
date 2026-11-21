# Adding a vendor adapter

The reference implementation is [`vendors/opencloud`](vendors/opencloud/opencloud.go) (a vendor with its own
API plus WebDAV). For a plain protocol server, [`drivers/webdav`](drivers/webdav/webdav.go) is the smaller
example. A new adapter must not require changes to existing domains or adapters.

## 1. Decide what you support

An adapter declares a set of **domains** (v1: `files`). Implement only what the backend really offers; the
rest returns `ErrUnsupported` automatically.

## 2. Create the package

`vendors/<name>/` (or `drivers/<name>/` for a generic protocol). Pick a stable adapter name, e.g. `"nextcloud"`.

## 3. Register in `init()`

```go
func init() {
    accounts.Register(accounts.Adapter{
        Name:        Name,
        Label:       "Nextcloud",
        AuthMethods: []accounts.AuthMethod{ /* see 4 */ },
        Fields:      append([]accounts.Field{{Name: "url", Label: "Server URL", Type: accounts.FieldURL, Required: true}}, accounts.ProxyFields...),
        Domains:     map[accounts.Domain]any{files.Domain: files.Factory(newClient)},
        Validate:    validate,
        // Refresh:  func(env *accounts.Env) accounts.RefreshFunc, only for refreshable bearer tokens
        //           (OIDC): see vendors/opencloud, which reuses accounts/oidc.
    })
}
```

`Register` panics on a duplicate name, like `database/sql`. Apps enable an adapter with a blank import.
Include `accounts.ProxyFields` in `Fields` so users get mTLS and the insecure-TLS option. Extra headers
need no declaration (`header.<Name>` / `secret_header.<Name>` are handled by `accounts`).

## 4. Declare AuthMethods

Each method has an ID, a label, a kind and, for forms, `Fields`. `AuthForm` methods are applied with
`accounts.Configure`. `AuthBrowser` methods (OIDC) are driven by the app with `accounts/oidc`; declare the
method so UIs can offer it, and wire `Adapter.Refresh` to `oidc.RefreshFunc`. Keep IDs stable and add new
methods next to old ones (OpenCloud has both `app-token` and `oidc`).

Secrets: a field with `Secret: true` goes to the `SecretStore` under its field name. The generic provider
reads these keys: `username` (config or store), `password`, or `access_token` / `refresh_token` / `expiry`.
OpenCloud's app token is therefore the `password` field, sent as Basic auth. Adapters never store secrets
themselves; they use `env.Creds`, `env.Headers` and `env.TLS` from the `*accounts.Env` they are given.

## 5. Implement `files.Client`

```go
type Client interface {
    Drives(ctx context.Context) ([]Drive, error)
    OpenDrive(ctx context.Context, driveID string) (FS, error)
}
```

- `Drives` maps whatever the backend has (spaces, buckets, shares) to `files.Drive` with an ID, name, kind
  (`personal`, `project`, `shared`, `other`), optional quota (`-1` for unknown) and `ReadOnly`.
- If the backend returns URLs, use the ones it returns (OpenCloud's `root.webDavUrl`), but keep the
  scheme and host the user configured: the server often reports its own public address, which a client
  behind a proxy cannot use.
- `OpenDrive` returns an `FS`. **Wrap it with `files.Wrap`** so paths are normalized and the drive root
  cannot be escaped. For a WebDAV-style backend reuse `internal/davfs.FS` on an `internal/dav.Client`
  (auth, extra headers, mTLS, redirect/HTML detection and status mapping are already there).
- `Validate` returns a clear error a UI can show: `ErrAuth`, `ErrNotFound` ("not this kind of server"),
  `ErrBlockedByProxy`.

Use `internal/dav` for HTTP: it is what makes `ErrBlockedByProxy` work for you. Do not build your own
`http.Client`.

## 6. Capabilities and optional interfaces

`FS.Capabilities()` must be honest (`AtomicMove`, `RealDirectories`, `RangeRead`, `ServerSideCopy`,
`ReadOnly`). Anything the backend cannot match goes there, not into silent differences. Implement
`files.Copier` only if the backend has a native copy; `files.Copy` falls back by itself. Read-only drives:
`files.ReadOnly(fs)`.

## 7. Add a `testenv`

Add `testenv/<name>/` with a `docker-compose.yml` and a `seed.sh` that brings up a **fresh** instance and prints
`export ODRAZ_TEST_<NAME>_*=...` lines (see `testenv/opencloud/seed.sh`). Probe the real server with `curl`
first and write what you find into `testenv/<name>/README.md` (status codes for MKCOL on an existing
path, PUT without a parent, MOVE/COPY with `Overwrite: F`, ranges, quota...). Those answers decide your
error mapping. Use tmpfs so every run starts empty.

## 8. Pass fstest

```go
func TestConformance(t *testing.T) {
    fstest.Run(t, func(t *testing.T) files.FS { return openRealDrive(t) })
}
```

Gate it on your `ODRAZ_TEST_*` variables and `t.Skip` otherwise. The suite checks listing, nested and
empty folders, unicode and spaces, range reads, overwrite on/off, moving and copying files and folders,
moving onto an existing target, recursive and non-recursive delete, not-found cases, a >16 MB upload and
attempts to escape the drive root. If a drive kind needs it, run it once per kind (OpenCloud runs
personal and project). Read the contract at the top of `files/fstest/fstest.go`: for example writing into a
missing parent must return `ErrNotFound`.

Also add unit tests with a fake server for the parts specific to you (drive mapping, quota, validation
errors, proxy detection) so they run without Docker.

## 9. Checklist

- [ ] `go vet ./...` and `gofmt` clean, `go test -race ./...` passes
- [ ] fstest passes against the real server
- [ ] `Capabilities` documented in the README table; quirks written in `testenv/<name>/README.md`
- [ ] No secrets in `Account.Config`; no WebDAV (or other protocol) types in `files`
- [ ] Existing domains and adapters untouched
