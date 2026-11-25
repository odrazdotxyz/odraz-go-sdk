package s3

// SetMaxCopyObject shrinks the largest object copied in one request, so a test
// can take the multipart copy path, and returns the function that restores it.
func SetMaxCopyObject(n int64) (restore func()) {
	old := maxCopyObject
	maxCopyObject = n
	return func() { maxCopyObject = old }
}

// SetUnknownSizeLimits shrinks the part size and the largest unknown-size
// stream for a test and returns the function that restores them.
func SetUnknownSizeLimits(part, max int64) (restore func()) {
	p, m := unknownPartSize, maxUnknownSize
	unknownPartSize, maxUnknownSize = part, max
	return func() { unknownPartSize, maxUnknownSize = p, m }
}
