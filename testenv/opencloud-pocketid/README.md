# testenv/opencloud-pocketid

OpenCloud using **Pocket ID as its external OIDC provider** (the built-in IdP is switched off), to test the
whole chain: SDK -> OpenCloud's WebFinger -> Pocket ID login -> Pocket ID token -> OpenCloud Graph + WebDAV.

```sh
eval "$(testenv/opencloud-pocketid/seed.sh)"   # stops testenv/opencloud and testenv/pocketid (same ports)
go test ./vendors/opencloud -run OpenCloudWithPocketID -v
```

Both containers use host networking so that `http://localhost:1411` is the same issuer URL for OpenCloud and for
the browser. OpenCloud is `https://localhost:9200` (self-signed), Pocket ID is `http://localhost:1411`.

## What worked (OpenCloud 9.2.0 + Pocket ID 2.19.3)

OpenCloud settings (see `docker-compose.yml`): `OC_EXCLUDE_RUN_SERVICES=idp`, `OC_OIDC_ISSUER` /
`PROXY_OIDC_ISSUER` = Pocket ID, `PROXY_OIDC_REWRITE_WELLKNOWN=true`,
`PROXY_OIDC_ACCESS_TOKEN_VERIFY_METHOD=jwt` (the two vendors' docs disagree: `jwt` vs `none`; `jwt` works with
Pocket ID's tokens), `PROXY_USER_OIDC_CLAIM=preferred_username`, `PROXY_USER_CS3_CLAIM=username`,
`PROXY_AUTOPROVISION_ACCOUNTS=true` (works with the built-in IDM; the OpenCloud docs say it needs a writable
LDAP, and the built-in IDM is one), `PROXY_ROLE_ASSIGNMENT_DRIVER=default` with
`GRAPH_ASSIGN_DEFAULT_USER_ROLE=true`. No groups or `opencloud_role` claim were needed for a plain user.

- OpenCloud's `/.well-known/webfinger` returns Pocket ID's issuer, so the SDK needs only the OpenCloud URL.
- A Pocket ID user that logs in for the first time is auto-provisioned in OpenCloud, with a personal space.
- A token the server does not know gets 401 (`failed to verify access token`), which triggers the SDK's
  refresh-and-retry.
- Seen once, on the very first upload right after OpenCloud's cold start: `Stat` did not see the file
  immediately. It did not recur in later runs; the test waits a bounded time for it and logs a NOTE if needed.
- Not covered: the OpenCloud web UI and desktop/mobile clients (they need their own clients registered in
  Pocket ID, see Pocket ID's OpenCloud guide), role/group claims, and real passkey logins.
