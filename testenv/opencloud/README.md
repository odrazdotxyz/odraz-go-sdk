# testenv/opencloud

OpenCloud (`opencloudeu/opencloud-rolling`) with its built-in IdP, for adapter tests.

```sh
eval "$(testenv/opencloud/seed.sh)"   # fresh instance + app token; exports ODRAZ_TEST_OPENCLOUD_{URL,USER,TOKEN}
```

- URL `https://localhost:9200`, **self-signed certificate** (the built-in IdP refuses a non-https issuer), so
  clients need an opt-in "skip certificate verification" option.
- Admin `admin` / `admin` (basic auth is enabled for probing only). The adapter uses the **app token**
  (a phrase with spaces, e.g. `freehand heroism pesticide ...`) as the Basic-auth password.
- State lives in tmpfs: every `seed.sh` run starts empty (and the token changes).

## Behaviour observed (OpenCloud 9.2.0, probed with curl)

| Topic | Result |
|---|---|
| Drives | `GET /graph/v1.0/me/drives` → `value[]` with `id`, `name`, `driveType` (`personal`, `project`, `virtual` = Shares), `quota`, `root.webDavUrl` |
| Space WebDAV URL | `root.webDavUrl`, e.g. `https://host/dav/spaces/<id>$<id>` (contains `$`); use it as the WebDAV base |
| Quota | `total: 0` means unlimited (personal space); project spaces report real `total/used/remaining` |
| PROPFIND quota props | `quota-available-bytes: -2` means unlimited/unknown (negative) |
| PROPFIND | 207, `application/xml`, ETags quoted, props the server lacks come in a `404` propstat; `Depth: infinity` → 400 |
| MKCOL existing / over file | 405 |
| MKCOL / PUT / MOVE / COPY with missing parent | 409 |
| PUT onto a folder | 409 |
| `PUT` + `If-None-Match: *` on existing file | **ignored** (204, file overwritten) → adapter must stat first |
| MOVE / COPY onto existing with `Overwrite: F` | 412 |
| MOVE / COPY with `Overwrite: T` | 204 |
| DELETE non-empty folder | 204 (recursive) |
| Range GET | 206 for `a-b`, `a-`, `-n`; 416 only when unsatisfiable |
| GET on a folder | 200 (not an error) |
| Missing path | 404 |
| Unauthenticated | 401 with `WWW-Authenticate: Bearer` |

## OIDC (built-in IdP, "Konnect")

- **Custom client**: `idp.yaml` is mounted into `/etc/opencloud` and registers the SDK's public test client
  `odraz-test` (native, no secret, redirect `http://127.0.0.1` / `http://localhost`, any port). The `clients`
  list replaces the generated defaults, so the standard clients are repeated in it. An unregistered
  `client_id` is refused at login with `unknown client_id`.
- **Tokens**: access tokens live **300 s**; refresh tokens work with `offline_access` and are rotated, but are
  stateless, so a rotated-away refresh token keeps working until it expires (other IdPs reject it).
  Tokens are accepted as `Authorization: Bearer` on `/graph` and `/dav`; a bad token gets 401.
- **Headless login** (what `vendors/opencloud/oidc_real_test.go` does, read from the sign-in page's scripts):
  1. `GET authorization_endpoint?...` (redirects to the login page; nothing needed from it)
  2. `POST /signin/v1/identifier/_/logon` JSON `{"state","params":[user,password,"1"],"hello":{"scope","client_id","redirect_uri","flow":"oidc"}}`
     with headers `Kopano-Konnect-XSRF: 1`, **`Origin`** and **`Referer`** (without them: 400, "missing origin or referer header")
  3. if the answer has `hello.next == "consent"` (clients that are not `trusted`): `POST .../_/consent`
     `{"state","allow":true,"scope":"openid profile offline_access","client_id","redirect_uri","ref":<authorize state>,"flow_nonce":""}`
  4. `GET /signin/v1/identifier/_/authorize?<original params>&konnect=<state from step 3, or from step 2>`
     -> `302` to the redirect URI with `code` and `state`
| Thumbnails | WebDAV `GET <file>?preview=1&x=64&y=64&a=1&scalingup=0` returns a PNG (9.9 KB for a 677 KB image, 64x64 crop); `404` for a type it cannot render (`.md` and `.bin` had none, `.txt` did); folders 404 |
| Avatar | `GET /graph/v1.0/me/photo/$value` is `404` `failed to get photo` for a user without a photo (the demo users); `/graph/v1.0/me` has `displayName` |
