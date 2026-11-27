package davfs

import (
	"testing"

	"github.com/odrazdotxyz/odraz-go-sdk/files"
)

// Only an adapter whose server was seen to honour it may offer DeleteIfMatch:
// OpenCloud ignores If-Match on DELETE, so the shared WebDAV code must not.
func TestPlainWebDAVOffersNoConditionalDelete(t *testing.T) {
	if _, ok := any(&FS{}).(files.ConditionalDeleter); ok {
		t.Fatal("every WebDAV adapter would claim it")
	}
}
