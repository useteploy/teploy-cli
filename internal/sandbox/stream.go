package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Stream is which of a command's outputs a chunk came from.
type Stream string

// The two output streams of an exec.
const (
	Stdout Stream = "stdout"
	Stderr Stream = "stderr"
)

// maxSSELineBytes bounds one `data:` line of an exec's stream: one chunk of
// a command's output, and no real chunk is this long.
const maxSSELineBytes = 16 << 20

var errLineTooLong = fmt.Errorf("a line of the engine's stream is longer than %d bytes", maxSSELineBytes)

// Errors of an exec's stream. Either leaves the command's fate unknown: the
// stream is the only word on how it ended, and it did not end with an exit.
var (
	ErrStreamEnded = errors.New("the engine's stream ended before the command's exit arrived")
	ErrBadExit     = errors.New("the engine's exit frame could not be read")
)

// execHeaderTimeout is how long the engine may take to start answering an
// exec: the command's own timeout, and a little more for its exit to be
// sent. An exec's 200 goes out with its first output, so a command that
// prints nothing for its whole timeout is answered only then.
func execHeaderTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = DefaultExecTimeoutSec
	}
	return time.Duration(seconds)*time.Second + execHeaderGrace
}

// Exec runs a command in the run and streams its output: each stdout and
// stderr chunk is passed to write in the order the engine sent it, so the
// two keep their interleaving. It returns once the command has ended.
//
// A refusal before the stream starts is an *APIError, and nothing ran. If
// the stream then dies before the exit frame, the error wraps
// ErrStreamEnded or ErrBadExit: closing the stream ends the command on the
// engine's side, but whether it had finished by then is not known.
func (c *Client) Exec(ctx context.Context, id string, req ExecRequest, write func(Stream, string)) (ExecResult, error) {
	return c.ExecStream(ctx, id, req, write, nil)
}

// ExecStream is Exec, and says when the command became the engine's.
// started is called once a 200 has arrived and the stream is about to be
// read; it is not called for a refusal before the stream (the engine's
// problem answer, a request that did not leave, or no answer at all). A
// caller that reports on the command can tell "nothing ran" (no 200, and
// OutcomeKnown says the engine refused it) from "it may have run" (a 200,
// or a lost answer), which is what the wire contract asks: an exec is not
// to be retried once the stream may have started.
func (c *Client) ExecStream(ctx context.Context, id string, req ExecRequest, write func(Stream, string), started func()) (ExecResult, error) {
	path, err := runPath(id, "exec")
	if err != nil {
		return ExecResult{}, err
	}
	if strings.TrimSpace(req.Cmd) == "" {
		return ExecResult{}, notSent(errors.New("the command is empty"))
	}
	var result ExecResult
	err = c.send(ctx, request{
		method: http.MethodPost, path: path, body: req, auth: true,
		headerTimeout: execHeaderTimeout(req.TimeoutSec),
	}, func(resp *http.Response) error {
		if resp.StatusCode != http.StatusOK {
			raw, readErr := readBounded(resp.Body, maxJSONBytes)
			return problemFrom(resp.StatusCode, raw, readErr)
		}
		// The engine only answers 200 once it has taken the command.
		if started != nil {
			started()
		}
		if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
			return fmt.Errorf("the engine answered the exec with %q, not an event stream", contentType)
		}
		var streamErr error
		result, streamErr = readExecStream(resp.Body, write)
		return streamErr
	})
	return result, err
}

// readExecStream reads an exec's event stream to its exit frame. A frame is
// `event: <name>`, then one or more `data:` lines, then a blank line. A
// chunk that held line feeds is one `data:` line per line of it, and is
// joined back with "\n"; a carriage return is sent raw and is kept raw. An
// event the client does not know is ignored, as the contract asks.
func readExecStream(body io.Reader, write func(Stream, string)) (ExecResult, error) {
	reader := newLineReader(body)
	var (
		result ExecResult
		event  string
		data   []string
		exited bool
	)
	frame := func() error {
		if len(data) == 0 {
			event, data = "", nil
			return nil
		}
		chunk := strings.Join(data, "\n")
		name := Stream(event)
		event, data = "", nil
		switch name {
		case Stdout, Stderr:
			write(name, chunk)
		case "exit":
			var exit struct {
				ExitCode int  `json:"exitCode"`
				TimedOut bool `json:"timedOut"`
			}
			if json.Unmarshal([]byte(chunk), &exit) != nil {
				return ErrBadExit
			}
			result, exited = ExecResult{ExitCode: exit.ExitCode, TimedOut: exit.TimedOut}, true
		}
		return nil
	}
	for {
		line, err := reader.next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return result, err
			}
			// The stream ended: a last frame may lack its blank line.
			if err := frame(); err != nil {
				return result, err
			}
			if !exited {
				return ExecResult{}, ErrStreamEnded
			}
			return result, nil
		}
		if line == "" {
			if err := frame(); err != nil {
				return result, err
			}
			if exited {
				return result, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // a comment, or a keep-alive
		}
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
}

// lineReader yields the lines of a stream, each without its "\n" and with
// anything else it ended in. next returns io.EOF after the last line,
// whether or not that line ended with "\n".
type lineReader struct {
	buffered *bufio.Reader
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{buffered: bufio.NewReaderSize(r, 64<<10)}
}

func (r *lineReader) next() (string, error) {
	var line []byte
	for {
		chunk, err := r.buffered.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			// The line is longer than the buffer: keep what there is of it
			// and read on. chunk is only good until the next read.
			line = append(line, chunk...)
			if len(line) > maxSSELineBytes {
				return "", errLineTooLong
			}
		case err != nil:
			// io.EOF, with the last of the stream before it when the final
			// line lacked its "\n"; or the read failed.
			if len(chunk) > 0 {
				return string(append(line, chunk...)), nil
			}
			return "", err
		default:
			return string(append(line, chunk[:len(chunk)-1]...)), nil // without the "\n", a "\r" before it is data
		}
	}
}
