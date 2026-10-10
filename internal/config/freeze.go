package config

import (
	"fmt"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // weekly windows name a time zone; do not depend on the host's zoneinfo
)

// Freeze is a change-freeze window declared in the config: an absolute
// period (start/end, RFC 3339) or a weekly recurring one. New deployments
// and phase starts are refused while it is active; automatic rollback stays
// allowed unless allow_rollback is false (manual rollback always is).
type Freeze struct {
	Name     string        `yaml:"name"`
	Reason   string        `yaml:"reason"`
	Start    string        `yaml:"start"` // RFC 3339, with Weekly unset
	End      string        `yaml:"end"`
	Weekly   *WeeklyWindow `yaml:"weekly"`
	Services []string      `yaml:"services"` // empty with no teams = every service
	Teams    []string      `yaml:"teams"`
	// AllowRollback keeps automatic rollback working during the freeze
	// (default true: recovering from a failure is not a change).
	AllowRollback *bool `yaml:"allow_rollback"`

	start, end time.Time
}

// WeeklyWindow repeats every week, e.g. Friday 18:00 to Monday 09:00.
type WeeklyWindow struct {
	From     string `yaml:"from"` // "fri 18:00"
	To       string `yaml:"to"`   // "mon 09:00"
	Timezone string `yaml:"timezone"`

	from, to int // minutes since Monday 00:00
	loc      *time.Location
}

var weekdays = map[string]int{"mon": 0, "tue": 1, "wed": 2, "thu": 3, "fri": 4, "sat": 5, "sun": 6}

func parseWeekMinute(s string) (int, error) {
	day, hm, ok := strings.Cut(strings.ToLower(strings.TrimSpace(s)), " ")
	d, okDay := weekdays[day]
	var h, m int
	if !ok || !okDay {
		return 0, fmt.Errorf("%q: use \"<mon..sun> HH:MM\"", s)
	}
	if _, err := fmt.Sscanf(hm, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q: time must be HH:MM", s)
	}
	return d*24*60 + h*60 + m, nil
}

func (f *Freeze) prepare() error {
	if f.Weekly != nil {
		if f.Start != "" || f.End != "" {
			return fmt.Errorf("use either start/end or weekly, not both")
		}
		w := f.Weekly
		var err error
		if w.from, err = parseWeekMinute(w.From); err != nil {
			return fmt.Errorf("weekly.from %w", err)
		}
		if w.to, err = parseWeekMinute(w.To); err != nil {
			return fmt.Errorf("weekly.to %w", err)
		}
		if w.from == w.to {
			return fmt.Errorf("weekly.from and weekly.to are the same moment")
		}
		tz := w.Timezone
		if tz == "" {
			tz = "Local"
		}
		if w.loc, err = time.LoadLocation(tz); err != nil {
			return fmt.Errorf("weekly.timezone: %w", err)
		}
		return nil
	}
	var err error
	if f.start, err = time.Parse(time.RFC3339, f.Start); err != nil {
		return fmt.Errorf("start must be RFC 3339 (e.g. 2026-12-24T00:00:00+09:00): %w", err)
	}
	if f.end, err = time.Parse(time.RFC3339, f.End); err != nil {
		return fmt.Errorf("end must be RFC 3339: %w", err)
	}
	if !f.end.After(f.start) {
		return fmt.Errorf("end must be after start")
	}
	return nil
}

// ActiveAt reports whether the window covers t and when it ends.
func (f *Freeze) ActiveAt(t time.Time) (until time.Time, active bool) {
	if w := f.Weekly; w != nil && w.loc != nil {
		lt := t.In(w.loc)
		wd := (int(lt.Weekday()) + 6) % 7 // Monday = 0
		now := wd*24*60 + lt.Hour()*60 + lt.Minute()
		const week = 7 * 24 * 60
		var in bool
		var left int
		if w.from < w.to {
			in, left = now >= w.from && now < w.to, w.to-now
		} else { // wraps over the end of the week (e.g. sun 22:00 -> mon 06:00)
			in = now >= w.from || now < w.to
			left = (w.to - now + week) % week
		}
		if !in {
			return time.Time{}, false
		}
		end := lt.Truncate(time.Minute).Add(time.Duration(left) * time.Minute)
		return end, true
	}
	if f.start.IsZero() {
		return time.Time{}, false
	}
	return f.end, !t.Before(f.start) && t.Before(f.end)
}

// Covers reports whether the window applies to a service of a team.
func (f *Freeze) Covers(service, team string) bool {
	if len(f.Services) == 0 && len(f.Teams) == 0 {
		return true
	}
	return slices.Contains(f.Services, service) || (team != "" && slices.Contains(f.Teams, team))
}

// RollbackAllowed is the effective allow_rollback (default true).
func (f *Freeze) RollbackAllowed() bool { return f.AllowRollback == nil || *f.AllowRollback }

func (c *Config) validateFreezes(bad func(string, ...any)) {
	seen := map[string]bool{}
	for i := range c.ChangeFreeze {
		f := &c.ChangeFreeze[i]
		where := fmt.Sprintf("change_freeze[%d]", i)
		if f.Name == "" {
			bad("%s: name required", where)
		} else if seen[f.Name] {
			bad("%s: duplicate name %q", where, f.Name)
		}
		seen[f.Name] = true
		if err := f.prepare(); err != nil {
			bad("%s %q: %v", where, f.Name, err)
		}
		for _, s := range f.Services {
			if _, ok := c.Service(s); !ok {
				bad("%s %q: unknown service %q", where, f.Name, s)
			}
		}
	}
}
