package audit

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/journal"
)

// testPKI is a CA plus a collector certificate (for siem.example.test only,
// so the exporter must send server_name) and a client certificate.
type testPKI struct {
	dir                 string
	caFile              string
	server              tls.Certificate
	pool                *x509.CertPool
	clientCert, clientK string
}

func newTestPKI(t *testing.T) *testPKI {
	t.Helper()
	dir := t.TempDir()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	p := &testPKI{dir: dir, caFile: filepath.Join(dir, "ca.pem"), pool: x509.NewCertPool()}
	p.pool.AddCert(ca)
	os.WriteFile(p.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600)

	issue := func(name string, serial int64, usage x509.ExtKeyUsage, dns []string) (certPEM, keyPEM []byte) {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, DNSNames: dns,
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		kb, _ := x509.MarshalECPrivateKey(key)
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	}
	sc, sk := issue("siem", 2, x509.ExtKeyUsageServerAuth, []string{"siem.example.test"})
	if p.server, err = tls.X509KeyPair(sc, sk); err != nil {
		t.Fatal(err)
	}
	cc, ck := issue("vigilante", 3, x509.ExtKeyUsageClientAuth, nil)
	p.clientCert, p.clientK = filepath.Join(dir, "client.crt"), filepath.Join(dir, "client.key")
	os.WriteFile(p.clientCert, cc, 0o600)
	os.WriteFile(p.clientK, ck, 0o600)
	return p
}

// collector is a TLS syslog receiver (tls.Listen with our certificate;
// httptest's StartTLS would substitute its own).
type collector struct {
	addr   string
	frames chan string
	peers  chan string // client certificate CN ("" without one)
	errs   chan error  // handshake failures
}

func listenCollector(t *testing.T, tc *tls.Config) *collector {
	t.Helper()
	l, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	c := &collector{addr: l.Addr().String(), frames: make(chan string, 10), peers: make(chan string, 10), errs: make(chan error, 10)}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(conn *tls.Conn) {
				defer conn.Close()
				if err := conn.Handshake(); err != nil {
					c.errs <- err
					return
				}
				cn := ""
				if pc := conn.ConnectionState().PeerCertificates; len(pc) > 0 {
					cn = pc[0].Subject.CommonName
				}
				c.peers <- cn
				// RFC 5425: MSG-LEN SP SYSLOG-MSG, back to back.
				rd := bufio.NewReader(conn)
				for {
					n, err := rd.ReadString(' ')
					if err != nil {
						return
					}
					size, err := strconv.Atoi(strings.TrimSuffix(n, " "))
					if err != nil {
						c.frames <- "bad frame length " + n
						return
					}
					msg := make([]byte, size)
					if _, err := io.ReadFull(rd, msg); err != nil {
						return
					}
					c.frames <- string(msg)
				}
			}(conn.(*tls.Conn))
		}
	}()
	return c
}

func runExporter(t *testing.T, cfg config.SyslogExport) *Exporter {
	t.Helper()
	x, err := NewExporter(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go x.Run(ctx)
	return x
}

func TestSyslogExporterTLS(t *testing.T) {
	p := newTestPKI(t)
	col := listenCollector(t, &tls.Config{Certificates: []tls.Certificate{p.server}, ClientCAs: p.pool,
		ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12})
	x := runExporter(t, config.SyslogExport{Address: "tls://" + col.addr, TLS: &config.SyslogTLS{
		CAFile: p.caFile, CertFile: p.clientCert, KeyFile: p.clientK, ServerName: "siem.example.test"}})

	x.Send(journal.Entry{Kind: journal.KindAudit, Time: time.Now(), Actor: "user:bob", Action: "denied", Reason: "multi\nline reason"})
	x.Send(journal.Entry{Kind: journal.KindAudit, Time: time.Now(), Actor: "user:ann", Action: "rollback.manual", Reason: "공지 없는 배포"})
	select {
	case cn := <-col.peers:
		if cn != "vigilante" {
			t.Errorf("client certificate: %q", cn)
		}
	case err := <-col.errs:
		t.Fatalf("handshake: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no connection")
	}
	var got []string
	for len(got) < 2 {
		select {
		case f := <-col.frames:
			got = append(got, f)
		case <-time.After(5 * time.Second):
			t.Fatalf("received %d frames: %v", len(got), got)
		}
	}
	if !strings.HasPrefix(got[0], "<108>1 ") || !strings.Contains(got[0], `actor="user:bob"`) || strings.HasSuffix(got[0], "\n") {
		t.Errorf("first frame: %q", got[0])
	}
	// Octet counting counts bytes: the Korean reason must arrive whole.
	if !strings.Contains(got[1], `actor="user:ann"`) || !strings.Contains(got[1], `"reason":"공지 없는 배포"`) || !strings.HasSuffix(got[1], "}") {
		t.Errorf("second frame: %q", got[1])
	}
	if x.Sent.Load() != 2 {
		t.Errorf("sent %d", x.Sent.Load())
	}
}

func TestSyslogExporterTLSRejectsUntrustedCollector(t *testing.T) {
	p := newTestPKI(t)
	col := listenCollector(t, &tls.Config{Certificates: []tls.Certificate{p.server}, MinVersion: tls.VersionTLS12})
	// No ca_file: the system roots do not trust the test CA.
	x := runExporter(t, config.SyslogExport{Address: "tls://" + col.addr, TLS: &config.SyslogTLS{ServerName: "siem.example.test"}})
	x.Send(journal.Entry{Kind: journal.KindAudit, Time: time.Now(), Actor: "user:bob", Action: "denied"})
	select {
	case err := <-col.errs:
		if err == nil {
			t.Fatal("handshake succeeded")
		}
	case cn := <-col.peers:
		t.Fatalf("exporter connected to an untrusted collector (client %q)", cn)
	case <-time.After(5 * time.Second):
		t.Fatal("exporter never tried to connect")
	}
	select {
	case f := <-col.frames:
		t.Fatalf("frame delivered without verification: %q", f)
	case <-time.After(200 * time.Millisecond):
	}
	if x.Sent.Load() != 0 {
		t.Fatalf("sent %d", x.Sent.Load())
	}

	// The certificate is for siem.example.test, not the address host.
	p2 := newTestPKI(t)
	col2 := listenCollector(t, &tls.Config{Certificates: []tls.Certificate{p2.server}, MinVersion: tls.VersionTLS12})
	x2 := runExporter(t, config.SyslogExport{Address: "tls://" + col2.addr, TLS: &config.SyslogTLS{CAFile: p2.caFile}})
	x2.Send(journal.Entry{Kind: journal.KindAudit, Time: time.Now(), Actor: "user:bob", Action: "denied"})
	select {
	case <-col2.errs:
	case cn := <-col2.peers:
		t.Fatalf("server name not checked (client %q)", cn)
	case <-time.After(5 * time.Second):
		t.Fatal("exporter never tried to connect")
	}
}

func TestNewExporterTLSConfig(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	x, err := NewExporter(config.SyslogExport{Address: "tls://siem.example.test"}, log)
	if err != nil || x.addr != "siem.example.test:6514" || x.tls.ServerName != "siem.example.test" || x.tls.MinVersion != tls.VersionTLS12 {
		t.Fatalf("defaults: %v %+v", err, x)
	}
	if string(x.frame("abc")) != "3 abc" {
		t.Errorf("tls frame %q", x.frame("abc"))
	}
	x, err = NewExporter(config.SyslogExport{Address: "tls://siem:1514", TLS: &config.SyslogTLS{MinVersion: "1.3"}}, log)
	if err != nil || x.tls.MinVersion != tls.VersionTLS13 {
		t.Fatalf("min_version: %v", err)
	}
	if x, _ := NewExporter(config.SyslogExport{Address: "udp://siem:514"}, log); string(x.frame("abc")) != "abc\n" {
		t.Errorf("udp frame %q", x.frame("abc"))
	}
	if _, err := NewExporter(config.SyslogExport{Address: "tcp://siem:514", TLS: &config.SyslogTLS{}}, log); err == nil {
		t.Error("tls block with a tcp address accepted")
	}
	if _, err := NewExporter(config.SyslogExport{Address: "tls://siem", TLS: &config.SyslogTLS{CAFile: filepath.Join(t.TempDir(), "missing.pem")}}, log); err == nil ||
		!strings.Contains(err.Error(), "audit.syslog.tls.ca_file") {
		t.Errorf("missing CA file: %v", err)
	}
}
