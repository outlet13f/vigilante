package executor

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/secrets"
)

// A small OpenStack client on the standard library: Keystone v3 tokens,
// the service catalog, and JSON calls to Nova, Cinder, Glance and Octavia.
// It covers only what the openstack executor and octavia controller need.

// openstackHTTP overrides the HTTP client (tests).
var openstackHTTP *http.Client

var (
	osMu      sync.Mutex
	osClients = map[string]*osClient{}
)

type osEndpoint struct {
	Interface string `json:"interface"`
	Region    string `json:"region"`
	RegionID  string `json:"region_id"`
	URL       string `json:"url"`
}

type osService struct {
	Type      string       `json:"type"`
	Endpoints []osEndpoint `json:"endpoints"`
}

type osClient struct {
	name string
	cred config.Credential
	http *http.Client

	mu        sync.Mutex
	token     string
	expires   time.Time
	projectID string
	catalog   []osService
}

// OpenStackError is a non-2xx answer from an OpenStack service.
type OpenStackError struct {
	Status int
	Method string
	URL    string
	Body   string
}

func (e *OpenStackError) Error() string {
	return fmt.Sprintf("openstack %s %s: %d %s", e.Method, e.URL, e.Status, e.Body)
}

// openstackClient returns the shared client for a credential.
func openstackClient(name string, creds map[string]config.Credential) (*osClient, error) {
	cred, ok := creds[name]
	if !ok {
		return nil, fmt.Errorf("unknown credential %q", name)
	}
	if cred.Type != "openstack" {
		return nil, fmt.Errorf("credential %q is not of type openstack", name)
	}
	key := name + "|" + cred.AuthURL + "|" + cred.ApplicationCredentialID + "|" + cred.User + "|" + cred.ProjectID + cred.ProjectName
	osMu.Lock()
	defer osMu.Unlock()
	if c, ok := osClients[key]; ok {
		return c, nil
	}
	hc := openstackHTTP
	if hc == nil {
		tlsCfg := &tls.Config{InsecureSkipVerify: cred.TLSSkipVerify, MinVersion: tls.VersionTLS12} //nolint:gosec // opt-in
		if cred.CACert != "" {
			pem, err := os.ReadFile(cred.CACert)
			if err != nil {
				return nil, fmt.Errorf("credential %s cacert: %w", name, err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("credential %s cacert: no certificates found", name)
			}
			tlsCfg.RootCAs = pool
		}
		hc = &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment}, Timeout: 60 * time.Second}
	}
	c := &osClient{name: name, cred: cred, http: hc}
	osClients[key] = c
	return c, nil
}

func (c *osClient) authBody(ctx context.Context) (map[string]any, error) {
	cr := c.cred
	if cr.ApplicationCredentialID != "" {
		secret, err := secrets.Resolve(ctx, cr.ApplicationCredentialSecretRef)
		if err != nil {
			return nil, err
		}
		return map[string]any{"auth": map[string]any{"identity": map[string]any{
			"methods":                []string{"application_credential"},
			"application_credential": map[string]string{"id": cr.ApplicationCredentialID, "secret": secret},
		}}}, nil
	}
	pass, err := secrets.Value(ctx, cr.PasswordRef, cr.PasswordEnv)
	if err != nil {
		return nil, err
	}
	dom := func(s string) string {
		if s == "" {
			return "Default"
		}
		return s
	}
	project := map[string]any{"id": cr.ProjectID}
	if cr.ProjectID == "" {
		project = map[string]any{"name": cr.ProjectName, "domain": map[string]string{"name": dom(cr.ProjectDomainName)}}
	}
	return map[string]any{"auth": map[string]any{
		"identity": map[string]any{"methods": []string{"password"}, "password": map[string]any{
			"user": map[string]any{"name": cr.User, "domain": map[string]string{"name": dom(cr.UserDomainName)}, "password": pass}}},
		"scope": map[string]any{"project": project},
	}}, nil
}

// login gets a token and the service catalog (caller holds c.mu).
func (c *osClient) login(ctx context.Context) error {
	body, err := c.authBody(ctx)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.cred.AuthURL, "/")+"/auth/tokens", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("keystone: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return &OpenStackError{Status: resp.StatusCode, Method: "POST", URL: "keystone /auth/tokens", Body: strings.TrimSpace(string(raw))}
	}
	var out struct {
		Token struct {
			ExpiresAt time.Time   `json:"expires_at"`
			Catalog   []osService `json:"catalog"`
			Project   struct {
				ID string `json:"id"`
			} `json:"project"`
		} `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("keystone: unreadable token: %w", err)
	}
	tok := resp.Header.Get("X-Subject-Token")
	if tok == "" {
		return errors.New("keystone: no X-Subject-Token in the response")
	}
	c.token, c.expires, c.catalog, c.projectID = tok, out.Token.ExpiresAt, out.Token.Catalog, out.Token.Project.ID
	return nil
}

func (c *osClient) session(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if force || c.token == "" || time.Now().After(c.expires.Add(-5*time.Minute)) {
		if err := c.login(ctx); err != nil {
			return "", err
		}
	}
	return c.token, nil
}

// endpoint picks a catalog URL for the first service type present,
// honouring the credential's region and interface.
func (c *osClient) endpoint(ctx context.Context, types ...string) (string, error) {
	if _, err := c.session(ctx, false); err != nil {
		return "", err
	}
	iface := c.cred.Interface
	if iface == "" {
		iface = "public"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range types {
		for _, s := range c.catalog {
			if s.Type != t {
				continue
			}
			for _, e := range s.Endpoints {
				if e.Interface == iface && (c.cred.Region == "" || e.Region == c.cred.Region || e.RegionID == c.cred.Region) {
					return strings.TrimSuffix(e.URL, "/"), nil
				}
			}
		}
	}
	return "", fmt.Errorf("no %s endpoint for %s interface in region %q in the service catalog", strings.Join(types, "/"), iface, c.cred.Region)
}

// Service types as registered in the catalog.
var (
	svcCompute = []string{"compute"}
	svcVolume  = []string{"volumev3", "block-storage", "volume"}
	svcImage   = []string{"image"}
	svcLB      = []string{"load-balancer"}
)

// call sends a JSON request to a service; a 401 re-authenticates once.
// hdr carries microversion headers. It returns the response headers.
func (c *osClient) call(ctx context.Context, svc []string, method, path string, hdr map[string]string, body, out any) (http.Header, error) {
	base, err := c.endpoint(ctx, svc...)
	if err != nil {
		return nil, err
	}
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.session(ctx, attempt > 0)
		if err != nil {
			return nil, err
		}
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Auth-Token", tok)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			continue
		}
		if resp.StatusCode >= 300 {
			return resp.Header, &OpenStackError{Status: resp.StatusCode, Method: method, URL: path, Body: strings.TrimSpace(string(raw))}
		}
		if out != nil && len(raw) > 0 {
			if err := json.Unmarshal(raw, out); err != nil {
				return resp.Header, fmt.Errorf("openstack %s %s: unreadable response: %w", method, path, err)
			}
		}
		return resp.Header, nil
	}
}

// isStatus reports whether err is an OpenStack answer with that status.
func isStatus(err error, code int) bool {
	var oe *OpenStackError
	return errors.As(err, &oe) && oe.Status == code
}

// poll calls check every interval until it reports done or ctx ends.
func poll(ctx context.Context, interval time.Duration, what string, check func() (bool, error)) error {
	for {
		done, err := check()
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s: %w", what, ctx.Err())
		case <-time.After(interval):
		}
	}
}

// pollInterval is how often OpenStack task states are checked.
var pollInterval = 3 * time.Second

// SetPollInterval changes the OpenStack polling interval (tests) and
// returns a function restoring the previous one.
func SetPollInterval(d time.Duration) (restore func()) {
	old := pollInterval
	pollInterval = d
	return func() { pollInterval = old }
}
