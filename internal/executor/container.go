package executor

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"vigilante/internal/config"
	"vigilante/internal/dockerapi"
)

func init() { Register("container", newContainer) }

// Strategy B — standalone Docker / Podman container switch.
//
// The running (bad) container is stopped and renamed aside, then a new
// container is created with the *same* config, host config and network
// settings but the previous image tag, under the original name. If the new
// container fails to start, the old one is renamed back and restarted, so a
// failed rollback never leaves the host with no container at all.
type containerExec struct {
	spec *config.ContainerExec
	// newClient is swappable for tests.
	newClient func(rc *RunContext) *dockerapi.Client
}

func newContainer(_ string, spec config.Executor) (Executor, error) {
	c := &containerExec{spec: spec.Container}
	c.newClient = func(rc *RunContext) *dockerapi.Client {
		dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
			if rc.Runner == nil {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			}
			return rc.Runner.Dial(ctx, network, addr)
		}
		return dockerapi.New(dial, c.spec.Socket, c.spec.Host)
	}
	return c, nil
}

func (c *containerExec) names(rc *RunContext) (name, tag string, err error) {
	if name, err = rc.render(c.spec.Name); err != nil {
		return
	}
	tag, err = rc.render(c.spec.Tag)
	if err == nil && tag == "" {
		err = fmt.Errorf("previous image tag unknown")
	}
	return
}

func (c *containerExec) Prepare(ctx context.Context, rc *RunContext) (map[string]string, error) {
	name, _, _ := c.names(rc)
	ct, err := c.newClient(rc).Inspect(ctx, name)
	if err != nil {
		return nil, err
	}
	return map[string]string{"container.previous_image": ct.Config.Image}, nil
}

func (c *containerExec) targetImage(rc *RunContext, current *dockerapi.Container, tag string) string {
	if prev := rc.Checkpoint["container.previous_image"]; prev != "" {
		return prev
	}
	repo := c.spec.ImageRepo
	if repo == "" {
		repo = dockerapi.RepoOf(current.Config.Image)
	}
	return repo + ":" + tag
}

func (c *containerExec) Rollback(ctx context.Context, rc *RunContext) error {
	name, tag, err := c.names(rc)
	if err != nil {
		return err
	}
	cl := c.newClient(rc)
	cur, err := cl.Inspect(ctx, name)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", name, err)
	}
	image := c.targetImage(rc, cur, tag)
	if cur.Config.Image == image && cur.State.Running {
		rc.Log.Info("container already on previous image", "container", name, "image", image)
		return nil // idempotent replay
	}
	if rc.DryRun {
		rc.Log.Info("DRY-RUN: would replace container", "container", name, "from", cur.Config.Image, "to", image)
		return nil
	}
	exists, err := cl.ImageExists(ctx, image)
	if err != nil {
		return err
	}
	if !exists {
		if !c.spec.Pull {
			return fmt.Errorf("image %s not present on host and pull disabled", image)
		}
		repo, t := dockerapi.RepoOf(image), image[len(dockerapi.RepoOf(image))+1:]
		if err := cl.Pull(ctx, repo, t); err != nil {
			return err
		}
	}
	aside := fmt.Sprintf("%s-failed-%d", strings.TrimPrefix(cur.Name, "/"), time.Now().Unix())
	if err := cl.Stop(ctx, cur.ID, c.spec.StopTimeoutSec); err != nil {
		return fmt.Errorf("stop %s: %w", name, err)
	}
	if err := cl.Rename(ctx, cur.ID, aside); err != nil {
		_ = cl.Start(ctx, cur.ID)
		return fmt.Errorf("rename %s: %w", name, err)
	}
	restore := func(cause error) error {
		// Compensation: bring the previous container back exactly as it was.
		_ = cl.Rename(ctx, cur.ID, name)
		if err := cl.Start(ctx, cur.ID); err != nil {
			return fmt.Errorf("%w; AND restoring original container failed: %v", cause, err)
		}
		return fmt.Errorf("%w (original container restored)", cause)
	}
	id, err := cl.Create(ctx, name, dockerapi.CloneCreateBody(cur.Raw, image))
	if err != nil {
		return restore(fmt.Errorf("create %s from %s: %w", name, image, err))
	}
	if err := cl.Start(ctx, id); err != nil {
		_ = cl.Remove(ctx, id, true)
		return restore(fmt.Errorf("start %s: %w", name, err))
	}
	rc.Log.Info("container switched", "container", name, "image", image, "failed_container", aside)
	return nil
}

func (c *containerExec) Verify(ctx context.Context, rc *RunContext) error {
	if rc.DryRun {
		return nil
	}
	name, tag, err := c.names(rc)
	if err != nil {
		return err
	}
	ct, err := c.newClient(rc).Inspect(ctx, name)
	if err != nil {
		return err
	}
	if want := c.targetImage(rc, ct, tag); ct.Config.Image != want {
		return fmt.Errorf("container %s runs %s, want %s", name, ct.Config.Image, want)
	}
	if !ct.State.Running {
		return fmt.Errorf("container %s is %s", name, ct.State.Status)
	}
	if h := ct.State.Health; h != nil && h.Status == "unhealthy" {
		return fmt.Errorf("container %s is unhealthy", name)
	}
	return nil
}
