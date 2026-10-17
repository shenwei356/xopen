package xopen

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// commandReader owns a direct child and its stdout pipe. Waiting is shared by
// EOF handling and Close because gzip read-ahead may read the pipe in a goroutine.
type commandReader struct {
	cmd      *exec.Cmd     // Direct child; no shell is implicitly started.
	pipe     io.ReadCloser // Child stdout, closed before killing an unfinished child.
	waitOnce sync.Once     // Reaps the child exactly once.
	waitErr  error         // Child status, published through waitOnce.
	readErr  error         // Sticky terminal error; Wait closes the pipe at EOF.
}

// wait returns the child's exit status and releases its process resources.
func (r *commandReader) wait() error {
	r.waitOnce.Do(func() { r.waitErr = r.cmd.Wait() })
	return r.waitErr
}

// Read reports nonzero exit status instead of a successful EOF.
func (r *commandReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.readErr != nil {
		return 0, r.readErr
	}
	n, err := r.pipe.Read(p)
	if err == io.EOF {
		if waitErr := r.wait(); waitErr != nil {
			err = waitErr
		}
	}
	r.readErr = err
	return n, err
}

// Close stops an unfinished direct child so early closure cannot wait forever
// for a producer blocked on a full stdout pipe. Already completed children are
// unaffected by Kill. A killed child's exit error is returned to the caller.
func (r *commandReader) Close() error {
	_ = r.pipe.Close()
	_ = r.cmd.Process.Kill()
	return r.wait()
}

// openCommand starts a command with whitespace, quoting, and escaping support.
func openCommand(command string) (*commandReader, error) {
	args, err := splitCommand(command)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(args[0], args[1:]...)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		_ = pipe.Close()
		return nil, err
	}
	return &commandReader{cmd: cmd, pipe: pipe}, nil
}

// splitCommand parses literal arguments without shell expansion or operators.
// Single quotes preserve every byte. Double quotes allow backslash escapes;
// unquoted backslashes remain literal unless escaping whitespace or a quote.
func splitCommand(command string) ([]string, error) {
	var args []string
	var arg strings.Builder
	var quote byte   // Active single/double quote, or zero outside quotes.
	started := false // Keeps explicitly quoted empty arguments.
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case c == '\\' && quote != '\'':
			// Preserve existing unquoted paths and literal arguments such as \\n.
			if quote == 0 && (i+1 == len(command) || !strings.ContainsRune(" \t\r\n'\"", rune(command[i+1]))) {
				arg.WriteByte(c)
				started = true
				continue
			}
			i++
			if i == len(command) {
				return nil, fmt.Errorf("xopen: trailing escape in command")
			}
			arg.WriteByte(command[i])
			started = true
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				arg.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
			started = true
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			if started {
				args = append(args, arg.String())
				arg.Reset()
				started = false
			}
		default:
			arg.WriteByte(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("xopen: unterminated quote in command")
	}
	if started {
		args = append(args, arg.String())
	}
	if len(args) == 0 || args[0] == "" {
		return nil, fmt.Errorf("xopen: empty command")
	}
	return args, nil
}
