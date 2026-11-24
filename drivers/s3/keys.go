package s3

import (
	"path"
	"strings"
)

// keyOf maps a drive path to an object key. prefix is the account's key
// prefix with a trailing slash, or empty. The root maps to the prefix itself
// (the folder marker key of the root). Whatever the input, the key never
// contains "//" or "..", and never starts with a slash.
func keyOf(prefix, drivePath string) string {
	p := strings.TrimPrefix(path.Clean("/"+drivePath), "/")
	return prefix + p
}

// pathOf is the inverse of keyOf: it maps an object key under prefix to a
// drive path with a leading slash. A trailing slash (a folder marker or a
// common prefix) is dropped.
func pathOf(prefix, key string) string {
	rest := strings.TrimSuffix(strings.TrimPrefix(key, prefix), "/")
	return "/" + rest
}

// keyPrefix turns the account prefix setting (no slashes at the ends) into
// the form keyOf takes.
func keyPrefix(prefix string) string {
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}
