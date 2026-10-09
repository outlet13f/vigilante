package probe

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"vigilante/internal/config"
)

func init() { Register("host", newHost) }

// hostCmd gathers everything in one round trip; sections are separated by markers.
const hostCmd = `cat /proc/loadavg; echo @@; cat /proc/stat | head -1; echo @@; cat /proc/meminfo; echo @@; cat /proc/diskstats; echo @@; (nproc 2>/dev/null || getconf _NPROCESSORS_ONLN)`

// Host probe reads /proc over the target's runner (SSH or local) — no agent.
// Metrics: load1, load_per_cpu, cpu_busy_pct, mem_available_pct,
// disk_util_pct (busiest device), cpu_count.
type hostProbe struct {
	spec    config.Probe
	env     Env
	devices map[string]bool
	prev    *hostSnap
}

type hostSnap struct {
	at       time.Time
	cpuTotal uint64
	cpuIdle  uint64
	ioTicks  map[string]uint64 // device -> ms spent doing IO
}

func newHost(spec config.Probe, env Env) (Probe, error) {
	if env.Runner == nil {
		return nil, errors.New("host probe needs an ssh or local connection")
	}
	p := &hostProbe{spec: spec, env: env}
	if spec.Host != nil && len(spec.Host.Devices) > 0 {
		p.devices = map[string]bool{}
		for _, d := range spec.Host.Devices {
			p.devices[d] = true
		}
	}
	return p, nil
}

func (p *hostProbe) Run(ctx context.Context, emit Emit) error {
	return poll(ctx, p.spec.Interval, func(ctx context.Context) {
		cctx, cancel := context.WithTimeout(ctx, max(p.spec.Timeout, 5*time.Second))
		defer cancel()
		out, err := p.env.Runner.Run(cctx, hostCmd, nil)
		if err != nil {
			p.env.Log.Debug("host probe failed", "err", err)
			emit("up", 0)
			return
		}
		snap, m, err := parseHost(out, p.devices)
		if err != nil {
			p.env.Log.Debug("host probe parse failed", "err", err)
			emit("up", 0)
			return
		}
		emit("up", 1)
		for k, v := range m {
			emit(k, v)
		}
		if p.prev != nil {
			if dt := snap.cpuTotal - p.prev.cpuTotal; dt > 0 && snap.cpuTotal > p.prev.cpuTotal {
				idle := float64(snap.cpuIdle-p.prev.cpuIdle) / float64(dt)
				emit("cpu_busy_pct", 100*(1-idle))
			}
			elapsed := snap.at.Sub(p.prev.at).Milliseconds()
			if elapsed > 0 {
				worst := 0.0
				for dev, ticks := range snap.ioTicks {
					if before, ok := p.prev.ioTicks[dev]; ok && ticks >= before {
						worst = max(worst, min(100, 100*float64(ticks-before)/float64(elapsed)))
					}
				}
				emit("disk_util_pct", worst)
			}
		}
		p.prev = snap
	})
}

func parseHost(out string, devices map[string]bool) (*hostSnap, map[string]float64, error) {
	parts := strings.Split(out, "@@")
	if len(parts) != 5 {
		return nil, nil, fmt.Errorf("unexpected host output (%d sections)", len(parts))
	}
	m := map[string]float64{}
	snap := &hostSnap{at: time.Now(), ioTicks: map[string]uint64{}}

	load := strings.Fields(parts[0])
	if len(load) < 1 {
		return nil, nil, errors.New("empty loadavg")
	}
	load1, err := strconv.ParseFloat(load[0], 64)
	if err != nil {
		return nil, nil, err
	}
	m["load1"] = load1
	cpus, _ := strconv.Atoi(strings.TrimSpace(parts[4]))
	if cpus > 0 {
		m["cpu_count"] = float64(cpus)
		m["load_per_cpu"] = load1 / float64(cpus)
	}

	// cpu  user nice system idle iowait irq softirq steal ...
	if f := strings.Fields(parts[1]); len(f) > 5 && f[0] == "cpu" {
		for i, s := range f[1:] {
			v, _ := strconv.ParseUint(s, 10, 64)
			snap.cpuTotal += v
			if i == 3 || i == 4 { // idle + iowait
				snap.cpuIdle += v
			}
		}
	}

	var total, avail float64
	for _, line := range strings.Split(parts[2], "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseFloat(f[1], 64)
		switch f[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			avail = v
		}
	}
	if total > 0 {
		m["mem_available_pct"] = 100 * avail / total
		m["mem_available_mb"] = avail / 1024
	}

	for _, line := range strings.Split(parts[3], "\n") {
		f := strings.Fields(line)
		if len(f) < 14 {
			continue
		}
		dev := f[2]
		if devices != nil {
			if !devices[dev] {
				continue
			}
		} else if !isWholeDisk(dev) {
			continue
		}
		ticks, err := strconv.ParseUint(f[12], 10, 64) // field 10 of the stats: io_ticks (ms)
		if err == nil {
			snap.ioTicks[dev] = ticks
		}
	}
	return snap, m, nil
}

func isWholeDisk(dev string) bool {
	for _, p := range []string{"sd", "vd", "xvd", "hd"} {
		if strings.HasPrefix(dev, p) {
			last := dev[len(dev)-1]
			return last < '0' || last > '9'
		}
	}
	if strings.HasPrefix(dev, "nvme") || strings.HasPrefix(dev, "dm-") {
		return !strings.Contains(dev, "p") || strings.HasPrefix(dev, "dm-")
	}
	return false
}
