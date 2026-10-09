package probe

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/tmpl"
)

func init() {
	Register("http", newHTTP)
	Register("tcp", newTCP)
}

// HTTP probe metrics: up, latency_ms, status, consecutive_failures,
// consecutive_timeouts, timeout.
type httpProbe struct {
	spec    config.Probe
	url     string
	client  *http.Client
	bodyRe  *regexp.Regexp
	headers map[string]string
	fc      failureCounter
}

func newHTTP(spec config.Probe, env Env) (Probe, error) {
	h := spec.HTTP
	url, err := tmpl.Render(h.URL, env.Data)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	for k, v := range h.Headers {
		if headers[k], err = tmpl.Render(v, env.Data); err != nil {
			return nil, err
		}
	}
	p := &httpProbe{spec: spec, url: url, headers: headers}
	if h.BodyRegex != "" {
		p.bodyRe = regexp.MustCompile(h.BodyRegex)
	}
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: spec.Timeout}).DialContext,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: h.TLSSkipVerify}, //nolint:gosec // opt-in per probe
		MaxIdleConnsPerHost: 2,
	}
	p.client = &http.Client{Transport: tr, Timeout: spec.Timeout}
	return p, nil
}

func (p *httpProbe) Run(ctx context.Context, emit Emit) error {
	return poll(ctx, p.spec.Interval, func(ctx context.Context) {
		start := time.Now()
		status, err := p.do(ctx)
		lat := time.Since(start)
		if status > 0 {
			emit("status", float64(status))
			emit("latency_ms", ms(lat))
		}
		p.fc.observe(emit, err == nil, err != nil && isTimeout(err))
	})
}

func (p *httpProbe) Check(ctx context.Context) error {
	_, err := p.do(ctx)
	return err
}

func (p *httpProbe) do(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, p.spec.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, p.spec.HTTP.Method, p.url, nil)
	if err != nil {
		return 0, err
	}
	for k, v := range p.headers {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	expect := p.spec.HTTP.ExpectStatus
	if len(expect) == 0 {
		if resp.StatusCode >= 400 {
			return resp.StatusCode, fmt.Errorf("status %d", resp.StatusCode)
		}
	} else if !slices.Contains(expect, resp.StatusCode) {
		return resp.StatusCode, fmt.Errorf("status %d not in %v", resp.StatusCode, expect)
	}
	if p.bodyRe != nil && !p.bodyRe.Match(body) {
		return resp.StatusCode, fmt.Errorf("body does not match %q", p.bodyRe)
	}
	if p.spec.HTTP.JSONPath != "" {
		got, err := jsonLookup(body, p.spec.HTTP.JSONPath)
		if err != nil {
			return resp.StatusCode, err
		}
		if want := p.spec.HTTP.JSONExpect; want != "" && got != want {
			return resp.StatusCode, fmt.Errorf("%s = %q, want %q", p.spec.HTTP.JSONPath, got, want)
		}
	}
	return resp.StatusCode, nil
}

// jsonLookup resolves a dotted path ("components.db.status") in a JSON document.
func jsonLookup(body []byte, path string) (string, error) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("json: %w", err)
	}
	for _, key := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return "", fmt.Errorf("json path %q: %q is not an object", path, key)
		}
		if v, ok = m[key]; !ok {
			return "", fmt.Errorf("json path %q: key %q missing", path, key)
		}
	}
	return fmt.Sprint(v), nil
}

// TCP probe metrics: up, latency_ms (connect time), consecutive_failures, consecutive_timeouts.
type tcpProbe struct {
	spec config.Probe
	addr string
	fc   failureCounter
}

func newTCP(spec config.Probe, env Env) (Probe, error) {
	addr, err := tmpl.Render(spec.TCP.Address, env.Data)
	if err != nil {
		return nil, err
	}
	return &tcpProbe{spec: spec, addr: addr}, nil
}

func (p *tcpProbe) Run(ctx context.Context, emit Emit) error {
	return poll(ctx, p.spec.Interval, func(ctx context.Context) {
		start := time.Now()
		err := p.Check(ctx)
		if err == nil {
			emit("latency_ms", ms(time.Since(start)))
		}
		p.fc.observe(emit, err == nil, err != nil && isTimeout(err))
	})
}

func (p *tcpProbe) Check(ctx context.Context) error {
	d := net.Dialer{Timeout: p.spec.Timeout}
	c, err := d.DialContext(ctx, "tcp", p.addr)
	if err != nil {
		return err
	}
	return c.Close()
}
