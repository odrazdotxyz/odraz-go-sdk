package s3

import (
	"context"
	"crypto/md5" //nolint:gosec // S3 ETags are MD5; compared, not relied on against an attacker
	"encoding/hex"
	"fmt"
	"hash"
	"strings"

	"github.com/minio/minio-go/v7"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
)

// maxParts is the most parts S3 allows in one multipart upload.
const maxParts = 10000

// PartSize is the part size Write uses for a file of a known size: 16 MiB, or
// for very large files the smallest whole number of MiB that needs no more than
// 10,000 parts. A Check depends on it: the ETag of a multipart object does.
func PartSize(size int64) int64 {
	const mib = 1 << 20
	p := (size + maxParts - 1) / maxParts
	p = (p + mib - 1) / mib * mib
	return max(p, singlePutLimit)
}

// check computes the ETag the server gives an object that Write stored from the
// bytes fed to it: the MD5 of the content for a single PUT, and for a multipart
// upload the MD5 of the parts' MD5s followed by the number of parts.
type check struct {
	f      *fs
	size   int64
	n      int64
	part   int64     // part size
	cur    hash.Hash // MD5 of the current part (of everything for a single PUT)
	inPart int64
	sums   []byte
	parts  int
}

func (f *fs) NewCheck(size int64) files.Check {
	c := &check{f: f, size: size, cur: md5.New()} //nolint:gosec // see above
	if size > singlePutLimit {
		c.part = PartSize(size)
	}
	return c
}

func (c *check) Write(b []byte) (int, error) {
	total := len(b)
	c.n += int64(total)
	for c.part > 0 && int64(len(b)) >= c.part-c.inPart {
		k := c.part - c.inPart
		c.cur.Write(b[:k])
		b = b[k:]
		c.closePart()
	}
	c.cur.Write(b)
	c.inPart += int64(len(b))
	return total, nil
}

func (c *check) closePart() {
	c.sums = c.cur.Sum(c.sums)
	c.parts++
	c.cur.Reset()
	c.inPart = 0
}

// etag is what the server should report; it does not change the check.
func (c *check) etag() string {
	if c.part == 0 {
		return hex.EncodeToString(c.cur.Sum(nil))
	}
	sums, parts := c.sums, c.parts
	if c.inPart > 0 {
		sums, parts = c.cur.Sum(append([]byte(nil), sums...)), parts+1
	}
	all := md5.Sum(sums) //nolint:gosec // see above
	return fmt.Sprintf("%s-%d", hex.EncodeToString(all[:]), parts)
}

// Verify compares the object at p with the bytes fed to the check, asking the
// server for its metadata only. It is meant for an object Write uploaded: a
// copy on the server can give the same content another ETag.
func (c *check) Verify(ctx context.Context, p string) error {
	if c.size < 0 {
		return fmt.Errorf("s3: %s: cannot check an upload of unknown size: %w", p, odraz.ErrUnsupported)
	}
	if c.n != c.size {
		return fmt.Errorf("s3: %s: %d bytes were checked, expected %d: %w", p, c.n, c.size, odraz.ErrChecksum)
	}
	st, err := c.f.mc.StatObject(ctx, c.f.bucket, keyOf(c.f.prefix, p), minio.StatObjectOptions{})
	if err != nil {
		return mapErr(err)
	}
	if st.Size != c.size {
		return fmt.Errorf("s3: %s: the object has %d bytes, expected %d: %w", p, st.Size, c.size, odraz.ErrChecksum)
	}
	if strings.HasPrefix(st.Metadata.Get("X-Amz-Server-Side-Encryption"), "aws:kms") ||
		st.Metadata.Get("X-Amz-Server-Side-Encryption-Customer-Algorithm") != "" {
		return fmt.Errorf("s3: %s: the ETag of an object encrypted this way is not a checksum: %w", p, odraz.ErrUnsupported)
	}
	if got, want := strings.Trim(st.ETag, `"`), c.etag(); !strings.EqualFold(got, want) {
		return fmt.Errorf("s3: %s: the server's checksum %q is not %q: %w", p, got, want, odraz.ErrChecksum)
	}
	return nil
}
