# testenv/nextcloud

Nextcloud (SQLite) with the Team folders ("Group folders") app, for the `vendors/nextcloud` tests.

```sh
eval "$(testenv/nextcloud/seed.sh)"   # first run pulls the image and installs Nextcloud (about a minute)
go test ./vendors/nextcloud           # exports ODRAZ_TEST_NEXTCLOUD_URL=http://localhost:8082
```

| User | Password | Group | Sees "Team Docs" |
|---|---|---|---|
| `admin` | `admin-pass-123` | | no |
| `alice` | `alice-pass-123` | `team` | read, write, delete, share (64 MiB quota) |
| `bob` | `bob-pass-123` | `viewers` | read only |

State lives in tmpfs, so a restart wipes it; run `seed.sh` again.

## Behaviour observed (Nextcloud 35.0.3, Team folders 23.0.1, probed with curl)

| Topic | Result |
|---|---|
| Files root | `/remote.php/dav/files/<user-id>/`. The **id** (not the login name) goes in the path: `GET /ocs/v2.php/cloud/user` with `OCS-APIRequest: true` and `Accept: application/json` returns `ocs.data.id` and `displayname`; wrong credentials give 401 |
| Team folder | a top-level folder of each member's tree; PROPFIND `nc:mount-type` is `group` (empty for ordinary folders). The admin API (`/apps/groupfolders/folders`) is admin-only, so the adapter does not use it |
| Permissions | `oc:permissions` flags: `G` read, `W` update, `C` create, `K` mkdir, `D` delete, `N` rename, `V` move, `R` share, `M` mounted. Alice: `RMGDNVCK`; bob (read only): `MG` |
| Quota | `quota-available-bytes` is the folder quota minus use; `-3` means unlimited. A changed quota is served from an APCu cache for a few minutes |
| Range GET | **`bytes=0-0` returns the whole file** as a `206` with `Content-Range: bytes 0-9/10`; other ranges are exact. The SDK trims by `Content-Range` |
| GET on a folder | 200 with an HTML listing (not an error), so the SDK checks `text/html` responses with a PROPFIND |
| Thumbnails | `PROPFIND` `nc:has-preview` is `true`/`false` per file; `GET /index.php/core/preview?fileId=<oc:fileid>&x=64&y=64&a=true&forceIcon=0` returns a small PNG (7 KB at 64 px for a 670 KB image, aspect kept; a request for 128 px came back 256 px wide, so the size is a hint and the server rounds up). Images and text files get one; ffmpeg is not in the image, so no video thumbnails. `forceIcon=0` makes a missing preview a 404 instead of a generic icon |
| Avatar | `GET /index.php/avatar/<user id>/<size>` returns a PNG (924 bytes at 64 px; initials on a coloured background when no photo was uploaded); an unknown user is 404 |
