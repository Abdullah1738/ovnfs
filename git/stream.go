package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Stream answers questions through one long-lived git process: Add queues a
// question without waiting for git, one goroutine writes queued questions to git,
// another parses the answers git writes in question order, and the answer
// callback runs exactly once per question, from that goroutine or from Add.
// Questions are answered with an error when git exits early, writes malformed
// output or is killed by the context; every later question gets that error too.
type Stream[T, R any] struct {
	name    string // git subcommand, for errors
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	sep     byte
	wake    chan struct{} // tells the writer that pending is worth a write, or that Close ran
	pending []byte        // questions not yet written to git
	parse   func(*bufio.Reader) (key string, value R, itemErr, err error)
	answer  func(tag T, value R, err error)
	stderr  bytes.Buffer
	mu      sync.Mutex
	queue   []request[T]
	closing bool  // Close has run, so the end of git's output is expected
	ended   error // set once the reader stops; later questions are answered with it
	failed  error // the failure that ended the stream early, returned by Close
	done    chan struct{}
}

// request is one question written to git and the caller's tag for its answer.
type request[T any] struct {
	key string
	tag T
}

// startStream launches cmd and its answer reader; sep terminates each question.
func startStream[T, R any](name string, cmd *exec.Cmd, sep byte, parse func(*bufio.Reader) (string, R, error, error), answer func(T, R, error)) (*Stream[T, R], error) {
	s := &Stream[T, R]{name: name, cmd: cmd, sep: sep, parse: parse, answer: answer, wake: make(chan struct{}, 1), done: make(chan struct{})}
	cmd.Stderr = &s.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("git %s: %w", name, err)
	}
	s.stdin = stdin
	go s.write()
	go s.read(bufio.NewReaderSize(stdout, 64<<10))
	return s, nil
}

// streamBatch is the pending size worth a write to git.
const streamBatch = 32 << 10

// Add queues one question; it must not be called concurrently or after Close.
func (s *Stream[T, R]) Add(key string, tag T) {
	s.mu.Lock()
	if err := s.ended; err != nil {
		s.mu.Unlock()
		var zero R
		s.answer(tag, zero, err)
		return
	}
	s.queue = append(s.queue, request[T]{key: key, tag: tag})
	s.pending = append(append(s.pending, key...), s.sep)
	full := len(s.pending) >= streamBatch
	s.mu.Unlock()
	if full {
		s.signal()
	}
}

// signal wakes the writer unless a wake-up is already due.
func (s *Stream[T, R]) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Close sends the remaining questions, waits until every one is answered and
// reports the failure that ended the stream early, if any.
func (s *Stream[T, R]) Close() error {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.signal()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// write sends pending questions to git until Close, then ends git's input. A
// failed write means git is gone: it is killed so the reader fails every question.
func (s *Stream[T, R]) write() {
	var buf []byte
	for range s.wake {
		s.mu.Lock()
		buf, s.pending = s.pending, buf[:0]
		closing := s.closing
		s.mu.Unlock()
		if len(buf) > 0 {
			if _, err := s.stdin.Write(buf); err != nil {
				s.cmd.Process.Kill()
				s.stdin.Close()
				return
			}
		}
		if closing {
			s.stdin.Close()
			return
		}
	}
}

// read answers questions in order until git's output ends, then fails the rest.
func (s *Stream[T, R]) read(out *bufio.Reader) {
	defer close(s.done)
	var err error
	for {
		key, value, itemErr, perr := s.parse(out)
		if perr != nil {
			err = perr
			break
		}
		s.mu.Lock()
		if len(s.queue) == 0 || s.queue[0].key != key {
			s.mu.Unlock()
			err = fmt.Errorf("unexpected answer for %q", key)
			break
		}
		r := s.queue[0]
		s.queue[0] = request[T]{}
		s.queue = s.queue[1:]
		s.mu.Unlock()
		s.answer(r.tag, value, itemErr)
	}
	if err != io.EOF {
		s.cmd.Process.Kill()
		io.Copy(io.Discard, out)
	}
	waitErr := s.cmd.Wait()
	s.mu.Lock()
	rest := s.queue
	s.queue = nil
	if err == io.EOF && len(rest) == 0 && s.closing {
		s.ended = os.ErrClosed
	} else {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		if msg := strings.TrimSpace(s.stderr.String()); msg != "" {
			err = fmt.Errorf("%w: %s", err, msg)
		} else if waitErr != nil {
			err = fmt.Errorf("%w: %v", err, waitErr)
		}
		s.failed = fmt.Errorf("git %s: %w", s.name, err)
		s.ended = s.failed
	}
	failure := s.ended
	s.mu.Unlock()
	var zero R
	for _, r := range rest {
		s.answer(r.tag, zero, failure)
	}
}

// Attribute states that git check-attr reports besides a value.
const (
	AttrUnspecified = "unspecified"
	AttrSet         = "set"
	AttrUnset       = "unset"
)

// CheckAttr starts `git check-attr` for names; each Add asks for a work-tree
// relative slash path and is answered with one state or value per name, in order.
func CheckAttr[T any](ctx context.Context, r *Repo, names []string, answer func(tag T, values []string, err error)) (*Stream[T, []string], error) {
	cmd := r.command(ctx, append([]string{"check-attr", "-z", "--stdin"}, names...)...)
	// Without per-path flushes git writes its answers in large blocks.
	cmd.Env = append(cmd.Env, "GIT_FLUSH=0")
	parse := func(out *bufio.Reader) (string, []string, error, error) {
		var path string
		values := make([]string, len(names))
		for i, name := range names {
			p, err := out.ReadSlice(0)
			if err != nil {
				return "", nil, nil, err
			}
			p = p[:len(p)-1]
			if i == 0 {
				path = string(p)
			} else if string(p) != path {
				return "", nil, nil, fmt.Errorf("answer for %q interleaved with %q", path, p)
			}
			n, err := out.ReadSlice(0)
			if err != nil {
				return "", nil, nil, noEOF(err)
			}
			if string(n[:len(n)-1]) != name {
				return "", nil, nil, fmt.Errorf("answer for %q names %q, not %q", path, n[:len(n)-1], name)
			}
			v, err := out.ReadSlice(0)
			if err != nil {
				return "", nil, nil, noEOF(err)
			}
			switch v = v[:len(v)-1]; string(v) {
			case AttrUnspecified:
				values[i] = AttrUnspecified
			case AttrSet:
				values[i] = AttrSet
			case AttrUnset:
				values[i] = AttrUnset
			default:
				values[i] = string(v)
			}
		}
		return path, values, nil, nil
	}
	return startStream("check-attr", cmd, 0, parse, answer)
}

// ObjectSizes starts `git cat-file --batch-check`; each Add asks for an object
// id and is answered with the object's size, or an os.ErrNotExist error.
func ObjectSizes[T any](ctx context.Context, r *Repo, answer func(tag T, size int64, err error)) (*Stream[T, int64], error) {
	cmd := r.command(ctx, "cat-file", "--batch-check=%(objectname) %(objectsize)", "--buffer")
	// A partial clone's missing blob is answered as missing rather than fetched.
	cmd.Env = append(cmd.Env, "GIT_NO_LAZY_FETCH=1")
	parse := func(out *bufio.Reader) (string, int64, error, error) {
		line, err := out.ReadSlice('\n')
		if err != nil {
			return "", 0, nil, err
		}
		oid, size, ok := strings.Cut(string(line[:len(line)-1]), " ")
		if !ok {
			return "", 0, nil, fmt.Errorf("invalid batch-check answer %q", line)
		}
		if size == "missing" {
			return oid, 0, fmt.Errorf("git object %s: %w", oid, os.ErrNotExist), nil
		}
		n, err := strconv.ParseInt(size, 10, 64)
		if err != nil || n < 0 {
			return "", 0, nil, fmt.Errorf("invalid batch-check answer %q", line)
		}
		return oid, n, nil, nil
	}
	return startStream("cat-file", cmd, '\n', parse, answer)
}

// noEOF turns an end of output inside an answer into a truncation error.
func noEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Conversion is the configuration that decides, together with gitattributes,
// whether git converts content between blobs and the work tree.
type Conversion struct {
	AutoCRLF bool            // core.autocrlf is true or input: unattributed text gets end-of-line conversion
	Filters  map[string]bool // filter drivers with a clean, smudge or process command
}

// Conversion reads the repository's effective core.autocrlf and filter drivers.
func (r *Repo) Conversion(ctx context.Context) (Conversion, error) {
	c := Conversion{Filters: map[string]bool{}}
	out, err := r.configValue(ctx, "-z", "--get-regexp", `^(core\.autocrlf|filter\..+\.(clean|smudge|process))$`)
	if err != nil {
		return c, err
	}
	for entry := range strings.SplitSeq(out, "\x00") {
		key, value, hasValue := strings.Cut(entry, "\n")
		if key == "core.autocrlf" {
			// A valueless key is boolean true; the last assignment wins.
			c.AutoCRLF = !hasValue || !isFalse(value)
		} else if name, ok := strings.CutPrefix(key, "filter."); ok {
			if dot := strings.LastIndexByte(name, '.'); dot > 0 {
				c.Filters[name[:dot]] = true
			}
		}
	}
	return c, nil
}

// isFalse reports git's spellings of a false boolean.
func isFalse(value string) bool {
	switch strings.ToLower(value) {
	case "false", "no", "off", "0", "":
		return true
	}
	return false
}
