package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"vigilante/internal/cienv"
	"vigilante/internal/model"
)

// lastGood is the part of the engine resolveInputs needs (nil in remote mode,
// where the server resolves the previous version itself).
type lastGood interface {
	Live(id string) *model.Deployment
	LastGoodVersion(service string) (version, fromID string, ok bool)
}

// resolveInputs fills --id, --version and --previous when they were not given:
// the ID and version from the CI environment, the previous version from the
// journal's last successful deployment of the service. It returns one note
// per filled value, naming its source.
func resolveInputs(c *common, e lastGood, getenv cienv.Getenv, git func() (string, error)) []string {
	var notes []string
	if c.id == "" {
		if v := cienv.DeploymentID(getenv); !v.Empty() {
			c.id = v.Value
			notes = append(notes, fmt.Sprintf("id=%s (%s)", v.Value, v.Source))
		}
	}
	// An existing deployment already carries its version and previous version.
	if e != nil && c.id != "" && e.Live(c.id) != nil {
		return notes
	}
	if c.ver == "" {
		if v := cienv.Version(getenv, git); !v.Empty() {
			c.ver = v.Value
			notes = append(notes, fmt.Sprintf("version=%s (%s)", v.Value, v.Source))
		}
	}
	if c.prev == "" && e != nil && c.service != "" {
		if v, from, ok := e.LastGoodVersion(c.service); ok {
			c.prev = v
			notes = append(notes, fmt.Sprintf("previous=%s (last good deployment %s)", v, from))
		}
	}
	return notes
}

func autoFill(c *common, e lastGood) []string {
	notes := resolveInputs(c, e, os.Getenv, cienv.GitDescribe)
	if len(notes) > 0 {
		fmt.Fprintln(os.Stderr, "vigilante: auto-filled "+strings.Join(notes, ", "))
	}
	return notes
}

// requireRollbackTarget fails early when a rollback would not know where to go.
func requireRollbackTarget(d *model.Deployment) error {
	if d.Version == "" {
		return errors.New("version unknown: pass --version or set VIGILANTE_VERSION")
	}
	if d.PreviousVersion == "" && len(d.Checkpoints) == 0 {
		return fmt.Errorf("previous version unknown for %s: pass --previous, run `vigilante prepare` first, "+
			"or register a known-good version with `vigilante mark-good --service %s --version <v>`", d.Service, d.Service)
	}
	return nil
}
