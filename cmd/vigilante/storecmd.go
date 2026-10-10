package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"vigilante/internal/store"
)

// cmdStore inspects and migrates the PostgreSQL state store schema:
//
//	vigilante store status  -c FILE [--json]
//	vigilante store migrate -c FILE [--down-to N --yes]
func cmdStore(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return 1, errors.New("usage: vigilante store status|migrate -c FILE [--down-to N --yes] [--json]")
	}
	action := args[0]
	c := newFlags("store")
	downTo := c.fs.Int("down-to", 0, "migrate: revert the schema to this version (for a downgrade; run with the newer binary)")
	yes := c.fs.Bool("yes", false, "migrate --down-to: confirm (stop every server first)")
	asJSON := c.fs.Bool("json", false, "status: JSON output")
	if err := c.fs.Parse(args[1:]); err != nil {
		return 1, err
	}
	cfg, err := loadConfig(c.config)
	if err != nil {
		return 1, err
	}
	dsn, err := store.PostgresDSN(ctx, cfg)
	if err != nil {
		return 1, err
	}
	switch action {
	case "status":
		st, err := store.Schema(ctx, dsn)
		if err != nil {
			return 1, err
		}
		if *asJSON {
			printJSON(st)
		} else {
			fmt.Printf("schema: %d applied, this binary (%s) knows up to %d\n", len(st.Applied), version, st.Latest)
			for _, a := range st.Applied {
				note := ""
				if a.Breaking {
					note = " (breaking)"
				}
				by := a.By
				if by == "" {
					by = "unknown"
				}
				fmt.Printf("  %03d  applied %s by vigilante %s%s\n", a.Version, a.AppliedAt.UTC().Format("2006-01-02 15:04:05Z"), by, note)
			}
			if len(st.Pending) > 0 {
				fmt.Printf("pending: %v (applied when a server starts, or run: vigilante store migrate)\n", st.Pending)
			}
			if len(st.Unknown) > 0 {
				fmt.Printf("newer than this binary: %d migration(s); see docs/08-upgrade.md before running this release\n", len(st.Unknown))
			}
		}
		if len(st.Pending) > 0 || len(st.Unknown) > 0 {
			return 4, nil // not current: scripts can tell without parsing
		}
		return 0, nil
	case "migrate":
		if *downTo > 0 && !*yes {
			return 1, fmt.Errorf("reverting to schema %d changes the database for an older release: stop every vigilante server, take a backup, then repeat with --yes", *downTo)
		}
		changed, err := store.Migrate(ctx, dsn, *downTo)
		if err != nil {
			return 1, err
		}
		switch {
		case len(changed) == 0:
			fmt.Println("schema is already at the requested version")
		case *downTo > 0:
			fmt.Printf("reverted migrations %v; the schema is now at %d\n", changed, *downTo)
		default:
			fmt.Printf("applied migrations %v\n", changed)
		}
		return 0, nil
	}
	return 1, fmt.Errorf("unknown store action %q (status|migrate)", action)
}
