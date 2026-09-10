package devtools

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"golang.org/x/crypto/ssh"

	"github.com/0x7d8/wings/config"
)

// SSHSessionRequest carries optional port hints from Atlas when creating SSH.
// An empty request uses the node config pool (or random bind if that pool is empty).
type SSHSessionRequest struct {
	// PreferredPort is tried first when valid (typically Atlas array_rand pick).
	PreferredPort int
	// Ports is a request-scoped pool (already merged from ports/ssh_ports/port_pool/preferred_ports).
	Ports []int
}

// SSHCredentials is returned to Atlas when an ephemeral SSH session is created.
type SSHCredentials struct {
	Port               int    `json:"port"`
	Username           string `json:"username"`
	Password           string `json:"password"`
	ExpiresAt          string `json:"expires_at"`
	IdleTimeoutSeconds int64  `json:"idle_timeout_seconds"`
	SessionTTLSeconds  int64  `json:"session_ttl_seconds"`
}

// SSHInfo is the non-secret view embedded in status (password included while live so the panel can refresh).
type SSHInfo struct {
	Active             bool   `json:"active"`
	Port               int    `json:"port,omitempty"`
	Username           string `json:"username,omitempty"`
	Password           string `json:"password,omitempty"`
	ExpiresAt          string `json:"expires_at,omitempty"`
	IdleTimeoutSeconds int64  `json:"idle_timeout_seconds,omitempty"`
	SessionTTLSeconds  int64  `json:"session_ttl_seconds,omitempty"`
}

type sshGateway struct {
	mu sync.Mutex

	mgr *Manager

	listener net.Listener
	config   *ssh.ServerConfig

	username string
	password string
	port     int
	expires  time.Time
	ttlSecs  int64
	idleSecs int64

	cancel context.CancelFunc
	closed atomic.Bool

	activeConns int32
}

// StartSSHSession ensures the sidecar is running and opens an ephemeral SSH listener
// with a random username/password. Replaces any prior session.
//
// Binding: request pool (if non-empty) else node config SSHPortPool(); empty pool
// falls back to a kernel-assigned high port. A non-empty pool never falls back to random.
func (m *Manager) StartSSHSession(ctx context.Context, req SSHSessionRequest) (*SSHCredentials, error) {
	if err := m.EnsureRunning(ctx); err != nil {
		return nil, err
	}

	cfg := m.cfg()
	ttl := cfg.SessionTTL
	if ttl <= 0 {
		ttl = 3600
	}
	idle := cfg.IdleTimeout
	if idle <= 0 {
		idle = 900
	}
	bind := cfg.SSHBind
	if bind == "" {
		bind = "0.0.0.0"
	}

	username, err := randomToken("dt", 8)
	if err != nil {
		return nil, err
	}
	password, err := randomPassword(24)
	if err != nil {
		return nil, err
	}

	hostKey, err := generateHostKey()
	if err != nil {
		return nil, err
	}

	pool := config.SanitizeSSHPorts(req.Ports)
	if len(pool) == 0 {
		pool = cfg.SSHPortPool()
	}
	ln, port, err := listenEphemeralSSH(bind, req.PreferredPort, pool)
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(time.Duration(ttl) * time.Second)

	gwCtx, cancel := context.WithCancel(context.Background())
	gw := &sshGateway{
		mgr:      m,
		listener: ln,
		username: username,
		password: password,
		port:     port,
		expires:  expires,
		ttlSecs:  ttl,
		idleSecs: idle,
		cancel:   cancel,
	}

	serverCfg := &ssh.ServerConfig{
		NoClientAuth: false,
		MaxAuthTries: 4,
		PasswordCallback: func(conn ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if time.Now().After(gw.expires) {
				return nil, fmt.Errorf("session expired")
			}
			userOK := subtle.ConstantTimeCompare([]byte(conn.User()), []byte(gw.username)) == 1
			passOK := subtle.ConstantTimeCompare(pass, []byte(gw.password)) == 1
			if userOK && passOK {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("invalid credentials")
		},
	}
	serverCfg.AddHostKey(hostKey)
	gw.config = serverCfg

	m.mu.Lock()
	if m.ssh != nil {
		old := m.ssh
		m.ssh = nil
		m.mu.Unlock()
		old.close()
		m.mu.Lock()
	}
	m.ssh = gw
	m.armIdleLocked()
	m.mu.Unlock()

	go gw.serve(gwCtx)
	go gw.watchExpiry(gwCtx)

	m.log.WithFields(log.Fields{
		"port":     port,
		"username": username,
		"expires":  expires.UTC().Format(time.RFC3339),
	}).Info("devtools ephemeral SSH session started")

	return &SSHCredentials{
		Port:               port,
		Username:           username,
		Password:           password,
		ExpiresAt:          expires.UTC().Format(time.RFC3339),
		IdleTimeoutSeconds: idle,
		SessionTTLSeconds:  ttl,
	}, nil
}

// SSHStatus returns the current ephemeral SSH session info, if any.
func (m *Manager) SSHStatus() *SSHInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ssh == nil || m.ssh.closed.Load() {
		return &SSHInfo{Active: false}
	}
	return &SSHInfo{
		Active:             true,
		Port:               m.ssh.port,
		Username:           m.ssh.username,
		Password:           m.ssh.password,
		ExpiresAt:          m.ssh.expires.UTC().Format(time.RFC3339),
		IdleTimeoutSeconds: m.ssh.idleSecs,
		SessionTTLSeconds:  m.ssh.ttlSecs,
	}
}

// StopSSHSession tears down the ephemeral SSH listener.
func (m *Manager) StopSSHSession() {
	m.mu.Lock()
	gw := m.ssh
	m.ssh = nil
	m.mu.Unlock()
	if gw != nil {
		gw.close()
	}
}

func (gw *sshGateway) close() {
	if !gw.closed.CompareAndSwap(false, true) {
		return
	}
	if gw.cancel != nil {
		gw.cancel()
	}
	if gw.listener != nil {
		_ = gw.listener.Close()
	}
}

func (gw *sshGateway) watchExpiry(ctx context.Context) {
	timer := time.NewTimer(time.Until(gw.expires))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
		gw.mgr.log.Info("devtools SSH session TTL reached; closing listener")
		gw.mgr.StopSSHSession()
	}
}

func (gw *sshGateway) serve(ctx context.Context) {
	defer gw.close()
	for {
		conn, err := gw.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				if gw.closed.Load() {
					return
				}
				gw.mgr.log.WithField("error", err).Debug("devtools SSH accept error")
				return
			}
		}
		go gw.handleConn(ctx, conn)
	}
}

func (gw *sshGateway) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	if time.Now().After(gw.expires) {
		return
	}

	sconn, chans, reqs, err := ssh.NewServerConn(conn, gw.config)
	if err != nil {
		gw.mgr.log.WithField("error", err).Debug("devtools SSH handshake failed")
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)

	atomic.AddInt32(&gw.activeConns, 1)
	gw.mgr.NoteAttach()
	defer func() {
		atomic.AddInt32(&gw.activeConns, -1)
		gw.mgr.NoteDetach()
	}()

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only session channels are allowed")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go gw.handleSession(ctx, channel, requests)
	}
}

func (gw *sshGateway) handleSession(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()

	var (
		ptyCols uint32 = 80
		ptyRows uint32 = 24
		gotPty  bool
	)

	for req := range requests {
		switch req.Type {
		case "pty-req":
			gotPty = true
			if len(req.Payload) >= 8 {
				// Skip term string; parse dims from ssh.ParseTerminalModes style payload is awkward.
				// Payload: term string + 4x uint32 (w, h, pixw, pixh) + modes.
				_, rest, ok := decodeSSHString(req.Payload)
				if ok && len(rest) >= 8 {
					ptyCols = uint32(rest[0])<<24 | uint32(rest[1])<<16 | uint32(rest[2])<<8 | uint32(rest[3])
					ptyRows = uint32(rest[4])<<24 | uint32(rest[5])<<16 | uint32(rest[6])<<8 | uint32(rest[7])
				}
			}
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		case "window-change":
			if len(req.Payload) >= 8 {
				ptyCols = uint32(req.Payload[0])<<24 | uint32(req.Payload[1])<<16 | uint32(req.Payload[2])<<8 | uint32(req.Payload[3])
				ptyRows = uint32(req.Payload[4])<<24 | uint32(req.Payload[5])<<16 | uint32(req.Payload[6])<<8 | uint32(req.Payload[7])
			}
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		case "shell", "exec":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
			if !gotPty {
				ptyCols, ptyRows = 120, 40
			}
			gw.runShell(ctx, channel, ptyCols, ptyRows)
			return
		case "env", "signal":
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func (gw *sshGateway) runShell(ctx context.Context, channel ssh.Channel, cols, rows uint32) {
	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sess, err := gw.mgr.AttachPTY(sessCtx)
	if err != nil {
		_, _ = io.WriteString(channel.Stderr(), "devtools: "+err.Error()+"\r\n")
		return
	}
	defer sess.Close()

	_ = sess.Resize(sessCtx, uint(cols), uint(rows))

	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(sess.Stdin(), channel)
		errCh <- err
	}()
	go func() {
		_, err := io.Copy(channel, sess.Stdout())
		errCh <- err
	}()

	select {
	case <-sessCtx.Done():
	case <-errCh:
	}
}

func decodeSSHString(b []byte) (string, []byte, bool) {
	if len(b) < 4 {
		return "", nil, false
	}
	n := int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if n < 0 || len(b) < 4+n {
		return "", nil, false
	}
	return string(b[4 : 4+n]), b[4+n:], true
}

func generateHostKey() (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, errors.Wrap(err, "devtools: host key generate failed")
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, errors.Wrap(err, "devtools: host key signer failed")
	}
	return signer, nil
}

func randomToken(prefix string, nbytes int) (string, error) {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

func randomPassword(n int) (string, error) {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789!@#$%^&*"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(out), nil
}

// listenEphemeralSSH binds an SSH listener. When pool is empty, uses a random high port.
// When pool is non-empty, tries preferred (if valid) then shuffled remaining pool ports;
// never falls back to a random port outside the pool.
func listenEphemeralSSH(bind string, preferred int, pool []int) (net.Listener, int, error) {
	candidates := buildSSHPortCandidates(preferred, pool)
	if len(candidates) == 0 {
		ln, err := net.Listen("tcp", net.JoinHostPort(bind, "0"))
		if err != nil {
			return nil, 0, errors.Wrap(err, "devtools: failed to bind ephemeral SSH port")
		}
		return ln, ln.Addr().(*net.TCPAddr).Port, nil
	}

	var lastErr error
	for _, p := range candidates {
		ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(p)))
		if err == nil {
			return ln, p, nil
		}
		lastErr = err
	}
	return nil, 0, errors.WithMessage(lastErr, "devtools: SSH port pool exhausted; all configured ports are in use")
}

// buildSSHPortCandidates returns preferred first (when valid), then the remaining
// unique valid pool ports in shuffled order.
func buildSSHPortCandidates(preferred int, pool []int) []int {
	pool = config.SanitizeSSHPorts(pool)
	seen := make(map[int]struct{}, len(pool)+1)
	out := make([]int, 0, len(pool)+1)

	if config.IsValidDevToolsSSHPort(preferred) {
		seen[preferred] = struct{}{}
		out = append(out, preferred)
	}

	rest := make([]int, 0, len(pool))
	for _, p := range pool {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		rest = append(rest, p)
	}
	mrand.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	return append(out, rest...)
}
