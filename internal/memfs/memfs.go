// Package memfs is an in-memory files.FS used to test the SDK itself.
package memfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	odraz "github.com/odrazdotxyz/odraz-go-sdk"
	"github.com/odrazdotxyz/odraz-go-sdk/files"
)

// memFS is a reference files.FS used to check the suite itself.
type memFS struct {
	mu    sync.Mutex
	nodes map[string]*node // key: cleaned path; "/" always present
}

type node struct {
	dir  bool
	data []byte
	mod  time.Time
}

// New returns an empty in-memory FS (normalizing paths via files.Wrap).
func New() files.FS {
	return files.Wrap(&memFS{nodes: map[string]*node{"/": {dir: true, mod: time.Now()}}})
}

func nf(p string) error { return fmt.Errorf("%s: %w", p, odraz.ErrNotFound) }

func (m *memFS) entry(p string, n *node) files.Entry {
	_, name := files.Split(p)
	return files.Entry{Path: p, Name: name, IsDir: n.dir, Size: int64(len(n.data)), ModTime: n.mod}
}

func (m *memFS) Stat(_ context.Context, p string) (files.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[p]
	if !ok {
		return files.Entry{}, nf(p)
	}
	return m.entry(p, n), nil
}

func (m *memFS) List(_ context.Context, p string) ([]files.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.nodes[p]; !ok || !n.dir {
		return nil, nf(p)
	}
	var out []files.Entry
	for k, n := range m.nodes {
		if d, _ := files.Split(k); d == p && k != "/" {
			out = append(out, m.entry(k, n))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (m *memFS) Read(_ context.Context, p string, off, n int64) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	nd, ok := m.nodes[p]
	if !ok {
		return nil, nf(p)
	}
	if nd.dir {
		return nil, fmt.Errorf("%s is a folder: %w", p, odraz.ErrInvalidPath)
	}
	d := nd.data
	if off > int64(len(d)) {
		off = int64(len(d))
	}
	d = d[off:]
	if n >= 0 && n < int64(len(d)) {
		d = d[:n]
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), d...))), nil
}

func (m *memFS) Write(_ context.Context, p string, r io.Reader, o files.WriteOptions) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if par, ok := m.nodes[parent(p)]; !ok || !par.dir {
		return nf(parent(p))
	}
	if ex, ok := m.nodes[p]; ok && (ex.dir || !o.Overwrite) {
		return fmt.Errorf("%s: %w", p, odraz.ErrExists)
	}
	m.nodes[p] = &node{data: b, mod: time.Now()}
	if o.Progress != nil {
		o.Progress(int64(len(b)), o.Size)
	}
	return nil
}

func parent(p string) string { d, _ := files.Split(p); return d }

func (m *memFS) Mkdir(_ context.Context, p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var chain []string
	for q := p; q != "/"; q = parent(q) {
		chain = append([]string{q}, chain...)
	}
	for _, q := range chain {
		if n, ok := m.nodes[q]; ok {
			if !n.dir {
				return fmt.Errorf("%s: %w", q, odraz.ErrExists)
			}
			continue
		}
		m.nodes[q] = &node{dir: true, mod: time.Now()}
	}
	return nil
}

func (m *memFS) Move(_ context.Context, from, to string, ow bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[from]; !ok {
		return nf(from)
	}
	if par, ok := m.nodes[parent(to)]; !ok || !par.dir {
		return nf(parent(to))
	}
	if _, ok := m.nodes[to]; ok {
		if !ow {
			return fmt.Errorf("%s: %w", to, odraz.ErrExists)
		}
		m.removeTree(to)
	}
	moved := map[string]*node{}
	for k, n := range m.nodes {
		if files.IsWithin(from, k) {
			moved[to+strings.TrimPrefix(k, from)] = n
			delete(m.nodes, k)
		}
	}
	for k, n := range moved {
		m.nodes[k] = n
	}
	return nil
}

func (m *memFS) removeTree(p string) {
	for k := range m.nodes {
		if files.IsWithin(p, k) {
			delete(m.nodes, k)
		}
	}
}

func (m *memFS) Delete(_ context.Context, p string, rec bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[p]
	if !ok {
		return nf(p)
	}
	if n.dir && !rec {
		for k := range m.nodes {
			if k != p && files.IsWithin(p, k) {
				return fmt.Errorf("%s: %w", p, odraz.ErrNotEmpty)
			}
		}
	}
	m.removeTree(p)
	return nil
}

func (m *memFS) Capabilities() files.Capabilities {
	return files.Capabilities{AtomicMove: true, RealDirectories: true, RangeRead: true}
}
