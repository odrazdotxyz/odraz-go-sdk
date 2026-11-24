// Command mkbucket waits for the test MinIO and creates the test bucket.
// Used by testenv/minio/seed.sh; test only.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: mkbucket host:port accessKey secretKey bucket")
		os.Exit(2)
	}
	c, err := minio.New(os.Args[1], &minio.Options{Creds: credentials.NewStaticV4(os.Args[2], os.Args[3], "")})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := context.Background()
	var last error
	for i := 0; i < 60; i++ {
		var ok bool
		if ok, last = c.BucketExists(ctx, os.Args[4]); last == nil {
			if !ok {
				last = c.MakeBucket(ctx, os.Args[4], minio.MakeBucketOptions{})
			}
			if last == nil {
				return
			}
		}
		time.Sleep(time.Second)
	}
	fmt.Fprintln(os.Stderr, "minio did not become ready:", last)
	os.Exit(1)
}
