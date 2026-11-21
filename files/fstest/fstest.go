// Package fstest is the conformance suite every files.FS implementation
// must pass. Adapters call Run from their own tests:
//
//	fstest.Run(t, func(t *testing.T) files.FS { return openTestDrive(t) })
//
// The FS must be rooted at an empty or disposable drive. Each test works in
// its own random subfolder and removes it afterwards. The FS must already
// normalize paths (use files.Wrap).
//
// Contract fixed by this suite:
//   - Write into a missing parent folder returns ErrNotFound (no implicit parents).
//   - Mkdir creates parents and is idempotent for folders; ErrExists if a file is in the way.
//   - Move without overwrite onto an existing target returns ErrExists; a missing
//     source or missing target parent returns ErrNotFound.
//   - Read past the end of a file is truncated; reads starting at or beyond EOF
//     are not tested because backends disagree (e.g. 416).
//   - Backends without RealDirectories skip the empty-folder test.
package fstest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"testing"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
)

// Run executes the whole suite. newFS is called once per subtest.
func Run(t *testing.T, newFS func(t *testing.T) files.FS) {
	t.Helper()
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			if c.long && testing.Short() {
				t.Skip("long test")
			}
			fs := newFS(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			base := fmt.Sprintf("/fstest-%d", rand.Int63())
			must(t, fs.Mkdir(ctx, base))
			t.Cleanup(func() { _ = fs.Delete(context.Background(), base, true) })
			c.fn(&env{t: t, fs: fs, ctx: ctx, base: base})
		})
	}
}

type env struct {
	t    *testing.T
	fs   files.FS
	ctx  context.Context
	base string
}

func (e *env) p(name string) string { return e.base + "/" + name }

func (e *env) put(name, content string) {
	e.t.Helper()
	must(e.t, e.fs.Write(e.ctx, e.p(name), bytes.NewReader([]byte(content)), files.WriteOptions{Size: int64(len(content))}))
}

func (e *env) get(path string) string {
	e.t.Helper()
	rc, err := e.fs.Read(e.ctx, path, 0, -1)
	must(e.t, err)
	defer rc.Close()
	b, err := io.ReadAll(rc)
	must(e.t, err)
	return string(b)
}

func (e *env) names(path string) []string {
	e.t.Helper()
	es, err := e.fs.List(e.ctx, path)
	must(e.t, err)
	var n []string
	for _, x := range es {
		n = append(n, x.Name)
	}
	sort.Strings(n)
	return n
}

func (e *env) wantErr(err, target error) {
	e.t.Helper()
	if !errors.Is(err, target) {
		e.t.Fatalf("got error %v; want errors.Is(%v)", err, target)
	}
}

func (e *env) wantMissing(path string) {
	e.t.Helper()
	_, err := e.fs.Stat(e.ctx, path)
	e.wantErr(err, odraz.ErrNotFound)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type testCase struct {
	name string
	long bool
	fn   func(*env)
}

var cases = []testCase{
	{"StatAndList", false, func(e *env) {
		root, err := e.fs.Stat(e.ctx, "/")
		must(e.t, err)
		if !root.IsDir {
			e.t.Error("root must be a directory")
		}
		e.put("a.txt", "hello")
		must(e.t, e.fs.Mkdir(e.ctx, e.p("sub")))
		st, err := e.fs.Stat(e.ctx, e.p("a.txt"))
		must(e.t, err)
		if st.IsDir || st.Size != 5 || st.Name != "a.txt" || st.Path != e.p("a.txt") || st.ModTime.IsZero() {
			e.t.Errorf("bad file entry %+v", st)
		}
		d, err := e.fs.Stat(e.ctx, e.p("sub"))
		must(e.t, err)
		if !d.IsDir || d.Name != "sub" {
			e.t.Errorf("bad dir entry %+v", d)
		}
		es, err := e.fs.List(e.ctx, e.base)
		must(e.t, err)
		if len(es) != 2 {
			e.t.Fatalf("list: want 2 entries, got %+v", es)
		}
		for _, x := range es {
			if x.Path != e.p(x.Name) {
				e.t.Errorf("entry %+v: Path must be drive-relative and cleaned", x)
			}
		}
	}},
	{"ListIsOneLevel", false, func(e *env) {
		must(e.t, e.fs.Mkdir(e.ctx, e.p("a/b/c")))
		e.put("a/b/c/deep.txt", "x")
		e.put("a/top.txt", "x")
		if got := fmt.Sprint(e.names(e.p("a"))); got != "[b top.txt]" {
			e.t.Errorf("List(a) = %s", got)
		}
		if got := fmt.Sprint(e.names(e.p("a/b/c"))); got != "[deep.txt]" {
			e.t.Errorf("List(a/b/c) = %s", got)
		}
	}},
	{"EmptyFolder", false, func(e *env) {
		if !e.fs.Capabilities().RealDirectories {
			e.t.Skip("backend has no real directories")
		}
		must(e.t, e.fs.Mkdir(e.ctx, e.p("empty")))
		if n := e.names(e.p("empty")); len(n) != 0 {
			e.t.Errorf("want empty, got %v", n)
		}
	}},
	{"MkdirCreatesParentsAndIsIdempotent", false, func(e *env) {
		must(e.t, e.fs.Mkdir(e.ctx, e.p("x/y/z")))
		must(e.t, e.fs.Mkdir(e.ctx, e.p("x/y/z")))
		must(e.t, e.fs.Mkdir(e.ctx, e.p("x")))
		st, err := e.fs.Stat(e.ctx, e.p("x/y"))
		must(e.t, err)
		if !st.IsDir {
			e.t.Error("parent must be a directory")
		}
	}},
	{"MkdirOverFile", false, func(e *env) {
		e.put("f", "x")
		e.wantErr(e.fs.Mkdir(e.ctx, e.p("f")), odraz.ErrExists)
	}},
	{"UnicodeAndSpaces", false, func(e *env) {
		names := []string{"with space.txt", "ünï cödé.txt", "файл №1.txt", "日本語.txt", "emoji 🎉.txt", "a+b&c=d.txt", "100%.txt", "hash#q?.txt"}
		must(e.t, e.fs.Mkdir(e.ctx, e.p("dir ü/sub dir")))
		for _, n := range names {
			e.put("dir ü/sub dir/"+n, "c:"+n)
		}
		got := e.names(e.p("dir ü/sub dir"))
		want := append([]string(nil), names...)
		sort.Strings(want)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			e.t.Fatalf("names: got %q want %q", got, want)
		}
		for _, n := range names {
			if c := e.get(e.p("dir ü/sub dir/" + n)); c != "c:"+n {
				e.t.Errorf("content of %q = %q", n, c)
			}
		}
	}},
	{"ReadRange", false, func(e *env) {
		e.put("r.txt", "0123456789")
		for _, c := range []struct {
			off, n int64
			want   string
		}{{0, -1, "0123456789"}, {3, 4, "3456"}, {5, -1, "56789"}, {8, 100, "89"}, {0, 1, "0"}, {9, 1, "9"}} {
			rc, err := e.fs.Read(e.ctx, e.p("r.txt"), c.off, c.n)
			must(e.t, err)
			b, err := io.ReadAll(rc)
			rc.Close()
			must(e.t, err)
			if string(b) != c.want {
				e.t.Errorf("Read(off=%d,n=%d) = %q, want %q", c.off, c.n, b, c.want)
			}
		}
	}},
	{"WriteOverwrite", false, func(e *env) {
		e.put("o.txt", "one")
		err := e.fs.Write(e.ctx, e.p("o.txt"), bytes.NewReader([]byte("two")), files.WriteOptions{Size: 3})
		e.wantErr(err, odraz.ErrExists)
		if c := e.get(e.p("o.txt")); c != "one" {
			e.t.Errorf("failed write changed content to %q", c)
		}
		must(e.t, e.fs.Write(e.ctx, e.p("o.txt"), bytes.NewReader([]byte("three")), files.WriteOptions{Size: 5, Overwrite: true}))
		if c := e.get(e.p("o.txt")); c != "three" {
			e.t.Errorf("content = %q", c)
		}
	}},
	{"WriteOntoFolder", false, func(e *env) {
		must(e.t, e.fs.Mkdir(e.ctx, e.p("d")))
		for _, ow := range []bool{false, true} {
			err := e.fs.Write(e.ctx, e.p("d"), bytes.NewReader([]byte("x")), files.WriteOptions{Size: 1, Overwrite: ow})
			e.wantErr(err, odraz.ErrExists)
		}
	}},
	{"WriteMissingParent", false, func(e *env) {
		err := e.fs.Write(e.ctx, e.p("nope/a.txt"), bytes.NewReader([]byte("x")), files.WriteOptions{Size: 1})
		e.wantErr(err, odraz.ErrNotFound)
		e.wantMissing(e.p("nope"))
	}},
	{"WriteProgress", false, func(e *env) {
		data := bytes.Repeat([]byte("p"), 300000)
		var calls int
		var last, lastTotal int64
		must(e.t, e.fs.Write(e.ctx, e.p("p.bin"), bytes.NewReader(data), files.WriteOptions{
			Size: int64(len(data)),
			Progress: func(done, total int64) {
				if done < last {
					e.t.Errorf("progress went backwards: %d after %d", done, last)
				}
				calls++
				last, lastTotal = done, total
			},
		}))
		if calls == 0 || last != int64(len(data)) || lastTotal != int64(len(data)) {
			e.t.Errorf("progress: calls=%d last=%d total=%d", calls, last, lastTotal)
		}
	}},
	{"EmptyFile", false, func(e *env) {
		e.put("empty.txt", "")
		st, err := e.fs.Stat(e.ctx, e.p("empty.txt"))
		must(e.t, err)
		if st.Size != 0 || st.IsDir || e.get(e.p("empty.txt")) != "" {
			e.t.Errorf("bad empty file %+v", st)
		}
	}},
	{"MoveFile", false, func(e *env) {
		e.put("src.txt", "data")
		must(e.t, e.fs.Mkdir(e.ctx, e.p("dst")))
		must(e.t, e.fs.Move(e.ctx, e.p("src.txt"), e.p("dst/moved ü.txt"), false))
		e.wantMissing(e.p("src.txt"))
		if c := e.get(e.p("dst/moved ü.txt")); c != "data" {
			e.t.Errorf("content = %q", c)
		}
	}},
	{"MoveFolder", false, func(e *env) {
		must(e.t, e.fs.Mkdir(e.ctx, e.p("f/inner")))
		e.put("f/a.txt", "A")
		e.put("f/inner/b.txt", "B")
		must(e.t, e.fs.Move(e.ctx, e.p("f"), e.p("g"), false))
		e.wantMissing(e.p("f"))
		if e.get(e.p("g/a.txt")) != "A" || e.get(e.p("g/inner/b.txt")) != "B" {
			e.t.Error("folder contents lost in move")
		}
	}},
	{"MoveOntoExisting", false, func(e *env) {
		e.put("m1", "AAA")
		e.put("m2", "BBB")
		e.wantErr(e.fs.Move(e.ctx, e.p("m1"), e.p("m2"), false), odraz.ErrExists)
		if e.get(e.p("m1")) != "AAA" || e.get(e.p("m2")) != "BBB" {
			e.t.Error("failed move changed data")
		}
		must(e.t, e.fs.Move(e.ctx, e.p("m1"), e.p("m2"), true))
		e.wantMissing(e.p("m1"))
		if e.get(e.p("m2")) != "AAA" {
			e.t.Error("overwrite did not replace content")
		}
	}},
	{"MoveFolderOntoExisting", false, func(e *env) {
		must(e.t, e.fs.Mkdir(e.ctx, e.p("d1")))
		e.put("d1/x", "1")
		must(e.t, e.fs.Mkdir(e.ctx, e.p("d2")))
		e.wantErr(e.fs.Move(e.ctx, e.p("d1"), e.p("d2"), false), odraz.ErrExists)
		must(e.t, e.fs.Move(e.ctx, e.p("d1"), e.p("d2"), true))
		if e.get(e.p("d2/x")) != "1" {
			e.t.Error("folder overwrite failed")
		}
	}},
	{"MoveNotFound", false, func(e *env) {
		e.put("here", "x")
		e.wantErr(e.fs.Move(e.ctx, e.p("missing"), e.p("z"), false), odraz.ErrNotFound)
		e.wantErr(e.fs.Move(e.ctx, e.p("here"), e.p("no/parent/z"), false), odraz.ErrNotFound)
		if e.get(e.p("here")) != "x" {
			e.t.Error("source disturbed")
		}
	}},
	{"DeleteFile", false, func(e *env) {
		e.put("d.txt", "x")
		must(e.t, e.fs.Delete(e.ctx, e.p("d.txt"), false))
		e.wantMissing(e.p("d.txt"))
	}},
	{"DeleteFolder", false, func(e *env) {
		must(e.t, e.fs.Mkdir(e.ctx, e.p("full/sub")))
		e.put("full/sub/f", "x")
		e.wantErr(e.fs.Delete(e.ctx, e.p("full"), false), odraz.ErrNotEmpty)
		if e.get(e.p("full/sub/f")) != "x" {
			e.t.Fatal("non-recursive delete removed data")
		}
		must(e.t, e.fs.Mkdir(e.ctx, e.p("empty")))
		must(e.t, e.fs.Delete(e.ctx, e.p("empty"), false))
		e.wantMissing(e.p("empty"))
		must(e.t, e.fs.Delete(e.ctx, e.p("full"), true))
		e.wantMissing(e.p("full"))
	}},
	{"NotFound", false, func(e *env) {
		_, err := e.fs.Stat(e.ctx, e.p("nope"))
		e.wantErr(err, odraz.ErrNotFound)
		_, err = e.fs.List(e.ctx, e.p("nope"))
		e.wantErr(err, odraz.ErrNotFound)
		_, err = e.fs.Read(e.ctx, e.p("nope"), 0, -1)
		e.wantErr(err, odraz.ErrNotFound)
		e.wantErr(e.fs.Delete(e.ctx, e.p("nope"), false), odraz.ErrNotFound)
		e.wantErr(e.fs.Delete(e.ctx, e.p("nope"), true), odraz.ErrNotFound)
	}},
	{"CopyFile", false, func(e *env) {
		e.put("c1.txt", "copy me")
		must(e.t, files.Copy(e.ctx, e.fs, e.p("c1.txt"), e.p("c2 ü.txt"), false))
		if e.get(e.p("c1.txt")) != "copy me" || e.get(e.p("c2 ü.txt")) != "copy me" {
			e.t.Error("copy must keep source and duplicate content")
		}
	}},
	{"CopyFolder", false, func(e *env) {
		must(e.t, e.fs.Mkdir(e.ctx, e.p("src/inner")))
		e.put("src/a.txt", "A")
		e.put("src/inner/b.txt", "B")
		must(e.t, files.Copy(e.ctx, e.fs, e.p("src"), e.p("dst"), false))
		if e.get(e.p("dst/a.txt")) != "A" || e.get(e.p("dst/inner/b.txt")) != "B" || e.get(e.p("src/inner/b.txt")) != "B" {
			e.t.Error("folder copy incomplete")
		}
	}},
	{"CopyOntoExisting", false, func(e *env) {
		e.put("x", "XXX")
		e.put("y", "YYY")
		e.wantErr(files.Copy(e.ctx, e.fs, e.p("x"), e.p("y"), false), odraz.ErrExists)
		if e.get(e.p("y")) != "YYY" {
			e.t.Error("failed copy changed target")
		}
		must(e.t, files.Copy(e.ctx, e.fs, e.p("x"), e.p("y"), true))
		if e.get(e.p("y")) != "XXX" {
			e.t.Error("overwrite did not replace")
		}
		must(e.t, e.fs.Mkdir(e.ctx, e.p("d1")))
		e.put("d1/old", "1")
		must(e.t, e.fs.Mkdir(e.ctx, e.p("d2")))
		e.put("d2/new", "2")
		must(e.t, files.Copy(e.ctx, e.fs, e.p("d2"), e.p("d1"), true))
		e.wantMissing(e.p("d1/old"))
		if e.get(e.p("d1/new")) != "2" {
			e.t.Error("folder overwrite must replace the target folder")
		}
	}},
	{"CopyErrors", false, func(e *env) {
		e.put("s", "x")
		e.wantErr(files.Copy(e.ctx, e.fs, e.p("missing"), e.p("z"), false), odraz.ErrNotFound)
		e.wantErr(files.Copy(e.ctx, e.fs, e.p("s"), e.p("no/parent/z"), false), odraz.ErrNotFound)
		must(e.t, e.fs.Mkdir(e.ctx, e.p("dir/sub")))
		e.wantErr(files.Copy(e.ctx, e.fs, e.p("dir"), e.p("dir/sub/self"), false), odraz.ErrInvalidPath)
		e.wantErr(files.Copy(e.ctx, e.fs, e.p("dir"), e.p("dir"), true), odraz.ErrInvalidPath)
	}},
	{"CannotEscapeRoot", false, func(e *env) {
		bad := []string{"/..", "../x", e.base + "/../../x", "/a/../../x"}
		sentinel := fmt.Sprintf("escape-%d.txt", rand.Int63())
		for _, p := range append(bad, "/../"+sentinel, e.base+"/../../"+sentinel) {
			_, err := e.fs.Stat(e.ctx, p)
			e.wantInvalid(err, "Stat", p)
			_, err = e.fs.List(e.ctx, p)
			e.wantInvalid(err, "List", p)
			_, err = e.fs.Read(e.ctx, p, 0, -1)
			e.wantInvalid(err, "Read", p)
			e.wantInvalid(e.fs.Write(e.ctx, p, bytes.NewReader([]byte("x")), files.WriteOptions{Size: 1, Overwrite: true}), "Write", p)
			e.wantInvalid(e.fs.Mkdir(e.ctx, p), "Mkdir", p)
			e.wantInvalid(e.fs.Delete(e.ctx, p, true), "Delete", p)
			e.wantInvalid(e.fs.Move(e.ctx, p, e.p("t"), true), "Move(from)", p)
			e.wantInvalid(e.fs.Move(e.ctx, e.p("t"), p, true), "Move(to)", p)
		}
		e.wantMissing("/" + sentinel)
		// "." and "//" segments are normalized, not rejected.
		e.put("n.txt", "x")
		for _, p := range []string{e.base + "/./sub/../n.txt", e.base + "//n.txt"} {
			if e.get(p) != "x" {
				e.t.Errorf("path %q was not normalized", p)
			}
		}
	}},
	{"LargeFile", true, func(e *env) {
		const size = 20<<20 + 12345 // > 16 MiB, not block-aligned
		gen := func() io.Reader { return io.LimitReader(rand.New(rand.NewSource(42)), size) }
		want := sha256.New()
		_, _ = io.Copy(want, gen())
		var last int64
		must(e.t, e.fs.Write(e.ctx, e.p("big.bin"), gen(), files.WriteOptions{
			Size: size, Progress: func(done, _ int64) { last = done },
		}))
		if last != size {
			e.t.Errorf("progress ended at %d, want %d", last, size)
		}
		st, err := e.fs.Stat(e.ctx, e.p("big.bin"))
		must(e.t, err)
		if st.Size != size {
			e.t.Fatalf("size = %d, want %d", st.Size, size)
		}
		rc, err := e.fs.Read(e.ctx, e.p("big.bin"), 0, -1)
		must(e.t, err)
		got := sha256.New()
		_, err = io.Copy(got, rc)
		rc.Close()
		must(e.t, err)
		if !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
			e.t.Error("downloaded content differs from uploaded")
		}
		// Range read in the middle must match the same bytes of the source.
		src := make([]byte, size)
		_, _ = io.ReadFull(gen(), src)
		off, n := int64(17<<20+5), int64(100000)
		rc, err = e.fs.Read(e.ctx, e.p("big.bin"), off, n)
		must(e.t, err)
		part, err := io.ReadAll(rc)
		rc.Close()
		must(e.t, err)
		if !bytes.Equal(part, src[off:off+n]) {
			e.t.Error("range read in large file returned wrong bytes")
		}
	}},
}

func (e *env) wantInvalid(err error, op, p string) {
	e.t.Helper()
	if !errors.Is(err, odraz.ErrInvalidPath) || !errors.Is(err, odraz.ErrPermission) {
		e.t.Errorf("%s(%q): got %v; want ErrInvalidPath and ErrPermission", op, p, err)
	}
}
