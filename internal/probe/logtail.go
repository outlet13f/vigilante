package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/tmpl"
	"vigilante/internal/transport"
)

func init() {
	Register("log", newLog)
	Register("access_log", newAccessLog)
}

// follow streams new lines appended to path. Locally (agent mode or local
// targets) it follows the file in-process and survives logrotate (inode change
// or truncation); remotely it runs `tail -n0 -F` over the SSH session.
func follow(ctx context.Context, env Env, path string, onLine func(string)) error {
	switch r := env.Runner.(type) {
	case nil:
		return errors.New("log probes need an ssh or local connection")
	case *transport.Local:
		return followLocal(ctx, path, onLine)
	default:
		return r.Stream(ctx, "tail -n0 -F "+transport.ShellQuote(path)+" 2>/dev/null", onLine)
	}
}

func followLocal(ctx context.Context, path string, onLine func(string)) error {
	var (
		f      *os.File
		rd     *bufio.Reader
		offset int64
		info   os.FileInfo
	)
	open := func(fromEnd bool) error {
		if f != nil {
			f.Close()
		}
		var err error
		if f, err = os.Open(path); err != nil {
			f = nil
			return err
		}
		if info, err = f.Stat(); err != nil {
			return err
		}
		offset = 0
		if fromEnd {
			if offset, err = f.Seek(0, io.SeekEnd); err != nil {
				return err
			}
		}
		rd = bufio.NewReaderSize(f, 64*1024)
		return nil
	}
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	_ = open(true) // file may not exist yet; retried below
	var partial strings.Builder
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if f != nil {
			for {
				chunk, err := rd.ReadString('\n')
				offset += int64(len(chunk))
				if strings.HasSuffix(chunk, "\n") {
					partial.WriteString(strings.TrimRight(chunk, "\r\n"))
					onLine(partial.String())
					partial.Reset()
					continue
				}
				partial.WriteString(chunk)
				if err != nil {
					break
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		// Rotation / truncation detection.
		cur, err := os.Stat(path)
		switch {
		case err != nil:
			// rotated away and not yet recreated; keep reading the old handle
		case f == nil:
			_ = open(false)
		case !os.SameFile(info, cur) || cur.Size() < offset:
			partial.Reset()
			_ = open(false)
		}
	}
}

// bucketer accumulates per-second counters and flushes them as samples, so a
// 10k lines/s log does not become 10k samples/s.
type bucketer struct {
	mu     sync.Mutex
	counts map[string]float64
	values map[string][]float64 // raw values (latency), reservoir-capped per flush
}

const reservoirCap = 256

func newBucketer() *bucketer {
	return &bucketer{counts: map[string]float64{}, values: map[string][]float64{}}
}

func (b *bucketer) inc(k string, n float64) {
	b.mu.Lock()
	b.counts[k] += n
	b.mu.Unlock()
}

func (b *bucketer) observe(k string, v float64) {
	b.mu.Lock()
	if len(b.values[k]) < reservoirCap {
		b.values[k] = append(b.values[k], v)
	}
	b.mu.Unlock()
}

func (b *bucketer) flush(emit Emit, zeroKeys []string) map[string]float64 {
	b.mu.Lock()
	counts, values := b.counts, b.values
	b.counts, b.values = map[string]float64{}, map[string][]float64{}
	b.mu.Unlock()
	for _, k := range zeroKeys {
		if _, ok := counts[k]; !ok {
			counts[k] = 0
		}
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		emit(k, counts[k])
	}
	for k, vs := range values {
		for _, v := range vs {
			emit(k, v)
		}
	}
	return counts
}

func runBucketed(ctx context.Context, env Env, path string, b *bucketer, onLine func(string), onFlush func()) error {
	errc := make(chan error, 1)
	go func() { errc <- follow(ctx, env, path, onLine) }()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			if ctx.Err() != nil {
				return nil
			}
			if err == nil {
				err = fmt.Errorf("log stream for %s ended", path)
			}
			return err
		case <-t.C:
			onFlush()
		}
	}
}

// Log probe: regex interception of application logs.
// Metrics per second: lines, match.<pattern> (count of matching lines).
type logProbe struct {
	spec     config.Probe
	env      Env
	path     string
	patterns map[string]*regexp.Regexp
	names    []string
}

func newLog(spec config.Probe, env Env) (Probe, error) {
	path, err := tmpl.Render(spec.Log.Path, env.Data)
	if err != nil {
		return nil, err
	}
	p := &logProbe{spec: spec, env: env, path: path, patterns: map[string]*regexp.Regexp{}}
	for name, re := range spec.Log.Patterns {
		p.patterns[name] = regexp.MustCompile(re)
		p.names = append(p.names, "match."+name)
	}
	sort.Strings(p.names)
	return p, nil
}

func (p *logProbe) Run(ctx context.Context, emit Emit) error {
	b := newBucketer()
	zero := append([]string{"lines"}, p.names...)
	return runBucketed(ctx, p.env, p.path, b, func(line string) {
		b.inc("lines", 1)
		for name, re := range p.patterns {
			if re.MatchString(line) {
				b.inc("match."+name, 1)
			}
		}
	}, func() { b.flush(emit, zero) })
}

// Access-log probe: computes HTTP error ratios from web server logs.
// Metrics per second: requests, count_5xx, count_4xx, error_rate_5xx (%),
// and latency_ms per request (reservoir-sampled) when request time is logged.
type accessLogProbe struct {
	spec config.Probe
	env  Env
	path string
}

// combined format, optionally followed by $request_time.
var combinedRe = regexp.MustCompile(`^\S+ \S+ \S+ \[[^\]]+\] "[^"]*" (\d{3}) \S+(?: "[^"]*" "[^"]*")?(?: (\d+(?:\.\d+)?))?`)

func newAccessLog(spec config.Probe, env Env) (Probe, error) {
	path, err := tmpl.Render(spec.AccessLog.Path, env.Data)
	if err != nil {
		return nil, err
	}
	return &accessLogProbe{spec: spec, env: env, path: path}, nil
}

// parseAccessLine returns status and latency in ms (-1 when absent).
func parseAccessLine(a *config.AccessLogProbe, line string) (status int, latencyMs float64, ok bool) {
	latencyMs = -1
	scale := 1000.0
	if a.LatencyUnit == "ms" {
		scale = 1
	}
	if a.Format == "json" {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			return 0, 0, false
		}
		status, ok = toInt(m[a.StatusField])
		if !ok {
			return 0, 0, false
		}
		if v, ok := toFloat(m[a.LatencyField]); ok {
			latencyMs = v * scale
		}
		return status, latencyMs, true
	}
	sm := combinedRe.FindStringSubmatch(line)
	if sm == nil {
		return 0, 0, false
	}
	status, _ = strconv.Atoi(sm[1])
	if sm[2] != "" {
		v, _ := strconv.ParseFloat(sm[2], 64)
		latencyMs = v * scale
	}
	return status, latencyMs, true
}

func toInt(v any) (int, bool) {
	f, ok := toFloat(v)
	return int(f), ok
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func (p *accessLogProbe) Run(ctx context.Context, emit Emit) error {
	b := newBucketer()
	return runBucketed(ctx, p.env, p.path, b, func(line string) {
		status, lat, ok := parseAccessLine(p.spec.AccessLog, line)
		if !ok {
			b.inc("unparsed", 1)
			return
		}
		b.inc("requests", 1)
		switch {
		case status >= 500:
			b.inc("count_5xx", 1)
		case status >= 400:
			b.inc("count_4xx", 1)
		}
		if lat >= 0 {
			b.observe("latency_ms", lat)
		}
	}, func() {
		c := b.flush(emit, []string{"requests", "count_5xx", "count_4xx"})
		if c["requests"] > 0 {
			emit("error_rate_5xx", 100*c["count_5xx"]/c["requests"])
		}
	})
}

// ParseAccessLine parses one access-log line as the access_log probe does;
// `vigilante doctor` uses it to check that a log's format is understood.
func ParseAccessLine(a *config.AccessLogProbe, line string) (status int, latencyMs float64, ok bool) {
	return parseAccessLine(a, line)
}
