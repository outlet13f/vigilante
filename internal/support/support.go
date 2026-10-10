// Package support builds the support bundle (`vigilante support-bundle`): a
// zip of what a support engineer needs to diagnose a problem, with secrets
// removed before anything is written.
package support

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Redacted replaces every removed value.
const Redacted = "REDACTED"

var (
	// Keys whose values are secrets. References (*_ref, *_env, *_file) and
	// settings (*_ttl) name where a secret lives and are kept.
	secretKey   = regexp.MustCompile(`(?i)(^|_)(password|passphrase|secret|token|private_key|dsn|api_key|routing_key|session_key|signing_key|key|sha256)$`)
	keptSuffix  = regexp.MustCompile(`(?i)_(ref|env|file|ttl|id|url|claim|path)$`)
	headerKey   = regexp.MustCompile(`(?i)authorization|token|key|cookie|secret|signature`)
	textSecrets = []struct {
		re   *regexp.Regexp
		repl string
	}{
		// Vigilante credentials: service-account tokens, API keys, access tokens, client secrets.
		{regexp.MustCompile(`\b(vgl|vgk|vat|vcs)_[A-Za-z0-9_\-]{8,}`), "${1}_" + Redacted},
		// JWTs (OIDC ID and access tokens).
		{regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`), Redacted},
		// Bearer / Basic credentials in headers or log lines.
		{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=\-]{8,}`), "${1} " + Redacted},
		// Passwords in URLs and DSNs: scheme://user:password@host.
		{regexp.MustCompile(`(://[^:/@\s]+):[^@\s]+@`), "${1}:" + Redacted + "@"},
		// key=value DSNs: password=... .
		{regexp.MustCompile(`(?i)\b(password|passwd|pwd)=[^\s&;]+`), "${1}=" + Redacted},
	}
)

// RedactText removes credentials from free text (logs, error messages).
func RedactText(s string) string {
	for _, t := range textSecrets {
		s = t.re.ReplaceAllString(s, t.repl)
	}
	return s
}

// RedactYAML returns a configuration file with secret values replaced,
// keeping its layout and comments, and the number of values removed.
// Removed: values of secret-named keys, alert webhook URLs (the URL is the
// credential for Slack/Teams), sensitive request headers, and credentials
// embedded in any string.
func RedactYAML(raw []byte) ([]byte, int, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, 0, err
	}
	n := 0
	var walk func(node *yaml.Node, path []string)
	walk = func(node *yaml.Node, path []string) {
		switch node.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, c := range node.Content {
				walk(c, path)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(node.Content); i += 2 {
				k, v := node.Content[i].Value, node.Content[i+1]
				if v.Kind == yaml.ScalarNode && v.Value != "" && sensitive(path, k) {
					v.Value, v.Style = Redacted, 0
					n++
					continue
				}
				walk(v, append(path, k))
			}
		case yaml.ScalarNode:
			if r := RedactText(node.Value); r != node.Value {
				node.Value = r
				n++
			}
		}
	}
	walk(&doc, nil)
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, 0, err
	}
	return b.Bytes(), n, nil
}

func sensitive(path []string, key string) bool {
	if len(path) > 0 {
		switch path[len(path)-1] {
		case "headers":
			return headerKey.MatchString(key)
		}
	}
	if len(path) > 0 && path[0] == "notify" && key == "url" {
		return true
	}
	return secretKey.MatchString(key) && !keptSuffix.MatchString(key)
}

// Bundle writes the zip.
type Bundle struct {
	zw    *zip.Writer
	files []string
	notes []string
	now   time.Time
}

func New(w io.Writer, now time.Time) *Bundle {
	return &Bundle{zw: zip.NewWriter(w), now: now}
}

// Add writes a file; text is redacted on the way in.
func (b *Bundle) Add(name string, data []byte) error {
	f, err := b.zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: b.now})
	if err != nil {
		return err
	}
	b.files = append(b.files, fmt.Sprintf("%-36s %8d bytes", name, len(data)))
	_, err = f.Write([]byte(RedactText(string(data))))
	return err
}

func (b *Bundle) AddJSON(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return b.Add(name, append(data, '\n'))
}

// Note records something that could not be collected (shown in MANIFEST.txt).
func (b *Bundle) Note(format string, args ...any) {
	b.notes = append(b.notes, RedactText(fmt.Sprintf(format, args...)))
}

// Close writes MANIFEST.txt and finishes the zip.
func (b *Bundle) Close(header string) error {
	sort.Strings(b.files)
	var m strings.Builder
	m.WriteString(header + "\n")
	m.WriteString("Collected " + b.now.UTC().Format(time.RFC3339) + ". Secrets were replaced with " + Redacted +
		" (configuration values, tokens, passwords in URLs and DSNs); review before sending.\n\nFiles:\n")
	for _, f := range b.files {
		m.WriteString("  " + f + "\n")
	}
	if len(b.notes) > 0 {
		m.WriteString("\nNot collected:\n")
		for _, n := range b.notes {
			m.WriteString("  - " + n + "\n")
		}
	}
	f, err := b.zw.CreateHeader(&zip.FileHeader{Name: "MANIFEST.txt", Method: zip.Deflate, Modified: b.now})
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(m.String())); err != nil {
		return err
	}
	return b.zw.Close()
}
