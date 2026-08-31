// Package ssh is the BMC's SSH server. The image ships no sshd: the transport
// is golang.org/x/crypto/ssh and sessions run on the shared PTY plumbing in
// pkg/shell — the same code behind the web terminal drawer, so a
// shell is a shell whether it arrived over SSH or over the browser. File
// transfer is served in-process too (see sftp.go), so `scp` and `sftp` work
// without an sftp-server binary the image does not have.
//
// Authentication reuses the BMC's own credentials: client public keys from
// authorized_keys (validated with the same rules as jetkvm-community/kvm's
// internal/utils/ssh.go) and, optionally, the account password that Redfish,
// IPMI and HTTP Basic already accept.
package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/pi-bmc/nanokvm-app/pkg/auth"
	"github.com/pi-bmc/nanokvm-app/pkg/config"
	"github.com/pi-bmc/nanokvm-app/pkg/shell"
)

const (
	// handshakeTimeout bounds the pre-authentication phase so a client that
	// opens a socket and never speaks cannot hold a slot forever.
	handshakeTimeout = 30 * time.Second
	// maxAuthTries per connection before the server drops it.
	maxAuthTries = 6
	// maxPendingHandshakes caps connections in the pre-auth phase. Each
	// handshake costs real crypto on the single C906 core, and the busiest
	// moments are exactly when that core is already saturated (the managed
	// Pi booting streams its 513 MB image through the USB gadget). Beyond
	// the cap, connections are closed immediately — a fast, retryable
	// refusal instead of a pile of crawling handshakes that deepen the
	// starvation. Established sessions are never affected.
	maxPendingHandshakes = 4
	// serverVersion is the SSH identification string. Must start with
	// "SSH-2.0-" per RFC 4253.
	serverVersion = "SSH-2.0-NanoKVM"
)

// Server is a running SSH listener.
type Server struct {
	ln  net.Listener
	cfg *ssh.ServerConfig

	stop chan struct{}
	wg   sync.WaitGroup

	// handshakes is a semaphore bounding concurrent pre-auth handshakes
	// (see maxPendingHandshakes).
	handshakes chan struct{}

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

var (
	mu      sync.Mutex
	current *Server
)

// Start launches the SSH server unless it is disabled in the config or already
// running. Safe to call repeatedly; only the first call binds.
func Start() error {
	mu.Lock()
	defer mu.Unlock()

	if current != nil {
		return nil
	}

	conf := config.GetInstance().SSH
	if !conf.Enabled {
		slog.Info("ssh: disabled by configuration")
		return nil
	}

	s, err := newServer(conf)
	if err != nil {
		return err
	}
	current = s
	return nil
}

// Stop shuts the SSH server down and waits for its goroutines. Live sessions
// are closed. No-op when the server is not running.
func Stop() {
	mu.Lock()
	s := current
	current = nil
	mu.Unlock()

	if s != nil {
		s.shutdown()
	}
}

// IsRunning reports whether the listener is up.
func IsRunning() bool {
	mu.Lock()
	defer mu.Unlock()
	return current != nil
}

// Addr is the listener's bound address, empty when the server is not running.
// Useful when the configured port is 0 (tests) and for diagnostics.
func Addr() string {
	mu.Lock()
	defer mu.Unlock()
	if current == nil {
		return ""
	}
	return current.ln.Addr().String()
}

// Restart applies a configuration change (enable/disable, port) by tearing the
// listener down and bringing it back per the current config.
func Restart() error {
	Stop()
	return Start()
}

func newServer(conf config.SSH) (*Server, error) {
	signer, err := loadOrCreateHostKey(conf.HostKeyPath)
	if err != nil {
		return nil, err
	}

	s := &Server{
		stop:       make(chan struct{}),
		handshakes: make(chan struct{}, maxPendingHandshakes),
		conns:      make(map[net.Conn]struct{}),
	}

	s.cfg = &ssh.ServerConfig{
		ServerVersion:     serverVersion,
		MaxAuthTries:      maxAuthTries,
		PublicKeyCallback: s.authPublicKey,
	}
	if conf.PasswordAuth {
		s.cfg.PasswordCallback = s.authPassword
	}
	s.cfg.AddHostKey(signer)

	addr := fmt.Sprintf(":%d", conf.Port)
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("ssh listen on %s: %w", addr, err)
	}
	s.ln = ln

	s.wg.Add(1)
	go s.accept()

	slog.Info("ssh: server started",
		slog.Int("port", conf.Port),
		slog.Bool("passwordAuth", conf.PasswordAuth),
		slog.String("hostKey", ssh.FingerprintSHA256(signer.PublicKey())))
	return s, nil
}

func (s *Server) shutdown() {
	close(s.stop)
	_ = s.ln.Close()

	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()

	s.wg.Wait()
	slog.Info("ssh: server stopped")
}

func (s *Server) accept() {
	defer s.wg.Done()

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.stop:
				return
			default:
			}
			// A transient accept error (fd pressure) should not kill the
			// listener; back off briefly and keep serving.
			slog.Warn("ssh: accept failed", slog.Any("err", err))
			select {
			case <-s.stop:
				return
			case <-time.After(time.Second):
				continue
			}
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) handleConn(nConn net.Conn) {
	s.track(nConn, true)
	defer func() {
		s.track(nConn, false)
		_ = nConn.Close()
	}()

	// Admission control before any crypto: past the cap, refuse instantly
	// rather than crawl (see maxPendingHandshakes).
	select {
	case s.handshakes <- struct{}{}:
	default:
		slog.Warn("ssh: refusing connection, handshakes already in flight",
			slog.String("addr", nConn.RemoteAddr().String()),
			slog.Int("handshakes", maxPendingHandshakes))
		return
	}

	// Bound the handshake, then clear the deadline so an idle interactive
	// session is never torn down mid-use.
	_ = nConn.SetDeadline(time.Now().Add(handshakeTimeout))

	conn, chans, reqs, err := ssh.NewServerConn(nConn, s.cfg)
	<-s.handshakes
	if err != nil {
		// Failed handshakes are routine (port scanners, key probes); debug.
		slog.Debug("ssh: handshake failed", slog.String("addr", nConn.RemoteAddr().String()), slog.Any("err", err))
		return
	}
	defer func() {
		_ = conn.Close()
	}()

	_ = nConn.SetDeadline(time.Time{})
	slog.Info("ssh: client authenticated", slog.String("user", conn.User()), slog.String("addr", conn.RemoteAddr().String()))

	go ssh.DiscardRequests(reqs)

	var sessions sync.WaitGroup
	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}

		ch, chReqs, err := newChan.Accept()
		if err != nil {
			slog.Warn("ssh: accept channel failed", slog.Any("err", err))
			continue
		}

		sessions.Add(1)
		go func() {
			defer sessions.Done()
			handleSession(ch, chReqs)
		}()
	}
	sessions.Wait()

	slog.Info("ssh: client disconnected", slog.String("user", conn.User()), slog.String("addr", conn.RemoteAddr().String()))
}

func (s *Server) track(c net.Conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		s.conns[c] = struct{}{}
	} else {
		delete(s.conns, c)
	}
}

// ── Authentication ────────────────────────────────────────────────────────

// allowedUser reports whether an SSH login name may be used. There is a single
// BMC account, so its username is accepted along with "root" — the session runs
// as root regardless, and `ssh root@bmc` is what users type out of habit.
func allowedUser(user string) bool {
	if strings.EqualFold(user, "root") {
		return true
	}
	account, err := auth.GetAccount()
	if err != nil {
		return false
	}
	return user == account.Username
}

func (s *Server) authPublicKey(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	if !allowedUser(conn.User()) {
		return nil, errors.New("unknown user")
	}

	for _, allowed := range authorizedKeys() {
		// Compare the wire encodings, as x/crypto/ssh's own server example
		// does: both sides are public data, so an exact match is the whole
		// requirement.
		if bytes.Equal(allowed.Marshal(), key.Marshal()) {
			slog.Info("ssh: user authenticated with key", slog.String("user", conn.User()), slog.String("fingerprint", ssh.FingerprintSHA256(key)))
			return &ssh.Permissions{}, nil
		}
	}

	return nil, errors.New("unauthorized key")
}

func (s *Server) authPassword(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
	clientIP := hostOf(conn.RemoteAddr())

	// Share the web login's lockout state so an attacker cannot sidestep it by
	// switching from HTTP to SSH.
	if locked, _, msg := auth.CheckLoginAttempt(clientIP); locked {
		slog.Warn("ssh: login rejected", slog.String("ip", clientIP), slog.String("reason", msg))
		return nil, errors.New("too many failed attempts")
	}

	account, err := auth.GetAccount()
	if err != nil {
		return nil, errors.New("authentication unavailable")
	}
	if !allowedUser(conn.User()) || !auth.ComparePlainAccount(account.Username, string(password)) {
		auth.RecordLoginFailure(clientIP)
		return nil, errors.New("invalid credentials")
	}

	auth.ClearLoginAttempt(clientIP)
	return &ssh.Permissions{}, nil
}

func hostOf(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

// ── Sessions ──────────────────────────────────────────────────────────────

type ptyRequestMsg struct {
	Term    string
	Columns uint32
	Rows    uint32
	Width   uint32
	Height  uint32
	Modes   string
}

type windowChangeMsg struct {
	Columns uint32
	Rows    uint32
	Width   uint32
	Height  uint32
}

type execMsg struct {
	Command string
}

type envMsg struct {
	Name  string
	Value string
}

type signalMsg struct {
	Signal string
}

type subsystemMsg struct {
	Name string
}

type exitStatusMsg struct {
	Status uint32
}

// handleSession services one session channel: it collects the pty-req/env
// requests, then starts a shell (or a command) on the first shell/exec request
// and pipes the channel to it until the process exits.
func handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	var (
		opts    shell.Options
		session *shell.Session
		started bool
	)

	defer func() {
		if session != nil {
			session.Close()
		}
		_ = ch.Close()
	}()

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			if started {
				replyRequest(req, false)
				continue
			}
			var pty ptyRequestMsg
			if err := ssh.Unmarshal(req.Payload, &pty); err != nil {
				replyRequest(req, false)
				continue
			}
			opts.Term = pty.Term
			opts.Cols = clampDimension(pty.Columns)
			opts.Rows = clampDimension(pty.Rows)
			replyRequest(req, true)

		case "env":
			var env envMsg
			if err := ssh.Unmarshal(req.Payload, &env); err == nil && acceptEnv(env.Name) {
				opts.Env = append(opts.Env, env.Name+"="+env.Value)
			}
			replyRequest(req, true)

		case "window-change":
			var wc windowChangeMsg
			if err := ssh.Unmarshal(req.Payload, &wc); err == nil && session != nil {
				session.Resize(clampDimension(wc.Columns), clampDimension(wc.Rows))
			}
			replyRequest(req, false)

		case "shell", "exec":
			if started {
				replyRequest(req, false)
				continue
			}
			if req.Type == "exec" {
				var exec execMsg
				if err := ssh.Unmarshal(req.Payload, &exec); err != nil {
					replyRequest(req, false)
					continue
				}
				opts.Command = exec.Command
			}
			// No pty-req means the client wants raw pipes (`ssh bmc uptime`,
			// or anything piping data), not a terminal.
			opts.NoPTY = opts.Term == ""

			s, err := shell.Start(opts)
			if err != nil {
				slog.Error("ssh: start session failed", slog.Any("err", err))
				replyRequest(req, false)
				return
			}
			session = s
			started = true
			replyRequest(req, true)

			slog.Debug("ssh: session started", slog.Int("pid", session.Pid()), slog.Bool("pty", session.HasPTY()))
			// Run the session inline: this goroutine owns the channel, and the
			// request loop keeps serving window-change/signal from the client
			// only after the pipes are wired, so pump in the background.
			go pump(ch, session)

		case "signal":
			var sig signalMsg
			if err := ssh.Unmarshal(req.Payload, &sig); err == nil && session != nil {
				if s, ok := signalNames[sig.Signal]; ok {
					session.Signal(s)
				}
			}
			replyRequest(req, false)

		case "subsystem":
			if started {
				replyRequest(req, false)
				continue
			}
			var sub subsystemMsg
			if err := ssh.Unmarshal(req.Payload, &sub); err != nil || sub.Name != "sftp" {
				// SFTP is the only subsystem served; anything else (and a
				// malformed request) is refused.
				replyRequest(req, false)
				continue
			}
			started = true
			replyRequest(req, true)
			// serveSFTP owns the channel from here, like pump does for a shell.
			go serveSFTP(ch)

		default:
			replyRequest(req, false)
		}
	}

	// The client closed the channel's request stream; if a process is still
	// running the deferred Close reaps it.
}

// pump wires the SSH channel to the session and reports the exit status when
// the process finishes.
func pump(ch ssh.Channel, session *shell.Session) {
	var out sync.WaitGroup

	go func() {
		_, _ = io.Copy(session, ch)
		// EOF on the client's stdin: a piped command needs to see it to
		// finish. A PTY session ignores this (the master stays open).
		session.CloseStdin()
	}()

	out.Add(1)
	go func() {
		defer out.Done()
		_, _ = io.Copy(ch, session)
	}()

	if stderr := session.Stderr(); stderr != nil {
		out.Add(1)
		go func() {
			defer out.Done()
			_, _ = io.Copy(ch.Stderr(), stderr)
		}()
	}

	out.Wait()

	code, _ := session.Wait()
	exitSession(ch, code)
}

// exitSession reports a session's exit status to the client and closes the
// channel — the last thing every session does, whether it ran a shell or the
// SFTP subsystem.
func exitSession(ch ssh.Channel, code int) {
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(exitStatusMsg{Status: uint32(code)})) //nolint:gosec // exit codes are 0-255
	_ = ch.CloseWrite()
	_ = ch.Close()
}

// clampDimension narrows a client-supplied terminal dimension to what the
// winsize ioctl can carry. The wire format is 32-bit and the client is not
// trusted, so a bogus 70000-column pty-req must saturate rather than wrap
// around to a tiny value.
func clampDimension(v uint32) uint16 {
	const maxDimension = 1 << 14 // 16384 — far beyond any real terminal
	if v > maxDimension {
		return maxDimension
	}
	return uint16(v)
}

func replyRequest(req *ssh.Request, ok bool) {
	if req.WantReply {
		_ = req.Reply(ok, nil)
	}
}

// signalNames maps the RFC 4254 signal names a client may send to the numbers
// the session forwards to its process group. Restricted to the signals that
// make sense for an interactive session — no SIGKILL by request; a client that
// wants the session gone closes the channel.
var signalNames = map[string]syscall.Signal{
	"ABRT": syscall.SIGABRT,
	"ALRM": syscall.SIGALRM,
	"HUP":  syscall.SIGHUP,
	"INT":  syscall.SIGINT,
	"QUIT": syscall.SIGQUIT,
	"TERM": syscall.SIGTERM,
	"USR1": syscall.SIGUSR1,
	"USR2": syscall.SIGUSR2,
}

// acceptEnv mirrors sshd's default AcceptEnv policy: the client may set only
// locale variables (TERM comes from pty-req, not env). Letting a client set
// arbitrary variables in a root shell — PATH, LD_PRELOAD — is not something
// the BMC needs.
func acceptEnv(name string) bool {
	return name == "LANG" || strings.HasPrefix(name, "LC_")
}
