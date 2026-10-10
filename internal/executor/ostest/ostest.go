// Package ostest is an in-process fake of the OpenStack APIs vigilante uses
// (Keystone v3, Nova, Cinder, Glance, Octavia), stateful enough to run the
// openstack executor and the octavia traffic controller end to end.
package ostest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"vigilante/internal/config"
)

// Fake is a stateful in-process Keystone + Nova + Cinder + Glance +
// Octavia, enough to exercise the executor and the traffic controller.
type Fake struct {
	T   *testing.T
	Srv *httptest.Server
	Mu  sync.Mutex

	Tokens       map[string]bool
	Logins       int
	LastAuth     map[string]any
	Servers      map[string]*Server
	Volumes      map[string]*Volume
	Snapshots    map[string]*Snapshot
	Images       map[string]*Image
	seq          int
	Log          []string // nova/cinder actions in order
	RefuseRevert bool
	QuotaLimit   int

	LBStatus  string
	LBPending int // GETs before a PENDING load balancer turns ACTIVE
	Conflicts int // PUTs to answer 409 even when ACTIVE
	Members   map[string]*Member
	// OnRevert runs (under the fake's lock) when a volume is reverted.
	OnRevert func(volumeID string)
}

type Server struct {
	Status, Image, Task string
	Power               int
	Volumes             []string
	Pending             int // GETs left before a transition settles
	Target              string
}
type Volume struct {
	Status, Server string
	Meta           map[string]string
	Snaps          []string // creation order
	RevertedTo     string
}
type Snapshot struct {
	Volume, Status, Name, Created string
	Meta                          map[string]string
}
type Image struct {
	Status, Server, Created string
	Meta                    map[string]string
}
type Member struct {
	ID, Address, Operating string
	Port                   int
	Up                     bool
}

func New(t *testing.T) *Fake {
	f := &Fake{T: t, Tokens: map[string]bool{}, Servers: map[string]*Server{}, Volumes: map[string]*Volume{},
		Snapshots: map[string]*Snapshot{}, Images: map[string]*Image{}, Members: map[string]*Member{}, LBStatus: "ACTIVE", QuotaLimit: 10}
	f.Srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Srv.Close)
	return f
}

func (f *Fake) next(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s-%d", prefix, f.seq)
}

func (f *Fake) stamp() string {
	return time.Unix(1700000000+int64(f.seq), 0).UTC().Format(time.RFC3339)
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func (f *Fake) catalog() []map[string]any {
	ep := func(typ, path string) map[string]any {
		return map[string]any{"type": typ, "endpoints": []map[string]string{
			{"interface": "public", "region": "RegionOne", "region_id": "RegionOne", "url": f.Srv.URL + path},
			{"interface": "internal", "region": "RegionOne", "region_id": "RegionOne", "url": f.Srv.URL + "/int" + path},
		}}
	}
	return []map[string]any{ep("compute", "/compute/v2.1"), ep("volumev3", "/volume/v3/proj-1"), ep("image", "/image"), ep("load-balancer", "/lb")}
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/int")
	var body map[string]any
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
	}
	if p == "/identity/v3/auth/tokens" && r.Method == http.MethodPost {
		f.Logins++
		f.LastAuth = body
		ident := body["auth"].(map[string]any)["identity"].(map[string]any)
		ok := false
		if ac, has := ident["application_credential"].(map[string]any); has {
			ok = ac["id"] == "ac-1" && ac["secret"] == "s3cret-app-cred"
		}
		if pw, has := ident["password"].(map[string]any); has {
			u := pw["user"].(map[string]any)
			ok = u["name"] == "deployer" && u["password"] == "pw-123456"
		}
		if !ok {
			reply(w, 401, map[string]any{"error": map[string]any{"message": "The request you have made requires authentication."}})
			return
		}
		tok := f.next("tok")
		f.Tokens[tok] = true
		w.Header().Set("X-Subject-Token", tok)
		reply(w, 201, map[string]any{"token": map[string]any{"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"project": map[string]string{"id": "proj-1"}, "catalog": f.catalog()}})
		return
	}
	if !f.Tokens[r.Header.Get("X-Auth-Token")] {
		reply(w, 401, nil)
		return
	}
	switch {
	case strings.HasPrefix(p, "/compute/v2.1/servers/"):
		f.nova(w, r, strings.TrimPrefix(p, "/compute/v2.1/servers/"), body)
	case p == "/volume/v3/":
		reply(w, 300, map[string]any{"versions": []map[string]string{{"id": "v3.0", "version": "3.70", "status": "CURRENT"}}})
	case strings.HasPrefix(p, "/volume/v3/proj-1"):
		f.cinder(w, r, strings.TrimPrefix(p, "/volume/v3/proj-1"), body)
	case strings.HasPrefix(p, "/image/v2/images"):
		f.glance(w, r, strings.TrimPrefix(p, "/image/v2/images"))
	case strings.HasPrefix(p, "/lb/v2/lbaas/"):
		f.octavia(w, r, strings.TrimPrefix(p, "/lb/v2/lbaas/"), body)
	default:
		reply(w, 404, map[string]string{"error": "no route " + p})
	}
}

func (f *Fake) nova(w http.ResponseWriter, r *http.Request, rest string, body map[string]any) {
	id, action, _ := strings.Cut(rest, "/")
	s := f.Servers[id]
	if s == nil {
		reply(w, 404, map[string]any{"itemNotFound": map[string]string{"message": "Instance could not be found"}})
		return
	}
	if action == "" && r.Method == http.MethodGet {
		if s.Pending > 0 {
			s.Pending--
		} else if s.Target != "" {
			s.Status, s.Task, s.Target = s.Target, "", ""
			s.Power = map[string]int{"ACTIVE": 1, "SHUTOFF": 4}[s.Status]
		}
		var image any = ""
		if s.Image != "" {
			image = map[string]string{"id": s.Image}
		}
		var vols []map[string]string
		for _, v := range s.Volumes {
			vols = append(vols, map[string]string{"id": v})
		}
		var task any
		if s.Task != "" {
			task = s.Task
		}
		reply(w, 200, map[string]any{"server": map[string]any{"id": id, "name": "vm-" + id, "status": s.Status, "image": image,
			"OS-EXT-STS:power_state": s.Power, "OS-EXT-STS:task_state": task, "os-extended-volumes:volumes_attached": vols}})
		return
	}
	switch {
	case body["createImage"] != nil:
		ci := body["createImage"].(map[string]any)
		img := f.next("img")
		meta := map[string]string{}
		for k, v := range ci["metadata"].(map[string]any) {
			meta[k] = v.(string)
		}
		f.Images[img] = &Image{Status: "queued", Server: id, Created: f.stamp(), Meta: meta}
		f.Log = append(f.Log, "createImage")
		if strings.Contains(r.Header.Get("OpenStack-API-Version"), "2.45") {
			reply(w, 202, map[string]string{"image_id": img})
		} else {
			w.Header().Set("Location", f.Srv.URL+"/image/v2/images/"+img)
			reply(w, 202, nil)
		}
	case body["rebuild"] != nil:
		s.Image = body["rebuild"].(map[string]any)["imageRef"].(string)
		s.Status, s.Task, s.Target, s.Pending = "REBUILD", "rebuilding", "ACTIVE", 1
		f.Log = append(f.Log, "rebuild")
		reply(w, 202, nil)
	case hasKey(body, "os-stop"):
		if s.Status == "SHUTOFF" {
			reply(w, 409, map[string]any{"conflictingRequest": map[string]string{"message": "already stopped"}})
			return
		}
		s.Task, s.Target, s.Pending = "powering-off", "SHUTOFF", 1
		f.Log = append(f.Log, "stop")
		reply(w, 202, nil)
	case hasKey(body, "os-start"):
		s.Task, s.Target, s.Pending = "powering-on", "ACTIVE", 1
		f.Log = append(f.Log, "start")
		reply(w, 202, nil)
	default:
		reply(w, 400, map[string]string{"error": "unknown action"})
	}
}

func hasKey(m map[string]any, k string) bool { _, ok := m[k]; return ok }

func (f *Fake) cinder(w http.ResponseWriter, r *http.Request, rest string, body map[string]any) {
	switch {
	case strings.HasPrefix(rest, "/os-quota-sets/"):
		used := 0
		for _, s := range f.Snapshots {
			if s.Status != "deleted" {
				used++
			}
		}
		reply(w, 200, map[string]any{"quota_set": map[string]any{"snapshots": map[string]int{"limit": f.QuotaLimit, "in_use": used}}})
	case rest == "/snapshots" && r.Method == http.MethodPost:
		sn := body["snapshot"].(map[string]any)
		vid := sn["volume_id"].(string)
		if sn["force"] != true {
			reply(w, 400, map[string]string{"error": "volume in-use needs force"})
			return
		}
		id := f.next("snap")
		meta := map[string]string{}
		for k, v := range sn["metadata"].(map[string]any) {
			meta[k] = v.(string)
		}
		f.Snapshots[id] = &Snapshot{Volume: vid, Status: "creating", Name: sn["name"].(string), Created: f.stamp(), Meta: meta}
		f.Volumes[vid].Snaps = append(f.Volumes[vid].Snaps, id)
		reply(w, 202, map[string]any{"snapshot": map[string]string{"id": id}})
	case rest == "/snapshots/detail":
		var out []map[string]any
		for id, s := range f.Snapshots {
			if s.Volume == r.URL.Query().Get("volume_id") && s.Status != "deleted" {
				out = append(out, map[string]any{"id": id, "created_at": s.Created, "metadata": s.Meta})
			}
		}
		reply(w, 200, map[string]any{"snapshots": out})
	case strings.HasPrefix(rest, "/snapshots/"):
		id := strings.TrimPrefix(rest, "/snapshots/")
		s := f.Snapshots[id]
		if s == nil || s.Status == "deleted" {
			reply(w, 404, nil)
			return
		}
		if r.Method == http.MethodDelete {
			s.Status = "deleted"
			reply(w, 202, nil)
			return
		}
		if s.Status == "creating" {
			s.Status = "available"
			reply(w, 200, map[string]any{"snapshot": map[string]string{"status": "creating"}})
			return
		}
		reply(w, 200, map[string]any{"snapshot": map[string]string{"status": s.Status}})
	case strings.HasPrefix(rest, "/volumes/"):
		id, sub, _ := strings.Cut(strings.TrimPrefix(rest, "/volumes/"), "/")
		v := f.Volumes[id]
		if v == nil {
			reply(w, 404, nil)
			return
		}
		switch sub {
		case "":
			st := v.Status
			if st == "reverting" {
				v.Status = "in-use"
			}
			reply(w, 200, map[string]any{"volume": map[string]any{"id": id, "status": st, "bootable": "true", "size": 20, "metadata": v.Meta,
				"attachments": []map[string]string{{"server_id": v.Server, "device": "/dev/vda"}}}})
		case "metadata":
			for k, val := range body["metadata"].(map[string]any) {
				v.Meta[k] = val.(string)
			}
			reply(w, 200, map[string]any{"metadata": v.Meta})
		case "action":
			rv := body["revert"].(map[string]any)
			snap := rv["snapshot_id"].(string)
			if !strings.Contains(r.Header.Get("OpenStack-API-Version"), "volume 3.40") {
				reply(w, 404, map[string]string{"error": "revert needs 3.40"})
				return
			}
			var live []string
			for _, s := range v.Snaps {
				if f.Snapshots[s].Status != "deleted" {
					live = append(live, s)
				}
			}
			if f.RefuseRevert || len(live) == 0 || live[len(live)-1] != snap {
				reply(w, 400, map[string]any{"badRequest": map[string]string{"message": "Invalid snapshot: only the latest snapshot can be reverted to"}})
				return
			}
			if f.Servers[v.Server].Status != "SHUTOFF" {
				reply(w, 400, map[string]any{"badRequest": map[string]string{"message": "server must be stopped (fake rule)"}})
				return
			}
			v.Status, v.RevertedTo = "reverting", snap
			if f.OnRevert != nil {
				f.OnRevert(id)
			}
			f.Log = append(f.Log, "revert")
			reply(w, 202, nil)
		}
	default:
		reply(w, 404, nil)
	}
}

func (f *Fake) glance(w http.ResponseWriter, r *http.Request, rest string) {
	if rest == "" { // list with property filters
		type row struct {
			ID        string `json:"id"`
			CreatedAt string `json:"created_at"`
		}
		var out []row
		for id, im := range f.Images {
			if im.Status != "deleted" && im.Meta["vigilante.server_id"] == r.URL.Query().Get("vigilante.server_id") {
				out = append(out, row{id, im.Created})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
		reply(w, 200, map[string]any{"images": out})
		return
	}
	id := strings.TrimPrefix(rest, "/")
	im := f.Images[id]
	if im == nil || im.Status == "deleted" {
		reply(w, 404, nil)
		return
	}
	if r.Method == http.MethodDelete {
		im.Status = "deleted"
		reply(w, 204, nil)
		return
	}
	st := im.Status
	if im.Status == "queued" {
		im.Status = "active"
	}
	reply(w, 200, map[string]string{"id": id, "status": st})
}

func (f *Fake) octavia(w http.ResponseWriter, r *http.Request, rest string, body map[string]any) {
	memberJSON := func(m *Member) map[string]any {
		return map[string]any{"id": m.ID, "address": m.Address, "protocol_port": m.Port, "admin_state_up": m.Up,
			"operating_status": m.Operating, "provisioning_status": "ACTIVE"}
	}
	switch {
	case rest == "loadbalancers/lb-1":
		st := f.LBStatus
		if st != "ACTIVE" {
			if f.LBPending > 0 {
				f.LBPending--
			} else {
				f.LBStatus = "ACTIVE"
			}
		}
		reply(w, 200, map[string]any{"loadbalancer": map[string]string{"id": "lb-1", "provisioning_status": st, "operating_status": "ONLINE"}})
	case rest == "pools/pool-1":
		reply(w, 200, map[string]any{"pool": map[string]any{"id": "pool-1", "loadbalancers": []map[string]string{{"id": "lb-1"}}}})
	case rest == "pools/pool-1/members":
		var out []map[string]any
		for _, m := range f.Members {
			out = append(out, memberJSON(m))
		}
		reply(w, 200, map[string]any{"members": out})
	case strings.HasPrefix(rest, "pools/pool-1/members/"):
		m := f.Members[strings.TrimPrefix(rest, "pools/pool-1/members/")]
		if m == nil {
			reply(w, 404, nil)
			return
		}
		if r.Method == http.MethodPut {
			if f.LBStatus != "ACTIVE" || f.Conflicts > 0 {
				if f.Conflicts > 0 {
					f.Conflicts--
				}
				reply(w, 409, map[string]string{"faultstring": "Load Balancer lb-1 is immutable and cannot be updated."})
				return
			}
			m.Up = body["member"].(map[string]any)["admin_state_up"].(bool)
			if m.Up {
				m.Operating = "OFFLINE" // until the health monitor passes
			} else {
				m.Operating = "OFFLINE"
			}
			f.LBStatus, f.LBPending = "PENDING_UPDATE", 1
			f.Log = append(f.Log, fmt.Sprintf("member %s up=%v", m.Address, m.Up))
			reply(w, 200, map[string]any{"member": memberJSON(m)})
			return
		}
		st := m.Operating
		if m.Up && m.Operating == "OFFLINE" {
			m.Operating = "ONLINE"
		}
		j := memberJSON(m)
		j["operating_status"] = st
		reply(w, 200, map[string]any{"member": j})
	default:
		reply(w, 404, nil)
	}
}

// Creds returns an application-credential credential for this fake; the
// secret must be in env var VGL_TEST_OS_SECRET ("s3cret-app-cred").
func (f *Fake) Creds() map[string]config.Credential {
	return map[string]config.Credential{"os": {Type: "openstack", AuthURL: f.Srv.URL + "/identity/v3", Region: "RegionOne",
		ApplicationCredentialID: "ac-1", ApplicationCredentialSecretRef: "env:VGL_TEST_OS_SECRET"}}
}

// AuthURL is the Keystone v3 URL of the fake.
func (f *Fake) AuthURL() string { return f.Srv.URL + "/identity/v3" }

// VolumeServer adds a running volume-booted server with root volume "vol-<id>".
func (f *Fake) VolumeServer(id string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.Servers[id] = &Server{Status: "ACTIVE", Power: 1, Volumes: []string{"vol-" + id}}
	f.Volumes["vol-"+id] = &Volume{Status: "in-use", Server: id, Meta: map[string]string{}}
}

func (f *Fake) Actions() string {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return strings.Join(f.Log, ",")
}
