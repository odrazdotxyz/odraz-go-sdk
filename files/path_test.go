package files

import (
	"errors"
	"testing"

	odraz "github.com/odraz/sdk"
)

func TestCleanPath(t *testing.T) {
	ok := map[string]string{
		"": "/", "/": "/", ".": "/", "a": "/a", "/a/": "/a", "a//b": "/a/b",
		"/a/./b": "/a/b", "/a/b/..": "/a", "a/../b": "/b", "/a/b/../../": "/",
		"/ünï cödé/файл 1.txt": "/ünï cödé/файл 1.txt", "/a\\..\\b": "/a\\..\\b",
		"/..a": "/..a", "/a../b": "/a../b",
	}
	for in, want := range ok {
		got, err := CleanPath(in)
		if err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"..", "/..", "../a", "a/../..", "/a/../../b", "a\x00b"} {
		_, err := CleanPath(in)
		if !errors.Is(err, odraz.ErrInvalidPath) || !errors.Is(err, odraz.ErrPermission) {
			t.Errorf("CleanPath(%q) err = %v; want ErrInvalidPath+ErrPermission", in, err)
		}
	}
}

func TestSplitJoinWithin(t *testing.T) {
	for in, want := range map[string][2]string{"/": {"/", ""}, "/a": {"/", "a"}, "/a/b": {"/a", "b"}} {
		d, n := Split(in)
		if d != want[0] || n != want[1] {
			t.Errorf("Split(%q) = %q,%q", in, d, n)
		}
	}
	if p, err := Join("/a", "b/../c"); err != nil || p != "/a/c" {
		t.Errorf("Join = %q, %v", p, err)
	}
	if !IsWithin("/a", "/a/b") || !IsWithin("/a", "/a") || IsWithin("/a", "/ab") || !IsWithin("/", "/x") {
		t.Error("IsWithin wrong")
	}
}
