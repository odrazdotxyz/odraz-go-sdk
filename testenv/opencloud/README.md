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
