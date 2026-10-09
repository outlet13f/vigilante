// Package tmpl renders the small Go templates allowed in config values such as
// "http://{{.Address}}:8080/health" or "{{.ReleasesDir}}/{{.PreviousVersion}}".
package tmpl

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"text/template"

	"vigilante/internal/config"
)

// Data is the value every template is executed against.
type Data struct {
	Name            string
	Address         string
	Kind            string
	Labels          map[string]string
	Service         string
	Version         string
	PreviousVersion string
	DeploymentID    string
	Phase           string
	Checkpoint      map[string]string
}

// ForTarget builds template data for a target, optionally within a deployment.
func ForTarget(t config.Target) Data {
	labels := t.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return Data{Name: t.Name, Address: t.Address, Kind: t.Kind, Labels: labels, Checkpoint: map[string]string{}}
}

var (
	cacheMu sync.Mutex
	cache   = map[string]*template.Template{}
	funcs   = template.FuncMap{
		"env":   os.Getenv,
		"lower": strings.ToLower,
		"upper": strings.ToUpper,
	}
)

// Render executes s as a template. Strings without "{{" are returned as-is.
func Render(s string, d Data) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	cacheMu.Lock()
	t, ok := cache[s]
	if !ok {
		var err error
		t, err = template.New("v").Funcs(funcs).Option("missingkey=error").Parse(s)
		if err != nil {
			cacheMu.Unlock()
			return "", fmt.Errorf("template %q: %w", s, err)
		}
		cache[s] = t
	}
	cacheMu.Unlock()
	var b strings.Builder
	if err := t.Execute(&b, d); err != nil {
		return "", fmt.Errorf("template %q: %w", s, err)
	}
	return b.String(), nil
}
