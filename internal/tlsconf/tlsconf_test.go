package tlsconf

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vigilante/internal/config"
)

type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T, name string) *ca {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &ca{cert: c, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue writes a leaf certificate and key into dir and returns their paths.
func (c *ca) issue(t *testing.T, dir, name string, serial int64, server bool) (string, string) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	if server {
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		tpl.DNSNames = []string{name + ".vigilante.svc"}
	} else {
		tpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	cf, kf := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	_ = os.WriteFile(cf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(kf, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return cf, kf
}

type server struct{ URL string }

// serve runs the server exactly as `vigilante server` does (httptest's
// StartTLS would substitute its own certificate).
func serve(t *testing.T, cfg *config.ServerTLS) *server {
	tc, err := Server(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			w.Header().Set("X-Client", r.TLS.PeerCertificates[0].Subject.CommonName)
		}
	}), ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = hs.Serve(l) }()
	t.Cleanup(func() { _ = hs.Close() })
	return &server{URL: "https://" + l.Addr().String()}
}

func client(t *testing.T, cfg *config.AgentTLS) *http.Client {
	tc, err := Client(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tc}, Timeout: 5 * time.Second}
}

func TestServerAndAgentTLS(t *testing.T) {
	dir := t.TempDir()
	root, other := newCA(t, "corp-ca"), newCA(t, "other-ca")
	caFile := filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(caFile, root.pem, 0o600)
	srvCert, srvKey := root.issue(t, dir, "server", 10, true)
	agCert, agKey := root.issue(t, dir, "agent-1", 11, false)
	badCert, badKey := other.issue(t, dir, "intruder", 12, false)

	// optional: agents present certificates (verified), others use tokens only.
	s := serve(t, &config.ServerTLS{CertFile: srvCert, KeyFile: srvKey, ClientCAFile: caFile, ClientAuth: "optional"})
	if _, err := (&http.Client{Timeout: 5 * time.Second}).Get(s.URL); err == nil {
		t.Fatal("a client without the private CA trusted the server")
	}
	r, err := client(t, &config.AgentTLS{CAFile: caFile}).Get(s.URL)
	if err != nil || r.Header.Get("X-Client") != "" {
		t.Fatalf("no client certificate under optional: %v", err)
	}
	r, err = client(t, &config.AgentTLS{CAFile: caFile, CertFile: agCert, KeyFile: agKey}).Get(s.URL)
	if err != nil || r.Header.Get("X-Client") != "agent-1" {
		t.Fatalf("agent certificate: %v %v", err, r)
	}
	if _, err := client(t, &config.AgentTLS{CAFile: caFile, CertFile: badCert, KeyFile: badKey}).Get(s.URL); err == nil {
		t.Fatal("a certificate from another CA was accepted")
	}

	// require: no certificate, no connection.
	s2 := serve(t, &config.ServerTLS{CertFile: srvCert, KeyFile: srvKey, ClientCAFile: caFile, ClientAuth: "require"})
	if _, err := client(t, &config.AgentTLS{CAFile: caFile}).Get(s2.URL); err == nil {
		t.Fatal("require accepted a client without a certificate")
	}
	if _, err := client(t, &config.AgentTLS{CAFile: caFile, CertFile: agCert, KeyFile: agKey}).Get(s2.URL); err != nil {
		t.Fatalf("require with an agent certificate: %v", err)
	}

	// TLS 1.1 is refused.
	old := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS11}}, Timeout: 5 * time.Second}
	if _, err := old.Get(s.URL); err == nil {
		t.Fatal("TLS 1.1 accepted")
	}
}

func TestServerCertificateReloads(t *testing.T) {
	dir := t.TempDir()
	root := newCA(t, "corp-ca")
	caFile := filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(caFile, root.pem, 0o600)
	cf, kf := root.issue(t, dir, "server", 100, true)
	s := serve(t, &config.ServerTLS{CertFile: cf, KeyFile: kf})
	serial := func() int64 {
		c := client(t, &config.AgentTLS{CAFile: caFile})
		r, err := c.Get(s.URL)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.TLS.PeerCertificates[0].SerialNumber.Int64()
	}
	if got := serial(); got != 100 {
		t.Fatalf("serial %d", got)
	}
	// Renewal: new files with a later modification time.
	root.issue(t, dir, "server", 101, true)
	later := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(cf, later, later)
	_ = os.Chtimes(kf, later, later)
	time.Sleep(1100 * time.Millisecond) // past the once-a-second stat
	if got := serial(); got != 101 {
		t.Fatalf("renewed certificate not picked up: serial %d", got)
	}
}

func TestServerConfigErrors(t *testing.T) {
	if _, err := Server(&config.ServerTLS{CertFile: "missing.crt", KeyFile: "missing.key"}); err == nil {
		t.Fatal("missing files accepted")
	}
	if tc, err := Server(nil); tc != nil || err != nil {
		t.Fatal("nil config must mean plain HTTP")
	}
}

// Followers forwarding to the leader by pod IP verify the certificate
// against server.ha.tls.server_name (the Service DNS name) instead.
func TestHAClientServerName(t *testing.T) {
	dir := t.TempDir()
	root := newCA(t, "corp-ca")
	caFile := filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(caFile, root.pem, 0o600)
	srvCert, srvKey := root.issue(t, dir, "leader", 20, true)
	s := serve(t, &config.ServerTLS{CertFile: srvCert, KeyFile: srvKey})
	get := func(c *config.HATLS, url string) error {
		tc, err := HAClient(c)
		if err != nil {
			return err
		}
		r, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: tc}, Timeout: 5 * time.Second}).Get(url)
		if err == nil {
			r.Body.Close()
		}
		return err
	}
	if tc, err := HAClient(nil); tc != nil || err != nil {
		t.Fatalf("nil config: %v %v", tc, err)
	}
	// "localhost" is not in the certificate: plain verification fails...
	byName := strings.Replace(s.URL, "127.0.0.1", "localhost", 1)
	if err := get(&config.HATLS{CAFile: caFile}, byName); err == nil {
		t.Fatal("host name outside the certificate accepted")
	}
	// ...and server_name makes it verify against the Service name.
	if err := get(&config.HATLS{CAFile: caFile, ServerName: "leader.vigilante.svc"}, byName); err != nil {
		t.Fatalf("server_name: %v", err)
	}
	if err := get(&config.HATLS{CAFile: caFile, ServerName: "other.vigilante.svc"}, byName); err == nil {
		t.Fatal("wrong server_name accepted")
	}
	if _, err := HAClient(&config.HATLS{CAFile: filepath.Join(dir, "missing.pem")}); err == nil || !strings.Contains(err.Error(), "server.ha.tls") {
		t.Fatalf("missing CA: %v", err)
	}
}
