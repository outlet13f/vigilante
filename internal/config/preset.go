package config

import (
	"bytes"
	"fmt"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"vigilante/internal/presets"
)

// presetFragment is what a preset body renders to.
type presetFragment struct {
	Probes   []Probe                `yaml:"probes"`
	Rules    []Rule                 `yaml:"rules"`
	Baseline *Baseline              `yaml:"baseline"`
	Phases   map[string]PhaseConfig `yaml:"phases"`
}

// PresetRegistry loads built-in presets plus the config's preset_dirs.
func (c *Config) PresetRegistry(baseDir string) (*presets.Registry, error) {
	dirs := make([]string, len(c.PresetDirs))
	for i, d := range c.PresetDirs {
		if !filepath.IsAbs(d) {
			d = filepath.Join(baseDir, d)
		}
		dirs[i] = d
	}
	return presets.NewRegistry(dirs...)
}

// expandPresets merges each service's preset into the service. It runs before
// defaults and validation, so preset output is checked like hand-written YAML.
func (c *Config) expandPresets(baseDir string) error {
	var reg *presets.Registry
	for i := range c.Services {
		s := &c.Services[i]
		if s.Preset == "" {
			if len(s.Overrides) > 0 {
				return fmt.Errorf("service %q: overrides given without a preset", s.Name)
			}
			continue
		}
		if reg == nil {
			var err error
			if reg, err = c.PresetRegistry(baseDir); err != nil {
				return err
			}
		}
		p, err := reg.Get(s.Preset)
		if err != nil {
			return fmt.Errorf("service %q: %w", s.Name, err)
		}
		text, err := p.Render(s.Overrides)
		if err != nil {
			return fmt.Errorf("service %q: %w", s.Name, err)
		}
		var frag presetFragment
		dec := yaml.NewDecoder(bytes.NewReader([]byte(text)))
		dec.KnownFields(true)
		if err := dec.Decode(&frag); err != nil {
			return fmt.Errorf("service %q: preset %s rendered invalid YAML: %w", s.Name, p.Ref(), err)
		}
		s.Preset = p.Ref() // record the resolved version
		mergeFragment(s, frag)
	}
	return nil
}

func mergeFragment(s *Service, f presetFragment) {
	own := map[string]bool{}
	for _, p := range s.Probes {
		own[p.ID] = true
	}
	var probes []Probe
	for _, p := range f.Probes {
		if !own[p.ID] {
			probes = append(probes, p)
		}
	}
	s.Probes = append(probes, s.Probes...)

	own = map[string]bool{}
	for _, r := range s.Rules {
		own[r.Name] = true
	}
	var rules []Rule
	for _, r := range f.Rules {
		if !own[r.Name] {
			rules = append(rules, r)
		}
	}
	s.Rules = append(rules, s.Rules...)

	if s.Baseline.Window == 0 && s.Baseline.MinSamples == 0 && f.Baseline != nil {
		s.Baseline = *f.Baseline
	}
	if s.Phases == nil {
		s.Phases = map[string]PhaseConfig{}
	}
	// Phases merge field by field: a service that only sets canary targets
	// keeps the preset's observation window and warmup.
	for name, pp := range f.Phases {
		sp, ok := s.Phases[name]
		if !ok {
			s.Phases[name] = pp
			continue
		}
		if sp.ObservationWindow == 0 {
			sp.ObservationWindow = pp.ObservationWindow
		}
		if sp.Warmup == 0 {
			sp.Warmup = pp.Warmup
		}
		if sp.EvalInterval == 0 {
			sp.EvalInterval = pp.EvalInterval
		}
		if sp.MinSamples == 0 {
			sp.MinSamples = pp.MinSamples
		}
		if sp.OnInconclusive == "" {
			sp.OnInconclusive = pp.OnInconclusive
		}
		if len(sp.Rules) == 0 {
			sp.Rules = pp.Rules
		}
		s.Phases[name] = sp
	}
}
