# Odraz SDK

One Go interface for self-hosted clouds. Apps (web gateway, mobile via gomobile, desktop via Wails)
import the SDK and talk to OpenCloud, Nextcloud, any WebDAV server, S3-compatible object storage, and
later other backends through the same types, without knowing the protocol underneath.

```
go get github.com/odrazdotxyz/odraz-go-sdk
```

Go 1.23+. Status: **v1 = accounts + files** with a generic WebDAV adapter, an OpenCloud adapter, a Nextcloud adapter and an S3 adapter (tested against MinIO only).

## Domains

The SDK is organized by domain. A vendor adapter declares which domains it supports; an app asks an
account for a domain and gets `ErrUnsupported` if the backend has none.

| Domain | Package | Status |
|---|---|---|
| accounts (shared) | `accounts` | v1 |
| files | `files` | v1 |
| calendar (CalDAV) | `calendar` | planned |
| contacts (CardDAV) | `contacts` | planned |
| sharing, photos | | planned |

Domain packages are protocol-neutral: no WebDAV types appear in `files`. Adding a vendor or a domain does
not change existing domains (`files.ForAccount` today, `calendar.ForAccount` later, same pattern).

| Adapter | Package | Domains | Login |
|---|---|---|---|
| WebDAV | `drivers/webdav` | files | username + password |
| S3 | `drivers/s3` | files | access key and secret |
| OpenCloud | `vendors/opencloud` | files | username + app token, or OIDC browser login |
| Nextcloud | `vendors/nextcloud` | files | username + app password |

## Usage

The full, compile-checked version is [`example_test.go`](example_test.go).

```go
import (
    "github.com/odrazdotxyz/odraz-go-sdk/accounts"
    "github.com/odrazdotxyz/odraz-go-sdk/files"
    _ "github.com/odrazdotxyz/odraz-go-sdk/vendors/opencloud" // registers the adapter
)

store := myKeychainStore // your accounts.SecretStore; accounts.NewMemoryStore() is for tests only

// Add an account from form values: secrets go to the store, the rest is Account.Config.
acc, err := accounts.Configure(ctx, store,
    accounts.Account{ID: "home", Label: "Home", Adapter: "opencloud"}, "app-token",
    map[string]string{"url": "https://cloud.example.org", "username": "alice", "password": "<app token>"})

err = accounts.Validate(ctx, acc, store)         // ErrAuth / ErrNotFound / ErrBlockedByProxy

handle, _ := accounts.Open(ctx, acc, store)
client, _ := files.ForAccount(ctx, handle)       // ErrUnsupported if no files domain
drives, _ := client.Drives(ctx)                  // personal, project, shared...
fs, _ := client.OpenDrive(ctx, drives[0].ID)

entries, _ := fs.List(ctx, "/")
err = files.Upload(ctx, fs, "/notes/hello.txt", r, files.WriteOptions{Size: n, Overwrite: true})
err = files.DownloadFile(ctx, fs, "/notes/hello.txt", "/tmp/hello.txt", files.DownloadOptions{}) // resumes
```

Which fields a UI must show comes from the adapter, not from the server: `accounts.Adapters()` lists the
registered adapters, each with its `AuthMethods` (and their `Fields`) and connection `Fields`.

### S3: an archive-style target

One account is a bucket, or a folder (`prefix`) in it, with a single drive. The compile-checked version is
`Example_s3Archive` in [`example_test.go`](example_test.go).

```go
import "github.com/odrazdotxyz/odraz-go-sdk/drivers/s3" // registers the adapter

acc, err := accounts.Configure(ctx, store,
    accounts.Account{ID: "archive", Label: "Archive", Adapter: s3.Name}, "access-key",
    map[string]string{
        "endpoint": "https://s3.example.org", // leave out for AWS
        "region": "us-east-1", "bucket": "photos", "prefix": "archive",
        "username": "<access key id>", "password": "<secret access key>",
    })
// Validate, Open and files.ForAccount as above, then client.OpenDrive(ctx, s3.DriveID).

h := sha256.New() // hash the source while it is uploaded
err = files.Upload(ctx, fs, "/2026/photo.jpg", io.TeeReader(f, h),
    files.WriteOptions{Size: size, Metadata: map[string]string{"source": "camera"}}) // no Overwrite: ErrExists

// To be sure what is stored, read it back: files.SHA256 would return the hash recorded at upload.
rc, err := fs.Read(ctx, "/2026/photo.jpg", 0, -1)
// ...hash rc and compare with h
```

Limits to know before you rely on it (details and provider notes in the
[package documentation](drivers/s3/s3.go)):

- **Only MinIO has been tested.** AWS S3, Backblaze B2, Wasabi and Cloudflare R2 are expected to work but are
  **untested** (as are Ceph and other S3 servers). The conformance suite runs only against a real MinIO
  (`testenv/minio`); there is no always-on conformance test.
- **Folders** are zero-byte marker objects (`name/`) or just common key prefixes.
- **`Move` is copy plus delete**, not atomic. `Copy` and `Move` keep the `Content-Type`, the other standard
  headers and the user metadata.
- **No overwrite** is only race-free for single-PUT uploads (known size up to 16 MiB) on servers that honour
  `If-None-Match: *`. Larger and unknown-size (multipart) uploads only check before they start, so ones that
  overlap in time can both succeed.
- **A source that does not match** fails the `Write` and replaces nothing, also with `Overwrite`: one longer or
  shorter than `Size`, past the unknown-size limit, or returning an error.
- **Metadata** keys `Cache-Control`, `Content-Disposition`, `Content-Encoding`, `Content-Language`,
  `Content-Type` and `Expires` set those headers; the rest is user metadata, and non-ASCII values are stored
  RFC 2047-encoded. Control characters in a value are refused (`ErrInvalidPath`).
- **The stored SHA-256** (user metadata `odraz-sha256`) is set only by this adapter, and a rewrite of the same
  length by another tool is not detected.
- **Uploads above 8 MiB and of unknown size** are copied server-side a second time to attach the hash: they need
  read permission and cost twice. If that copy fails, `Write` returns an error although the content is stored.
  Unknown-size uploads are limited to about 78 GiB.
- **Reads** from an offset above 0 at or past the end of a file fail, as does a positive length on an empty
  file; reading a whole empty file works.
- **Auth proxies** are not detected (no `ErrBlockedByProxy`).
- Give the bucket an **`AbortIncompleteMultipartUpload` lifecycle rule**: a crash can leave unfinished, billed
  uploads behind.

### What `files.FS` gives you

`Stat`, `List` (one level), `Read` (offset/length), `Write` (streaming, size, progress), `Mkdir` (creates
parents, idempotent), `Move`, `Delete` (recursive flag), `Capabilities`. Helpers: `Upload`, `UploadFile`,
`Download`, `DownloadFile` (resumes via a `.part` file), `Rename`, `Walk`, `Copy` (server-side when the
backend supports it, otherwise read+write), `ReadOnly`.

- Paths are `/`-separated and relative to the drive root. They are normalized by the `files` package;
  `..` can never escape the root (`ErrInvalidPath`, which also matches `ErrPermission`).
- Overwrite defaults to `false` and gives `ErrExists`. Deleting a non-empty folder without `recursive`
  gives `ErrNotEmpty`. Writing into a missing parent folder gives `ErrNotFound` (use `files.Upload`,
  which creates parents).
- Errors work with `errors.Is` and always wrap the original error: `ErrNotFound`, `ErrExists`,
  `ErrNotEmpty`, `ErrPermission`, `ErrUnsupported`, `ErrAuth`, `ErrBlockedByProxy`, `ErrInvalidPath`.
- Optional features are optional interfaces, found by type assertion: `files.Copier`, `Previewer` and `Hasher`
  today, `EditorOpener` reserved.
- `WriteOptions.Metadata` sets object metadata on write; backends without object metadata ignore it.

## Capabilities

`fs.Capabilities()` tells you what the backend does natively. What every adapter must do is the same
(`files/fstest`); capabilities only describe how.

| Capability | WebDAV (Apache `mod_dav`) | OpenCloud personal / project | OpenCloud Shares space | S3 |
|---|---|---|---|---|
| `AtomicMove` | yes | yes | no | no |
| `RealDirectories` | yes | yes | yes | no |
| `RangeRead` | yes | yes | yes | yes |
| `ServerSideCopy` | yes | yes | no | yes |
| `ReadOnly` | no | no | **yes** (`Drive.ReadOnly`) | no |

Other behaviour worth knowing (the S3 adapter's own limits are listed above):

- Other WebDAV servers: `Capabilities` is static for the generic adapter. Servers without `COPY`,
  `MKCOL` on nested paths, or `Range` are not detected yet; run `files/fstest` against yours.
- WebDAV: "Overwrite = false" is a stat followed by a PUT. Some servers ignore `If-None-Match: *` (OpenCloud
  does), so there is a small race window between the two requests.
- Deleting a non-empty folder without `recursive` is a list followed by a delete (WebDAV DELETE is always
  recursive); same race window.
- Reads at or past the end of a file return no data on the WebDAV based adapters. S3 returns an error for them
  (details above).
- WebDAV uploads are a single PUT. TUS (resumable uploads) is planned.
- OpenCloud quota with `total: 0` means unlimited and is reported as `Quota.Total == -1`.
- **Avatars** (`Handle.Avatar`, the optional `Adapter.Avatar`): the signed-in user's profile picture. Nextcloud always
  has one (it generates an initials picture); OpenCloud has one only if the user uploaded a photo and answers
  `ErrNotFound` otherwise, so show your own placeholder (the vendor's icon).
- **Thumbnails** (`files.Preview`, the optional `files.Previewer`): OpenCloud and Nextcloud render them on the server
  and the adapters fetch them (OpenCloud: WebDAV GET with `?preview=1`; Nextcloud: `core/preview` by file id). A
  backend or file type without one gives `ErrUnsupported`; fall back to reading the original. Which types get one is
  the server's decision (both render images and text files; video needs ffmpeg on Nextcloud). Plain WebDAV and S3 have none. The size is a hint: servers keep the aspect ratio and may round up to a cached size.
- **Checksums** (`files.SHA256`, the optional `files.Hasher`): returns the lowercase hex SHA-256 of a file. It uses the
  checksum the backend stored when the `FS` is a `Hasher` and has one (today only S3), else reads the file through
  `crypto/sha256`. A stored checksum is not a verification of the stored bytes.
- Nextcloud: the personal drive is the user's files. Each **Team folder** (Group folders app) the user can see
  is also reported as a `DriveProject` drive with its own quota, and `ReadOnly` when the user has no write
  permission. It still appears as a normal folder inside the personal drive, as in Nextcloud's own UI. Quota
  changes can take a few minutes to show (Nextcloud caches them). OIDC login needs Nextcloud's `user_oidc`
  app and is not supported yet.

## OIDC login (browser)

For servers that trust an OpenID Connect provider (OpenCloud's built-in IdP, Pocket ID, Keycloak...),
`accounts/oidc` does the authorization-code flow with PKCE as a public client: no client secret, no
backend, no user database. The only thing stored is the token set, in your `SecretStore`.

```go
sess, _ := oidc.Begin(ctx, nil, "https://cloud.example.org", oidc.Config{
    ClientID: "odraz-app", RedirectURI: redirectURI}) // webfinger + openid-configuration discovery
// Open sess.AuthorizationURL in the system browser / custom tab. The redirect delivers callbackURL.
tokens, err := sess.Complete(ctx, callbackURL)         // checks state, exchanges the code with the PKCE verifier
err = oidc.Save(ctx, store, "home", tokens)
acc := accounts.Account{ID: "home", Adapter: "opencloud",
    Config: map[string]string{"url": "https://cloud.example.org", accounts.ConfigOIDCClientID: "odraz-app"}}
// accounts.Open(ctx, acc, store) now refreshes tokens automatically.
```

- **The SDK never opens a browser**; the app does. `sess.Marshal()` / `oidc.Resume` keep a login alive if the
  app is suspended while the user is in the browser. `oidc.NewLoopback()` receives the redirect on
  `127.0.0.1` for desktop apps.
- **Default scopes** are the minimum: `openid profile email offline_access` (`profile` carries the username the
  server maps users by, `email` is needed because OpenCloud re-compares the token's claims with the stored user
  and fails for existing users when the email claim is missing, `offline_access` gives a refresh token).
- **Refresh**: tokens are refreshed shortly before they expire, and once more if the server answers 401
  (for example a revoked token), then the request is retried once. Requests with a streaming body (uploads)
  are not replayed: that attempt fails with `ErrAuth`, the next one uses the fresh token. Concurrent requests
  share one refresh; rotated refresh tokens are saved. Basic-auth app tokens are never retried.
- **One-time setup per server**: the server's admin must register your app (client ID and redirect URI(s))
  in the identity provider. Without that, the provider refuses the login; users can still use an app token.
  Users are not registered anywhere: they log in with their existing provider account, and the server
  maps the token to its user (OpenCloud: `preferred_username` by default). Both OpenCloud and your app must
  trust the same provider.
- ID tokens are not verified (they come straight from the token endpoint over TLS; the server validates the
  access token on every request).

Verified against a fake provider (discovery, PKCE, state, code replay, refresh rotation, revocation) and
against a **real OpenCloud with its built-in IdP** (`testenv/opencloud`): login with a registered third-party
client, the token on the Graph API and WebDAV, refresh before expiry, recovery from a rejected token, and the
refusals (unregistered client, wrong password, code replay). It is also verified against **Pocket ID** (`testenv/pocketid`) as a standalone provider: discovery without
WebFinger, the desktop loopback redirect on a random port, code exchange with PKCE, userinfo with the token,
refresh, and refusals (unregistered redirect, unknown client, bogus refresh token). And the **whole chain with
OpenCloud trusting Pocket ID** (`testenv/opencloud-pocketid`): given only the OpenCloud URL, the SDK follows
OpenCloud's WebFinger to Pocket ID, logs in there, and uses the token on OpenCloud's Graph API and WebDAV
(upload and stat), including refresh and recovery from a rejected token. Real passkey logins are not covered
(the tests use a one-time access token instead). To try your own setup run
`go run ./examples/oidc-login -server <url> -client-id <id>` (add `-insecure` for self-signed certificates).

## Auth proxies

Self-hosted servers often sit behind an access proxy (Pangolin, Cloudflare Access, an mTLS gateway).
Every adapter supports, through the same field system (S3 sends them too, but see below):

- **Extra headers on every request.** In forms: `header.<Name>` for plain values, `secret_header.<Name>` for
  secret ones. Secret values go through your `SecretStore`, never `Account.Config`.
  ```go
  accounts.Configure(ctx, store, acc, "basic", map[string]string{
      "url": "https://dav.example.org", "username": "u", "password": "p",
      "secret_header.P-Access-Token-Id": "...", "secret_header.P-Access-Token": "...",
  })
  ```
- **Client certificate (mTLS):** `client_cert` and `client_key` (PEM, stored as secrets).
- **Skip TLS verification** (`insecure_skip_verify=true`) for self-signed certificates. This allows
  interception; make users opt in explicitly.

**Detection** (WebDAV based adapters only, not S3). If a response redirects to another host, or returns HTML where XML or JSON is expected (a
login page), the shared transport returns `ErrBlockedByProxy` with a message saying where it went, so a UI
can tell "wrong password" (`ErrAuth`) from "a gateway wants a different credential". Redirects within the
same host are followed. `testenv/proxy` reproduces all three variants.

## Embedding notes

- The main API avoids generics and channels so it can be wrapped for gomobile later; `accounts.Open`
  currently takes variadic options (`WithRefresh`), which a gomobile wrapper would hide.
- Tokens that expire (or are rejected with 401) are refreshed through a pluggable `RefreshFunc`
  (`Adapter.Refresh` or `accounts.WithRefresh`), saved back through `SecretStore`, and concurrent requests
  share one refresh.
- No global state except the adapter registry.

## Development

```sh
go test ./...                                   # unit tests, in-memory and in-process fake servers
testenv/webdav/seed.sh && export ODRAZ_TEST_WEBDAV_URL=http://localhost:8081/dav
eval "$(testenv/nextcloud/seed.sh)"             # Nextcloud + Team folders, port 8082; needs Docker
eval "$(testenv/opencloud/seed.sh)"             # needs Docker
eval "$(testenv/minio/seed.sh)"                # MinIO for the S3 adapter, port 9300; needs Docker
eval "$(testenv/proxy/seed.sh)"
go test -race ./...                             # now also runs against the real servers
```

Real-server tests skip themselves when their `ODRAZ_TEST_*` variables are unset. See
[CONTRIBUTING.md](CONTRIBUTING.md) to add a vendor adapter.
