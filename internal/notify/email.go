package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/secrets"
)

// email sends a plain-text mail over SMTP (STARTTLS by default, or implicit
// TLS on port 465), with optional PLAIN authentication.
func (n *Notifier) email(ctx context.Context, t config.Notifier, m *Message) error {
	s := t.SMTP
	if s == nil || s.Host == "" || len(s.To) == 0 {
		return fmt.Errorf("email: smtp.host and smtp.to required")
	}
	port := s.Port
	if port == 0 {
		port = 587
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(port))
	tlsCfg := &tls.Config{ServerName: s.Host, InsecureSkipVerify: s.TLSSkipVerify, MinVersion: tls.VersionTLS12} //nolint:gosec // opt-in
	d := net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if s.ImplicitTLS || port == 465 {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, tlsCfg)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("email: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("email: %w", err)
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok && !(s.ImplicitTLS || port == 465) && !s.NoStartTLS {
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("email starttls: %w", err)
		}
	}
	if s.Username != "" || s.PasswordRef != "" {
		pass, err := secrets.Value(ctx, s.PasswordRef, s.PasswordEnv)
		if err != nil {
			return err
		}
		if err := c.Auth(smtp.PlainAuth("", s.Username, pass, s.Host)); err != nil {
			return fmt.Errorf("email auth: %w", err)
		}
	}
	if err := c.Mail(s.From); err != nil {
		return fmt.Errorf("email MAIL FROM: %w", err)
	}
	for _, to := range s.To {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("email RCPT TO %s: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(mailBody(s, m)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func mailBody(s *config.SMTP, m *Message) []byte {
	var b bytes.Buffer
	prefix := map[Level]string{Info: "[vigilante]", Warning: "[vigilante WARNING]", Critical: "[vigilante CRITICAL]"}[m.Level]
	fmt.Fprintf(&b, "From: %s\r\n", s.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(s.To, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", prefix+" "+m.Title))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(strings.ReplaceAll(m.Text, "\n", "\r\n"))
	if d := m.Deployment; d != nil {
		fmt.Fprintf(&b, "\r\n\r\nService: %s\r\nDeployment: %s\r\nVersion: %s -> %s\r\nState: %s\r\n", d.Service, d.ID, d.PreviousVersion, d.Version, d.State)
	}
	return b.Bytes()
}
