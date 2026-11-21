package files

import (
	"fmt"
	"strings"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
)

// CleanPath normalizes a drive-relative path: it always starts with "/",
// has no empty or "." segments, and resolves "..". A path that would climb
// above the drive root, or contains a NUL byte, is rejected with an error
// matching both odraz.ErrInvalidPath and odraz.ErrPermission.
func CleanPath(p string) (string, error) {
	if strings.IndexByte(p, 0) >= 0 {
		return "", fmt.Errorf("files: path contains NUL: %w, %w", odraz.ErrInvalidPath, odraz.ErrPermission)
	}
	var stack []string
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(stack) == 0 {
				return "", fmt.Errorf("files: path %q escapes drive root: %w, %w", p, odraz.ErrInvalidPath, odraz.ErrPermission)
			}
			stack = stack[:len(stack)-1]
		default:
			stack = append(stack, seg)
		}
	}
	return "/" + strings.Join(stack, "/"), nil
}

// Split returns the parent directory and base name of a cleaned path.
// Split("/") returns ("/", "").
func Split(p string) (dir, name string) {
	i := strings.LastIndexByte(p, '/')
	if i <= 0 {
		return "/", strings.TrimPrefix(p, "/")
	}
	return p[:i], p[i+1:]
}

// Join joins segments and cleans the result.
func Join(elem ...string) (string, error) { return CleanPath(strings.Join(elem, "/")) }

// IsRoot reports whether a cleaned path is the drive root.
func IsRoot(p string) bool { return p == "/" }

// IsWithin reports whether cleaned path p is dir or lies below it.
func IsWithin(dir, p string) bool {
	return dir == "/" || p == dir || strings.HasPrefix(p, dir+"/")
}
