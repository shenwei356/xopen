package xopen

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os/user"
	"path/filepath"
	"testing"
)

// TestPaths covers empty input, missing files, directories, and both user forms.
func TestPaths(t *testing.T) {
	for _, p := range []string{"", filepath.Join(t.TempDir(), "missing")} {
		if r, err := Ropen(p); r != nil || err == nil {
			t.Fatalf("%q: %v %v", p, r, err)
		}
	}
	if _, err := Ropen(t.TempDir()); !errors.Is(err, ErrDirNotSupported) {
		t.Fatal(err)
	}
	if !Exists("xopen.go") || Exists(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("Exists result")
	}
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"~", "~" + u.Username} {
		for _, suffix := range []string{"", "/", "/data", "/dir/file"} {
			got, err := ExpandUser(prefix + suffix)
			want := u.HomeDir
			if len(suffix) > 1 {
				want += suffix
			}
			if err != nil || got != want {
				t.Fatalf("%q => %q %v, want %q", prefix+suffix, got, err, want)
			}
		}
	}
	if !Exists("~") {
		t.Fatal("home directory not found")
	}
	if _, err := ExpandUser("~xopen_nonexistent_user_645354"); err == nil {
		t.Fatal("unknown user accepted")
	}
	for _, p := range []string{"", "relative/path", "/absolute/path"} {
		got, err := ExpandUser(p)
		if got != p || err != nil {
			t.Fatalf("%q => %q %v", p, got, err)
		}
	}
}

// rejectedTransport makes the rejected HTTP body's ownership observable.
type rejectedTransport struct{ body *failingReader }

// RoundTrip supplies a non-successful response without network access.
func (r rejectedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 404, Status: "404 Not Found", Body: r.body}, nil
}

// TestHTTPRejectedBody checks that unsuccessful responses release their body.
func TestHTTPRejectedBody(t *testing.T) {
	previous := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = previous })
	source := &failingReader{}
	http.DefaultClient = &http.Client{Transport: rejectedTransport{source}}
	if r, err := XReader("http://fixture.invalid/missing"); r != nil || err == nil || !source.closed {
		t.Fatalf("reader=%v err=%v closed=%v", r, err, source.closed)
	}
}

// TestLegacyLevelBehavior retains accepted level-zero and out-of-range fallback settings.
func TestLegacyLevelBehavior(t *testing.T) {
	previous := Level
	t.Cleanup(func() { Level = previous })
	for _, tc := range []struct {
		ext   string
		level int
	}{{"bz2", 0}, {"lz4", 1000}, {"lz4", -2}} {
		Level = tc.level
		p := filepath.Join(t.TempDir(), "out."+tc.ext)
		w, err := Wopen(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.WriteString("payload"); err != nil {
			t.Fatal(err)
		}
		if err = w.Close(); err != nil {
			t.Fatal(err)
		}
		r, err := Ropen(p)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil || !bytes.Equal(got, []byte("payload")) {
			t.Fatalf("%q: %v", got, err)
		}
	}
}
