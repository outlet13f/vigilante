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

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"

	"vigilante/internal/config"
)

// Manager hands out Runners per target and pools SSH connections so that a
// hundred probes on one host share a single TCP/SSH session.
type Manager struct {
	cfg *config.Config
	mu  sync.Mutex
	ssh map[string]*sshConn
}

func NewManager(cfg *config.Config) *Manager {
	return &Manager{cfg: cfg, ssh: map[string]*sshConn{}}
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
		}
	}
	m.mu.Unlock()

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
	m.mu.Lock()
	if existing, ok := m.ssh[t.Name]; ok { // lost a race; keep the first
		m.mu.Unlock()
		cl.Close()
		return existing.client, nil
	}
	m.ssh[t.Name] = &sshConn{client: cl}
	m.mu.Unlock()
	return cl, nil
}

func (m *Manager) clientConfig(t *config.Target) (*ssh.ClientConfig, error) {
	cred := m.cfg.Credentials[t.Connection.Credential]
	var auths []ssh.AuthMethod
	if cred.PrivateKeyFile != "" {
		key, err := os.ReadFile(expandHome(cred.PrivateKeyFile))
		if err != nil {
			return nil, fmt.Errorf("read private key: %w", err)
		}
		var signer ssh.Signer
		if cred.PassphraseEnv != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(key, []byte(os.Getenv(cred.PassphraseEnv)))
		} else {
			signer, err = ssh.ParsePrivateKey(key)
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
	if cred.PasswordEnv != "" {
		auths = append(auths, ssh.Password(os.Getenv(cred.PasswordEnv)))
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

func (s *SSH) session(ctx context.Context) (*ssh.Session, error) {
	cl, err := s.m.client(ctx, s.target)
	if err != nil {
		return nil, err
	}
	return cl.NewSession()
}

func (s *SSH) Run(ctx context.Context, cmd string, stdin io.Reader) (string, error) {
	sess, err := s.session(ctx)
	if err != nil {
		return "", err
	}
	defer sess.Close()
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
	sess, err := s.session(ctx)
	if err != nil {
		return err
	}
	defer sess.Close()
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
