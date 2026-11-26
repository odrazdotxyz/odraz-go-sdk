package s3

import (
	"fmt"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/minio/minio-go/v7"
	"golang.org/x/net/http/httpguts"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

// normKey normalises a caller metadata key: it lowercases it and strips
// "x-amz-meta-" prefixes (any case, repeatedly). It reports false for the
// reserved key of the stored hash and every key that still starts with "x-amz-",
// which are dropped. minio-go sends a key that starts with "x-amz-" as a raw
// header (PutObjectOptions.Header) and strips one "x-amz-meta-" from keys of a
// copy (filterCustomMeta), so with this normalisation no key reaching it can
// be, or become, the reserved key in any spelling, and the real hash is the only
// one sent. Callers cannot set the reserved key; their value is silently dropped.
func normKey(k string) (string, bool) {
	k = strings.ToLower(k)
	for strings.HasPrefix(k, "x-amz-meta-") {
		k = strings.TrimPrefix(k, "x-amz-meta-")
	}
	return k, k != shaMetaKey && !strings.HasPrefix(k, "x-amz-")
}

// putOptions turns the caller's metadata for the file p into upload options.
// Keys naming a standard header set that header (Content-Type defaults to the
// one of the file name); the others become user metadata. What cannot be sent
// gives ErrInvalidPath; the message names the key, never the value.
func putOptions(p string, metadata map[string]string) (minio.PutObjectOptions, error) {
	po := minio.PutObjectOptions{UserMetadata: map[string]string{}, ContentType: mime.TypeByExtension(path.Ext(p))}
	keys := make([]string, 0, len(metadata))
	for k := range metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys) // of two spellings of one key, always the same one wins
	for _, raw := range keys {
		k, ok := normKey(raw)
		if !ok {
			continue
		}
		v := metadata[raw]
		bad := func(why string) (minio.PutObjectOptions, error) {
			return po, fmt.Errorf("s3: %s: metadata %q: %s: %w", p, raw, why, odraz.ErrInvalidPath)
		}
		switch {
		case !httpguts.ValidHeaderFieldName(k):
			return bad("the name is not a valid header name")
		case strings.HasPrefix(k, "x-minio-"):
			return bad("names starting with x-minio- are reserved")
		case !httpguts.ValidHeaderFieldValue(v):
			return bad("the value contains CR, LF or another control character")
		}
		var header *string
		switch k {
		case "cache-control":
			header = &po.CacheControl
		case "content-disposition":
			header = &po.ContentDisposition
		case "content-encoding":
			header = &po.ContentEncoding
		case "content-language":
			header = &po.ContentLanguage
		case "content-type":
			header = &po.ContentType
		case "expires":
			if v != "" {
				t, err := http.ParseTime(v)
				if err != nil {
					return bad("not an HTTP date such as " + http.TimeFormat)
				}
				po.Expires = t
			}
			continue
		default:
			po.UserMetadata[k] = wireValue(v)
			continue
		}
		if wireValue(v) != v {
			return bad("the value of a standard header must be ASCII")
		}
		if v != "" { // an empty value keeps the default
			*header = v
		}
	}
	return po, nil
}

// wireValue returns a user metadata value as it is sent: RFC 2047-encoded
// unless it is printable ASCII, because some servers (Backblaze B2) refuse a
// request with other bytes in a header. The client library decodes it again.
func wireValue(v string) string {
	return mime.QEncoding.Encode("UTF-8", v)
}

// storedMetadata returns an object's headers and user metadata in the form a
// copy that replaces metadata takes.
//
// With the "x-amz-meta-" prefix a key stays user metadata even when it is named
// like a standard header. CopyObject (oneRequest) strips one such prefix before
// it builds the headers and ComposeObject does not, so the former gets two.
func storedMetadata(st minio.ObjectInfo, oneRequest bool) map[string]string {
	prefix := "X-Amz-Meta-"
	if oneRequest {
		prefix += prefix
	}
	md := map[string]string{}
	for k, v := range st.UserMetadata {
		md[prefix+k] = wireValue(v) // the client library decoded it
	}
	for _, h := range []string{"Cache-Control", "Content-Disposition", "Content-Encoding", "Content-Language"} {
		if v := st.Metadata.Get(h); v != "" {
			md[h] = v
		}
	}
	if st.ContentType != "" {
		md["Content-Type"] = st.ContentType
	}
	if !st.Expires.IsZero() {
		md["Expires"] = st.Expires.UTC().Format(http.TimeFormat)
	}
	return md
}
