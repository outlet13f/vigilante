// Package dockerapi is a minimal Docker Engine API client (also works with the
// Podman docker-compatible socket). It speaks raw HTTP over whatever dialer it
// is given — local unix socket, TCP, or a unix socket tunnelled over SSH — so
// no Docker SDK and no agent on the host is required.
package dockerapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const apiVersion = "v1.41"

type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

type Client struct {
	http *http.Client
	base string
}

// New builds a client. socket is a unix socket path; host may instead be
// "tcp://h:p" (plain HTTP) or "http(s)://h:p" for a test server.
func New(dial DialFunc, socket, host string) *Client {
	base := "http://docker/" + apiVersion
	tr := &http.Transport{MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second}
	switch {
	case strings.HasPrefix(host, "http://"), strings.HasPrefix(host, "https://"):
		base = strings.TrimSuffix(host, "/") + "/" + apiVersion
	case strings.HasPrefix(host, "tcp://"):
		addr := strings.TrimPrefix(host, "tcp://")
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx, "tcp", addr) }
	default:
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx, "unix", socket) }
	}
	return &Client{http: &http.Client{Transport: tr}, base: base}
}

// APIError is a non-2xx response from the engine.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("docker api %d: %s", e.Status, e.Message) }

func IsNotFound(err error) bool {
	ae, ok := err.(*APIError)
	return ok && ae.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified { // start/stop on an already started/stopped container
		return nil
	}
	if resp.StatusCode >= 300 {
		var m struct{ Message string }
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(raw, &m) != nil || m.Message == "" {
			m.Message = strings.TrimSpace(string(raw))
		}
		return &APIError{Status: resp.StatusCode, Message: m.Message}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Container is the subset of /containers/{id}/json we act on. Raw keeps the
// full document so a rollback can recreate the container faithfully.
type Container struct {
	ID           string
	Name         string
	Image        string // image ID
	RestartCount int
	State        struct {
		Status    string
		Running   bool
		OOMKilled bool
		ExitCode  int
		StartedAt string
		Health    *struct{ Status string }
	}
	Config struct {
		Image  string
		Labels map[string]string
	}
	Raw map[string]any `json:"-"`
}

func (c *Client) Inspect(ctx context.Context, name string) (*Container, error) {
	var raw map[string]any
	if err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, nil, &raw); err != nil {
		return nil, err
	}
	b, _ := json.Marshal(raw)
	var ct Container
	if err := json.Unmarshal(b, &ct); err != nil {
		return nil, err
	}
	ct.Raw = raw
	return &ct, nil
}

func (c *Client) Stop(ctx context.Context, id string, timeoutSec int) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop", url.Values{"t": {fmt.Sprint(timeoutSec)}}, nil, nil)
}

func (c *Client) Start(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

func (c *Client) Rename(ctx context.Context, id, newName string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/rename", url.Values{"name": {newName}}, nil, nil)
}

func (c *Client) Remove(ctx context.Context, id string, force bool) error {
	return c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), url.Values{"force": {fmt.Sprint(force)}}, nil, nil)
}

// Create creates a container from a full create body (Config fields + HostConfig + NetworkingConfig).
func (c *Client) Create(ctx context.Context, name string, body map[string]any) (string, error) {
	var out struct{ Id string }
	if err := c.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, body, &out); err != nil {
		return "", err
	}
	return out.Id, nil
}

func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, nil)
	if IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// Pull pulls repo:tag and waits for the progress stream to finish.
func (c *Client) Pull(ctx context.Context, repo, tag string) error {
	u := c.base + "/images/create?" + url.Values{"fromImage": {repo}, "tag": {tag}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Status: resp.StatusCode, Message: string(b)}
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var m struct{ Error string }
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.Error != "" {
			return fmt.Errorf("pull %s:%s: %s", repo, tag, m.Error)
		}
	}
	return sc.Err()
}

// Event is one message of the /events stream.
type Event struct {
	Type   string
	Action string
	Actor  struct {
		ID         string
		Attributes map[string]string
	}
	Time int64
}

// Events streams engine events for one container until ctx is cancelled.
func (c *Client) Events(ctx context.Context, container string, onEvent func(Event)) error {
	filters, _ := json.Marshal(map[string][]string{"container": {container}, "type": {"container"}})
	u := c.base + "/events?" + url.Values{"filters": {string(filters)}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Message: "events"}
	}
	dec := json.NewDecoder(resp.Body)
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		onEvent(ev)
	}
}

// RepoOf strips the tag (and digest) from an image reference, keeping registry ports.
func RepoOf(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		return ref[:colon]
	}
	return ref
}

// CloneCreateBody turns an inspect document into a create body with a new image.
func CloneCreateBody(raw map[string]any, image string) map[string]any {
	body := map[string]any{}
	if cfg, ok := raw["Config"].(map[string]any); ok {
		for k, v := range cfg {
			body[k] = v
		}
	}
	body["Image"] = image
	delete(body, "Hostname") // let the engine assign one; the old value is the old container ID
	if hc, ok := raw["HostConfig"].(map[string]any); ok {
		body["HostConfig"] = hc
	}
	if ns, ok := raw["NetworkSettings"].(map[string]any); ok {
		if nets, ok := ns["Networks"].(map[string]any); ok {
			eps := map[string]any{}
			var mode string
			if hc, ok := body["HostConfig"].(map[string]any); ok {
				mode, _ = hc["NetworkMode"].(string)
			}
			for name, v := range nets {
				if mode != "" && name != mode && len(nets) > 1 {
					continue // older engines accept only one network at create time
				}
				n, _ := v.(map[string]any)
				eps[name] = map[string]any{"IPAMConfig": n["IPAMConfig"], "Links": n["Links"], "Aliases": n["Aliases"]}
			}
			body["NetworkingConfig"] = map[string]any{"EndpointsConfig": eps}
		}
	}
	return body
}
