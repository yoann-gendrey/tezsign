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
	var cancel context.CancelFunc
	var closeCompletedChan <-chan struct{}
	isEnabled := false
	for en := range enabled {
		if en && !isEnabled {
			isEnabled = true
			ctx := context.Background()
			closeCompletedChan = serveEnabled(ctx, sockPath, l)
		} else if !en && isEnabled {
			if cancel != nil {
				cancel()
				cancel = nil
				<-closeCompletedChan // wait for close
			}
		}
	}
}
