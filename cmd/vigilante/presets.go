package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"vigilante/internal/presets"
)

// setFlags collects repeated --set key=value pairs with typed values.
type setFlags map[string]any

func (s setFlags) String() string { return "" }
func (s setFlags) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("--set expects key=value, got %q", v)
	}
	s[k] = parseScalar(val)
	return nil
}

func parseScalar(v string) any {
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	if i, err := strconv.Atoi(v); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	return v
}

// cmdPresets: `presets` lists presets; `presets show NAME[@V] [--set k=v]...`
// prints the probes, rules and phases the preset expands to.
func cmdPresets(args []string) (int, error) {
	action := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	var ref string
	if action == "show" {
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			return 1, errors.New("presets show: preset name required, e.g. `vigilante presets show java-web`")
		}
		ref, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("presets", flag.ContinueOnError)
	dirs := fs.String("dir", "", "comma-separated directories with organisation presets")
	set := setFlags{}
	fs.Var(set, "set", "parameter override key=value (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 1, err
	}
	var dl []string
	if *dirs != "" {
		dl = strings.Split(*dirs, ",")
	}
	reg, err := presets.NewRegistry(dl...)
	if err != nil {
		return 1, err
	}
	switch action {
	case "list":
		for _, p := range reg.List() {
			fmt.Printf("%-24s %s\n", p.Ref(), p.Description)
			for _, prm := range p.Params {
				val := "required"
				if !prm.Required {
					val = fmt.Sprintf("default %v", prm.Default)
					if prm.Default == "" {
						val = `default ""`
					}
				}
				fmt.Printf("    %-22s %-16s %s\n", prm.Name, val, prm.Description)
			}
		}
		return 0, nil
	case "show":
		p, err := reg.Get(ref)
		if err != nil {
			return 1, err
		}
		out, err := p.Preview(set)
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(os.Stderr, "# %s (%s) — <name> marks a required override\n", p.Ref(), p.Source)
		fmt.Print(out)
		return 0, nil
	}
	return 1, fmt.Errorf("presets: unknown action %q (list | show)", action)
}
