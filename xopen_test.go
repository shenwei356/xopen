package xopen

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	. "gopkg.in/check.v1"
)

func Test(t *testing.T) { TestingT(t) }

type XopenTest struct{}

var _ = Suite(&XopenTest{})

func gzFromString(s string) string {
	var c bytes.Buffer
	gz := gzip.NewWriter(&c)
	gz.Write([]byte(s))
	gz.Close()
	return c.String()
}

var gzTests = []struct {
	isGz bool
	data string
}{
	{false, "asdf"},
	{true, gzFromString("asdf")},
}

func (s *XopenTest) TestIsGzip(c *C) {
	for _, t := range gzTests {
		isGz, err := IsGzip(bufio.NewReader(strings.NewReader(t.data)))
		c.Assert(err, IsNil)
		c.Assert(t.isGz, Equals, isGz)
	}
}

func (s *XopenTest) TestIsStdin(c *C) {
	previous := os.Stdin
	devnull, err := os.Open(os.DevNull)
	c.Assert(err, IsNil)
	os.Stdin = devnull
	defer func() { os.Stdin = previous; devnull.Close() }()
	r := IsStdin()
	c.Assert(r, Equals, false)
}

func (s *XopenTest) TestRopen(c *C) {
	previous := os.Stdin
	devnull, err := os.Open(os.DevNull)
	c.Assert(err, IsNil)
	os.Stdin = devnull
	defer func() { os.Stdin = previous; devnull.Close() }()
	rdr, err := Ropen("-")
	c.Assert(err, ErrorMatches, "stdin not detected")
	c.Assert(rdr, IsNil)
}

func (s *XopenTest) TestWopen(c *C) {
	for _, f := range []string{"t.gz", "t"} {
		testString := "ASDF1234"
		wtr, err := Wopen(f)
		c.Assert(err, IsNil)
		_, err = os.Stat(f)
		c.Assert(err, IsNil)
		c.Assert(wtr.wtr, NotNil)
		fmt.Fprint(wtr, testString)
		wtr.Close()

		rdr, err := Ropen(f)
		c.Assert(err, IsNil)

		str, err := rdr.ReadString(99)
		c.Assert(str, Equals, testString)
		c.Assert(err, Equals, io.EOF)
		str, err = rdr.ReadString(99)
		c.Assert(str, Equals, "")

		rdr.Close()
		os.Remove(f)
	}
}

// fixtureTransport supplies deterministic HTTP status and content without a network.
type fixtureTransport struct{}

// RoundTrip returns a fixed body or a missing-file response.
func (fixtureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	code, status := 200, "200 OK"
	if req.URL.Path == "/missing" {
		code, status = 404, "404 Not Found"
	}
	return &http.Response{StatusCode: code, Status: status, Body: io.NopCloser(strings.NewReader("fixture content\n")), Header: make(http.Header)}, nil
}

func (s *XopenTest) TestReadHttp(c *C) {
	previous := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: fixtureTransport{}}
	defer func() { http.DefaultClient = previous }()
	for _, url := range []string{"https://fixture.invalid/read", "http://fixture.invalid/read", "http://fixture.invalid/missing"} {
		rdr, err := Ropen(url)
		if strings.HasSuffix(url, "/missing") {
			c.Assert(err, ErrorMatches, ".*404 Not Found")
		} else {
			c.Assert(err, IsNil)
			v, err := rdr.ReadString('\n')
			c.Assert(err, IsNil)
			c.Assert(v, Equals, "fixture content\n")
			c.Assert(rdr.Close(), IsNil)
		}
	}
}

func (s *XopenTest) TestReadProcess(c *C) {
	for _, cmd := range []string{"|ls -lh", "|ls", "|ls -lh xopen_test.go"} {
		rdr, err := Ropen(cmd)
		c.Assert(err, IsNil)
		b := make([]byte, 1000)
		_, err = rdr.Read(b)
		if err != io.EOF {
			c.Assert(err, IsNil)
		}
		lines := strings.Split(string(b), "\n")
		has := false
		for _, line := range lines {
			if strings.Contains(line, "xopen_test.go") {
				has = true
			}
		}
		c.Assert(has, Equals, true)
		rdr.Close()
	}
}

func (s *XopenTest) TestOpenStdout(c *C) {
	w, err := Wopen("-")
	c.Assert(err, IsNil)
	c.Assert(w.wtr, Equals, os.Stdout)
}

func (s *XopenTest) TestOpenBadFile(c *C) {
	r, err := Ropen("XXXXXXXXXXXXXXXXXXXXXXX")
	c.Assert(r, IsNil)
	c.Assert(err, ErrorMatches, ".*no such file.*")
}

func (s *XopenTest) TestExists(c *C) {
	c.Assert(Exists("xopen.go"), Equals, true)
	c.Assert(Exists("____xx"), Equals, false)
}

func (s *XopenTest) TestUser(c *C) {
	c.Assert(Exists("~"), Equals, true)
}

func (s *XopenTest) TestExpand(c *C) {
	_, err := ExpandUser("~baduser66")
	c.Assert(err, Not(IsNil))
}
