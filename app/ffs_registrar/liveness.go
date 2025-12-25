package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"sync/atomic"
	"time"
)

// stopServerTimeout is the maximum time to wait for the liveness server to shut down.
// This prevents blocking the enabled channel consumer if shutdown takes too long.
const stopServerTimeout = 5 * time.Second

func watchLiveness(sockPath string, ready *atomic.Uint32, l *slog.Logger) {
	for {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			ready.Store(0)
			l.Debug("gadget not ready (socket down), retrying", "err", err)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		l.Info("connected to gadget liveness socket")
		ready.Store(1)
		// Block until the socket dies, then loop.
		_, _ = io.Copy(io.Discard, conn)
		_ = conn.Close()
		ready.Store(0)
		l.Warn("lost liveness socket; marking not ready")
	}
}

func serveEnabled(ctx context.Context, sockPath string, l *slog.Logger) <-chan struct{} {
	closeCompletedChan := make(chan struct{})
	go func() {
		l.Info("starting liveness server", "socket", sockPath)
		ln, err := net.Listen("unix", sockPath)
		// world-readable is fine; it's just liveness
		_ = os.Chmod(sockPath, 0666)

		if err != nil {
			l.Error("failed to start liveness server", "err", err.Error())
			return
		}
		defer ln.Close()
		go func() {
			<-ctx.Done()
			ln.Close()
			closeCompletedChan <- struct{}{}
			close(closeCompletedChan)
		}()

		for {
			select {
			case <-ctx.Done():
				l.Info("liveness server shutting down")
				return
			default:
			}

			conn, err := ln.Accept()
			if err != nil {
				l.Error("failed to accept liveness connection", "err", err)
				continue
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(io.Discard, c)
			}(conn)
		}
	}()
	return closeCompletedChan
}

func runEnabledWatcher(enabled <-chan bool, sockPath string, l *slog.Logger) {
	var activeCancel context.CancelFunc
	var closeCompletedChan <-chan struct{}
	isEnabled := false

	// stopServer cancels the current server and waits for cleanup with timeout
	stopServer := func() {
		if activeCancel != nil {
			activeCancel()
			activeCancel = nil
			if closeCompletedChan != nil {
				select {
				case <-closeCompletedChan:
					// Clean shutdown
				case <-time.After(stopServerTimeout):
					l.Warn("stopServer timeout waiting for liveness server shutdown")
				}
			}
		}
	}
	defer stopServer()

	for en := range enabled {
		if en && !isEnabled {
			isEnabled = true
			ctx, cancel := context.WithCancel(context.Background())
			activeCancel = cancel
			closeCompletedChan = serveEnabled(ctx, sockPath, l)
		} else if !en && isEnabled {
			isEnabled = false
			stopServer()
		}
	}
}
