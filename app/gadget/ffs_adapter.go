package main

import (
	"context"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const pollTimeoutMs = 100 // Check context every 100ms

type Reader struct {
	fd int
}

type Writer struct {
	fd int
}

func NewReader(f *os.File) (*Reader, error) {
	return &Reader{fd: int(f.Fd())}, nil
}

func NewWriter(f *os.File) (*Writer, error) {
	return &Writer{fd: int(f.Fd())}, nil
}

// ReadContext reads from the file descriptor with context cancellation support.
// Uses poll() with timeout to avoid spawning goroutines that could leak.
func (r *Reader) ReadContext(ctx context.Context, p []byte) (int, error) {
	for {
		// Check context first
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}

		// Poll with timeout
		fds := []unix.PollFd{{Fd: int32(r.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, pollTimeoutMs)
		if err != nil {
			if err == unix.EINTR {
				continue // Interrupted, retry
			}
			return 0, err
		}
		if n == 0 {
			// Timeout, loop back to check context
			continue
		}

		// Check for errors or hangup
		if fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			return 0, io.EOF
		}

		// Data available
		if fds[0].Revents&unix.POLLIN != 0 {
			return unix.Read(r.fd, p)
		}
	}
}

// WriteContext writes to the file descriptor with context cancellation support.
// Uses poll() with timeout to avoid spawning goroutines that could leak.
func (w *Writer) WriteContext(ctx context.Context, p []byte) (int, error) {
	written := 0
	total := len(p)

	for written < total {
		// Check context first
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		default:
		}

		// Poll with timeout
		fds := []unix.PollFd{{Fd: int32(w.fd), Events: unix.POLLOUT}}
		n, err := unix.Poll(fds, pollTimeoutMs)
		if err != nil {
			if err == unix.EINTR {
				continue // Interrupted, retry
			}
			return written, err
		}
		if n == 0 {
			// Timeout, loop back to check context
			continue
		}

		// Check for errors or hangup
		if fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			return written, io.EOF
		}

		// Ready to write
		if fds[0].Revents&unix.POLLOUT != 0 {
			n, err := unix.Write(w.fd, p[written:])
			if err != nil {
				return written, err
			}
			written += n
		}
	}

	return written, nil
}
