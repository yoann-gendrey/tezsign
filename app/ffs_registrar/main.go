package main

import (
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/tez-capital/tezsign/app/gadget/common"
)

// trySendEnabled attempts to send a value to the enabled channel without blocking.
// This prevents the EP0 event loop from blocking if events arrive faster than
// they can be consumed, which would cause USB enumeration failures.
func trySendEnabled(ch chan<- bool, val bool, l *slog.Logger) {
	select {
	case ch <- val:
		// Successfully sent
	default:
		l.Warn("enabled channel full, dropping event (consumer may be slow)", "value", val)
	}
}

func triggerSoftConnect(l *slog.Logger) error {
	const udcClassPath = "/sys/class/udc"
	const softConnectFile = "soft_connect"

	l.Info("--- Checking %s for %s ---", udcClassPath, softConnectFile)

	softConnectPath := "/tmp/soft_connect"
	if _, err := os.Stat(softConnectPath); err != nil {
		l.Error("soft_connect symlink missing", "err", err.Error())
		return err
	}
	l.Info("triggering soft connect", "udc", softConnectPath)
	err := os.WriteFile(softConnectPath, []byte("disconnect\n"), 0644) // writing '0' disconnects, '1' connects
	if err != nil {
		l.Error("writing soft_connect file", "err", err.Error())
		return err
	}
	time.Sleep(500 * time.Millisecond)

	err = os.WriteFile(softConnectPath, []byte("connect\n"), 0644) // writing '0' disconnects, '1' connects
	if err != nil {
		l.Error("writing soft_connect file", "err", err.Error())
		return err
	}
	l.Info("soft connect triggered successfully")
	return nil
}

func drainEP0Events(ep0 *os.File, enabled chan<- bool, ready *atomic.Uint32, l *slog.Logger) {
	buf := make([]byte, evSize)

	for {
		n, err := ep0.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			l.Error("ep0 read events", "err", err)
			triggerSoftConnect(l) // try to recover by soft reconnect
			continue
		}

		if n < evSize {
			l.Warn("ep0 read event too short", "n", n)
			continue
		}

		// [65 90 0 0 0 0 8 0 | 4 | 0 0 0]
		//                      ^ evType
		evType := int(buf[8])
		switch evType {
		case evTypeBind:
			l.Info("tezsign gadget bound")
			continue
		case evTypeUnbind:
			l.Info("tezsign gadget unbound")
			continue
		case evTypeEnable:
			l.Info("tezsign gadget enabled")
			trySendEnabled(enabled, true, l)
			continue
		case evTypeDisable:
			l.Info("tezsign gadget disabled")
			trySendEnabled(enabled, false, l)
			triggerSoftConnect(l)
			continue
		case evTypeSuspend:
			l.Info("tezsign gadget suspended")
			trySendEnabled(enabled, false, l)
			continue
		case evTypeResume:
			l.Info("tezsign gadget resumed")
			trySendEnabled(enabled, true, l)
			continue
		case evTypeSetup:
			// Handle below
		default:
			continue
		}

		// [65 90 0 0 0 0 8 0 | 4 | 0 0 0]
		// ^____ request ____^
		req := parseCtrlReq(buf[0:8])
		l.Info("parsed", "type", req.bmRequestType, "request", req.bRequest, "length", req.wLength)
		// Handle our vendor IN request
		if req.bmRequestType == bmReqTypeVendorIn && req.bRequest == vendorReqReady {
			// Prepare reply
			reply := [8]byte{}
			copy(reply[:4], []byte("TZSG"))
			binary.LittleEndian.PutUint16(reply[4:6], protoVersion)
			reply[6] = byte(ready.Load())

			// Respect host's wLength (shorter read is OK)
			wlen := int(req.wLength)
			if wlen > len(reply) {
				wlen = len(reply)
			}
			// Write data stage
			if _, err := ep0.Write(reply[:wlen]); err != nil {
				l.Error("ep0 write vendor reply", "err", err)
			}
			continue
		}
		l.Warn("Unhandled SETUP request, STALLING", "type", req.bmRequestType, "req", req.bRequest)
		// A 0-byte write on an unhandled request is the
		// userspace way to signal a STALL to the kernel.
		if _, err := ep0.Write(nil); err != nil {
			l.Error("ep0 write ZLP/STALL failed", "err", err)
		}
	}
}

func main() {
	l := slog.Default()

	ep0, err := os.OpenFile(Ep0Path, os.O_RDWR, 0)
	if err != nil {
		slog.Error("failed to open ep0", "error", err.Error(), "function", FunctionName, "ffs_root", common.FfsInstanceRoot)
		os.Exit(1)
	}
	defer ep0.Close()

	if _, err := ep0.Write(deviceDescriptors); err != nil {
		slog.Error("failed to write device descriptors", "error", err.Error())
		os.Exit(1)
	}

	if _, err := ep0.Write(deviceStrings); err != nil {
		slog.Error("failed to write device strings", "error", err.Error())
		os.Exit(1)
	}

	// Start watching gadget liveness
	var ready atomic.Uint32
	go watchLiveness(common.ReadySock, &ready, l)
	// Buffer size 8 to handle rapid event sequences (enable/disable/suspend/resume)
	// without blocking the EP0 event loop
	enabled := make(chan bool, 8)
	go runEnabledWatcher(enabled, common.EnabledSock, l)

	l.Info("FFS registrar online; handling EP0 control & events")

	drainEP0Events(ep0, enabled, &ready, l)
}
