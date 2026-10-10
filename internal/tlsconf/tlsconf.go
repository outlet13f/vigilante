// Package tlsconf builds the TLS settings of the API server (server.tls), of
// the push agent (agent.tls) and of the SIEM exporter (audit.syslog.tls). Certificates are re-read when their files
// change, so a renewed certificate (cert-manager, an internal CA's renewal
// job) takes effect without a restart.
package tlsconf

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"vigilante/internal/config"
)

// keyPair reloads a certificate when either file's modification time changes.
type keyPair struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	modTime time.Time
	checked time.Time
}

func newKeyPair(certFile, keyFile string) (*keyPair, error) {
	kp := &keyPair{certFile: certFile, keyFile: keyFile}
	if _, err := kp.get(); err != nil {
		return nil, err
	}
	return kp, nil
}

func (kp *keyPair) get() (*tls.Certificate, error) {
	kp.mu.Lock()
	defer kp.mu.Unlock()
	// At most one stat per second, however many handshakes.
	if kp.cert != nil && time.Since(kp.checked) < time.Second {
		return kp.cert, nil
	}
	kp.checked = time.Now()
	var latest time.Time
	for _, f := range []string{kp.certFile, kp.keyFile} {
		st, err := os.Stat(f)
		if err != nil {
			if kp.cert != nil {
				return kp.cert, nil // keep serving the loaded one during a renewal
			}
			return nil, err
		}
		if st.ModTime().After(latest) {
			latest = st.ModTime()
		}
	}
	if kp.cert != nil && !latest.After(kp.modTime) {
		return kp.cert, nil
	}
	c, err := tls.LoadX509KeyPair(kp.certFile, kp.keyFile)
	if err != nil {
		if kp.cert != nil {
			return kp.cert, nil // a half-written renewal: try again next time
		}
		return nil, fmt.Errorf("load %s / %s: %w", kp.certFile, kp.keyFile, err)
	}
	kp.cert, kp.modTime = &c, latest
	return kp.cert, nil
}

func pool(file string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: no PEM certificates", file)
	}
	return p, nil
}

// Server returns the API server's TLS config, or nil for plain HTTP.
func Server(c *config.ServerTLS) (*tls.Config, error) {
	if c == nil {
		return nil, nil
	}
	kp, err := newKeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("server.tls: %w", err)
	}
	tc := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return kp.get() },
	}
	if c.MinVersion == "1.3" {
		tc.MinVersion = tls.VersionTLS13
	}
	if c.ClientCAFile != "" {
		if tc.ClientCAs, err = pool(c.ClientCAFile); err != nil {
			return nil, fmt.Errorf("server.tls.client_ca_file: %w", err)
		}
	}
	switch c.ClientAuth {
	case "", "none":
		tc.ClientAuth = tls.NoClientCert
	case "optional":
		// Certificates that are presented must be valid (agents); browsers
		// and CI jobs without one still authenticate with tokens.
		tc.ClientAuth = tls.VerifyClientCertIfGiven
	case "require":
		tc.ClientAuth = tls.RequireAndVerifyClientCert
	default:
		return nil, errors.New("server.tls.client_auth must be none, optional or require")
	}
	return tc, nil
}

// HAClient returns the TLS config followers use to forward to the leader,
// or nil to use the system defaults.
func HAClient(c *config.HATLS) (*tls.Config, error) {
	if c == nil {
		return nil, nil
	}
	tc, err := Client(&config.AgentTLS{CAFile: c.CAFile, CertFile: c.CertFile, KeyFile: c.KeyFile})
	if err != nil {
		return nil, fmt.Errorf("server.ha.tls: %w", err)
	}
	tc.ServerName = c.ServerName
	return tc, nil
}

// Client returns the agent's TLS config, or nil to use the system defaults.
func Client(c *config.AgentTLS) (*tls.Config, error) {
	if c == nil {
		return nil, nil
	}
	return clientConfig("agent.tls", c.CAFile, c.CertFile, c.KeyFile)
}

// Syslog returns the TLS config of the SIEM exporter (audit.syslog with a
// tls:// address). host is the address host, the default server name.
func Syslog(c *config.SyslogTLS, host string) (*tls.Config, error) {
	if c == nil {
		c = &config.SyslogTLS{}
	}
	tc, err := clientConfig("audit.syslog.tls", c.CAFile, c.CertFile, c.KeyFile)
	if err != nil {
		return nil, err
	}
	tc.ServerName = host
	if c.ServerName != "" {
		tc.ServerName = c.ServerName
	}
	if c.MinVersion == "1.3" {
		tc.MinVersion = tls.VersionTLS13
	}
	return tc, nil
}

// clientConfig: a private CA (empty = system roots) and an optional client
// certificate, re-read when renewed.
func clientConfig(where, caFile, certFile, keyFile string) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		p, err := pool(caFile)
		if err != nil {
			return nil, fmt.Errorf("%s.ca_file: %w", where, err)
		}
		tc.RootCAs = p
	}
	if certFile != "" {
		kp, err := newKeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		tc.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return kp.get() }
	}
	return tc, nil
}
