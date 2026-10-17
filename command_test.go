package xopen

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestCommandHelperProcess supplies portable command fixtures from the test binary.
func TestCommandHelperProcess(t *testing.T) {
	if os.Getenv("XOPEN_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(2)
	}
	args = args[1:]
	switch args[0] {
	case "echo":
		fmt.Fprint(os.Stdout, strings.Join(args[1:], "|"))
	case "exit":
		fmt.Fprint(os.Stdout, "partial output")
		os.Exit(7)
	case "empty":
	case "fail":
		os.Exit(7)
	case "block":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), 1<<20))
		time.Sleep(time.Hour)
	case "gzip-block":
		gz := gzip.NewWriter(os.Stdout)
		data := make([]byte, 1<<20)
		_, _ = rand.New(rand.NewSource(1)).Read(data)
		for i := 0; i < 8; i++ {
			_, _ = gz.Write(data)
			_ = gz.Flush()
		}
		time.Sleep(time.Hour)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

// helperCommand quotes the executable path and passes fixture mode literally.
func helperCommand(t *testing.T, args string) string {
	t.Helper()
	t.Setenv("XOPEN_HELPER_PROCESS", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Double quoting with escaped backslashes also supports Windows paths.
	exe = strings.ReplaceAll(strings.ReplaceAll(exe, `\`, `\\`), `"`, `\"`)
	return `| "` + exe + `" -test.run=^TestCommandHelperProcess$ -- ` + args
}

// TestSplitCommand checks argument boundaries without invoking a shell.
func TestSplitCommand(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  []string
	}{
		{" echo alpha beta ", []string{"echo", "alpha", "beta"}},
		{"echo\talpha\n beta", []string{"echo", "alpha", "beta"}},
		{`echo C:\tmp\file \n C:\`, []string{"echo", `C:\tmp\file`, `\n`, `C:\`}},
		{`echo 'a b' "c d" a\ b ''`, []string{"echo", "a b", "c d", "a b", ""}},
		{`echo a"b c"d '\path' '$HOME' '*.txt' '|'`, []string{"echo", "ab cd", "\\path", "$HOME", "*.txt", "|"}},
	} {
		got, err := splitCommand(tc.input)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q: %q %v, want %q", tc.input, got, err, tc.want)
		}
	}
	for _, input := range []string{"", "   ", `''`, `echo 'broken`, `echo "broken`} {
		if _, err := splitCommand(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}

// TestReadProcess compares exact command output so dropped arguments cannot pass.
func TestReadProcess(t *testing.T) {
	for _, tc := range []struct{ args, want string }{
		{"echo alpha beta", "alpha|beta"},
		{`echo 'a b' "c d" '' tail`, "a b|c d||tail"},
	} {
		r, err := Ropen(helperCommand(t, tc.args))
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
		if string(got) != tc.want {
			t.Fatalf("got %q, want %q", got, tc.want)
		}
	}
}

// TestProcessExit preserves unsuccessful exit status with or without stdout.
func TestProcessExit(t *testing.T) {
	for _, mode := range []string{"exit", "fail"} {
		r, err := Ropen(helperCommand(t, mode))
		if err == nil {
			_, err = io.ReadAll(r)
			closeErr := r.Close()
			if err == nil {
				err = closeErr
			}
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 7 {
			t.Fatalf("%s: %v", mode, err)
		}
	}
	if r, err := Ropen(helperCommand(t, "empty")); r != nil || err != ErrNoContent {
		t.Fatalf("empty: %v %v", r, err)
	}
	if r, err := Ropen("|xopen_nonexistent_executable_645354"); r != nil || err == nil {
		t.Fatalf("start failure: %v %v", r, err)
	}
	for _, command := range []string{"|", "|  ", "| ''"} {
		if r, err := Ropen(command); r != nil || err == nil {
			t.Fatalf("empty command: %v %v", r, err)
		}
	}
}

// TestProcessEarlyClose stops and reaps a producer, including blocked gzip read-ahead.
func TestProcessEarlyClose(t *testing.T) {
	for _, mode := range []string{"block", "gzip-block"} {
		t.Run(mode, func(t *testing.T) {
			command := strings.TrimPrefix(helperCommand(t, mode), "|")
			source, err := openCommand(command)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { source.Close() })
			r, err := Buf(source)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- r.Close() }()
			select {
			case err := <-done:
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("killed child status: %v", err)
				}
			case <-time.After(5 * time.Second):
				source.Close()
				t.Fatal("early Close did not stop producer")
			}
			if source.cmd.ProcessState == nil {
				t.Fatal("child was not reaped")
			}
		})
	}
}
