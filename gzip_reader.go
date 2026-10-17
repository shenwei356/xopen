package xopen

import (
	"bufio"
	"io"

	gzip "github.com/klauspost/pgzip"
)

// gzipSource makes a missing eight-byte trailer an error. pgzip otherwise
// returns the bare EOF from io.ReadFull, indistinguishable from successful EOF.
// Other header EOFs are checked explicitly when opening each member.
type gzipSource struct{ *bufio.Reader }

// Read preserves ordinary EOF except while pgzip requests a required trailer.
func (r gzipSource) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if len(p) == 8 && err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// gzipReader manages members explicitly so a truncated subsequent header is
// never confused with EOF. Failed or abandoned decoders are not reused.
type gzipReader struct {
	reader *gzip.Reader // Decoder for the current member.
	source gzipSource   // Buffered compressed source, positioned between members.
	failed bool         // Prevents reuse after any decoding error.
	err    error        // Sticky terminal status for subsequent reads/copies.
}

// nextMember distinguishes a clean end from a present but incomplete header.
func (r *gzipReader) nextMember() error {
	if _, err := r.source.Peek(1); err != nil {
		return err
	}
	if err := r.reader.Reset(r.source); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return err
	}
	r.reader.Multistream(false)
	return nil
}

// Read advances across complete members while preserving all decoding errors.
func (r *gzipReader) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	defer func() {
		r.err = err
		if err != nil && err != io.EOF {
			r.failed = true
		}
	}()
	for {
		n, err = r.reader.Read(p)
		if err != io.EOF {
			return n, err
		}
		err = r.nextMember()
		if err != nil || n > 0 {
			return n, err
		}
	}
}

// WriteTo retains pgzip's direct block-to-writer path for io.Copy callers.
func (r *gzipReader) WriteTo(w io.Writer) (total int64, err error) {
	if r.err == io.EOF {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	defer func() {
		if err != nil {
			r.failed = true
			r.err = err
		} else {
			r.err = io.EOF
		}
	}()
	for {
		var n int64
		n, err = r.reader.WriteTo(w)
		total += n
		if err != nil {
			return total, err
		}
		err = r.nextMember()
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// Close stops decoder read-ahead without closing the source.
func (r *gzipReader) Close() error { return r.reader.Close() }
