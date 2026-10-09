package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"vigilante/internal/config"
	"vigilante/internal/secrets"
	"vigilante/internal/secrets/vaulttest"
	"vigilante/internal/telemetry"
)

// startSSHD runs an SSH server that accepts only user certificates signed by
// ca, answering every exec with "<user> ran <cmd>".
func startSSHD(t *testing.T, ca ssh.PublicKey) (host string, port int) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	hostKey, _ := ssh.NewSignerFromKey(priv)
	checker := &ssh.CertChecker{IsUserAuthority: func(k ssh.PublicKey) bool { return bytes.Equal(k.Marshal(), ca.Marshal()) }}
	cfg := &ssh.ServerConfig{PublicKeyCallback: checker.Authenticate}
	cfg.AddHostKey(hostKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSSH(c, cfg)
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

func serveSSH(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		c.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "")
			continue
		}
		ch, rq, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for req := range rq {
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				var p struct{ Cmd string }
				_ = ssh.Unmarshal(req.Payload, &p)
				_ = req.Reply(true, nil)
				fmt.Fprintf(ch, "%s ran %s", sc.User(), p.Cmd)
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ S uint32 }{0}))
				ch.Close()
				return
			}
		}()
	}
}

func caConfig(t *testing.T, principals []string) (*config.Config, *vaulttest.Server) {
	t.Helper()
	v := vaulttest.New(t)
	host, port := startSSHD(t, v.CA.PublicKey())
	t.Setenv("VGL_VAULT_TOKEN", v.RootToken)
	if err := secrets.Configure(config.Secrets{Vault: &config.Vault{Address: v.URL, TokenEnv: "VGL_VAULT_TOKEN"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secrets.Configure(config.Secrets{}) })
	cfg := &config.Config{
		Credentials: map[string]config.Credential{"ca": {Type: "ssh", User: "deploy", InsecureIgnoreKey: true,
			SSHCA: &config.SSHCA{Mount: "ssh-client-signer", Role: "ops", TTL: 10 * time.Minute, Principals: principals}}},
		Targets: []config.Target{{Name: "app-1", Address: host,
			Connection: config.Connection{Type: "ssh", Credential: "ca", Port: port, Timeout: 5 * time.Second}}},
	}
	return cfg, v
}

func TestSSHWithVaultCertificate(t *testing.T) {
	cfg, v := caConfig(t, nil)
	m := NewManager(cfg)
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r, err := m.ForTarget("app-1")
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Run(ctx, "uptime", nil)
	if err != nil || out != "deploy ran uptime" {
		t.Fatalf("run = %q, %v", out, err)
	}
	// New connection after the pool is dropped reuses the cached certificate.
	m.Close()
	if out, err := r.Run(ctx, "id", nil); err != nil || !strings.HasSuffix(out, "ran id") {
		t.Fatalf("reconnect = %q, %v", out, err)
	}
	if v.Signs != 1 {
		t.Fatalf("certificate must be cached until 80%% of its lifetime, signs=%d", v.Signs)
	}

	// Past the refresh point a new ephemeral key is signed.
	m.mu.Lock()
	m.certs["ca"].until = time.Now().Add(-time.Second)
	m.mu.Unlock()
	m.Close()
	if _, err := r.Run(ctx, "true", nil); err != nil {
		t.Fatal(err)
	}
	if v.Signs != 2 {
		t.Fatalf("expired certificate must be re-signed, signs=%d", v.Signs)
	}
	if telemetry.SSHDials.Value("ok") < 3 || telemetry.SSHSessions.Value() != 0 || telemetry.SSHConnections.Value() != 1 {
		t.Fatalf("ssh telemetry: dials=%v sessions=%v conns=%v", telemetry.SSHDials.Value("ok"), telemetry.SSHSessions.Value(), telemetry.SSHConnections.Value())
	}
	m.Close()
	if telemetry.SSHConnections.Value() != 0 {
		t.Fatalf("closed pool still counted: %v", telemetry.SSHConnections.Value())
	}
}

func TestSSHCertificateWrongPrincipalRejected(t *testing.T) {
	cfg, _ := caConfig(t, []string{"root"}) // cert for root, login as deploy
	m := NewManager(cfg)
	defer m.Close()
	r, _ := m.ForTarget("app-1")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.Run(ctx, "uptime", nil); err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("server must reject a certificate without the login principal: %v", err)
	}
}
