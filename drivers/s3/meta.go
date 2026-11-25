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
			po.UserMetadata[k] = v
			continue
		}
		if v != "" { // an empty value keeps the default
			*header = v
		}
	}
	return po, nil
}

// replacementMetadata returns the headers and user metadata of an upload in the
// form a copy that replaces metadata takes, so that the copy keeps them all.
func replacementMetadata(po minio.PutObjectOptions) map[string]string {
	md := make(map[string]string, len(po.UserMetadata)+7)
	for k, v := range po.UserMetadata {
		md[k] = v
	}
	// Without one the server would pick its own default.
	md["Content-Type"] = "application/octet-stream"
	for k, v := range map[string]string{
		"Cache-Control":       po.CacheControl,
		"Content-Disposition": po.ContentDisposition,
		"Content-Encoding":    po.ContentEncoding,
		"Content-Language":    po.ContentLanguage,
		"Content-Type":        po.ContentType,
	} {
		if v != "" {
			md[k] = v
		}
	}
	if !po.Expires.IsZero() {
		md["Expires"] = po.Expires.UTC().Format(http.TimeFormat)
	}
	return md
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
		md[prefix+k] = v
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
