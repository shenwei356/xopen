package xopen

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/dsnet/compress/bzip2"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/ulikunitz/xz"
)

// failingReader returns its terminal error only once, as bufio.Reader may do.
type failingReader struct {
	data     []byte // Bytes returned together with the terminal error.
	err      error  // Error cleared after the first read.
	closed   bool   // Whether the caller-owned source was closed.
	closeErr error  // Optional decoder-close failure.
}

// Read returns the scripted data and error once, followed by EOF.
func (r *failingReader) Read(p []byte) (int, error) {
	if r.err == nil {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	err := r.err
	r.err = nil
	return n, err
}

// Close records ownership cleanup and returns the scripted close error.
func (r *failingReader) Close() error {
	r.closed = true
	return r.closeErr
}

// TestBufReadErrors covers errors consumed by magic detection or BOM probing.
func TestBufReadErrors(t *testing.T) {
	want := errors.New("read failed")
	for _, input := range []string{"", "a", "ab", "abc", "abcd", "abcde", "abcdef", "\xef", "\xef\xbb"} {
		t.Run(fmt.Sprintf("%x", input), func(t *testing.T) {
			source := &failingReader{data: []byte(input), err: want}
			r, err := Buf(source)
			if err == nil {
				_, err = io.ReadAll(r)
				defer r.Close()
			}
			if !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
			if source.closed {
				t.Fatal("Buf closed the caller-owned input")
			}
		})
	}
	// A later failure must also survive the buffered reader's initial probe.
	r, err := Buf(io.MultiReader(strings.NewReader("abcdef"), &failingReader{err: want}))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err = io.ReadAll(r); !errors.Is(err, want) {
		t.Fatalf("late error = %v, want %v", err, want)
	}
}

// compressInput creates complete streams with checksums, including empty frames.
func compressInput(t *testing.T, format string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.WriteCloser
	var err error
	switch format {
	case "gzip":
		w = gzip.NewWriter(&buf)
	case "xz":
		w, err = xz.NewWriter(&buf)
	case "zstd":
		w, err = zstd.NewWriter(&buf, zstd.WithEncoderConcurrency(1), zstd.WithZeroFrames(true))
	case "bzip2":
		w, err = bzip2.NewWriter(&buf, &bzip2.WriterConfig{Level: 1})
	case "lz4":
		w = lz4.NewWriter(&buf)
	default:
		t.Fatalf("unknown format %s", format)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// readInput preserves both construction and streaming errors for assertions.
func readInput(data []byte) ([]byte, error) {
	r, err := Buf(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// TestBufCompressedErrors exercises early failures and failures after buffering.
func TestBufCompressedErrors(t *testing.T) {
	for _, size := range []int{1024, 300000} {
		data := make([]byte, size)
		_, _ = rand.New(rand.NewSource(1)).Read(data)
		for _, format := range []string{"gzip", "xz", "zstd", "bzip2", "lz4"} {
			t.Run(fmt.Sprintf("%s/%d", format, size), func(t *testing.T) {
				encoded := compressInput(t, format, data)
				got, err := readInput(encoded)
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("valid stream: bytes=%d, err=%v", len(got), err)
				}
				corrupt := bytes.Clone(encoded)
				// Change stored integrity data, leaving the payload intact.
				offset := 4
				switch format {
				case "gzip":
					offset = 8
				case "xz":
					offset = 12
				case "bzip2":
					offset = 2
				}
				corrupt[len(corrupt)-offset] ^= 0x80
				for name, damaged := range map[string][]byte{
					"early truncation":  encoded[:60],
					"truncated payload": encoded[:len(encoded)/2],
					"truncated trailer": encoded[:len(encoded)-1],
					"bad checksum":      corrupt,
				} {
					t.Run(name, func(t *testing.T) {
						_, err := readInput(damaged)
						if err == nil || errors.Is(err, ErrNoContent) || errors.Is(err, io.EOF) {
							t.Fatalf("damage hidden: %v", err)
						}
					})
				}
			})
		}
	}
}

// TestBufInputCompatibility preserves empty, short, BOM, and short-read inputs.
func TestBufInputCompatibility(t *testing.T) {
	for _, input := range []string{"", "a", "ab", "abc", "abcd", "abcde", "\ufeff", "\ufeff>a\r\nACGT", ">a\nACGT"} {
		t.Run(fmt.Sprintf("%x", input), func(t *testing.T) {
			for _, short := range []bool{false, true} {
				var source io.Reader = strings.NewReader(input)
				if short {
					source = iotest.OneByteReader(source)
				}
				r, err := Buf(source)
				if input == "" {
					if err != ErrNoContent {
						t.Fatalf("empty input: %v", err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(r)
				r.Close()
				if err != nil || string(got) != strings.TrimPrefix(input, "\ufeff") {
					t.Fatalf("short=%v: got %q, err=%v", short, got, err)
				}
			}
		})
	}
	for _, format := range []string{"gzip", "xz", "zstd", "bzip2", "lz4"} {
		t.Run(format+"/empty", func(t *testing.T) {
			if _, err := readInput(compressInput(t, format, nil)); err != ErrNoContent {
				t.Fatalf("empty stream: %v", err)
			}
		})
	}
}

// readerTransport provides an owned HTTP response body without network access.
type readerTransport struct {
	body io.ReadCloser // Owned response body supplied to Ropen.
}

// RoundTrip supplies a successful response with the scripted body.
func (r readerTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: r.body, Header: make(http.Header)}, nil
}

// TestRopenClosesFailedInput verifies that open failures release owned sources.
func TestRopenClosesFailedInput(t *testing.T) {
	previous := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = previous })
	for _, want := range []error{io.ErrUnexpectedEOF, io.EOF} {
		source := &failingReader{err: want}
		http.DefaultClient = &http.Client{Transport: readerTransport{body: source}}
		r, err := Ropen("http://fixture.invalid/input")
		if want == io.EOF {
			want = ErrNoContent
		}
		if r != nil || !errors.Is(err, want) || !source.closed {
			t.Fatalf("r=%v err=%v closed=%v", r, err, source.closed)
		}
	}
}

// TestReaderCloseAfterDecoderError keeps input cleanup independent of decoder errors.
func TestReaderCloseAfterDecoderError(t *testing.T) {
	want := errors.New("decoder close failed")
	decoder := &failingReader{closeErr: want}
	source := &failingReader{}
	r := &Reader{rdr: source, gz: decoder}
	if err := r.Close(); !errors.Is(err, want) || !decoder.closed || !source.closed {
		t.Fatalf("err=%v decoder closed=%v source closed=%v", err, decoder.closed, source.closed)
	}
}

// TestCompressedSourceErrors preserves a source failure after complete frame bytes.
func TestCompressedSourceErrors(t *testing.T) {
	want := errors.New("source failed after compressed data")
	for _, format := range []string{"gzip", "xz", "zstd", "bzip2", "lz4"} {
		t.Run(format, func(t *testing.T) {
			encoded := compressInput(t, format, bytes.Repeat([]byte("payload"), 10000))
			source := io.MultiReader(bytes.NewReader(encoded), &failingReader{err: want})
			r, err := Buf(source)
			if err == nil {
				_, err = io.Copy(io.Discard, r)
				r.Close()
			}
			if !errors.Is(err, want) {
				t.Fatalf("error=%v, want %v", err, want)
			}
		})
	}
}

// TestGzipHeaderTruncation covers optional fields and truncated later members.
func TestGzipHeaderTruncation(t *testing.T) {
	var encoded bytes.Buffer
	w := gzip.NewWriter(&encoded)
	w.Name = "a file name"
	w.Comment = "comment"
	w.Extra = []byte("0123456789")
	if _, err := w.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	prefix := compressInput(t, "gzip", []byte("first"))
	for _, concat := range []bool{false, true} {
		for n := 2; n < len(encoded.Bytes()); n++ {
			data := bytes.Clone(encoded.Bytes()[:n])
			if concat {
				data = append(bytes.Clone(prefix), data...)
			}
			r, err := Buf(bytes.NewReader(data))
			if err == nil {
				_, err = io.Copy(io.Discard, r)
				r.Close()
			}
			if err == nil || errors.Is(err, ErrNoContent) || errors.Is(err, io.EOF) {
				t.Fatalf("concat=%v cut=%d: %v", concat, n, err)
			}
		}
	}
}

// TestGzipErrorsDoNotExhaustPool exercises failures after the initial buffer fill.
func TestGzipErrorsDoNotExhaustPool(t *testing.T) {
	data := make([]byte, 300000)
	_, _ = rand.New(rand.NewSource(1)).Read(data)
	encoded := compressInput(t, "gzip", data)
	encoded = encoded[:len(encoded)/2]
	for i := 0; i < 12; i++ {
		r, err := Buf(bytes.NewReader(encoded))
		if err == nil {
			_, err = io.ReadAll(r)
			r.Close()
		}
		if err == nil || errors.Is(err, ErrNoContent) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
}

// TestGzipEarlyCloseReuse checks that abandoned read-ahead cannot corrupt the next reader.
func TestGzipEarlyCloseReuse(t *testing.T) {
	data := make([]byte, 300000)
	_, _ = rand.New(rand.NewSource(1)).Read(data)
	encoded := compressInput(t, "gzip", data)
	done := make(chan error, 4)
	for worker := 0; worker < 4; worker++ {
		go func() {
			for i := 0; i < 32; i++ {
				r, err := Buf(bytes.NewReader(encoded))
				if err != nil {
					done <- err
					return
				}
				if err = r.Close(); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
	}
	for worker := 0; worker < 4; worker++ {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}
