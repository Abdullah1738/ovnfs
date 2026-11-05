package git

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// CatFile serializes a long-lived batch process and restarts a broken subprocess.
type CatFile struct {
	mu     sync.Mutex
	repo   *Repo
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Reader
	closed bool
}

// CatFile starts a persistent object reader in the repository's shared object database.
func (r *Repo) CatFile() (*CatFile, error) {
	c := &CatFile{repo: r}
	if err := c.start(); err != nil {
		return nil, err
	}
	return c, nil
}

// start opens both batch pipes before launching Git.
func (c *CatFile) start() error {
	cmd := c.repo.command(context.Background(), "cat-file", "--batch")
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		input.Close()
		output.Close()
		return err
	}
	c.cmd, c.input, c.output = cmd, input, bufio.NewReader(output)
	return nil
}

// stop terminates and reaps a subprocess, including one with a broken protocol stream.
func (c *CatFile) stop() {
	if c.cmd == nil {
		return
	}
	c.input.Close()
	c.cmd.Process.Kill()
	c.cmd.Wait()
	c.cmd, c.input, c.output = nil, nil, nil
}

// blob reads exactly one batch response, including its trailing newline.
func (c *CatFile) blob(oid string) ([]byte, error) {
	if _, err := io.WriteString(c.input, oid+"\n"); err != nil {
		return nil, err
	}
	header, err := c.output.ReadString('\n')
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(header)
	if len(fields) == 2 && fields[1] == "missing" {
		return nil, fmt.Errorf("git object %s: %w", oid, os.ErrNotExist)
	}
	if len(fields) != 3 || fields[0] != oid {
		return nil, fmt.Errorf("invalid cat-file response %q", header)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 || uint64(size) > uint64(^uint(0)>>1)-1 {
		return nil, fmt.Errorf("invalid cat-file size %q", fields[2])
	}
	data := make([]byte, int(size)+1)
	if _, err := io.ReadFull(c.output, data); err != nil {
		return nil, err
	}
	if data[len(data)-1] != '\n' {
		return nil, fmt.Errorf("invalid cat-file terminator")
	}
	if fields[1] != "blob" {
		return nil, fmt.Errorf("git object %s is %s, not blob", oid, fields[1])
	}
	return data[:len(data)-1], nil
}

// Blob returns a blob's bytes, retrying once with a fresh batch process after failure.
func (c *CatFile) Blob(oid string) ([]byte, error) {
	if len(oid) != 40 {
		return nil, fmt.Errorf("invalid SHA-1 object name %q", oid)
	}
	if _, err := hex.DecodeString(oid); err != nil {
		return nil, fmt.Errorf("invalid SHA-1 object name %q", oid)
	}
	oid = strings.ToLower(oid)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, os.ErrClosed
	}
	for attempt := range 2 {
		if c.cmd == nil {
			if err := c.start(); err != nil {
				return nil, err
			}
		}
		data, err := c.blob(oid)
		if err == nil {
			return data, nil
		}
		c.stop()
		if attempt == 1 {
			return nil, err
		}
	}
	panic("unreachable cat-file retry")
}

// Close stops the child process and rejects future object requests.
func (c *CatFile) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.stop()
	return nil
}
