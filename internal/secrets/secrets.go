// Package secrets resolves secret references used in vigilante.yaml:
//
//	vault:<mount>/<path>#<key>   HashiCorp Vault KV v2 (e.g. vault:secret/prod/f5#password)
//	env:NAME                     environment variable
//	file:/path                   file content (e.g. a mounted Kubernetes Secret), trailing newline trimmed
//
// Values are cached in memory for cache_ttl and never written anywhere.
// Every resolved value is remembered so RedactHandler can mask it in logs.
// The package also signs short-lived SSH certificates with Vault's SSH CA.
package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"vigilante/internal/config"
)

// Resolver turns references into values.
type Resolver struct {
	vault *vaultClient
	ttl   time.Duration
	Now   func() time.Time

	mu    sync.Mutex
	cache map[string]cached
	known map[string]bool // values to redact
}

type cached struct {
	value   string
	expires time.Time
}

// New builds a resolver; Vault is contacted lazily on first use.
func New(cfg config.Secrets) (*Resolver, error) {
	r := &Resolver{ttl: cfg.CacheTTL, Now: time.Now, cache: map[string]cached{}, known: map[string]bool{}}
	if r.ttl == 0 {
		r.ttl = 5 * time.Minute
	}
	if cfg.Vault != nil {
		v, err := newVault(*cfg.Vault)
		if err != nil {
			return nil, err
		}
		r.vault = v
	}
	return r, nil
}

var (
	gmu    sync.RWMutex
	global = &Resolver{ttl: 5 * time.Minute, Now: time.Now, cache: map[string]cached{}, known: map[string]bool{}}
)

// Configure installs the process-wide resolver (called once after loading config).
func Configure(cfg config.Secrets) error {
	r, err := New(cfg)
	if err != nil {
		return err
	}
	gmu.Lock()
	global = r
	gmu.Unlock()
	return nil
}

// Default returns the process-wide resolver.
func Default() *Resolver {
	gmu.RLock()
	defer gmu.RUnlock()
	return global
}

// Resolve resolves a reference with the process-wide resolver.
func Resolve(ctx context.Context, ref string) (string, error) { return Default().Resolve(ctx, ref) }

// Value returns the secret named by ref, else the env var, else "".
// It is how credentials read username/password/token fields.
func Value(ctx context.Context, ref, env string) (string, error) {
	if ref != "" {
		return Resolve(ctx, ref)
	}
	if env != "" {
		v := os.Getenv(env)
		Default().remember(v)
		return v, nil
	}
	return "", nil
}

// ErrNotFound means the reference resolved to nothing.
var ErrNotFound = errors.New("secret not found")

func (r *Resolver) Resolve(ctx context.Context, ref string) (string, error) {
	r.mu.Lock()
	if c, ok := r.cache[ref]; ok && r.Now().Before(c.expires) {
		r.mu.Unlock()
		return c.value, nil
	}
	r.mu.Unlock()
	v, err := r.fetch(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("secret %s: %w", ref, err)
	}
	r.mu.Lock()
	r.cache[ref] = cached{value: v, expires: r.Now().Add(r.ttl)}
	r.mu.Unlock()
	r.remember(v)
	return v, nil
}

func (r *Resolver) fetch(ctx context.Context, ref string) (string, error) {
	switch {
	case strings.HasPrefix(ref, "env:"):
		v, ok := os.LookupEnv(strings.TrimPrefix(ref, "env:"))
		if !ok || v == "" {
			return "", ErrNotFound
		}
		return v, nil
	case strings.HasPrefix(ref, "file:"):
		b, err := os.ReadFile(strings.TrimPrefix(ref, "file:"))
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	case strings.HasPrefix(ref, "vault:"):
		if r.vault == nil {
			return "", errors.New("no secrets.vault configured")
		}
		path, key, _ := strings.Cut(strings.TrimPrefix(ref, "vault:"), "#")
		mount, sub, _ := strings.Cut(path, "/")
		return r.vault.kv(ctx, mount, sub, key)
	}
	return "", fmt.Errorf("unsupported reference (use vault:, env: or file:)")
}

func (r *Resolver) remember(v string) {
	if len(v) < 6 { // too short to redact without mangling ordinary text
		return
	}
	r.mu.Lock()
	r.known[v] = true
	r.mu.Unlock()
}

// Redact masks every secret value this resolver has handed out.
func (r *Resolver) Redact(s string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for v := range r.known {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
	}
	return s
}

// RedactHandler masks resolved secret values in log messages and attributes.
type RedactHandler struct{ slog.Handler }

func (h RedactHandler) Handle(ctx context.Context, rec slog.Record) error {
	r := Default()
	out := slog.NewRecord(rec.Time, rec.Level, r.Redact(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(r, a))
		return true
	})
	return h.Handler.Handle(ctx, out)
}

func (h RedactHandler) WithAttrs(as []slog.Attr) slog.Handler {
	r := Default()
	red := make([]slog.Attr, len(as))
	for i, a := range as {
		red[i] = redactAttr(r, a)
	}
	return RedactHandler{h.Handler.WithAttrs(red)}
}

func (h RedactHandler) WithGroup(name string) slog.Handler {
	return RedactHandler{h.Handler.WithGroup(name)}
}

func redactAttr(r *Resolver, a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, r.Redact(a.Value.String()))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, r.Redact(err.Error()))
		}
	}
	return a
}
