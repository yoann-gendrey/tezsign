package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tez-capital/tezsign/app/gadget/common"
	"github.com/tez-capital/tezsign/broker"
	"github.com/tez-capital/tezsign/health"
	"github.com/tez-capital/tezsign/keychain"
	"github.com/tez-capital/tezsign/logging"
	"github.com/tez-capital/tezsign/signer"
	"github.com/tez-capital/tezsign/watchdog"
	"google.golang.org/protobuf/proto"
)

const (
	waitEndpointsTime = 30 * time.Second

	securedAttemptWindow = 30 * time.Second
	securedAttemptLimit  = 5

	// Backoff after fatal broker errors to avoid tight restart loops
	fatalErrorBackoff = 5 * time.Second

	// Handler timeout prevents signing operations from hanging indefinitely
	handlerTimeout = 30 * time.Second
)

var securedRPCLimiter = newAttemptLimiter(securedAttemptLimit, securedAttemptWindow)

// isFatalBrokerError returns true if the error indicates a permanent failure
// that requires a clean restart with backoff, rather than immediate retry.
func isFatalBrokerError(err error) bool {
	if err == nil {
		return false
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.EBADF: // Bad file descriptor - fd closed/invalid
			return true
		case syscall.ENOENT: // No such file - endpoint removed
			return true
		}
	}

	return false
}

// withTimeout wraps a handler with a context timeout to prevent hanging.
// If the handler doesn't complete within the timeout, a timeout error is returned.
func withTimeout(h broker.Handler, timeout time.Duration) broker.Handler {
	return func(ctx context.Context, payload []byte) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return h(ctx, payload)
	}
}

func main() {
	logCfg := logging.NewConfigFromEnv()
	if logCfg.File == "" {
		dataStore := strings.TrimSpace(os.Getenv("DATA_STORE"))
		if dataStore != "" {
			if err := os.MkdirAll(dataStore, 0o700); err != nil {
				panic(fmt.Errorf("could not create DATA_STORE=%q: %w", dataStore, err))
			}
			logCfg.File = filepath.Join(dataStore, "gadget.log")
		} else {
			logCfg.File = logging.DefaultFileInExecDir("gadget.log")
		}
	}

	if err := logging.EnsureDir(logCfg.File); err != nil {
		panic("Could not create dir for path of configuration file!")
	}

	l, _ := logging.New(logCfg)

	l.Debug("logging to file", "path", logging.CurrentFile())

	// Initialize systemd watchdog notifier (nil if not running under systemd)
	notifier := watchdog.New()
	defer func() {
		if notifier != nil {
			_ = notifier.Close()
		}
	}()

	if err := run(l, notifier); err != nil {
		l.Error("RUN ERROR", slog.Any("err", err))
		os.Exit(1)
	}
}

func handleSignAndStatus(base func(context.Context, []byte) ([]byte, error)) broker.Handler {
	return func(ctx context.Context, payload []byte) ([]byte, error) {
		var req signer.Request
		if err := proto.Unmarshal(payload, &req); err != nil {
			return marshalErr(1, fmt.Sprintf("bad protobuf: %v", err)), nil
		}
		switch req.Payload.(type) {
		case *signer.Request_Sign, *signer.Request_Status:
			// allowed on IF0
		default:
			return marshalErr(98, "wrong interface: use management (IF1) for this request"), nil
		}

		return base(ctx, payload)
	}
}

func handleMgmtOnly(base func(context.Context, []byte) ([]byte, error)) broker.Handler {
	return func(ctx context.Context, payload []byte) ([]byte, error) {
		var req signer.Request
		if err := proto.Unmarshal(payload, &req); err != nil {
			return marshalErr(1, fmt.Sprintf("bad protobuf: %v", err)), nil
		}
		// reject Sign here
		if _, isSign := req.Payload.(*signer.Request_Sign); isSign {
			return marshalErr(1001, "wrong interface: use sign (IF0) for signing"), nil
		}
		return base(ctx, payload)
	}
}

func handleRequestsFactory(fs *keychain.FileStore, kr *keychain.KeyRing, l *slog.Logger) broker.Handler {
	return func(ctx context.Context, payload []byte) ([]byte, error) {
		var req signer.Request
		if err := proto.Unmarshal(payload, &req); err != nil {
			return marshalErr(1, fmt.Sprintf("bad protobuf: %v", err)), nil
		}
		defer wipeReq(&req)
		defer keychain.MemoryWipe(payload)

		switch p := req.Payload.(type) {
		case *signer.Request_Unlock:
			pass := p.Unlock.GetPassphrase()
			defer keychain.MemoryWipe(pass)

			ids := p.Unlock.GetKeyIds()
			if len(ids) == 0 {
				return marshalErr(10, "unlock: no key_ids provided"), nil
			}
			if len(pass) == 0 {
				return marshalErr(11, "unlock: passphrase required"), nil
			}
			if ok, wait := securedRPCLimiter.Allow(); !ok {
				l.Warn("unlock throttled", slog.Duration("retry_in", wait))
				msg := fmt.Sprintf(
					"unlock throttled: retry in ~%s (max %d attempts per %s)",
					wait.Round(time.Second),
					securedAttemptLimit,
					securedAttemptWindow,
				)
				return marshalErr(rpcUnlockThrottled, msg), nil
			}

			results := make([]*signer.PerKeyResult, 0, len(ids))
			for _, id := range ids {
				res := &signer.PerKeyResult{KeyId: id}
				if err := kr.Unlock(id, pass); err != nil {
					res.Ok = false
					res.Error = err.Error()
					l.Error("unlock", "key", id, "err", err)
				} else {
					res.Ok = true
					l.Debug("UNLOCKED " + id)
				}
				results = append(results, res)
			}

			l.Debug("UNLOCK batch", "count", len(ids))

			return proto.Marshal(&signer.Response{
				Payload: &signer.Response_Unlock{
					Unlock: &signer.UnlockResponse{Results: results},
				},
			})

		case *signer.Request_Lock:
			ids := p.Lock.GetKeyIds()
			if len(ids) == 0 {
				return marshalErr(20, "lock: no key_ids provided"), nil
			}

			results := make([]*signer.PerKeyResult, 0, len(ids))
			for _, id := range ids {
				res := &signer.PerKeyResult{KeyId: id}
				if err := kr.Lock(id); err != nil {
					res.Ok = false
					res.Error = err.Error()
					l.Error("LOCK", "key", id, "err", err)
				} else {
					res.Ok = true
					l.Debug("LOCKED " + id)
				}
				results = append(results, res)
			}

			l.Debug("LOCK batch", "count", len(ids))

			return proto.Marshal(&signer.Response{
				Payload: &signer.Response_Lock{
					Lock: &signer.LockResponse{Results: results},
				},
			})

		case *signer.Request_Status:
			st := kr.Status()

			return proto.Marshal(&signer.Response{
				Payload: &signer.Response_Status{
					Status: &signer.StatusResponse{Keys: st},
				},
			})

		case *signer.Request_Sign:
			tz4 := p.Sign.GetTz4()
			sig, err := kr.SignAndUpdate(tz4, p.Sign.GetMessage())
			if err != nil {
				switch {
				case errors.Is(err, keychain.ErrKeyLocked):
					return marshalErr(rpcKeyLocked, keychain.ErrKeyLocked.Error()), nil
				case errors.Is(err, keychain.ErrKeyNotFound):
					return marshalErr(rpcKeyNotFound, keychain.ErrKeyNotFound.Error()), nil
				case errors.Is(err, keychain.ErrStaleWatermark):
					return marshalErr(rpcStaleWatermark, keychain.ErrStaleWatermark.Error()), nil
				case errors.Is(err, keychain.ErrBadPayload):
					return marshalErr(rpcBadPayload, keychain.ErrBadPayload.Error()), nil

				default:
					return marshalErr(30, "sign: "+err.Error()), nil
				}
			}

			l.Debug("SIGNED", "tz4", tz4)

			result, err := proto.Marshal(&signer.Response{
				Payload: &signer.Response_Sign{
					Sign: &signer.SignResponse{Signature: sig},
				},
			})

			return result, err

		case *signer.Request_NewKeys:
			pass := p.NewKeys.GetPassphrase()
			defer keychain.MemoryWipe(pass)
			ids := p.NewKeys.GetKeyIds()
			if len(ids) == 0 {
				ids = []string{""}
			}

			results := make([]*signer.NewKeyPerKeyResult, 0, len(ids))
			for _, alias := range ids {
				id, blPubkey, tz4, err := kr.CreateKey(alias, pass)
				r := &signer.NewKeyPerKeyResult{
					KeyId:    id,
					BlPubkey: blPubkey,
					Tz4:      tz4,
				}
				if err != nil {
					if id == "" {
						r.KeyId = alias
					}
					r.Ok = false
					r.Error = err.Error()
					l.Error("NEW_KEY", "alias", alias, "err", err)
				} else {
					r.Ok = true
					l.Debug("NEW_KEY", "key", id, "tz4", tz4)
				}
				results = append(results, r)
			}

			l.Debug("NEW_KEY batch", "count", len(ids))

			return proto.Marshal(&signer.Response{
				Payload: &signer.Response_NewKey{
					NewKey: &signer.NewKeysResponse{
						Results: results,
					},
				},
			})

		case *signer.Request_DeleteKeys:
			pass := p.DeleteKeys.GetPassphrase()
			defer keychain.MemoryWipe(pass)
			ids := p.DeleteKeys.GetKeyIds()
			if len(ids) == 0 {
				return marshalErr(90, "delete_keys: no key_ids provided"), nil
			}
			if len(pass) == 0 {
				return marshalErr(91, "delete_keys: passphrase required"), nil
			}
			if ok, wait := securedRPCLimiter.Allow(); !ok {
				l.Warn("delete_keys throttled", slog.Duration("retry_in", wait))
				msg := fmt.Sprintf(
					"delete_keys throttled: retry in ~%s (max %d attempts per %s)",
					wait.Round(time.Second),
					securedAttemptLimit,
					securedAttemptWindow,
				)
				return marshalErr(rpcDeleteThrottled, msg), nil
			}
			if err := kr.VerifyMasterPassword(pass); err != nil {
				l.Warn("delete_keys: bad passphrase", slog.Any("err", err))
				return marshalErr(rpcDeleteBadPass, "delete_keys: invalid passphrase"), nil
			}

			results := make([]*signer.PerKeyResult, 0, len(ids))
			for _, alias := range ids {
				res := &signer.PerKeyResult{KeyId: alias}
				if err := kr.DeleteKey(alias); err != nil {
					res.Ok = false
					res.Error = err.Error()
					l.Error("DELETE_KEY", "key", alias, "err", err)
				} else {
					res.Ok = true
					l.Debug("DELETE_KEY", "key", alias)
				}
				results = append(results, res)
			}

			l.Debug("DELETE_KEY batch", "count", len(ids))

			return proto.Marshal(&signer.Response{
				Payload: &signer.Response_DeleteKeys{
					DeleteKeys: &signer.DeleteKeysResponse{
						Results: results,
					},
				},
			})

		case *signer.Request_Logs:
			path := logging.CurrentFile()
			if path == "" {
				return marshalErr(50, "logs: file logging not enabled"), nil
			}

			lim := int(p.Logs.GetLimit())
			lines, err := logging.TailLastLines(path, lim)
			if err != nil {
				return marshalErr(51, fmt.Sprintf("logs: %v", err)), nil
			}

			return proto.Marshal(&signer.Response{
				Payload: &signer.Response_Logs{
					Logs: &signer.LogsResponse{Lines: lines},
				},
			})

		case *signer.Request_InitMaster:
			det := p.InitMaster.GetDeterministic()
			pass := p.InitMaster.GetPassphrase()
			defer keychain.MemoryWipe(pass)

			if len(pass) == 0 {
				return marshalErr(60, "init_master: passphrase required"), nil
			}

			// master.json
			if err := fs.InitMaster(); err != nil {
				return marshalErr(61, "init_master: "+err.Error()), nil
			}

			// seed.bin under KEK(pass)
			if err := fs.WriteSeed(pass, det); err != nil {
				return marshalErr(62, "init_master/seed: "+err.Error()), nil
			}

			return marshalOK(true), nil

		case *signer.Request_InitInfo:
			master, det, e := fs.InitInfo()
			if e != nil {
				return marshalErr(70, "init_info: "+e.Error()), nil
			}

			return proto.Marshal(&signer.Response{
				Payload: &signer.Response_InitInfo{
					InitInfo: &signer.InitInfoResponse{
						MasterPresent:        master,
						DeterministicEnabled: det,
					},
				},
			})

		case *signer.Request_SetLevel:
			keyID := p.SetLevel.GetKeyId()
			if err := kr.SetLevel(keyID, p.SetLevel.GetLevel()); err != nil {
				return marshalErr(80, fmt.Sprintf("set_level for key=%s error: %v", keyID, err)), nil
			}

			return marshalOK(true), nil

		default:
			return marshalErr(1000, "unknown request"), nil
		}
	}
}

func runBrokers(ctx context.Context, fs *keychain.FileStore, kr *keychain.KeyRing, l *slog.Logger) error {
	l.Info("Waiting for endpoints...")
	in0, out0, in1, out1, err := waitForFunctionFSEndpoints(common.FfsInstanceRoot, waitEndpointsTime)
	if err != nil {
		return err
	}

	l.Info("Endpoints ready; starting broker",
		slog.String("IF0.in", in0), slog.String("IF0.out", out0),
		slog.String("IF1.in", in1), slog.String("IF1.out", out1),
	)

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	bLogger := broker.WithLogger(l.With("component", "broker"))
	// IF0 (sign) endpoints
	in0Fd, err := os.OpenFile(in0, os.O_WRONLY, 0) // device -> host
	if err != nil {
		return fmt.Errorf("open IF0 IN: %w", err)
	}
	defer in0Fd.Close()
	out0Fd, err := os.OpenFile(out0, os.O_RDONLY, 0) // host -> device
	if err != nil {
		return fmt.Errorf("open IF0 OUT: %w", err)
	}
	defer out0Fd.Close()

	// IF1 (mgmt) endpoints
	in1Fd, err := os.OpenFile(in1, os.O_WRONLY, 0) // device -> host
	if err != nil {
		return fmt.Errorf("open IF1 IN: %w", err)
	}
	defer in1Fd.Close()
	out1Fd, err := os.OpenFile(out1, os.O_RDONLY, 0) // host -> device
	if err != nil {
		return fmt.Errorf("open IF1 OUT: %w", err)
	}
	defer out1Fd.Close()

	r0, _ := NewReader(out0Fd)
	w0, _ := NewWriter(in0Fd)
	r1, _ := NewReader(out1Fd)
	w1, _ := NewWriter(in1Fd)

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	cleanupSock := serveReadySocket(l)
	defer cleanupSock()
	// IF0: sign channel (with timeout wrapper to prevent hanging)
	signBroker := broker.New(r0, w0, bLogger, broker.WithHandler(
		withTimeout(handleSignAndStatus(handleRequestsFactory(fs, kr, l)), handlerTimeout)))
	defer signBroker.Stop()
	// IF1: management channel (with timeout wrapper to prevent hanging)
	mgmtBroker := broker.New(r1, w1, bLogger, broker.WithHandler(
		withTimeout(handleMgmtOnly(handleRequestsFactory(fs, kr, l)), handlerTimeout)))
	defer mgmtBroker.Stop()

	l.Info("Signer gadget online; awaiting requests.")

	// Wait for shutdown or broker failure
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-signBroker.Done():
		return fmt.Errorf("sign broker exited unexpectedly")
	case <-mgmtBroker.Done():
		return fmt.Errorf("management broker exited unexpectedly")
	}
}

func run(l *slog.Logger, notifier *watchdog.Notifier) error {
	// Keystore directory: DATA_STORE/keystore when DATA_STORE is set; else next to binary
	var baseDir string
	if ds := strings.TrimSpace(os.Getenv("DATA_STORE")); ds != "" {
		baseDir = filepath.Join(ds, "keystore")
	} else {
		baseDir = logging.DefaultFileInExecDir("keystore") // e.g. /path/to/bin/keystore
	}
	// Ensure keystore dir exists (0700 since it holds secrets)
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return fmt.Errorf("keystore mkdir %q: %w", baseDir, err)
	}

	fs, err := keychain.NewFileStore(baseDir)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}

	kr := keychain.NewKeyRing(l, fs)

	// Initialize health monitor (goroutine limit of 100 for embedded devices)
	healthMon := health.NewMonitor(100)
	_ = healthMon // Will be used for health checks in watchdog context

	// Graceful shutdown on SIGTERM/SIGINT
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Start watchdog pinger (runs in background, signals WATCHDOG=1 periodically)
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	stopPinger := notifier.StartPinger(rootCtx)
	defer stopPinger()

	// Signal READY early so systemd doesn't kill us while waiting for gadget to be enabled
	// The service is ready to handle requests as soon as it enters the revival loop
	if err := notifier.Ready(); err != nil {
		l.Warn("failed to signal ready to systemd", slog.Any("err", err))
	}
	l.Info("tezsign service ready, waiting for gadget to be enabled")

	// Revival loop: wait for enabled socket, run brokers, restart on disable
	for {
		// Check for shutdown signal before waiting for enabled socket
		select {
		case sig := <-sigCh:
			l.Info("Received shutdown signal", slog.String("signal", sig.String()))
			_ = notifier.Stopping()
			return nil
		default:
		}

		enabled, err := net.Dial("unix", common.EnabledSock)
		if err != nil {
			l.Debug("gadget not enabled (socket down), retrying", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		l.Info("gadget enabled; starting brokers")

		ctx, cancel := context.WithCancel(context.Background())

		// Monitor for disable (socket close) or shutdown signal
		go func() {
			select {
			case <-func() chan struct{} {
				ch := make(chan struct{})
				go func() {
					_, _ = io.Copy(io.Discard, enabled)
					close(ch)
				}()
				return ch
			}():
				l.Warn("gadget disabled; stopping brokers")
			case sig := <-sigCh:
				l.Info("Received shutdown signal during operation", slog.String("signal", sig.String()))
				_ = notifier.Stopping()
			}
			cancel()
			_ = enabled.Close()
		}()

		err = runBrokers(ctx, fs, kr, l)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				l.Info("Brokers stopped (context canceled)")
			} else if isFatalBrokerError(err) {
				l.Error("Fatal broker error, restarting with backoff", slog.Any("err", err))
				time.Sleep(fatalErrorBackoff)
			} else {
				l.Warn("Transient broker error, retrying", slog.Any("err", err))
			}
			continue
		}
	}
}
