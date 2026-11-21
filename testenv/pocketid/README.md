# testenv/pocketid

Pocket ID (`ghcr.io/pocket-id/pocket-id:v2`, tested with 2.19.3) as an external OIDC provider.

```sh
eval "$(testenv/pocketid/seed.sh)"   # fresh instance; exports ODRAZ_TEST_POCKETID_{URL,API_KEY}
go test ./accounts/oidc -run PocketID
```

`http://localhost:1411`, state in tmpfs. `STATIC_API_KEY` gives an admin API (`X-API-KEY` header), which is how
the tests avoid passkeys: create a user and an OIDC client, then log the "browser" in with a one-time access token.

## Behaviour observed

| Topic | Result |
|---|---|
| Discovery | `/.well-known/openid-configuration`; **no WebFinger** (404), so the SDK uses the server URL as issuer |
| Public clients | `token_endpoint_auth_methods_supported` includes `none`; client needs `isPublic` + `pkceEnabled` |
| Redirect URIs | exact match on scheme/host/path, but **any port on loopback** (RFC 8252): register `http://127.0.0.1/callback` |
| `state` | must be at least 8 characters (the SDK sends 32) |
| Scopes | `openid profile email groups offline_access` |
| Login flow | `GET /authorize` -> `302 /interaction?interaction=<id>` (consent needed) -> `GET /api/oidc/interactions/<id>` -> `POST /api/oidc/interactions/<id>/complete {"step":"consent","scopes":[...]}` -> `GET /authorize?interaction=<id>` -> `302` to the app with `code`. Consent is remembered: the next `/authorize` redirects straight to the app |
| Session | cookie `access_token`; from `POST /api/one-time-access-token/<token>` (token from `POST /api/users/<id>/one-time-access-token {"ttl":"15m"}`) |
| Refresh | works with `offline_access`; the refresh token rotates, and the old one was still accepted when replayed |
| Admin API | `POST /api/users`, `POST /api/oidc/clients` (`id`, `name`, `callbackURLs`, `isPublic`, `pkceEnabled`), `DELETE` of both |

OpenCloud trusting Pocket ID as its issuer is covered by `testenv/opencloud-pocketid`.
