package xopen

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestWriterRoundTrip covers every output codec, empty files, and larger writes.
func TestWriterRoundTrip(t *testing.T) {
	for _, ext := range []string{"txt", "gz", "xz", "zst", "bz2", "lz4", "GZ"} {
		for _, size := range []int{0, 7, 300000} {
			t.Run(fmt.Sprintf("%s/%d", ext, size), func(t *testing.T) {
				data := bytes.Repeat([]byte("abcdefg"), size/7+1)[:size]
				p := filepath.Join(t.TempDir(), "sub", "out."+ext)
				w, err := Wopen(p)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := w.Write(data); err != nil || n != len(data) {
					t.Fatalf("write %d: %v", n, err)
				}
				if err = w.Close(); err != nil {
					t.Fatal(err)
				}
				if err = w.Close(); err != nil {
					t.Fatalf("repeat close: %v", err)
				}
				r, err := Ropen(p)
				if size == 0 {
					if err != ErrNoContent {
						t.Fatalf("empty: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(r)
				if err != nil {
					t.Fatal(err)
				}
				if err = r.Close(); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, data) {
					t.Fatalf("round trip: got %d bytes, want %d", len(got), len(data))
				}
			})
		}
	}
}

// TestInvalidWriterSettings preserves existing files and avoids creating new ones.
func TestInvalidWriterSettings(t *testing.T) {
	old := Level
	t.Cleanup(func() { Level = old })
	Level = 1000
	for _, ext := range []string{"gz", "zst", "bz2"} {
		p := filepath.Join(t.TempDir(), "out."+ext)
		original := []byte("original contents")
		if err := os.WriteFile(p, original, 0600); err != nil {
			t.Fatal(err)
		}
		w, err := Wopen(p)
		if w != nil || err == nil {
			t.Fatalf("%s: writer=%v err=%v", ext, w, err)
		}
		got, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(got, original) {
			t.Fatalf("existing file damaged: %q %v", got, err)
		}
	}

}

// TestWriterFailureCleanup verifies that write failures still close each output file.
func TestWriterFailureCleanup(t *testing.T) {
	for _, ext := range []string{"txt", "gz", "xz", "zst", "bz2", "lz4"} {
		t.Run(ext, func(t *testing.T) {
			w, err := Wopen(filepath.Join(t.TempDir(), "out."+ext))
			if err != nil {
				t.Fatal(err)
			}
			want := errors.New("write failed")
			w.Writer.Reset(failedWriter{want})
			w.WriteString("payload")
			err = w.Close()
			if err != want {
				t.Fatalf("Close error=%v, want %v", err, want)
			}
			if _, statErr := w.wtr.Stat(); !errors.Is(statErr, os.ErrClosed) {
				t.Fatalf("file still open: %v", statErr)
			}
			if again := w.Close(); again != err {
				t.Fatalf("repeated Close: %v, want %v", again, err)
			}
		})
	}
}

// TestWriterFlush checks immediately visible data, including parallel LZ4 frames.
func TestWriterFlush(t *testing.T) {
	for _, ext := range []string{"txt", "gz", "zst", "lz4"} {
		t.Run(ext, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "out."+ext)
			w, err := Wopen(p)
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			want := []byte{}
			for _, part := range [][]byte{[]byte("first"), bytes.Repeat([]byte("second"), 20000), []byte("last")} {
				want = append(want, part...)
				if _, err = w.Write(part); err != nil {
					t.Fatal(err)
				}
				if err = w.Flush(); err != nil {
					t.Fatal(err)
				}
				r, err := Ropen(p)
				if err != nil {
					t.Fatal(err)
				}
				got, readErr := io.ReadAll(r)
				r.Close()
				if !bytes.Equal(got, want) {
					t.Fatalf("flushed bytes=%d, want=%d (err=%v)", len(got), len(want), readErr)
				}
				// Gzip/zstd need Close to write their trailer; flushed LZ4 frames are complete.
				if (ext == "txt" || ext == "lz4") && readErr != nil {
					t.Fatal(readErr)
				}
			}
			if err = w.Close(); err != nil {
				t.Fatal(err)
			}
			r, err := Ropen(p)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("closed: %d %v", len(got), err)
			}
		})
	}
}

// TestStdoutAndFileFlags preserves stdout ownership and explicit append behavior.
func TestStdoutAndFileFlags(t *testing.T) {
	prev := os.Stdout
	t.Cleanup(func() { os.Stdout = prev })
	file, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = file
	w, err := Wopen("-")
	if err != nil {
		t.Fatal(err)
	}
	if w.wtr != file {
		t.Fatal("stdout not used")
	}
	if _, err = w.WriteString("output"); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("stdout ownership changed: %v", err)
	}
	p := filepath.Join(t.TempDir(), "append.gz")
	for _, data := range []string{"first", "second"} {
		w, err := WopenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.WriteString(data); err != nil {
			t.Fatal(err)
		}
		if err = w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Ropen(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if string(got) != "firstsecond" || err != nil {
		t.Fatalf("%q %v", got, err)
	}
}

// TestLZ4FlushFailure returns the asynchronous file error and still closes cleanly.
func TestLZ4FlushFailure(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out.lz4")
	w, err := WopenFile(p, os.O_RDONLY|os.O_CREATE, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.WriteString("payload"); err != nil {
		t.Fatal(err)
	}
	flushErr := w.Flush()
	if flushErr == nil {
		t.Fatal("read-only file write succeeded")
	}
	if _, err = w.Write(bytes.Repeat([]byte("x"), bufSize*2)); err == nil {
		t.Fatal("write after failed Flush succeeded")
	}
	if err = w.Close(); err == nil {
		t.Fatal("Close lost the flush error")
	}
	if _, err = w.wtr.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("destination still open: %v", err)
	}
}

// TestLZ4AsyncFlushFailure covers an error after the header was already written.
func TestLZ4AsyncFlushFailure(t *testing.T) {
	w, err := Wopen(filepath.Join(t.TempDir(), "out.lz4"))
	if err != nil {
		t.Fatal(err)
	}
	// Initialize the frame, then make the queued block's eventual file write fail.
	if _, err = w.lz.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err = w.wtr.Close(); err != nil {
		t.Fatal(err)
	}
	flushErr := w.Flush()
	if flushErr == nil {
		t.Fatal("asynchronous block write error hidden")
	}
	if _, err = w.Write(bytes.Repeat([]byte("x"), bufSize*2)); err == nil {
		t.Fatal("write after failed frame succeeded")
	}
	if err = w.Close(); err != flushErr {
		t.Fatalf("Close error=%v, want original Flush error %v", err, flushErr)
	}
}
