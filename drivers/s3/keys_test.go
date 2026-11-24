package s3

import (
	"strings"
	"testing"
)

func TestKeyRoundTrip(t *testing.T) {
	paths := []string{"/", "/a", "/a/b c.jpg", "/é/日本.png", "/a+b/%41#?.txt"}
	for _, prefix := range []string{"", "p/", "deep/er/"} {
		for _, p := range paths {
			k := keyOf(prefix, p)
			if !strings.HasPrefix(k, prefix) {
				t.Errorf("keyOf(%q, %q) = %q: missing prefix", prefix, p, k)
			}
			if got := pathOf(prefix, k); got != p {
				t.Errorf("pathOf(%q, keyOf(%q)) = %q (key %q)", prefix, p, got, k)
			}
		}
	}
	if got := keyOf("p/", "/a/b.jpg"); got != "p/a/b.jpg" {
		t.Errorf("keyOf = %q", got)
	}
	if got := keyOf("p/", "/"); got != "p/" {
		t.Errorf("root key = %q", got)
	}
	if got := keyOf("", "/"); got != "" {
		t.Errorf("root key without prefix = %q", got)
	}
}

func TestKeyRejectsDoubleSlashAndDotDot(t *testing.T) {
	for _, p := range []string{"//a//b", "/a//b/", "/a/../b", "/../a", "/a/./b", "a/b"} {
		for _, prefix := range []string{"", "p/"} {
			k := keyOf(prefix, p)
			if strings.Contains(k, "//") || strings.Contains(k, "..") || strings.HasPrefix(k, "/") {
				t.Errorf("keyOf(%q, %q) = %q", prefix, p, k)
			}
		}
	}
}
