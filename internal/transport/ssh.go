package transport

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"crypto/ed25519"
	"crypto/rand"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"vigilante/internal/config"
	"vigilante/internal/secrets"
	"vigilante/internal/telemetry"
)

// Manager hands out Runners per target and pools SSH connections so that a
// hundred probes on one host share a single TCP/SSH session.
type Manager struct {
	cfg      *config.Config
	mu       sync.Mutex
	ssh      map[string]*sshConn
	limiters map[string]*sessionLimiter // per target: the SSH session budget
	certs    map[string]*caCert         // credential name -> current SSH certificate
}

// caCert is an ephemeral key with a Vault-signed certificate.
type caCert struct {
	signer ssh.Signer
	until  time.Time
}

func NewManager(cfg *config.Config) *Manager {
	return &Manager{cfg: cfg, ssh: map[string]*sshConn{}, limiters: map[string]*sessionLimiter{}}
}

// ForTarget returns the runner for a target name.
func (m *Manager) ForTarget(name string) (Runner, error) {
	t, ok := m.cfg.Target(name)
	if !ok {
		return nil, fmt.Errorf("unknown target %q", name)
	}
	switch t.Connection.Type {
	case "local":
		return &Local{Sudo: t.Connection.Sudo}, nil
	case "ssh":
		return &SSH{m: m, target: t}, nil
	}
	return nil, ErrNoRunner
}

// Close tears down pooled connections.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, c := range m.ssh {
		c.client.Close()
		delete(m.ssh, k)
		telemetry.SSHConnections.Add(-1)
	}
}

type sshConn struct {
	client *ssh.Client
}

func (m *Manager) client(ctx context.Context, t *config.Target) (*ssh.Client, error) {
	m.mu.Lock()
	if c, ok := m.ssh[t.Name]; ok {
		m.mu.Unlock()
		// Cheap liveness check; reconnect on failure.
		if _, _, err := c.client.SendRequest("keepalive@vigilante", true, nil); err == nil {
			return c.client, nil
		}
		m.mu.Lock()
		if cur, ok := m.ssh[t.Name]; ok && cur == c {
			c.client.Close()
			delete(m.ssh, t.Name)
			telemetry.SSHConnections.Add(-1)
		}
	}
	m.mu.Unlock()

	cl, err := m.dial(ctx, t)
	if err != nil {
		telemetry.SSHDials.Inc("error")
		return nil, err
	}
	telemetry.SSHDials.Inc("ok")
	m.mu.Lock()
	if existing, ok := m.ssh[t.Name]; ok { // lost a race; keep the first
		m.mu.Unlock()
		cl.Close()
		return existing.client, nil
	}
	m.ssh[t.Name] = &sshConn{client: cl}
	m.mu.Unlock()
	telemetry.SSHConnections.Add(1)
	return cl, nil
}

func (m *Manager) dial(ctx context.Context, t *config.Target) (*ssh.Client, error) {
	cc, err := m.clientConfig(t)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(t.Address, strconv.Itoa(t.Connection.Port))
	var cl *ssh.Client
	if t.Connection.Bastion != "" {
		bt, _ := m.cfg.Target(t.Connection.Bastion)
		jump, err := m.client(ctx, bt)
		if err != nil {
			return nil, fmt.Errorf("bastion %s: %w", bt.Name, err)
		}
		conn, err := jump.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("dial %s via bastion %s: %w", addr, bt.Name, err)
		}
		c, chans, reqs, err := ssh.NewClientConn(conn, addr, cc)
		if err != nil {
			conn.Close()
			return nil, err
		}
		cl = ssh.NewClient(c, chans, reqs)
	} else {
		d := net.Dialer{Timeout: t.Connection.Timeout}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", addr, err)
		}
		c, chans, reqs, err := ssh.NewClientConn(conn, addr, cc)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
		}
		cl = ssh.NewClient(c, chans, reqs)
	}
	return cl, nil
}

func (m *Manager) clientConfig(t *config.Target) (*ssh.ClientConfig, error) {
	cred := m.cfg.Credentials[t.Connection.Credential]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var auths []ssh.AuthMethod
	if cred.SSHCA != nil {
		signer, err := m.caSigner(ctx, t.Connection.Credential, cred)
		if err != nil {
			return nil, err
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	var keyPEM []byte
	switch {
	case cred.PrivateKeyRef != "":
		v, err := secrets.Resolve(ctx, cred.PrivateKeyRef)
		if err != nil {
			return nil, err
		}
		keyPEM = []byte(v)
	case cred.PrivateKeyFile != "":
		b, err := os.ReadFile(expandHome(cred.PrivateKeyFile))
		if err != nil {
			return nil, fmt.Errorf("read private key: %w", err)
		}
		keyPEM = b
	}
	if keyPEM != nil {
		pass, err := secrets.Value(ctx, cred.PassphraseRef, cred.PassphraseEnv)
		if err != nil {
			return nil, err
		}
		var signer ssh.Signer
		if pass != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(keyPEM, []byte(pass))
		} else {
			signer, err = ssh.ParsePrivateKey(keyPEM)
		}
		if err != nil {
			return nil, fmt.Errorf("parse private key: %w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if cred.UseAgent {
		if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
			if conn, err := net.Dial("unix", sock); err == nil {
				auths = append(auths, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
			}
		}
	}
	if cred.PasswordRef != "" || cred.PasswordEnv != "" {
		pw, err := secrets.Value(ctx, cred.PasswordRef, cred.PasswordEnv)
		if err != nil {
			return nil, err
		}
		auths = append(auths, ssh.Password(pw))
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("credential %q: no usable ssh auth method", t.Connection.Credential)
	}
	var hostKey ssh.HostKeyCallback
	switch {
	case cred.InsecureIgnoreKey:
		hostKey = ssh.InsecureIgnoreHostKey()
	default:
		path := cred.KnownHostsFile
		if path == "" {
			path = "~/.ssh/known_hosts"
		}
		cb, err := knownhosts.New(expandHome(path))
		if err != nil {
			return nil, fmt.Errorf("known_hosts: %w", err)
		}
		hostKey = cb
	}
	return &ssh.ClientConfig{User: cred.User, Auth: auths, HostKeyCallback: hostKey, Timeout: t.Connection.Timeout}, nil
}

// caSigner returns a cached certificate signer for the credential, asking
// Vault to sign a fresh ephemeral ed25519 key when the current certificate
// has used up 80% of its lifetime. No long-lived key exists on disk.
func (m *Manager) caSigner(ctx context.Context, name string, cred config.Credential) (ssh.Signer, error) {
	m.mu.Lock()
	if c, ok := m.certs[name]; ok && time.Now().Before(c.until) {
		m.mu.Unlock()
		return c.signer, nil
	}
	m.mu.Unlock()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	keySigner, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, err
	}
	cert, err := secrets.SignSSHKey(ctx, *cred.SSHCA, cred.User, keySigner.PublicKey())
	if err != nil {
		return nil, err
	}
	certSigner, err := ssh.NewCertSigner(cert, keySigner)
	if err != nil {
		return nil, err
	}
	start, end := time.Unix(int64(cert.ValidAfter), 0), time.Unix(int64(cert.ValidBefore), 0)
	if cert.ValidAfter == 0 || cert.ValidBefore == ssh.CertTimeInfinity {
		start, end = time.Now(), time.Now().Add(30*time.Minute)
	}
	until := start.Add(end.Sub(start) * 8 / 10)
	m.mu.Lock()
	if m.certs == nil {
		m.certs = map[string]*caCert{}
	}
	m.certs[name] = &caCert{signer: certSigner, until: until}
	m.mu.Unlock()
	return certSigner, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

// SSH is a Runner over a pooled SSH client.
type SSH struct {
	m      *Manager
	target *config.Target
}

func (s *SSH) String() string { return "ssh://" + s.target.Name }

func (s *SSH) wrap(cmd string) string {
	if s.target.Connection.Sudo {
		return "sudo -n sh -c " + ShellQuote(cmd)
	}
	return cmd
}

func (m *Manager) limiter(t *config.Target) *sessionLimiter {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.limiters[t.Name]
	if !ok {
		reserved := DefaultReservedSessions
		if t.Connection.ReservedSessions != nil {
			reserved = *t.Connection.ReservedSessions
		}
		l = newSessionLimiter(t.Connection.MaxSessions, reserved)
		m.limiters[t.Name] = l
	}
	return l
}

// session opens a session within the target's budget (waiting for a free
// one); the caller must call done() when finished.
func (s *SSH) session(ctx context.Context) (sess *ssh.Session, done func(), err error) {
	l := s.m.limiter(s.target)
	if err := l.acquire(ctx); err != nil {
		return nil, nil, fmt.Errorf("waiting for an SSH session on %s (%d in use): %w", s.target.Name, l.used(), err)
	}
	cl, err := s.m.client(ctx, s.target)
	if err != nil {
		l.release()
		return nil, nil, err
	}
	if sess, err = cl.NewSession(); err != nil {
		l.release()
		return nil, nil, err
	}
	telemetry.SSHSessions.Add(1)
	return sess, func() {
		sess.Close()
		telemetry.SSHSessions.Add(-1)
		l.release()
	}, nil
}

func (s *SSH) Run(ctx context.Context, cmd string, stdin io.Reader) (string, error) {
	sess, release, err := s.session(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	var out, errb strings.Builder
	sess.Stdout, sess.Stderr, sess.Stdin = &out, &errb, stdin
	done := make(chan error, 1)
	go func() { done <- sess.Run(s.wrap(cmd)) }()
	select {
	case err := <-done:
		if err != nil {
			return out.String(), &CommandError{Cmd: cmd, Stderr: errb.String(), Err: err}
		}
		return out.String(), nil
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return out.String(), ctx.Err()
	}
}

func (s *SSH) Stream(ctx context.Context, cmd string, onLine func(string)) error {
	sess, release, err := s.session(ctx)
	if err != nil {
		return err
	}
	defer release()
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return err
	}
	if err := sess.Start(s.wrap(cmd)); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = sess.Signal(ssh.SIGTERM)
		time.Sleep(200 * time.Millisecond)
		sess.Close()
	}()
	scanLines(stdout, onLine)
	err = sess.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// Dial uses SSH direct-tcpip / direct-streamlocal channels, so the remote
// docker.sock or HAProxy admin socket is reachable without exposing it.
func (s *SSH) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	cl, err := s.m.client(ctx, s.target)
	if err != nil {
		return nil, err
	}
	return cl.DialContext(ctx, network, addr)
}
