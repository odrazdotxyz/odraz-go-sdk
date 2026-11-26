package s3

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// strictReader passes on at most a fixed number of bytes and ends in an error,
// never in io.EOF, if its source holds more. It holds back the last permitted
// byte until it has looked for the one after it, so an upload of exactly that
// many bytes can never complete from a longer source: the client library gives
// the upload up and the object it would have replaced stays.
//
// Every error is final. The library drops one that arrives together with the
// bytes that fill its buffer, and retries a failed request on the same reader.
type strictReader struct {
	mu      sync.Mutex // a request's body can still be read while it is retried
	r       io.Reader
	left    int64 // bytes still permitted
	tooLong error
	err     error // how the stream ended, once it has
	// check, if set, is asked once the source has ended, before the end (and the
	// held-back last byte) is passed on; an error from it fails the stream.
	check func() error
	// failed is err unless that is io.EOF. It is apart from mu so that asking
	// never waits for a Read of a source that hangs.
	failed atomic.Pointer[error]
}

func newStrictReader(r io.Reader, limit int64, tooLong error) *strictReader {
	return &strictReader{r: r, left: limit, tooLong: tooLong}
}

// failure returns why the source cannot be uploaded: it failed, or it holds
// more than permitted. A source that ended is not a failure.
func (s *strictReader) failure() error {
	if p := s.failed.Load(); p != nil {
		return *p
	}
	return nil
}

// finish looks past the bytes read so far, for a source of which nothing is
// permitted and so nothing has been read. See failure.
func (s *strictReader) finish() {
	var b [1]byte
	_, _ = s.Read(b[:])
}

func (s *strictReader) end(err error) error {
	if err == io.EOF && s.check != nil {
		if cerr := s.check(); cerr != nil {
			err = cerr
		}
		s.check = nil
	}
	switch {
	case err == io.EOF:
	case errors.Is(err, io.ErrUnexpectedEOF):
		// The library takes a bare io.ErrUnexpectedEOF (what a truncated
		// request body gives) for the normal end of a stream of unknown length,
		// and would store what it got.
		err = fmt.Errorf("s3: source failed: %w", err)
		s.failed.Store(&err)
	default:
		s.failed.Store(&err)
	}
	s.err = err
	return err
}

// next reads one byte; ok reports whether there was one. Unlike io.ReadFull it
// keeps the source's own error.
func (s *strictReader) next() (b byte, ok bool, err error) {
	var buf [1]byte
	for {
		n, err := s.r.Read(buf[:])
		if n > 0 || err != nil {
			return buf[0], n > 0, err
		}
	}
}

func (s *strictReader) Read(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	if len(b) == 0 {
		return 0, nil
	}
	if s.left > 1 {
		if int64(len(b)) > s.left-1 {
			b = b[:s.left-1]
		}
		n, err := s.r.Read(b)
		s.left -= int64(n)
		if err != nil {
			err = s.end(err)
		}
		return n, err
	}
	// The last permitted byte, or (nothing permitted) the first one too many.
	last, ok, err := s.next()
	if ok && s.left == 0 {
		return 0, s.end(s.tooLong)
	}
	if ok && err == nil {
		var more bool
		if _, more, err = s.next(); more {
			return 0, s.end(s.tooLong)
		}
	}
	if err != io.EOF {
		return 0, s.end(err) // the byte of a failed source is not passed on
	}
	if err := s.end(io.EOF); err != io.EOF {
		return 0, err // the check refused it
	}
	if !ok {
		return 0, io.EOF
	}
	b[0], s.left = last, 0
	return 1, nil
}
