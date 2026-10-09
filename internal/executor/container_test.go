package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"vigilante/internal/config"
	"vigilante/internal/dockerapi"
)

// fakeEngine is a tiny in-memory Docker Engine API.
type fakeEngine struct {
	mu         sync.Mutex
	containers map[string]map[string]any // name -> inspect doc
	calls      []string
	failStart  string // image whose container refuses to start
	images     map[string]bool
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		containers: map[string]map[string]any{
			"pay-api": {
				"Id": "old", "Name": "/pay-api", "RestartCount": 4,
				"State":           map[string]any{"Status": "running", "Running": true, "OOMKilled": true},
				"Config":          map[string]any{"Image": "registry/pay-api:v2", "Env": []any{"A=1"}, "Hostname": "old"},
				"HostConfig":      map[string]any{"NetworkMode": "pay-net", "PortBindings": map[string]any{"8080/tcp": []any{map[string]any{"HostPort": "8080"}}}},
				"NetworkSettings": map[string]any{"Networks": map[string]any{"pay-net": map[string]any{"Aliases": []any{"pay"}}}},
			},
		},
		images: map[string]bool{"registry/pay-api:v1": true},
	}
}

func (f *fakeEngine) find(id string) (string, map[string]any) {
	for n, c := range f.containers {
		if n == id || c["Id"] == id {
			return n, c
		}
	}
	return "", nil
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1.41")
	f.calls = append(f.calls, r.Method+" "+path)
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case parts[0] == "images" && r.Method == http.MethodGet:
		ref := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		if !f.images[ref] {
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"no such image"}`)
		}
	case path == "/containers/create":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		name := r.URL.Query().Get("name")
		if _, exists := f.containers[name]; exists {
			w.WriteHeader(409)
			io.WriteString(w, `{"message":"name in use"}`)
			return
		}
		f.containers[name] = map[string]any{"Id": "new", "Name": "/" + name, "State": map[string]any{"Status": "created"},
			"Config": map[string]any{"Image": body["Image"]}, "HostConfig": body["HostConfig"]}
		io.WriteString(w, `{"Id":"new"}`)
	case parts[0] == "containers" && len(parts) == 3:
		name, c := f.find(parts[1])
		if c == nil {
			w.WriteHeader(404)
			return
		}
		switch parts[2] {
		case "json":
			json.NewEncoder(w).Encode(c)
		case "stop":
			c["State"] = map[string]any{"Status": "exited", "Running": false}
		case "start":
			if img := c["Config"].(map[string]any)["Image"]; img == f.failStart {
				w.WriteHeader(500)
				io.WriteString(w, `{"message":"port is already allocated"}`)
				return
			}
			c["State"] = map[string]any{"Status": "running", "Running": true}
		case "rename":
			delete(f.containers, name)
			newName := r.URL.Query().Get("name")
			c["Name"] = "/" + newName
			f.containers[newName] = c
		}
	case parts[0] == "containers" && r.Method == http.MethodDelete:
		name, _ := f.find(parts[1])
		delete(f.containers, name)
	default:
		w.WriteHeader(404)
	}
}

func containerExecFor(srv *httptest.Server) Executor {
	ex, _ := New("c", config.Executor{Type: "container", Container: &config.ContainerExec{Name: "pay-api", Tag: "{{.PreviousVersion}}", StopTimeoutSec: 1}})
	ce := ex.(*containerExec)
	ce.newClient = func(*RunContext) *dockerapi.Client { return dockerapi.New(nil, "", srv.URL) }
	return ex
}

func TestContainerSwitch(t *testing.T) {
	fe := newFakeEngine()
	srv := httptest.NewServer(fe)
	defer srv.Close()
	ex := containerExecFor(srv)
	rc := rcFor(nil, "v1")
	if err := ex.Rollback(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	if err := ex.Verify(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	c := fe.containers["pay-api"]
	if c["Id"] != "new" || c["Config"].(map[string]any)["Image"] != "registry/pay-api:v1" {
		t.Fatalf("pay-api not replaced: %+v", c)
	}
	if hc, _ := c["HostConfig"].(map[string]any); hc["NetworkMode"] != "pay-net" {
		t.Fatal("host config not carried over")
	}
	// Idempotent replay: second rollback is a no-op.
	n := len(fe.calls)
	if err := ex.Rollback(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	if len(fe.calls) != n+1 { // just the inspect
		t.Fatalf("replay issued extra calls: %v", fe.calls[n:])
	}
}

func TestContainerCompensatesOnStartFailure(t *testing.T) {
	fe := newFakeEngine()
	fe.failStart = "registry/pay-api:v1"
	srv := httptest.NewServer(fe)
	defer srv.Close()
	err := containerExecFor(srv).Rollback(context.Background(), rcFor(nil, "v1"))
	if err == nil || !strings.Contains(err.Error(), "original container restored") {
		t.Fatalf("expected compensated failure, got %v", err)
	}
	c := fe.containers["pay-api"]
	if c == nil || c["Id"] != "old" || !c["State"].(map[string]any)["Running"].(bool) {
		t.Fatalf("original container not restored: %+v", fe.containers)
	}
}

func TestRepoOf(t *testing.T) {
	cases := map[string]string{
		"nginx:1.25":                "nginx",
		"registry:5000/team/app:v2": "registry:5000/team/app",
		"registry:5000/team/app":    "registry:5000/team/app",
		"app@sha256:abc":            "app",
	}
	for in, want := range cases {
		if got := dockerapi.RepoOf(in); got != want {
			t.Errorf("RepoOf(%q)=%q want %q", in, got, want)
		}
	}
}
