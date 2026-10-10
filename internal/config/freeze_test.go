package config

import (
	"strings"
	"testing"
	"time"
)

func TestWeeklyFreezeWindows(t *testing.T) {
	seoul, _ := time.LoadLocation("Asia/Seoul")
	weekend := Freeze{Name: "weekend", Weekly: &WeeklyWindow{From: "fri 18:00", To: "mon 09:00", Timezone: "Asia/Seoul"}}
	if err := weekend.prepare(); err != nil {
		t.Fatal(err)
	}
	// 2026-10-09 is a Friday.
	cases := []struct {
		at        time.Time
		active    bool
		untilWant time.Time
	}{
		{time.Date(2026, 10, 9, 17, 59, 0, 0, seoul), false, time.Time{}},
		{time.Date(2026, 10, 9, 18, 0, 0, 0, seoul), true, time.Date(2026, 10, 12, 9, 0, 0, 0, seoul)},
		{time.Date(2026, 10, 10, 12, 30, 0, 0, seoul), true, time.Date(2026, 10, 12, 9, 0, 0, 0, seoul)},
		{time.Date(2026, 10, 12, 8, 59, 0, 0, seoul), true, time.Date(2026, 10, 12, 9, 0, 0, 0, seoul)},
		{time.Date(2026, 10, 12, 9, 0, 0, 0, seoul), false, time.Time{}},
		{time.Date(2026, 10, 14, 12, 0, 0, 0, seoul), false, time.Time{}},
		// The same instant expressed in UTC is judged in the window's zone.
		{time.Date(2026, 10, 10, 3, 30, 0, 0, time.UTC), true, time.Date(2026, 10, 12, 9, 0, 0, 0, seoul)},
	}
	for _, c := range cases {
		until, active := weekend.ActiveAt(c.at)
		if active != c.active || (active && !until.Equal(c.untilWant)) {
			t.Errorf("%s: active=%v until=%s, want %v %s", c.at, active, until, c.active, c.untilWant)
		}
	}
	// A window inside one week that does not wrap.
	night := Freeze{Name: "batch", Weekly: &WeeklyWindow{From: "wed 01:00", To: "wed 03:00", Timezone: "UTC"}}
	if err := night.prepare(); err != nil {
		t.Fatal(err)
	}
	if _, ok := night.ActiveAt(time.Date(2026, 10, 14, 2, 0, 0, 0, time.UTC)); !ok {
		t.Error("wed 02:00 must be frozen")
	}
	if _, ok := night.ActiveAt(time.Date(2026, 10, 15, 2, 0, 0, 0, time.UTC)); ok {
		t.Error("thu 02:00 must not be frozen")
	}
}

func TestFreezeValidation(t *testing.T) {
	base := `
version: v1
targets: [{name: a}]
executors: {x: {type: exec, exec: {rollback: "true"}}}
services:
  - name: s
    team: pay
    targets: [a]
    probes: [{id: h, type: tcp, tcp: {address: "x:1"}}]
    rules: [{name: down, when: {metric: h.up, op: "==", value: 0}}]
    rollback: {executor: x, mode: auto}
change_freeze:
`
	for tail, want := range map[string]string{
		"  - {start: \"2026-12-24T00:00:00+09:00\", end: \"2026-12-26T00:00:00+09:00\"}\n":                                                            "name required",
		"  - {name: x, start: \"2026-12-26T00:00:00+09:00\", end: \"2026-12-24T00:00:00+09:00\"}\n":                                                   "end must be after start",
		"  - {name: x, start: tomorrow, end: \"2026-12-24T00:00:00+09:00\"}\n":                                                                        "RFC 3339",
		"  - {name: x, weekly: {from: \"fri 18:00\", to: \"funday 09:00\"}}\n":                                                                        "weekly.to",
		"  - {name: x, weekly: {from: \"fri 25:00\", to: \"mon 09:00\"}}\n":                                                                           "HH:MM",
		"  - {name: x, weekly: {from: \"fri 18:00\", to: \"mon 09:00\", timezone: Mars/Base}}\n":                                                      "timezone",
		"  - {name: x, start: \"2026-12-24T00:00:00+09:00\", end: \"2026-12-25T00:00:00+09:00\", weekly: {from: \"fri 18:00\", to: \"mon 09:00\"}}\n": "either start/end or weekly",
		"  - {name: x, start: \"2026-12-24T00:00:00+09:00\", end: \"2026-12-25T00:00:00+09:00\", services: [nope]}\n":                                 "unknown service",
		"  - {name: x, weekly: {from: \"fri 18:00\", to: \"mon 09:00\"}}\n  - {name: x, weekly: {from: \"sat 18:00\", to: \"sun 09:00\"}}\n":          "duplicate name",
	} {
		if _, err := Parse([]byte(base + tail)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", strings.TrimSpace(tail), err, want)
		}
	}
	c, err := Parse([]byte(base + "  - {name: year-end, reason: closing, start: \"2026-12-28T00:00:00+09:00\", end: \"2027-01-02T00:00:00+09:00\", teams: [pay], allow_rollback: false}\n"))
	if err != nil {
		t.Fatal(err)
	}
	f := c.ChangeFreeze[0]
	if f.RollbackAllowed() || !f.Covers("s", "pay") || f.Covers("other", "search") {
		t.Fatalf("freeze %+v", f)
	}
	if _, ok := f.ActiveAt(time.Date(2026, 12, 30, 0, 0, 0, 0, time.UTC)); !ok {
		t.Fatal("inside the absolute window")
	}
}
