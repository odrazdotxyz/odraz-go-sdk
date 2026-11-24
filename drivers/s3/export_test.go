package s3

// SetUnknownSizeLimits shrinks the part size and the largest unknown-size
// stream for a test and returns the function that restores them.
func SetUnknownSizeLimits(part, max int64) (restore func()) {
	p, m := unknownPartSize, maxUnknownSize
	unknownPartSize, maxUnknownSize = part, max
	return func() { unknownPartSize, maxUnknownSize = p, m }
}
