// Package xopen makes it easy to get buffered readers and writers.
// Ropen opens a (possibly gzipped) file/process/http site for buffered reading.
// Wopen opens a (possibly gzipped) file for buffered writing.
// Both will use gzip when appropriate and will user buffered IO.
package xopen

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"

	"github.com/dsnet/compress/bzip2"
	"github.com/klauspost/compress/zstd"
	gzip "github.com/klauspost/pgzip"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
)

// Level is the default compression level of gzip.
// The default (-1) selects the format default for zstd, bzip2 and LZ4.
// Set Level before concurrent use; changing it during writes is unsupported.
var Level = gzip.DefaultCompression

// ErrNoContent means nothing in the stream/file.
var ErrNoContent = errors.New("xopen: no content")

// ErrDirNotSupported means the path is a directory.
var ErrDirNotSupported = errors.New("xopen: input is a directory")

// IsGzip returns true buffered Reader has the gzip magic.
func IsGzip(b *bufio.Reader) (bool, error) {
	return CheckBytes(b, []byte{0x1f, 0x8b})
}

// IsXz returns true buffered Reader has the xz magic.
func IsXz(b *bufio.Reader) (bool, error) {
	return CheckBytes(b, []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00})
}

// IsZst returns true buffered Reader has the zstd magic.
func IsZst(b *bufio.Reader) (bool, error) {
	return CheckBytes(b, []byte{0x28, 0xB5, 0x2f, 0xfd})
}

// IsBzip2 returns true buffered Reader has the bzip2 magic.
func IsBzip2(b *bufio.Reader) (bool, error) {
	return CheckBytes(b, []byte{0x42, 0x5a, 0x68})
}

// IsLZ4 reports modern or legacy LZ4 magic.
func IsLZ4(b *bufio.Reader) (bool, error) {
	ok, err := CheckBytes(b, []byte{0x04, 0x22, 0x4D, 0x18}) // new
	if err != nil || ok {
		return ok, err
	}
	return CheckBytes(b, []byte{0x02, 0x21, 0x4C, 0x18}) // legacy
}

// IsStdin checks if we are getting data from stdin.
func IsStdin() bool {
	// http://stackoverflow.com/a/26567513
	stat, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) == 0
}

// ExpandUser expands ~/path and ~otheruser/path appropriately
func ExpandUser(path string) (string, error) {
	if len(path) == 0 || path[0] != '~' {
		return path, nil
	}
	name, suffix, _ := strings.Cut(path[1:], "/")
	var u *user.User
	var err error
	if name == "" {
		u, err = user.Current()
	} else {
		u, err = user.Lookup(name)
	}
	if err != nil {
		return "", err
	}
	if suffix == "" {
		return u.HomeDir, nil
	}
	return u.HomeDir + "/" + suffix, nil
}

// Exists checks if a local file exits
func Exists(path string) bool {
	path, perr := ExpandUser(path)
	if perr != nil {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// CheckBytes peeks at a buffered stream and checks if the first read bytes match.
func CheckBytes(b *bufio.Reader, buf []byte) (bool, error) {

	m, err := b.Peek(len(buf))
	if err != nil {
		// return false, ErrNoContent
		return false, err // EOF
	}
	for i := range buf {
		if m[i] != buf[i] {
			return false, nil
		}
	}
	return true, nil
}

// Reader is returned by Ropen
type Reader struct {
	*bufio.Reader
	rdr io.Reader
	gz  io.ReadCloser

	gzReader *gzip.Reader
	isGzip   bool
	closed   bool  // Prevents repeated cleanup and duplicate pool entries.
	closeErr error // Result retained for repeated Close calls.
}

// Close releases the input and decoder, preserving both cleanup errors.
// Repeated calls return the original result without releasing resources again.
func (r *Reader) Close() error {
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	// Close the source first so a command's blocked gzip read-ahead can stop.
	var sourceErr error
	if c, ok := r.rdr.(io.Closer); ok {
		sourceErr = c.Close()
	}
	if r.gz != nil {
		r.closeErr = r.gz.Close()
	}
	if r.closeErr == nil {
		r.closeErr = sourceErr
	} else if sourceErr != nil {
		r.closeErr = errors.Join(r.closeErr, sourceErr)
	}
	if r.isGzip {
		// Recycle only after a validated EOF, keeping failed or abandoned
		// stream state out of the pool.
		if gz, ok := r.gz.(*gzipReader); ok && !gz.failed && gz.err == io.EOF && r.closeErr == nil {
			poolGzipReader.Put(r.gzReader)
		}
	}
	r.rdr, r.gz, r.gzReader, r.isGzip = nil, nil, nil, false
	return r.closeErr
}

// Writer is returned by Wopen
type Writer struct {
	*bufio.Writer
	wtr      *os.File
	gz       *gzip.Writer
	xw       *xz.Writer
	zw       *zstd.Encoder
	bz2      *bzip2.Writer
	lz       *lz4.Writer
	closed   bool  // Prevents repeated compressor and file cleanup.
	closeErr error // First Close result.
	flushErr error // Compressor failure retained for Close.
}

// Close finishes compression and closes the file even when flushing fails.
// Repeated calls return the original result.
func (w *Writer) Close() error {
	if w.closed {
		return w.closeErr
	}
	w.closed = true
	w.closeErr = w.flushErr
	// Finalize compressors directly; an extra compressor Flush is unnecessary.
	if err := w.Writer.Flush(); w.closeErr == nil {
		w.closeErr = err
	}
	close := func(c io.Closer) {
		if err := c.Close(); w.closeErr == nil {
			w.closeErr = err
		}
	}
	if w.gz != nil {
		close(w.gz)
	}
	if w.xw != nil {
		close(w.xw)
	}
	if w.zw != nil {
		close(w.zw)
	}
	if w.bz2 != nil {
		close(w.bz2)
	}
	if w.lz != nil {
		close(w.lz)
	}
	close(w.wtr)
	return w.closeErr
}

// failedWriter preserves an asynchronous LZ4 error after its frame is closed.
type failedWriter struct{ err error }

// Write prevents subsequent writes from touching a failed frame.
func (w failedWriter) Write([]byte) (int, error) { return 0, w.err }

// Flush writes buffered data without syncing the file. LZ4 finishes a frame;
// xz and bzip2 require Close to finish their compressor buffers.
func (w *Writer) Flush() (err error) {
	if w.closed {
		return os.ErrClosed
	}
	if w.flushErr != nil {
		return w.flushErr
	}
	defer func() {
		if err != nil {
			w.flushErr = err
		}
	}()
	err = w.Writer.Flush()
	if err != nil {
		return err
	}

	if w.gz != nil {
		err = w.gz.Flush()
		if err != nil {
			return err
		}
	}
	if w.zw != nil {
		err = w.zw.Flush()
		if err != nil {
			return err
		}
	}
	if w.lz != nil {
		// Parallel LZ4 Flush only queues blocks. Close waits and reports file errors.
		if err = w.lz.Close(); err != nil {
			w.lz = nil // Closing this failed parallel frame again is unsafe.
			w.Writer.Reset(failedWriter{err})
			return err
		}
		w.lz.Reset(w.wtr) // Preserve the original compression options for the next frame.
	}
	return nil
}

var bufSize = 65536

// poolGzipReader starts without a dummy stream or background checksum work.
var poolGzipReader = &sync.Pool{New: func() interface{} { return new(gzip.Reader) }}

// readerOnly hides LZ4's WriteTo, which can return EOF after a completed read
// or copy. io.Copy requires successful completion to return nil instead.
type readerOnly struct{ io.Reader }

// xzSource distinguishes incomplete blocks from a clean stream end. The xz
// decoder probes subsequent stream headers/padding in four-byte units. Its
// block-header path requests one byte followed by 4*n-1 bytes, but otherwise
// treats a bare EOF in either request as a successfully completed stream.
type xzSource struct{ io.Reader }

// Read preserves the stream-boundary EOF probe but rejects missing block fields.
func (r xzSource) Read(p []byte) (int, error) {
	// Complete fixed-size decoder requests so a short block-header read cannot
	// leave a four-byte remainder that looks like a new stream-header probe.
	n, err := io.ReadFull(r.Reader, p)
	if n == 0 && len(p) != 4 && err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// Buf returns a buffered reader, detecting supported compression and removing a BOM.
// Only a clean end of an empty stream returns ErrNoContent; read errors are preserved.
// On failure, the caller retains ownership of r.
func Buf(r io.Reader) (_ *Reader, err error) {
	b := bufio.NewReaderSize(r, bufSize)
	var decoded io.Reader = b
	var decoder io.ReadCloser
	var gzReader *gzip.Reader
	pooled := false
	defer func() {
		if err != nil {
			var closeErr error
			if decoder != nil {
				closeErr = decoder.Close()
			}
			// A valid empty stream is reusable; discard failed decoder state.
			if gz, ok := decoder.(*gzipReader); ok && pooled && !gz.failed && err == ErrNoContent && closeErr == nil {
				poolGzipReader.Put(gzReader)
			}
		}
	}()

	// Probe once: successive Peek calls can consume a saved read error or stop
	// at a long magic before checking a shorter one (e.g. xz before bzip2).
	magic, probeErr := b.Peek(6)
	if probeErr != nil && probeErr != io.EOF {
		return nil, probeErr
	}
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		source := gzipSource{b}
		pooled = true
		gzReader = poolGzipReader.Get().(*gzip.Reader)
		decoder = gzReader
		err = gzReader.Reset(source)
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		if err == nil {
			gzReader.Multistream(false)
			decoder = &gzipReader{reader: gzReader, source: source}
			decoded = decoder
		}
	case bytes.HasPrefix(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		var z *zstd.Decoder
		z, err = zstd.NewReader(b)
		if err == nil {
			decoder = z.IOReadCloser()
			decoded = decoder
		}
	case bytes.HasPrefix(magic, []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}):
		decoded, err = xz.NewReader(xzSource{b})
	case bytes.HasPrefix(magic, []byte{0x42, 0x5a, 0x68}):
		decoder, err = bzip2.NewReader(b, &bzip2.ReaderConfig{})
		decoded = decoder
	case bytes.HasPrefix(magic, []byte{0x04, 0x22, 0x4d, 0x18}),
		bytes.HasPrefix(magic, []byte{0x02, 0x21, 0x4c, 0x18}):
		decoded = readerOnly{lz4.NewReader(b)}
	}
	if err != nil {
		return nil, err
	}
	b = bufio.NewReaderSize(decoded, bufSize)
	// Probe decompressed content and strip a UTF-8 BOM, preserving read errors.
	t, _, err := b.ReadRune()
	if err != nil {
		if err == io.EOF {
			return nil, ErrNoContent
		}
		return nil, err
	}
	if t != '\uFEFF' {
		_ = b.UnreadRune()
	}
	return &Reader{Reader: b, rdr: r, gz: decoder, gzReader: gzReader, isGzip: pooled}, nil
}

// XReader returns a reader from a url string or a file.
func XReader(f string) (io.Reader, error) {
	if strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
		var rsp *http.Response
		rsp, err := http.Get(f)
		if err != nil {
			return nil, err
		}
		if rsp.StatusCode != 200 {
			_ = rsp.Body.Close()
			return nil, fmt.Errorf("http error downloading %s. status: %s", f, rsp.Status)
		}
		rdr := rsp.Body
		return rdr, nil
	}
	f, err := ExpandUser(f)
	if err != nil {
		return nil, err
	}

	fi, err := os.Stat(f)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return nil, ErrDirNotSupported
	}

	return os.Open(f)
}

// Ropen opens a buffered reader.
func Ropen(f string) (*Reader, error) {
	if f == "" {
		return nil, errors.New("xopen: empty input path")
	}
	var err error
	var rdr io.Reader
	if f == "-" {
		if !IsStdin() {
			return nil, errors.New("stdin not detected")
		}
		b, err := Buf(os.Stdin)
		return b, err
	} else if f[0] == '|' {
		rdr, err = openCommand(f[1:])
	} else {
		rdr, err = XReader(f)
	}
	if err != nil {
		return nil, err
	}
	b, err := Buf(rdr)
	if err != nil {
		// Buf leaves its input open on failure; Ropen owns the input it opened.
		if closer, ok := rdr.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	return b, err
}

// Wopen opens a buffered writer.
// If f == "-", then stdout will be used.
// If f endswith ".gz", then the output will be gzipped.
// If f endswith ".xz", then the output will be xz-compressed.
// If f endswith ".zst", then the output will be zstd-compressed.
// If f endswith ".bz2", then the output will be bzip2-compressed.
// If f endswith ".lz4", then the output will be LZ4-compressed.
func Wopen(f string) (*Writer, error) {
	return WopenFile(f, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
}

// WopenFile opens a buffered writer.
// If f == "-", then stdout will be used.
// If f endswith ".gz", then the output will be gzipped.
// If f endswith ".xz", then the output will be xz-compressed.
// If f endswith ".lz4", then the output will be LZ4-compressed.
func WopenFile(f string, flag int, perm os.FileMode) (_ *Writer, err error) {
	// Reject invalid levels before opening (and possibly truncating) the file.
	// Preserve the existing LZ4 fallback and bzip2 level-zero behavior.
	format := strings.ToLower(filepath.Ext(f))
	level := Level
	switch format {
	case ".gz":
		if level < gzip.HuffmanOnly || level > gzip.BestCompression {
			return nil, fmt.Errorf("xopen: invalid gzip level: %d", level)
		}
	case ".zst":
		if level != gzip.DefaultCompression && (level < int(zstd.SpeedFastest) || level > int(zstd.SpeedBestCompression)) {
			return nil, fmt.Errorf("xopen: invalid zstd level: %d", level)
		}
	case ".bz2":
		if level != gzip.DefaultCompression && (level < 0 || level > 9) {
			return nil, fmt.Errorf("xopen: invalid bzip2 level: %d", level)
		}
	}
	var wtr *os.File
	defer func() {
		// Constructor failures must not leak the file or return an unusable writer.
		if err != nil && wtr != nil && f != "-" {
			_ = wtr.Close()
		}
	}()
	if f == "-" {
		wtr = os.Stdout
	} else {
		dir := filepath.Dir(f)
		fi, err := os.Stat(dir)
		if err == nil && !fi.IsDir() {
			return nil, fmt.Errorf("can not write file into a non-directory path: %s", dir)
		}
		if os.IsNotExist(err) {
			if err = os.MkdirAll(dir, 0755); err != nil {
				return nil, err
			}
		}
		wtr, err = os.OpenFile(f, flag, perm)
		if err != nil {
			return nil, err
		}
	}

	f2 := strings.ToLower(f)
	if strings.HasSuffix(f2, ".gz") {
		gz, err := gzip.NewWriterLevel(wtr, Level)
		if err != nil {
			return nil, fmt.Errorf("xopen: gzip: %w", err)
		}
		return &Writer{Writer: bufio.NewWriterSize(gz, bufSize), wtr: wtr, gz: gz}, err
	}
	if strings.HasSuffix(f2, ".xz") {
		xw, err := xz.NewWriter(wtr)
		if err != nil {
			return nil, err
		}
		return &Writer{Writer: bufio.NewWriterSize(xw, bufSize), wtr: wtr, xw: xw}, err
	}
	if strings.HasSuffix(f2, ".zst") {
		level := Level
		if level == gzip.DefaultCompression {
			level = 2
		}
		zw, err := zstd.NewWriter(wtr, zstd.WithEncoderLevel(zstd.EncoderLevel(level)))
		if err != nil {
			return nil, fmt.Errorf("xopen: zstd: %w", err)
		}
		return &Writer{Writer: bufio.NewWriterSize(zw, bufSize), wtr: wtr, zw: zw}, err
	}
	if strings.HasSuffix(f2, ".bz2") {
		level := Level
		if level == gzip.DefaultCompression {
			level = 6
		}
		bz2, err := bzip2.NewWriter(wtr, &bzip2.WriterConfig{Level: level})
		if err != nil {
			return nil, fmt.Errorf("xopen: bz2: %w", err)
		}
		return &Writer{Writer: bufio.NewWriterSize(bz2, bufSize), wtr: wtr, bz2: bz2}, err
	}
	if strings.HasSuffix(f2, ".lz4") {
		level := Level
		if level == gzip.DefaultCompression {
			level = 5
		}

		var lvl lz4.CompressionLevel
		switch level {
		case 0:
			lvl = lz4.Fast
		case 1:
			lvl = lz4.Level1
		case 2:
			lvl = lz4.Level2
		case 3:
			lvl = lz4.Level3
		case 4:
			lvl = lz4.Level4
		case 5:
			lvl = lz4.Level5
		case 6:
			lvl = lz4.Level6
		case 7:
			lvl = lz4.Level7
		case 8:
			lvl = lz4.Level8
		case 9:
			lvl = lz4.Level9
		default:
			lvl = lz4.Level9
		}

		lz := lz4.NewWriter(wtr)
		options := []lz4.Option{
			lz4.BlockChecksumOption(false),
			lz4.BlockSizeOption(lz4.BlockSize(4 << 20)),
			lz4.ChecksumOption(true),
			lz4.CompressionLevelOption(lvl),
			lz4.ConcurrencyOption(8),
		}
		var err error
		if err = lz.Apply(options...); err != nil {
			return nil, fmt.Errorf("xopen: lz4: %w", err)
		}
		return &Writer{Writer: bufio.NewWriterSize(lz, bufSize), wtr: wtr, lz: lz}, err
	}
	return &Writer{Writer: bufio.NewWriterSize(wtr, bufSize), wtr: wtr}, nil
}
