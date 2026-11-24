# testenv/minio

MinIO (`cgr.dev/chainguard/minio`, pinned by digest in `docker-compose.yml`) for the S3 adapter tests.

```sh
eval "$(testenv/minio/seed.sh)"       # fresh server + bucket; exports ODRAZ_TEST_S3_{ENDPOINT,BUCKET,KEY,SECRET}
go test -race -count=1 ./drivers/s3/ -run MinIO -v     # the conformance suite (fstest.Run)
go test -race -count=1 ./drivers/s3/                   # also runs the other *Real tests
(cd testenv/minio && docker compose down -v)           # tear down
```

- Endpoint `http://localhost:9300` (plain HTTP, bound to 127.0.0.1), bucket `odraz-test`, path-style.
- Credentials `odraz-test-only` / `odraz-test-only-not-a-secret`: **test only**, demo values for a throwaway container.
- State lives in tmpfs: every `seed.sh` run starts empty. The image has no shell and no `mc`, so the bucket is created by
  `testenv/minio/mkbucket` (a tiny Go helper using the SDK's own minio-go dependency), which also waits for readiness.
- Docker Hub `minio/minio` and quay.io refused pulls in the environment where this was written; the Chainguard image works.
- The suite is the only conformance coverage of `drivers/s3`: the in-process fake (gofakes3) cannot store folder markers
  (keys ending in `/`) or zero-byte PUTs, and `fstest.Run` cannot be subset. Without the variables it skips.

## Behaviour observed (MinIO from the pinned image, minio-go 7.0.84, probed with a throwaway Go program on 2026-11-24)

The probes used minio-go (`PutObject`, `core.*`, `StatObject`...) rather than curl, since SigV4 by hand is too heavy.
Status and code are what the server returned.

| Topic | Probe | Result |
|---|---|---|
| Single PUT, `If-None-Match: *`, key exists | `PutObject` 10 B with `SetMatchETagExcept("*")` | **412** `PreconditionFailed` |
| Same, key missing | same | ok |
| 20 MiB (multipart) with `If-None-Match: *`, key exists | `PutObject` 20 MiB with the option set | **412** `PreconditionFailed`: minio-go puts the header on `CreateMultipartUpload`, and MinIO honours it there |
| `NewMultipartUpload` with `If-None-Match: *`, key exists | `core.NewMultipartUpload` | 412 `PreconditionFailed` |
| `CompleteMultipartUpload` with `If-None-Match: *` passed in opts, key exists | `core.CompleteMultipartUpload(..., opts)` | 412 `PreconditionFailed`, object unchanged (10 B). So with minio-go 7.0.84 the header on completion is **honoured by MinIO** when the caller passes it; it is the adapter's choice not to send it (see `drivers/s3` doc). Completion without the header: not run against an existing key |
| Ranged GET at/after the end | object of 10 B, `GetObject` with `SetRange` | `10-` and `10-20` and `11-12`: **416** `InvalidRange`; `5-100`: ok, 5 bytes (end is clamped) |
| Copy onto itself, no change | `CopyObject` src == dst | **400** `InvalidRequest` ("trying to copy an object to itself without changing the object's metadata...") |
| Copy onto itself, `REPLACE` metadata | `ReplaceMetadata: true`, one user key | ok |
| Batch delete of missing keys | `RemoveObjects` with 2 missing + 1 existing | no error entries; existing key gone (404 on Stat). Single `RemoveObject` of a missing key: ok |
| Multipart minimum part size | two 1 MiB parts, complete | **400** `EntityTooSmall`; a single 1 MiB (last) part completes fine. Parts of 5 MiB were used elsewhere and complete |
| Initiate multipart on a key ending in `/` | `core.NewMultipartUpload("inc2/dir/")` | fails client side: "Object name contains unsupported characters." |
| Zero-byte PUT of `f/m/` | `PutObject` 0 B | ok; `Stat f/m/` ok, size 0; `Stat f/m` (no slash): 404 `NoSuchKey` |
| List with a marker and a file below it | keys `f/m/`, `f/m/file`, `f/zero` | prefix `f/` non-recursive: `f/zero(0) f/m/(0)` (the marker is listed as a key `f/m/`, not as a common prefix, when it exists); recursive: `f/m/(0) f/m/file(3) f/zero(0)`; prefix `f/m/`: `f/m/(0) f/m/file(3)` (the marker itself is returned) |
| User metadata casing | `UserMetadata{"Odraz-Sha256", "MixedCase"}` | stored and returned as `X-Amz-Meta-Odraz-Sha256`, `X-Amz-Meta-Mixedcase`; minio-go's `UserMetadata` map gives `Odraz-Sha256`, `Mixedcase`. Keys must be matched case-insensitively and come back canonicalised (first letter of each dash part upper case, rest lower) |
| Checksum | `PutObject` with `Checksum: ChecksumCRC32C` | 400 `InvalidArgument` ("Checksum requires Client with TrailingHeaders enabled") unless the client is created with `TrailingHeaders: true`; then ok and `StatObject(Checksum: true)` returns `ChecksumCRC32C`. An object put without a checksum has none on Stat; the ETag is the MD5 for single PUTs |
| Incomplete multipart listing | `core.ListMultipartUploads` with prefixes `inc2/dir`, `inc2/dir/`, `inc2/`, `inc2/di`, with and without delimiter `/`, two open uploads under `inc2/` | **empty for every non-empty prefix**; with the empty prefix all open uploads of the bucket are listed (no common prefixes even with a delimiter). `ListIncompleteUploads` through minio-go gave the same (empty) answers for `inc/dir`, `inc/dir/`, `inc/` |

Not run: multipart completion without `If-None-Match` over an existing key (assumed to overwrite, as in S3); anything over
TLS; versioned buckets; checksum behaviour of multipart uploads.
