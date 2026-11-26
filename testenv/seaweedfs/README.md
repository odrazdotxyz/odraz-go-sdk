# SeaweedFS (S3 gateway), test only

```sh
eval "$(testenv/seaweedfs/seed.sh)"
go test -race -count=1 ./drivers/s3/ -v
docker compose -f testenv/seaweedfs/docker-compose.yml down -v
```

Observed (version 4.54): it keeps real folders. Removing a folder marker that still has children does nothing and
reports success, so `Delete` removes children first.
