package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"vigilante/internal/config"
)

// ---------------------------------------------------------------------------
// Strategy D on OpenStack: snapshot the server before the deployment and
// restore it on rollback.
//
//   volume-booted server: Cinder snapshot of the root volume (force, while
//                         running) -> rollback: stop, revert-to-snapshot
//                         (volume API 3.40), start.
//   image-booted server:  Nova server snapshot (a Glance image) -> rollback:
//                         rebuild from that image (IP, ports, metadata kept).
//
// Rollback is idempotent: a server already on the checkpoint image, or a
// volume already reverted to the checkpoint snapshot, is left alone.

func init() { Register("openstack", newOpenStack) }

type openstackExec struct{ spec *config.OpenStackExec }

func newOpenStack(_ string, spec config.Executor) (Executor, error) {
	return &openstackExec{spec: spec.OpenStack}, nil
}

const (
	cpMode     = "openstack.mode"
	cpServer   = "openstack.server_id"
	cpVolume   = "openstack.volume_id"
	cpSnapshot = "openstack.volume_snapshot_id"
	cpImage    = "openstack.image_id"

	metaManaged    = "vigilante.managed"
	metaService    = "vigilante.service"
	metaDeployment = "vigilante.deployment_id"
	metaServer     = "vigilante.server_id"
	metaRevertedTo = "vigilante.reverted_to"
)

var (
	hdrCompute245 = map[string]string{"OpenStack-API-Version": "compute 2.45", "X-OpenStack-Nova-API-Version": "2.45"}
	hdrVolume340  = map[string]string{"OpenStack-API-Version": "volume 3.40"}
)

type osServer struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Status     string          `json:"status"`
	Image      json.RawMessage `json:"image"`
	PowerState int             `json:"OS-EXT-STS:power_state"`
	TaskState  *string         `json:"OS-EXT-STS:task_state"`
	Volumes    []struct {
		ID string `json:"id"`
	} `json:"os-extended-volumes:volumes_attached"`
	Fault *struct {
		Message string `json:"message"`
	} `json:"fault"`
}

// imageID is "" for volume-booted servers (Nova reports image as "").
func (s *osServer) imageID() string {
	var obj struct {
		ID string `json:"id"`
	}
	if len(s.Image) > 0 && s.Image[0] == '{' && json.Unmarshal(s.Image, &obj) == nil {
		return obj.ID
	}
	return ""
}

func (s *osServer) settled() bool { return s.TaskState == nil || *s.TaskState == "" }

func (s *osServer) fault() string {
	if s.Fault != nil {
		return ": " + s.Fault.Message
	}
	return ""
}

type osVolume struct {
	ID          string            `json:"id"`
	Status      string            `json:"status"`
	Bootable    string            `json:"bootable"`
	Size        int               `json:"size"`
	Metadata    map[string]string `json:"metadata"`
	Attachments []struct {
		ServerID string `json:"server_id"`
		Device   string `json:"device"`
	} `json:"attachments"`
}

func (o *openstackExec) client(rc *RunContext) (*osClient, error) {
	return openstackClient(o.spec.Credential, rc.Creds)
}

func (o *openstackExec) serverID(rc *RunContext) (string, error) {
	if id := rc.Checkpoint[cpServer]; id != "" {
		return id, nil
	}
	id, err := rc.render(o.spec.ServerID)
	if err == nil && id == "" {
		err = fmt.Errorf("target %s: no OpenStack server ID (set labels.openstack_server_id or openstack.server_id)", rc.Target.Name)
	}
	return id, err
}

func getServer(ctx context.Context, c *osClient, id string) (*osServer, error) {
	var out struct {
		Server osServer `json:"server"`
	}
	if _, err := c.call(ctx, svcCompute, http.MethodGet, "/servers/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Server, nil
}

func getVolume(ctx context.Context, c *osClient, id string) (*osVolume, error) {
	var out struct {
		Volume osVolume `json:"volume"`
	}
	if _, err := c.call(ctx, svcVolume, http.MethodGet, "/volumes/"+url.PathEscape(id), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out.Volume, nil
}

var rootDevices = map[string]bool{"/dev/vda": true, "/dev/sda": true, "/dev/xvda": true, "/dev/hda": true}

// rootVolume finds the boot volume of a volume-booted server.
func rootVolume(ctx context.Context, c *osClient, srv *osServer) (*osVolume, error) {
	var best *osVolume
	bestDev := ""
	for _, a := range srv.Volumes {
		v, err := getVolume(ctx, c, a.ID)
		if err != nil {
			return nil, err
		}
		dev := ""
		for _, at := range v.Attachments {
			if at.ServerID == srv.ID {
				dev = at.Device
			}
		}
		if rootDevices[dev] {
			return v, nil
		}
		if v.Bootable == "true" && (best == nil || dev < bestDev) {
			best, bestDev = v, dev
		}
	}
	if best == nil {
		return nil, fmt.Errorf("server %s: no bootable volume attached", srv.ID)
	}
	return best, nil
}

func (o *openstackExec) mode(srv *osServer) (string, error) {
	booted := "image"
	if srv.imageID() == "" {
		booted = "volume"
	}
	switch o.spec.Mode {
	case "", "auto":
		return booted, nil
	case booted:
		return booted, nil
	case "image":
		return "", fmt.Errorf("server %s boots from a volume; openstack.mode image needs an image-booted server (use mode auto or volume)", srv.ID)
	default:
		return "", fmt.Errorf("server %s boots from an image; it has no root volume to snapshot (use mode auto or image)", srv.ID)
	}
}

func (o *openstackExec) metadata(rc *RunContext, serverID string) map[string]string {
	return map[string]string{metaManaged: "true", metaService: rc.Data.Service, metaDeployment: rc.Data.DeploymentID, metaServer: serverID}
}

func (o *openstackExec) Prepare(ctx context.Context, rc *RunContext) (map[string]string, error) {
	id, err := o.serverID(rc)
	if err != nil {
		return nil, err
	}
	name, err := rc.render(o.spec.Snapshot)
	if err != nil {
		return nil, err
	}
	c, err := o.client(rc)
	if err != nil {
		return nil, err
	}
	srv, err := getServer(ctx, c, id)
	if err != nil {
		return nil, err
	}
	mode, err := o.mode(srv)
	if err != nil {
		return nil, err
	}
	cp := map[string]string{cpMode: mode, cpServer: id}
	if rc.DryRun {
		rc.Log.Info("DRY-RUN: would snapshot OpenStack server", "server", id, "mode", mode, "name", name)
		return cp, nil
	}
	if mode == "volume" {
		vol, err := rootVolume(ctx, c, srv)
		if err != nil {
			return nil, err
		}
		var out struct {
			Snapshot struct {
				ID string `json:"id"`
			} `json:"snapshot"`
		}
		body := map[string]any{"snapshot": map[string]any{"volume_id": vol.ID, "name": name, "force": true, "metadata": o.metadata(rc, id)}}
		if _, err := c.call(ctx, svcVolume, http.MethodPost, "/snapshots", nil, body, &out); err != nil {
			return nil, err
		}
		if err := waitSnapshot(ctx, c, out.Snapshot.ID); err != nil {
			return nil, err
		}
		cp[cpVolume], cp[cpSnapshot] = vol.ID, out.Snapshot.ID
		o.pruneSnapshots(ctx, rc, c, vol.ID, out.Snapshot.ID)
		return cp, nil
	}
	var out struct {
		ImageID string `json:"image_id"`
	}
	h, err := c.call(ctx, svcCompute, http.MethodPost, "/servers/"+url.PathEscape(id)+"/action", hdrCompute245,
		map[string]any{"createImage": map[string]any{"name": name, "metadata": o.metadata(rc, id)}}, &out)
	if err != nil {
		return nil, err
	}
	img := out.ImageID
	if img == "" && h != nil { // microversions before 2.45 answer with a Location header
		img = path.Base(h.Get("Location"))
	}
	if img == "" || img == "." || img == "/" {
		return nil, errors.New("nova createImage returned no image ID")
	}
	if err := waitImage(ctx, c, img); err != nil {
		return nil, err
	}
	cp[cpImage] = img
	o.pruneImages(ctx, rc, c, id, img)
	return cp, nil
}

func waitSnapshot(ctx context.Context, c *osClient, id string) error {
	return poll(ctx, pollInterval, "cinder snapshot "+id, func() (bool, error) {
		var out struct {
			Snapshot struct {
				Status string `json:"status"`
			} `json:"snapshot"`
		}
		if _, err := c.call(ctx, svcVolume, http.MethodGet, "/snapshots/"+url.PathEscape(id), nil, nil, &out); err != nil {
			return false, err
		}
		switch out.Snapshot.Status {
		case "available":
			return true, nil
		case "error", "error_deleting":
			return false, fmt.Errorf("cinder snapshot %s: status %s", id, out.Snapshot.Status)
		}
		return false, nil
	})
}

func waitImage(ctx context.Context, c *osClient, id string) error {
	return poll(ctx, pollInterval, "glance image "+id, func() (bool, error) {
		var img struct {
			Status string `json:"status"`
		}
		if _, err := c.call(ctx, svcImage, http.MethodGet, "/v2/images/"+url.PathEscape(id), nil, nil, &img); err != nil {
			return false, err
		}
		switch img.Status {
		case "active":
			return true, nil
		case "killed", "deleted", "deactivated":
			return false, fmt.Errorf("glance image %s: status %s", id, img.Status)
		}
		return false, nil
	})
}

func (o *openstackExec) keep() int {
	if o.spec.KeepSnapshots == nil {
		return 3
	}
	return *o.spec.KeepSnapshots
}

// pruneSnapshots deletes vigilante's older snapshots of the volume beyond
// keep_snapshots. Failures are logged: they must not fail the deployment.
func (o *openstackExec) pruneSnapshots(ctx context.Context, rc *RunContext, c *osClient, volumeID, current string) {
	if o.keep() == 0 {
		return
	}
	var out struct {
		Snapshots []struct {
			ID        string            `json:"id"`
			CreatedAt string            `json:"created_at"`
			Metadata  map[string]string `json:"metadata"`
		} `json:"snapshots"`
	}
	if _, err := c.call(ctx, svcVolume, http.MethodGet, "/snapshots/detail?volume_id="+url.QueryEscape(volumeID), nil, nil, &out); err != nil {
		rc.Log.Warn("openstack: listing old snapshots failed", "volume", volumeID, "err", err)
		return
	}
	snaps := out.Snapshots[:0]
	for _, s := range out.Snapshots {
		if s.Metadata[metaManaged] == "true" {
			snaps = append(snaps, s)
		}
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].CreatedAt > snaps[j].CreatedAt })
	for i, s := range snaps {
		if i < o.keep() || s.ID == current {
			continue
		}
		if _, err := c.call(ctx, svcVolume, http.MethodDelete, "/snapshots/"+url.PathEscape(s.ID), nil, nil, nil); err != nil {
			rc.Log.Warn("openstack: deleting old snapshot failed", "snapshot", s.ID, "err", err)
		}
	}
}

func (o *openstackExec) pruneImages(ctx context.Context, rc *RunContext, c *osClient, serverID, current string) {
	if o.keep() == 0 {
		return
	}
	var out struct {
		Images []struct {
			ID        string `json:"id"`
			CreatedAt string `json:"created_at"`
		} `json:"images"`
	}
	q := url.Values{metaServer: {serverID}, metaManaged: {"true"}, "sort": {"created_at:desc"}}
	if _, err := c.call(ctx, svcImage, http.MethodGet, "/v2/images?"+q.Encode(), nil, nil, &out); err != nil {
		rc.Log.Warn("openstack: listing old images failed", "server", serverID, "err", err)
		return
	}
	imgs := out.Images
	sort.Slice(imgs, func(i, j int) bool { return imgs[i].CreatedAt > imgs[j].CreatedAt })
	for i, im := range imgs {
		if i < o.keep() || im.ID == current {
			continue
		}
		if _, err := c.call(ctx, svcImage, http.MethodDelete, "/v2/images/"+url.PathEscape(im.ID), nil, nil, nil); err != nil {
			rc.Log.Warn("openstack: deleting old image failed", "image", im.ID, "err", err)
		}
	}
}

func (o *openstackExec) checkpoint(rc *RunContext) (mode, server string, err error) {
	mode, server = rc.Checkpoint[cpMode], rc.Checkpoint[cpServer]
	switch {
	case mode == "" || server == "":
		err = errors.New("no OpenStack checkpoint: run `vigilante prepare` before the deployment")
	case mode == "volume" && rc.Checkpoint[cpSnapshot] == "":
		err = errors.New("checkpoint has no openstack.volume_snapshot_id")
	case mode == "image" && rc.Checkpoint[cpImage] == "":
		err = errors.New("checkpoint has no openstack.image_id")
	}
	return
}

func serverAction(ctx context.Context, c *osClient, id string, action map[string]any) error {
	_, err := c.call(ctx, svcCompute, http.MethodPost, "/servers/"+url.PathEscape(id)+"/action", nil, action, nil)
	return err
}

// waitServer polls until the server reaches status with no task running.
func waitServer(ctx context.Context, c *osClient, id, status string, extra func(*osServer) bool) error {
	return poll(ctx, pollInterval, "server "+id+" "+status, func() (bool, error) {
		s, err := getServer(ctx, c, id)
		if err != nil {
			return false, err
		}
		if s.Status == "ERROR" {
			return false, fmt.Errorf("server %s went to ERROR%s", id, s.fault())
		}
		return s.Status == status && s.settled() && (extra == nil || extra(s)), nil
	})
}

func (o *openstackExec) Rollback(ctx context.Context, rc *RunContext) error {
	mode, id, err := o.checkpoint(rc)
	if err != nil {
		return err
	}
	if rc.DryRun {
		rc.Log.Info("DRY-RUN: would restore OpenStack server", "server", id, "mode", mode,
			"snapshot", rc.Checkpoint[cpSnapshot], "image", rc.Checkpoint[cpImage])
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, o.spec.RevertTimeout)
	defer cancel()
	c, err := o.client(rc)
	if err != nil {
		return err
	}
	srv, err := getServer(ctx, c, id)
	if err != nil {
		return err
	}
	if mode == "image" {
		img := rc.Checkpoint[cpImage]
		if srv.imageID() == img && srv.Status == "ACTIVE" && srv.settled() {
			rc.Log.Info("openstack: server already rebuilt from the checkpoint image", "server", id, "image", img)
			return nil
		}
		if err := serverAction(ctx, c, id, map[string]any{"rebuild": map[string]any{"imageRef": img}}); err != nil {
			return err
		}
		return waitServer(ctx, c, id, "ACTIVE", func(s *osServer) bool { return s.imageID() == img })
	}

	volID, snap := rc.Checkpoint[cpVolume], rc.Checkpoint[cpSnapshot]
	vol, err := getVolume(ctx, c, volID)
	if err != nil {
		return err
	}
	if vol.Metadata[metaRevertedTo] == snap && (!o.powerOn() || srv.Status == "ACTIVE") {
		rc.Log.Info("openstack: volume already reverted to the checkpoint", "volume", volID, "snapshot", snap)
		return nil
	}
	if srv.Status != "SHUTOFF" {
		if err := serverAction(ctx, c, id, map[string]any{"os-stop": nil}); err != nil && !isStatus(err, http.StatusConflict) {
			return err
		}
		if err := waitServer(ctx, c, id, "SHUTOFF", nil); err != nil {
			return err
		}
	}
	if vol.Metadata[metaRevertedTo] != snap {
		_, err := c.call(ctx, svcVolume, http.MethodPost, "/volumes/"+url.PathEscape(volID)+"/action", hdrVolume340,
			map[string]any{"revert": map[string]string{"snapshot_id": snap}}, nil)
		if err != nil {
			if isStatus(err, http.StatusBadRequest) || isStatus(err, http.StatusNotFound) || isStatus(err, http.StatusNotAcceptable) {
				return fmt.Errorf("%w — Cinder refused revert-to-snapshot. It needs volume API 3.40, a backend that supports revert, "+
					"and the checkpoint must be the volume's newest snapshot; some releases also refuse in-use volumes. "+
					"The server is left stopped; restore it by hand or add an escalation step (see `vigilante doctor`)", err)
			}
			return err
		}
		if err := poll(ctx, pollInterval, "volume "+volID+" revert", func() (bool, error) {
			v, err := getVolume(ctx, c, volID)
			if err != nil {
				return false, err
			}
			switch v.Status {
			case "available", "in-use":
				return true, nil
			case "error", "error_restoring":
				return false, fmt.Errorf("volume %s: status %s after revert", volID, v.Status)
			}
			return false, nil
		}); err != nil {
			return err
		}
		if _, err := c.call(ctx, svcVolume, http.MethodPost, "/volumes/"+url.PathEscape(volID)+"/metadata", nil,
			map[string]any{"metadata": map[string]string{metaRevertedTo: snap}}, nil); err != nil {
			return fmt.Errorf("volume reverted, but recording it failed: %w", err)
		}
	}
	if !o.powerOn() {
		return nil
	}
	if err := serverAction(ctx, c, id, map[string]any{"os-start": nil}); err != nil && !isStatus(err, http.StatusConflict) {
		return err
	}
	return waitServer(ctx, c, id, "ACTIVE", nil)
}

func (o *openstackExec) powerOn() bool { return o.spec.PowerOn == nil || *o.spec.PowerOn }

func (o *openstackExec) Verify(ctx context.Context, rc *RunContext) error {
	if rc.DryRun {
		return nil
	}
	mode, id, err := o.checkpoint(rc)
	if err != nil {
		return err
	}
	c, err := o.client(rc)
	if err != nil {
		return err
	}
	srv, err := getServer(ctx, c, id)
	if err != nil {
		return err
	}
	if o.powerOn() && (srv.Status != "ACTIVE" || srv.PowerState != 1) {
		return fmt.Errorf("server %s status=%s power_state=%d%s", id, srv.Status, srv.PowerState, srv.fault())
	}
	if mode == "image" {
		if img := rc.Checkpoint[cpImage]; srv.imageID() != img {
			return fmt.Errorf("server %s runs image %s, not checkpoint %s", id, srv.imageID(), img)
		}
		return nil
	}
	vol, err := getVolume(ctx, c, rc.Checkpoint[cpVolume])
	if err != nil {
		return err
	}
	if got, want := vol.Metadata[metaRevertedTo], rc.Checkpoint[cpSnapshot]; got != want {
		return fmt.Errorf("volume %s is not marked as reverted to %s (marker %q)", vol.ID, want, got)
	}
	return nil
}

var microversionRe = regexp.MustCompile(`^(\d+)\.(\d+)$`)

// supports reports whether a "3.70"-style max version reaches want.
func supports(max, want string) bool {
	a, b := microversionRe.FindStringSubmatch(max), microversionRe.FindStringSubmatch(want)
	if a == nil || b == nil {
		return false
	}
	var am, an, bm, bn int
	fmt.Sscan(a[1], &am)
	fmt.Sscan(a[2], &an)
	fmt.Sscan(b[1], &bm)
	fmt.Sscan(b[2], &bn)
	return am > bm || (am == bm && an >= bn)
}

// Diagnose checks, read-only, everything a rollback of rc's target needs.
func (o *openstackExec) Diagnose(ctx context.Context, rc *RunContext) []Finding {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := o.client(rc)
	if err != nil {
		return []Finding{finding("OpenStack 자격증명", err, "")}
	}
	if _, err := c.session(ctx, false); err != nil {
		return []Finding{finding("Keystone 로그인", err, "")}
	}
	out := []Finding{{Name: "Keystone 로그인", Status: "ok", Detail: "project " + c.projectID}}
	id, err := o.serverID(rc)
	if err != nil {
		return append(out, finding("서버 ID", err, ""))
	}
	srv, err := getServer(ctx, c, id)
	if err != nil {
		return append(out, finding("서버 조회", err, ""))
	}
	mode, err := o.mode(srv)
	if err != nil {
		return append(out, finding("부팅 방식", err, ""))
	}
	out = append(out, Finding{Name: "서버 조회", Status: "ok", Detail: fmt.Sprintf("%s %s, %s 부팅", srv.Name, srv.Status, map[string]string{"volume": "볼륨", "image": "이미지"}[mode])})
	if mode == "image" {
		_, err := c.endpoint(ctx, svcImage...)
		return append(out, finding("Glance 엔드포인트", err, "스냅샷 이미지 확인 가능"))
	}
	vol, err := rootVolume(ctx, c, srv)
	if err != nil {
		return append(out, finding("루트 볼륨", err, ""))
	}
	out = append(out, Finding{Name: "루트 볼륨", Status: "ok", Detail: fmt.Sprintf("%s (%dGB)", vol.ID, vol.Size)})
	// The revert API needs volume microversion 3.40.
	if base, err := c.endpoint(ctx, svcVolume...); err == nil {
		root := base
		if i := strings.Index(base, "/v3"); i >= 0 {
			root = base[:i+3]
		}
		var ver struct {
			Version struct {
				Version string `json:"version"`
			} `json:"version"`
			Versions []struct {
				ID      string `json:"id"`
				Version string `json:"version"`
			} `json:"versions"`
		}
		max := ""
		if err := c.getAbsolute(ctx, root+"/", &ver); err == nil {
			max = ver.Version.Version
			for _, v := range ver.Versions {
				if strings.HasPrefix(v.ID, "v3") {
					max = v.Version
				}
			}
		}
		switch {
		case max == "":
			out = append(out, Finding{Name: "Cinder revert API", Status: "warn", Detail: "최대 마이크로버전을 확인하지 못함(3.40 필요)"})
		case supports(max, "3.40"):
			out = append(out, Finding{Name: "Cinder revert API", Status: "ok", Detail: "최대 " + max + " (3.40 필요). 백엔드의 revert 지원 여부는 실장비에서 확인"})
		default:
			out = append(out, Finding{Name: "Cinder revert API", Status: "fail", Detail: "최대 " + max + ": revert-to-snapshot(3.40) 미지원"})
		}
	}
	var q struct {
		QuotaSet struct {
			Snapshots struct {
				Limit int `json:"limit"`
				InUse int `json:"in_use"`
			} `json:"snapshots"`
		} `json:"quota_set"`
	}
	if _, err := c.call(ctx, svcVolume, http.MethodGet, "/os-quota-sets/"+url.PathEscape(c.projectID)+"?usage=true", nil, nil, &q); err != nil {
		out = append(out, Finding{Name: "스냅샷 쿼터", Status: "warn", Detail: "조회 실패: " + err.Error()})
	} else if s := q.QuotaSet.Snapshots; s.Limit >= 0 && s.Limit-s.InUse < 1 {
		out = append(out, Finding{Name: "스냅샷 쿼터", Status: "fail", Detail: fmt.Sprintf("%d/%d 사용 중: prepare가 스냅샷을 만들 수 없음", s.InUse, s.Limit)})
	} else {
		out = append(out, Finding{Name: "스냅샷 쿼터", Status: "ok", Detail: fmt.Sprintf("%d/%d 사용 중", s.InUse, s.Limit)})
	}
	return out
}

// getAbsolute GETs a full URL with the session token (version documents).
func (c *osClient) getAbsolute(ctx context.Context, u string, out any) error {
	tok, err := c.session(ctx, false)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Auth-Token", tok)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusMultipleChoices {
		return fmt.Errorf("GET %s: %d", u, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
