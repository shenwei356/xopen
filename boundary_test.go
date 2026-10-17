package xopen

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/pierrec/lz4/v4"
)

// TestCompressedTruncation checks every byte boundary once a full magic is present.
func TestCompressedTruncation(t *testing.T) {
	magicLen := map[string]int{"gzip": 2, "xz": 6, "zstd": 4, "bzip2": 3, "lz4": 4}
	for _, format := range []string{"gzip", "xz", "zstd", "bzip2", "lz4"} {
		for _, payload := range []string{"", "some payload\n"} {
			t.Run(fmt.Sprintf("%s/%d", format, len(payload)), func(t *testing.T) {
				encoded := compressInput(t, format, []byte(payload))
				for n := magicLen[format]; n < len(encoded); n++ {
					_, err := readInput(encoded[:n])
					if err == nil || errors.Is(err, ErrNoContent) || errors.Is(err, io.EOF) {
						t.Fatalf("truncated at %d/%d: %v", n, len(encoded), err)
					}
				}
			})
		}
	}
}

// TestCompressedConcatenation preserves empty frames, concatenated frames, and BOMs.
func TestCompressedConcatenation(t *testing.T) {
	for _, format := range []string{"gzip", "xz", "zstd", "bzip2", "lz4"} {
		t.Run(format, func(t *testing.T) {
			var encoded []byte
			for _, payload := range []string{"", "\ufefffirst", "", "second", ""} {
				encoded = append(encoded, compressInput(t, format, []byte(payload))...)
			}
			for _, short := range []bool{false, true} {
				var source io.Reader = bytes.NewReader(encoded)
				if short {
					source = iotest.OneByteReader(source)
				}
				r, err := Buf(source)
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(r)
				r.Close()
				if err != nil || string(got) != "firstsecond" {
					t.Fatalf("short=%v: %q %v", short, got, err)
				}
			}
		})
	}
}

// TestXZPadding checks that strict block-header EOF does not reject stream padding.
func TestXZPadding(t *testing.T) {
	stream := compressInput(t, "xz", []byte("payload"))
	for _, n := range []int{0, 4, 8, 12} {
		padded := append(bytes.Clone(stream), make([]byte, n)...)
		got, err := readInput(padded)
		if err != nil || string(got) != "payload" {
			t.Fatalf("padding=%d: %q %v", n, got, err)
		}
	}
	for _, n := range []int{1, 2, 3, 5} {
		_, err := readInput(append(bytes.Clone(stream), make([]byte, n)...))
		if err == nil || errors.Is(err, ErrNoContent) {
			t.Fatalf("bad padding=%d: %v", n, err)
		}
	}
}

// TestLZ4Framing covers optional checksums, content sizes, and skippable frames.
func TestLZ4Framing(t *testing.T) {
	for _, checksum := range []bool{false, true} {
		for _, blockChecksum := range []bool{false, true} {
			var buf bytes.Buffer
			w := lz4.NewWriter(&buf)
			if err := w.Apply(lz4.ChecksumOption(checksum), lz4.BlockChecksumOption(blockChecksum), lz4.SizeOption(7)); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte("payload")); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			full := bytes.Clone(buf.Bytes())
			got, err := readInput(full)
			if err != nil || string(got) != "payload" {
				t.Fatalf("%q %v", got, err)
			}
			for n := 4; n < len(full); n++ {
				_, err := readInput(full[:n])
				if err == nil || errors.Is(err, ErrNoContent) {
					t.Fatalf("checksum=%v block=%v cut=%d: %v", checksum, blockChecksum, n, err)
				}
			}
			// Skippable frames are accepted between normal frames, including empty skips.
			for _, size := range []int{0, 7} {
				skip := make([]byte, 8+size)
				binary.LittleEndian.PutUint32(skip, 0x184d2a50)
				binary.LittleEndian.PutUint32(skip[4:], uint32(size))
				joined := append(bytes.Clone(full), skip...)
				joined = append(joined, full...)
				got, err = readInput(joined)
				if err != nil || string(got) != "payloadpayload" {
					t.Fatalf("skip=%d: %q %v", size, got, err)
				}
			}
		}
	}
	// Legacy streams use EOF termination and must retain their existing behavior.
	var buf bytes.Buffer
	w := lz4.NewWriter(&buf)
	if err := w.Apply(lz4.LegacyOption(true)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("legacy")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := readInput(buf.Bytes())
	if err != nil || string(got) != "legacy" {
		t.Fatalf("legacy: %q %v", got, err)
	}
}

// TestReaderRepeatedClose checks cleanup ownership and pooled decoder isolation.
func TestReaderRepeatedClose(t *testing.T) {
	original := poolGzipReader
	poolGzipReader = &sync.Pool{New: original.New}
	t.Cleanup(func() { poolGzipReader = original })
	procs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(procs)
	data := compressInput(t, "gzip", []byte("payload"))
	r, err := Buf(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadAll(r); err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := Buf(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Buf(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if a.gzReader == b.gzReader {
		t.Fatal("two readers share a pooled decoder")
	}
	for _, r := range []*Reader{a, b} {
		got, err := io.ReadAll(r)
		if err != nil || string(got) != "payload" {
			t.Fatalf("%q %v", got, err)
		}
	}
}

// TestCopyAfterBufferedRead covers decoder WriterTo paths after Buf's BOM probe
// and a caller's partial Read. Copy must preserve pending decoded block bytes.
func TestCopyAfterBufferedRead(t *testing.T) {
	for _, format := range []string{"gzip", "xz", "zstd", "bzip2", "lz4"} {
		for _, size := range []int{7, 65536, 300000} {
			t.Run(fmt.Sprintf("%s/%d", format, size), func(t *testing.T) {
				data := bytes.Repeat([]byte("abcdefgh"), size/8+1)[:size]
				encoded := compressInput(t, format, data)
				r, err := Buf(bytes.NewReader(encoded))
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				prefix := make([]byte, 3)
				if _, err = io.ReadFull(r, prefix); err != nil {
					t.Fatal(err)
				}
				var out bytes.Buffer
				out.Write(prefix)
				n, err := io.Copy(&out, r)
				if err != nil || n != int64(size-3) || !bytes.Equal(out.Bytes(), data) {
					t.Fatalf("copy: n=%d size=%d err=%v", n, out.Len(), err)
				}
				// Repeated reads/copies at EOF must remain successful and never hang.
				for i := 0; i < 3; i++ {
					if n, err := io.Copy(io.Discard, r); n != 0 || err != nil {
						t.Fatalf("copy at EOF: %d %v", n, err)
					}
				}
			})
		}
	}
}

// TestLegacyLZ4Truncation allows EOF between blocks but rejects missing payloads.
func TestLegacyLZ4Truncation(t *testing.T) {
	data := bytes.Repeat([]byte("legacy data"), 10000)
	var buf bytes.Buffer
	w := lz4.NewWriter(&buf)
	if err := w.Apply(lz4.LegacyOption(true)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	encoded := buf.Bytes()
	for n := 5; n < len(encoded); n++ {
		_, err := readInput(encoded[:n])
		if err == nil || errors.Is(err, ErrNoContent) || errors.Is(err, io.EOF) {
			t.Fatalf("cut=%d/%d: %v", n, len(encoded), err)
		}
	}
	for _, kernelTrailer := range []bool{false, true} {
		input := bytes.Clone(encoded)
		if kernelTrailer {
			trailer := make([]byte, 4)
			binary.LittleEndian.PutUint32(trailer, uint32(len(data)))
			input = append(input, trailer...)
		}
		got, err := readInput(input)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("kernel trailer=%v: bytes=%d %v", kernelTrailer, len(got), err)
		}
	}
	joined := append(compressInput(t, "lz4", []byte("prefix")), encoded...)
	got, err := readInput(joined)
	if err != nil || !bytes.Equal(got, append([]byte("prefix"), data...)) {
		t.Fatalf("modern/legacy: bytes=%d %v", len(got), err)
	}
}
