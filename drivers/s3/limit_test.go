package s3

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"testing/iotest"
)

// script returns one step per Read: some bytes, an error, or both.
type script []struct {
	data string
	err  error
}

func (s *script) Read(b []byte) (int, error) {
	if len(*s) == 0 {
		return 0, io.EOF
	}
	step := &(*s)[0]
	n := copy(b, step.data)
	if step.data = step.data[n:]; step.data != "" {
		return n, nil
	}
	err := step.err
	*s = (*s)[1:]
	return n, err
}

// read drains r through a buffer of size n.
func read(r io.Reader, n int) (got []byte, err error) {
	b := make([]byte, n)
	for {
		k, err := r.Read(b)
		got = append(got, b[:k]...)
		if err != nil {
			return got, err
		}
	}
}

func TestStrictReader(t *testing.T) {
	tooLong := errors.New("too long")
	data := []byte("0123456789")
	wraps := map[string]func(io.Reader) io.Reader{
		"plain":    func(r io.Reader) io.Reader { return r },
		"one byte": iotest.OneByteReader,
		"data+EOF": iotest.DataErrReader,
	}
	for name, wrap := range wraps {
		for limit := int64(0); limit <= 12; limit++ {
			for _, buf := range []int{1, 3, 10, 64} {
				s := newStrictReader(wrap(bytes.NewReader(data)), limit, tooLong)
				got, err := read(s, buf)
				if limit < int64(len(data)) {
					// The last permitted byte is never passed on.
					if err != tooLong || s.failure() != tooLong || int64(len(got)) > max(limit-1, 0) {
						t.Fatalf("%s limit %d buf %d: got %q, %v", name, limit, buf, got, err)
					}
				} else if err != io.EOF || s.failure() != nil || !bytes.Equal(got, data) {
					t.Fatalf("%s limit %d buf %d: got %q, %v", name, limit, buf, got, err)
				}
				if n, again := s.Read(make([]byte, 4)); n != 0 || again != err {
					t.Fatalf("%s limit %d: error not final: %d, %v", name, limit, n, again)
				}
			}
		}
	}
}

// A source's error is final, also if the source would go on afterwards, if it
// came with data, and if it hit while looking past the last permitted byte.
func TestStrictReaderSourceErrors(t *testing.T) {
	boom := errors.New("boom")
	for _, srcErr := range []error{boom, io.ErrUnexpectedEOF} {
		for _, limit := range []int64{4, 5, 100} {
			for name, src := range map[string]*script{
				"alone":     {{data: "abcd"}, {err: srcErr}, {data: "efgh"}},
				"with data": {{data: "abcd", err: srcErr}, {data: "efgh"}},
			} {
				s := newStrictReader(src, limit, errors.New("too long"))
				got, err := read(s, 64)
				if limit == 4 && name == "alone" && int64(len(got)) >= limit {
					t.Fatalf("limit %d %s: last byte passed on before a failure", limit, name)
				}
				if !errors.Is(err, srcErr) || err == io.ErrUnexpectedEOF || !errors.Is(s.failure(), srcErr) {
					t.Fatalf("%v limit %d %s: got %q, %v, failure %v", srcErr, limit, name, got, err, s.failure())
				}
				if n, again := s.Read(make([]byte, 4)); n != 0 || again != err {
					t.Fatalf("error not final: %d, %v", n, again)
				}
			}
		}
	}
}
