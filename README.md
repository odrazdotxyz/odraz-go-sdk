# Odraz SDK

One Go interface for self-hosted clouds. Apps (web gateway, mobile via gomobile, desktop via Wails)
import the SDK and talk to OpenCloud, any WebDAV server, and later other backends through the same
types, without knowing the protocol underneath.

```
go get github.com/odrazdotxyz/odraz-go-sdk
```

Go 1.23+. Status: **v1 = accounts + files** with a generic WebDAV adapter, an OpenCloud adapter and a Nextcloud adapter.

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
- Optional features are optional interfaces, found by type assertion: `files.Copier` today, `Previewer`
  and `EditorOpener` reserved.

## Capabilities

`fs.Capabilities()` tells you what the backend does natively. What every adapter must do is the same
(`files/fstest`); capabilities only describe how.

| Capability | WebDAV (Apache `mod_dav`) | OpenCloud personal / project | OpenCloud Shares space |
|---|---|---|---|
| `AtomicMove` | yes | yes | no |
| `RealDirectories` | yes | yes | yes |
| `RangeRead` | yes | yes | yes |
| `ServerSideCopy` | yes | yes | no |
| `ReadOnly` | no | no | **yes** (`Drive.ReadOnly`) |

Other behaviour worth knowing:

- Other WebDAV servers: `Capabilities` is static for the generic adapter. Servers without `COPY`,
  `MKCOL` on nested paths, or `Range` are not detected yet; run `files/fstest` against yours.
- "Overwrite = false" is a stat followed by a PUT. Some servers ignore `If-None-Match: *` (OpenCloud
  does), so there is a small race window between the two requests.
- Deleting a non-empty folder without `recursive` is a list followed by a DELETE (WebDAV DELETE is always
  recursive); same race window.
- Reads at or past the end of a file return no data on every backend.
- Uploads are a single PUT. TUS (resumable uploads) is planned.
- OpenCloud quota with `total: 0` means unlimited and is reported as `Quota.Total == -1`.
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
Every adapter supports, through the same field system:

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

**Detection.** If a response redirects to another host, or returns HTML where XML or JSON is expected (a
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
go test ./...                                   # unit tests, in-memory and in-process fake server
testenv/webdav/seed.sh && export ODRAZ_TEST_WEBDAV_URL=http://localhost:8081/dav
eval "$(testenv/nextcloud/seed.sh)"             # Nextcloud + Team folders, port 8082; needs Docker
eval "$(testenv/opencloud/seed.sh)"             # needs Docker
eval "$(testenv/proxy/seed.sh)"
go test -race ./...                             # now also runs against the real servers
```

Real-server tests skip themselves when their `ODRAZ_TEST_*` variables are unset. See
[CONTRIBUTING.md](CONTRIBUTING.md) to add a vendor adapter.
