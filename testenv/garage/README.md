# Garage, test only

```sh
eval "$(testenv/garage/seed.sh)"
go test -race -count=1 ./drivers/s3/ -v
docker compose -f testenv/garage/docker-compose.yml down -v
```

Observed (1.0.1):

- `If-None-Match: *` on a PUT is ignored, so only the adapter's check before an upload protects against overwriting
  (the race test skips).
- Over plain HTTP, uploads of up to 16 MiB are recorded with `aws-chunked` in `Content-Encoding`.
- A multipart copy of a source under 5 MiB is refused (the adapter only does multipart copies above 5 GiB).
- The test key cannot create buckets, so the versioned-bucket test skips (Garage has no versioning).
