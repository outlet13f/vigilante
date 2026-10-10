package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"vigilante/internal/sudoers"
	"vigilante/internal/transport"
)

// cmdSudoers prints the sudoers rules for hosts using connection.sudo_scope:
// changes: exactly the commands Vigilante runs with sudo, nothing else.
//
//	vigilante sudoers -c FILE [--target HOST] [--no-resolve] [--json]
func cmdSudoers(ctx context.Context, args []string) (int, error) {
	c := newFlags("sudoers")
	target := c.fs.String("target", "", "only this host")
	noResolve := c.fs.Bool("no-resolve", false, "do not connect to hosts to find command paths (use common defaults)")
	asJSON := c.fs.Bool("json", false, "the rules as JSON")
	if err := c.fs.Parse(args); err != nil {
		return 1, err
	}
	cfg, err := loadConfig(c.config)
	if err != nil {
		return 1, err
	}
	rules, notes := sudoers.Rules(cfg)
	var hosts []*sudoers.Host
	for _, h := range sudoers.Group(cfg, rules) {
		if *target == "" || h.Name == *target {
			hosts = append(hosts, h)
		}
	}
	if *asJSON {
		printJSON(map[string]any{"hosts": hosts, "notes": notes})
		return 0, nil
	}
	if len(hosts) == 0 {
		fmt.Println("# no command needs sudo here: the configured strategies use APIs, sockets or operator-written commands")
	}
	if !*noResolve {
		mgr := transport.NewManager(cfg)
		defer mgr.Close()
		for _, h := range hosts {
			r, err := mgr.ForTarget(h.Name)
			if err != nil {
				h.Comments = append(h.Comments, "command paths not checked: "+err.Error())
				r = nil
			}
			hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			for _, rule := range h.Rules {
				if _, done := h.Paths[rule.Command]; done {
					continue
				}
				p, ok := sudoers.Resolve(hctx, r, rule.Command)
				h.Paths[rule.Command], h.Guessed[rule.Command] = p, !ok
			}
			cancel()
		}
	}
	for _, n := range notes {
		fmt.Println("# NOTE: " + n)
	}
	for i, h := range hosts {
		if i > 0 || len(notes) > 0 {
			fmt.Println()
		}
		fmt.Print(h.Render())
	}
	if len(hosts) > 0 {
		names := make([]string, len(hosts))
		for i, h := range hosts {
			names[i] = h.Name
		}
		fmt.Printf("\n# Check with: vigilante doctor -c %s   (verifies each rule on %s with sudo -n -l)\n", c.config, strings.Join(names, ", "))
	}
	return 0, nil
}
