// Package presets provides vetted bundles of probes, rollback rules and phase
// timings, so a service is declared by naming a preset and filling a few
// parameters instead of writing its rules by hand.
//
// A preset file is a YAML header (name, version, description, params), a
// "---" line, and a body: a text/template with [[ ]] delimiters that renders
// to the probes / rules / phases / baseline part of a service. The [[ ]]
// delimiters leave runtime templates such as {{.Address}} untouched.
//
// This package only renders text; internal/config parses the result, so the
// two packages do not depend on each other's types.
package presets

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

//go:embed builtin/*.yaml
var builtinFS embed.FS

// Param is one preset parameter.
type Param struct {
	Name        string `yaml:"name"`
	Required    bool   `yaml:"required"`
	Default     any    `yaml:"default"`
	Description string `yaml:"description"`
}

// Preset is one parsed preset file.
type Preset struct {
	Name        string  `yaml:"name"`
	Version     int     `yaml:"version"`
	Description string  `yaml:"description"`
	Params      []Param `yaml:"params"`
	Source      string  `yaml:"-"` // "builtin" or the file path
	body        *template.Template
}

// Ref is the canonical "name@version" reference.
func (p *Preset) Ref() string { return fmt.Sprintf("%s@%d", p.Name, p.Version) }

func parsePreset(raw []byte, source string) (*Preset, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	head, body, ok := strings.Cut(text, "\n---\n")
	if !ok {
		return nil, fmt.Errorf("preset %s: missing '---' line between header and body", source)
	}
	var p Preset
	dec := yaml.NewDecoder(strings.NewReader(head))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("preset %s: header: %w", source, err)
	}
	if p.Name == "" || p.Version < 1 {
		return nil, fmt.Errorf("preset %s: name and a version >= 1 are required", source)
	}
	seen := map[string]bool{}
	for _, prm := range p.Params {
		if prm.Name == "" || seen[prm.Name] {
			return nil, fmt.Errorf("preset %s: empty or duplicate param %q", source, prm.Name)
		}
		seen[prm.Name] = true
	}
	t, err := template.New(p.Name).Delims("[[", "]]").Option("missingkey=error").Parse(body)
	if err != nil {
		return nil, fmt.Errorf("preset %s: body: %w", source, err)
	}
	p.body, p.Source = t, source
	return &p, nil
}

// Render fills the preset with overrides and returns the service fragment as
// YAML. Unknown override keys and missing required params are errors.
func (p *Preset) Render(overrides map[string]any) (string, error) {
	data, err := p.values(overrides, false)
	if err != nil {
		return "", err
	}
	return p.exec(data)
}

// Preview renders like Render but shows missing required params as
// <name> placeholders instead of failing (for `vigilante presets show`).
func (p *Preset) Preview(overrides map[string]any) (string, error) {
	data, err := p.values(overrides, true)
	if err != nil {
		return "", err
	}
	return p.exec(data)
}

func (p *Preset) values(overrides map[string]any, placeholders bool) (map[string]any, error) {
	data := map[string]any{}
	known := map[string]bool{}
	var missing []string
	for _, prm := range p.Params {
		known[prm.Name] = true
		if v, ok := overrides[prm.Name]; ok {
			data[prm.Name] = v
		} else if prm.Required {
			if placeholders {
				data[prm.Name] = "<" + prm.Name + ">"
			} else {
				missing = append(missing, prm.Name)
			}
		} else {
			data[prm.Name] = prm.Default
		}
	}
	var unknown []string
	for k := range overrides {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	switch {
	case len(unknown) > 0:
		return nil, fmt.Errorf("preset %s: unknown override %v (known: %s)", p.Ref(), unknown, strings.Join(p.paramNames(), ", "))
	case len(missing) > 0:
		return nil, fmt.Errorf("preset %s: required override missing: %s", p.Ref(), strings.Join(missing, ", "))
	}
	return data, nil
}

func (p *Preset) paramNames() []string {
	out := make([]string, len(p.Params))
	for i, prm := range p.Params {
		out[i] = prm.Name
	}
	return out
}

func (p *Preset) exec(data map[string]any) (string, error) {
	var b bytes.Buffer
	if err := p.body.Execute(&b, data); err != nil {
		return "", fmt.Errorf("preset %s: %w", p.Ref(), err)
	}
	return b.String(), nil
}

// Registry holds the built-in presets plus any loaded from preset_dirs.
type Registry struct {
	byName map[string][]*Preset // versions ascending
}

// NewRegistry loads the built-in presets and every *.yaml in dirs.
func NewRegistry(dirs ...string) (*Registry, error) {
	r := &Registry{byName: map[string][]*Preset{}}
	entries, _ := fs.Glob(builtinFS, "builtin/*.yaml")
	for _, name := range entries {
		raw, _ := builtinFS.ReadFile(name)
		p, err := parsePreset(raw, "builtin")
		if err != nil {
			return nil, err
		}
		if err := r.add(p); err != nil {
			return nil, err
		}
	}
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			if _, err := os.Stat(dir); err != nil {
				return nil, fmt.Errorf("preset_dirs: %w", err)
			}
		}
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			p, err := parsePreset(raw, f)
			if err != nil {
				return nil, err
			}
			if err := r.add(p); err != nil {
				return nil, err
			}
		}
	}
	return r, nil
}

func (r *Registry) add(p *Preset) error {
	for _, q := range r.byName[p.Name] {
		if q.Version == p.Version {
			return fmt.Errorf("preset %s defined twice (%s and %s)", p.Ref(), q.Source, p.Source)
		}
	}
	list := append(r.byName[p.Name], p)
	sort.Slice(list, func(i, j int) bool { return list[i].Version < list[j].Version })
	r.byName[p.Name] = list
	return nil
}

// Get resolves "name" (latest version) or "name@N" (pinned version). Pinning
// is recommended: a newer preset version never changes a pinned service.
func (r *Registry) Get(ref string) (*Preset, error) {
	name, ver, pinned := strings.Cut(ref, "@")
	list := r.byName[name]
	if len(list) == 0 {
		return nil, fmt.Errorf("unknown preset %q (available: %s)", name, strings.Join(r.names(), ", "))
	}
	if !pinned {
		return list[len(list)-1], nil
	}
	n, err := strconv.Atoi(ver)
	if err != nil {
		return nil, fmt.Errorf("preset %q: version must be a number", ref)
	}
	for _, p := range list {
		if p.Version == n {
			return p, nil
		}
	}
	return nil, fmt.Errorf("preset %s@%d not found (versions: %d..%d)", name, n, list[0].Version, list[len(list)-1].Version)
}

// List returns every preset version, sorted by name then version.
func (r *Registry) List() []*Preset {
	var out []*Preset
	for _, n := range r.names() {
		out = append(out, r.byName[n]...)
	}
	return out
}

func (r *Registry) names() []string {
	var out []string
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
