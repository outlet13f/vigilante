package api

import (
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"vigilante/internal/config"
)

var formHdr = map[string]string{"Content-Type": "application/x-www-form-urlencoded"}

func basicHdr(id, secret string) map[string]string {
	return map[string]string{"Content-Type": "application/x-www-form-urlencoded",
		"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(id+":"+secret))}
}

func TestV2OAuthClientsAndScopes(t *testing.T) {
	deployerTok, deployer := sa(t, "ci", "deployer", "*")
	_, base := newV2Server(t, "auth:\n  service_accounts:\n"+deployer)
	admin := &v2Client{t: t, base: base, token: "tok"}
	anon := &v2Client{t: t, base: base}

	// Only admins manage clients.
	wantProblem(t, (&v2Client{t: t, base: base, token: deployerTok}).do("GET", "/v2/api-clients", "", nil), 403, "forbidden")

	r := admin.do("POST", "/v2/api-clients", `{"name":"deploy-console","type":"oauth","description":"internal deploy portal",
		"scopes":["deployments:read","deployments:write"],"grants":["deployer@team=payments"]}`, nil)
	if r.StatusCode != 201 || !strings.HasPrefix(r.JSON["secret"].(string), "vcs_") || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("create oauth client: %d %s", r.StatusCode, r.Body)
	}
	client := r.JSON["client"].(map[string]any)
	id, secret := client["id"].(string), r.JSON["secret"].(string)
	if client["status"] != "active" || client["secret_hint"] != secret[:8] {
		t.Fatalf("client view: %s", r.Body)
	}
	wantProblem(t, admin.do("POST", "/v2/api-clients", `{"name":"deploy-console","type":"oauth","scopes":["deployments:read"],"grants":["viewer@*"]}`, nil), 409, "conflict")
	wantProblem(t, admin.do("POST", "/v2/api-clients", `{"name":"x","type":"oauth","scopes":["everything"],"grants":["viewer@*"]}`, nil), 422, "validation_failed")
	wantProblem(t, admin.do("POST", "/v2/api-clients", `{"name":"x","type":"oauth","scopes":["deployments:read"],"grants":["boss@*"]}`, nil), 422, "validation_failed")
	wantProblem(t, admin.do("POST", "/v2/api-clients", `{"name":"x","type":"oauth","scopes":["deployments:read"],"grants":["viewer@service=nope"]}`, nil), 422, "validation_failed")

	// Token endpoint (RFC 6749 errors).
	if r := anon.do("POST", "/v2/oauth/token", "grant_type=client_credentials", basicHdr(id, "vcs_wrong")); r.StatusCode != 401 || r.JSON["error"] != "invalid_client" {
		t.Fatalf("wrong secret: %d %s", r.StatusCode, r.Body)
	}
	if r := anon.do("POST", "/v2/oauth/token", "grant_type=password", basicHdr(id, secret)); r.StatusCode != 400 || r.JSON["error"] != "unsupported_grant_type" {
		t.Fatalf("grant type: %d %s", r.StatusCode, r.Body)
	}
	if r := anon.do("POST", "/v2/oauth/token", "grant_type=client_credentials&scope=circuit:admin", basicHdr(id, secret)); r.StatusCode != 400 || r.JSON["error"] != "invalid_scope" {
		t.Fatalf("scope beyond the client: %d %s", r.StatusCode, r.Body)
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {id}, "client_secret": {secret}, "scope": {"deployments:read"}}
	r = anon.do("POST", "/v2/oauth/token", form.Encode(), formHdr)
	if r.StatusCode != 200 || r.JSON["token_type"] != "Bearer" || r.JSON["scope"] != "deployments:read" || r.JSON["expires_in"].(float64) < 3500 {
		t.Fatalf("token: %d %s", r.StatusCode, r.Body)
	}
	readOnly := &v2Client{t: t, base: base, token: r.JSON["access_token"].(string)}
	if !strings.HasPrefix(readOnly.token, "vat_") {
		t.Fatalf("token prefix: %s", readOnly.token)
	}
	if r := readOnly.do("GET", "/v2/me", "", nil); r.JSON["kind"] != "client" || r.JSON["id"] != "client:deploy-console" || len(r.JSON["scopes"].([]any)) != 1 {
		t.Fatalf("me as client: %s", r.Body)
	}
	admin.do("PUT", "/v2/services/svc/last-good", `{"version":"v1"}`, nil)
	// The role allows deploying, but the token's scope does not.
	wantProblem(t, readOnly.do("POST", "/v2/deployments", `{"service":"svc","version":"v2"}`, nil), 403, "forbidden")
	if r := readOnly.do("GET", "/v2/services", "", nil); len(r.JSON["items"].([]any)) != 1 {
		t.Fatalf("grant scope limits reads to team payments: %s", r.Body)
	}

	r = anon.do("POST", "/v2/oauth/token", "grant_type=client_credentials", basicHdr(id, secret))
	full := &v2Client{t: t, base: base, token: r.JSON["access_token"].(string)}
	if r := full.do("POST", "/v2/deployments", `{"id":"c1","service":"svc","version":"v2"}`, nil); r.StatusCode != 201 || r.JSON["created_by"] != "client:deploy-console" {
		t.Fatalf("client creates a deployment: %d %s", r.StatusCode, r.Body)
	}
	admin.do("PUT", "/v2/services/zeta/last-good", `{"version":"z1"}`, nil)
	wantProblem(t, full.do("POST", "/v2/deployments", `{"service":"zeta","version":"z2"}`, nil), 403, "forbidden")
	wantProblem(t, full.do("GET", "/v2/audit-events", "", nil), 403, "forbidden")
	wantProblem(t, full.do("POST", "/v2/deployments/c1/rollbacks", `{}`, nil), 403, "forbidden")

	// Narrowing the client's scopes narrows tokens already issued.
	if r := admin.do("PATCH", "/v2/api-clients/"+id, `{"scopes":["deployments:read"]}`, nil); r.StatusCode != 200 || len(r.JSON["scopes"].([]any)) != 1 {
		t.Fatalf("patch: %d %s", r.StatusCode, r.Body)
	}
	wantProblem(t, full.do("POST", "/v2/deployments", `{"id":"c2","service":"svc","version":"v3"}`, nil), 403, "forbidden")
	wantProblem(t, admin.do("PATCH", "/v2/api-clients/"+id, `{"name":"renamed"}`, nil), 422, "validation_failed")

	// An API key with audit:read only.
	r = admin.do("POST", "/v2/api-clients", `{"name":"siem-pull","type":"api_key","scopes":["audit:read"],"grants":["viewer@*"]}`, nil)
	key, keyID := r.JSON["secret"].(string), r.JSON["client"].(map[string]any)["id"].(string)
	if !strings.HasPrefix(key, "vgk_") {
		t.Fatalf("api key: %s", r.Body)
	}
	siem := &v2Client{t: t, base: base, token: key}
	if r := siem.do("GET", "/v2/audit-events?limit=500", "", nil); r.StatusCode != 200 {
		t.Fatalf("audit with audit:read: %d %s", r.StatusCode, r.Body)
	}
	wantProblem(t, siem.do("GET", "/v2/circuit", "", nil), 403, "forbidden")
	wantProblem(t, siem.do("GET", "/v2/deployments/c1", "", nil), 403, "forbidden")
	if r := admin.do("GET", "/v2/api-clients/"+keyID, "", nil); r.JSON["last_used_at"] == nil || r.JSON["secret_sha256"] != nil {
		t.Fatalf("last used recorded, hash never shown: %s", r.Body)
	}

	// Rotation invalidates the old secret and the tokens issued with it.
	r = admin.do("POST", "/v2/api-clients/"+id+"/secret", "", nil)
	newSecret := r.JSON["secret"].(string)
	if r.StatusCode != 200 || newSecret == secret || r.JSON["client"].(map[string]any)["rotated_at"] == nil {
		t.Fatalf("rotate: %d %s", r.StatusCode, r.Body)
	}
	wantProblem(t, readOnly.do("GET", "/v2/deployments", "", nil), 401, "unauthenticated")
	if r := anon.do("POST", "/v2/oauth/token", "grant_type=client_credentials", basicHdr(id, secret)); r.StatusCode != 401 {
		t.Fatalf("old secret after rotation: %d", r.StatusCode)
	}
	if r := anon.do("POST", "/v2/oauth/token", "grant_type=client_credentials", basicHdr(id, newSecret)); r.StatusCode != 200 {
		t.Fatalf("new secret: %d %s", r.StatusCode, r.Body)
	}

	// Revocation.
	if r := admin.do("DELETE", "/v2/api-clients/"+keyID, "", nil); r.StatusCode != 200 || r.JSON["status"] != "revoked" {
		t.Fatalf("revoke: %d %s", r.StatusCode, r.Body)
	}
	wantProblem(t, siem.do("GET", "/v2/audit-events", "", nil), 401, "unauthenticated")
	wantProblem(t, admin.do("POST", "/v2/api-clients/"+keyID+"/secret", "", nil), 409, "conflict")
	wantProblem(t, admin.do("GET", "/v2/api-clients/nope", "", nil), 404, "not_found")
	if r := admin.do("GET", "/v2/api-clients?limit=1", "", nil); len(r.JSON["items"].([]any)) != 1 || r.JSON["next_cursor"] == nil {
		t.Fatalf("client paging: %s", r.Body)
	}

	// The audit trail names the clients.
	r = admin.do("GET", "/v2/audit-events?limit=500", "", nil)
	var trail []string
	for _, it := range r.JSON["items"].([]any) {
		m := it.(map[string]any)
		a, _ := m["action"].(string)
		who, _ := m["actor"].(string)
		trail = append(trail, who+" "+a)
	}
	joined := strings.Join(trail, "\n")
	for _, want := range []string{"token:legacy api-client.create", "client:deploy-console oauth.token", "client:deploy-console deployment.create",
		"token:legacy api-client.update", "token:legacy api-client.rotate", "token:legacy api-client.revoke", "client-id:" + id + " denied"} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit trail lacks %q", want)
		}
	}
}

func TestV2RateLimits(t *testing.T) {
	opTok, op := sa(t, "oncall", "operator", "*")
	s, base := newV2Server(t, "api:\n  rate_limit: {rate: 0.5, burst: 3}\n  emergency_rate_limit: {rate: 0.5, burst: 2}\nauth:\n  service_accounts:\n"+op)
	c := &v2Client{t: t, base: base, token: opTok}
	admin := &v2Client{t: t, base: base, token: "tok"}
	admin.do("PUT", "/v2/services/svc/last-good", `{"version":"v1"}`, nil)
	admin.do("POST", "/v2/deployments", `{"id":"r1","service":"svc","version":"v2"}`, nil)

	for i := 0; i < 3; i++ {
		if r := c.do("GET", "/v2/deployments", "", nil); r.StatusCode != 200 || r.Header.Get("RateLimit-Limit") != "3" {
			t.Fatalf("call %d: %d %v", i, r.StatusCode, r.Header)
		}
	}
	r := c.do("GET", "/v2/deployments", "", nil)
	wantProblem(t, r, 429, "rate_limited")
	if ra, _ := strconv.Atoi(r.Header.Get("Retry-After")); ra < 1 || r.Header.Get("RateLimit-Remaining") != "0" {
		t.Fatalf("429 headers: %v", r.Header)
	}
	// Reads are exhausted, but the emergency bucket still takes a rollback.
	if r := c.do("POST", "/v2/deployments/r1/rollbacks", `{"reason":"page from on-call"}`, nil); r.StatusCode != 202 {
		t.Fatalf("emergency call blocked by read flood: %d %s", r.StatusCode, r.Body)
	}
	// The break-glass legacy token is never limited.
	for i := 0; i < 5; i++ {
		if r := admin.do("GET", "/v2/deployments", "", nil); r.StatusCode != 200 {
			t.Fatalf("legacy token limited: %d", r.StatusCode)
		}
	}

	// A client's own daily quota.
	r = admin.do("POST", "/v2/api-clients", `{"name":"batch","type":"api_key","scopes":["deployments:read"],"grants":["viewer@*"],"rate_limit":{"rate":0,"burst":0,"daily":2}}`, nil)
	batch := &v2Client{t: t, base: base, token: r.JSON["secret"].(string)}
	batch.do("GET", "/v2/services", "", nil)
	batch.do("GET", "/v2/services", "", nil)
	r = batch.do("GET", "/v2/services", "", nil)
	wantProblem(t, r, 429, "rate_limited")
	if !strings.Contains(r.JSON["detail"].(string), "daily quota") {
		t.Fatalf("daily quota detail: %s", r.Body)
	}

	// Buckets refill with time.
	now := time.Now()
	s.limits.now = func() time.Time { return now.Add(10 * time.Second) }
	if r := c.do("GET", "/v2/deployments", "", nil); r.StatusCode != 200 {
		t.Fatalf("bucket did not refill: %d", r.StatusCode)
	}
}

func TestRateLimitConfigValidation(t *testing.T) {
	base := `
version: v1
targets: [{name: a}]
executors: {x: {type: exec, exec: {rollback: "true"}}}
services:
  - name: s
    targets: [a]
    probes: [{id: h, type: tcp, tcp: {address: "x:1"}}]
    rules: [{name: down, when: {metric: h.up, op: "==", value: 0}}]
    rollback: {executor: x}
`
	for tail, want := range map[string]string{
		"api: {rate_limit: {rate: 5, burst: 0}}\n": "burst >= 1",
		"api: {token_ttl: 48h}\n":                  "token_ttl",
	} {
		if _, err := config.Parse([]byte(base + tail)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", tail, err)
		}
	}
	c, err := config.Parse([]byte(base))
	if err != nil || c.API.RateLimit.Rate != 20 || c.API.EmergencyRateLimit.Burst != 10 || c.API.TokenTTL != time.Hour {
		t.Fatalf("defaults: %v %+v", err, c.API)
	}
}
