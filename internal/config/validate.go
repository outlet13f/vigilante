package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

func (c *Config) applyDefaults() {
	if c.API.RateLimit == nil {
		c.API.RateLimit = &RateLimit{Rate: 20, Burst: 40}
	}
	if c.API.EmergencyRateLimit == nil {
		c.API.EmergencyRateLimit = &RateLimit{Rate: 1, Burst: 10}
	}
	if c.API.TokenTTL == 0 {
		c.API.TokenTTL = time.Hour
	}
	if c.Server.Listen == "" {
		c.Server.Listen = ":8088"
	}
	if c.Server.JournalPath == "" {
		c.Server.JournalPath = "vigilante-journal.jsonl"
	}
	if c.Server.State.Backend == "" {
		c.Server.State.Backend = "file"
	}
	if c.Server.HA.LeaseTTL == 0 {
		c.Server.HA.LeaseTTL = 15 * time.Second
	}
	if c.Agent.PushInterval == 0 {
		c.Agent.PushInterval = time.Second
	}
	if c.Agent.HeartbeatInterval == 0 {
		c.Agent.HeartbeatInterval = 5 * time.Second
	}
	if c.Agent.FailsafeAfter == 0 {
		c.Agent.FailsafeAfter = 30 * time.Second
	}
	if c.Agent.Failsafe == "" {
		c.Agent.Failsafe = "hold"
	}
	for i := range c.Targets {
		t := &c.Targets[i]
		if t.Connection.Type == "" {
			t.Connection.Type = "none"
		}
		if t.Connection.Port == 0 && t.Connection.Type == "ssh" {
			t.Connection.Port = 22
		}
		if t.Connection.Timeout == 0 {
			t.Connection.Timeout = 10 * time.Second
		}
	}
	for name, tr := range c.Traffic {
		if tr.Nginx != nil {
			if tr.Nginx.MemberFormat == "" {
				tr.Nginx.MemberFormat = "{{.Address}}:80"
			}
			if tr.Nginx.TestCmd == "" {
				tr.Nginx.TestCmd = "nginx -t"
			}
			if tr.Nginx.ReloadCmd == "" {
				tr.Nginx.ReloadCmd = "nginx -s reload"
			}
		}
		if tr.HAProxy != nil && tr.HAProxy.ServerName == "" {
			tr.HAProxy.ServerName = "{{.Name}}"
		}
		if tr.Envoy != nil && tr.Envoy.MemberFormat == "" {
			tr.Envoy.MemberFormat = "{{.Address}}:80"
		}
		if tr.F5 != nil && tr.F5.MemberFormat == "" {
			tr.F5.MemberFormat = "{{.Address}}:80"
		}
		if tr.AWSALB != nil {
			if tr.AWSALB.TargetID == "" {
				tr.AWSALB.TargetID = "{{.Labels.instance_id}}"
			}
			if tr.AWSALB.WaitTimeout == 0 {
				tr.AWSALB.WaitTimeout = 5 * time.Minute
			}
		}
		c.Traffic[name] = tr
	}
	for name, ex := range c.Executors {
		if ex.Symlink != nil {
			if ex.Symlink.Release == "" {
				ex.Symlink.Release = "{{.PreviousVersion}}"
			}
			if ex.Symlink.Init == "" {
				ex.Symlink.Init = "systemd"
			}
		}
		if ex.Container != nil {
			if ex.Container.Tag == "" {
				ex.Container.Tag = "{{.PreviousVersion}}"
			}
			if ex.Container.Socket == "" && ex.Container.Host == "" {
				ex.Container.Socket = "/var/run/docker.sock"
			}
			if ex.Container.StopTimeoutSec == 0 {
				ex.Container.StopTimeoutSec = 20
			}
		}
		if ex.VSphere != nil && ex.VSphere.Snapshot == "" {
			ex.VSphere.Snapshot = "vigilante-{{.Service}}-{{.Version}}"
		}
		if ex.KVM != nil && ex.KVM.Snapshot == "" {
			ex.KVM.Snapshot = "vigilante-{{.Service}}-{{.Version}}"
		}
		if ex.Nutanix != nil && ex.Nutanix.Snapshot == "" {
			ex.Nutanix.Snapshot = "vigilante-{{.Service}}-{{.Version}}"
		}
		if ex.Webhook != nil && ex.Webhook.Method == "" {
			ex.Webhook.Method = "POST"
		}
		c.Executors[name] = ex
	}
	for i := range c.Services {
		s := &c.Services[i]
		for j := range s.Probes {
			p := &s.Probes[j]
			if p.Interval == 0 {
				p.Interval = 2 * time.Second
			}
			if p.Timeout == 0 {
				p.Timeout = time.Second
			}
			if p.HTTP != nil && p.HTTP.Method == "" {
				p.HTTP.Method = "GET"
			}
			if a := p.AccessLog; a != nil {
				if a.Format == "" {
					a.Format = "combined"
				}
				if a.StatusField == "" {
					a.StatusField = "status"
				}
				if a.LatencyField == "" {
					a.LatencyField = "request_time"
				}
				if a.LatencyUnit == "" {
					a.LatencyUnit = "s"
				}
			}
			if p.DB != nil {
				if p.DB.PoolSize == 0 {
					p.DB.PoolSize = 3
				}
				if p.DB.Query == "" {
					p.DB.Query = "SELECT 1"
				}
			}
		}
		if s.Baseline.Window == 0 {
			s.Baseline.Window = 5 * time.Minute
		}
		for j := range s.Rules {
			if s.Rules[j].Action == "" {
				s.Rules[j].Action = "rollback"
			}
			defaultNode(&s.Rules[j].When)
		}
		for name, pc := range s.Phases {
			if pc.ObservationWindow == 0 {
				pc.ObservationWindow = 5 * time.Minute
			}
			if pc.EvalInterval == 0 {
				pc.EvalInterval = 5 * time.Second
			}
			if pc.OnInconclusive == "" {
				pc.OnInconclusive = "hold"
			}
			s.Phases[name] = pc
		}
		r := &s.Rollback
		if r.Scope == "" {
			r.Scope = "deployed"
		}
		if r.Parallelism == 0 {
			r.Parallelism = 1
		}
		if r.StepTimeout == 0 {
			r.StepTimeout = 2 * time.Minute
		}
		if r.Retry.Attempts == 0 {
			r.Retry.Attempts = 3
		}
		if r.Retry.Backoff == 0 {
			r.Retry.Backoff = 2 * time.Second
		}
		if r.Retry.MaxBackoff == 0 {
			r.Retry.MaxBackoff = 30 * time.Second
		}
		if len(r.Plan) == 0 {
			if r.Traffic != "" {
				r.Plan = append(r.Plan, Step{Action: "traffic.drain"})
			}
			r.Plan = append(r.Plan, Step{Action: "app.rollback"}, Step{Action: "app.verify"})
			if r.Traffic != "" {
				r.Plan = append(r.Plan, Step{Action: "traffic.enable"})
			}
		}
		for j := range r.Plan {
			if r.Plan[j].Action == "probe.verify" && r.Plan[j].Successes == 0 {
				r.Plan[j].Successes = 3
			}
		}
	}
	cb := &c.Safety.CircuitBreaker
	if cb.FailureThreshold == 0 {
		cb.FailureThreshold = 3
	}
	if cb.Window == 0 {
		cb.Window = time.Hour
	}
	if c.Safety.BlastRadius.MinHealthy == 0 && c.Safety.BlastRadius.MinHealthyPercent == 0 {
		c.Safety.BlastRadius.MinHealthy = 1
	}
	if c.Safety.Flapping.MaxRollbacksPerHour == 0 {
		c.Safety.Flapping.MaxRollbacksPerHour = 3
	}
}

func defaultNode(n *Node) {
	for i := range n.Any {
		defaultNode(&n.Any[i])
	}
	for i := range n.All {
		defaultNode(&n.All[i])
	}
	if n.Not != nil {
		defaultNode(n.Not)
	}
	if n.Metric == "" {
		return
	}
	if n.Agg == "" {
		if n.RatioOf != "" {
			n.Agg = "sum"
		} else {
			n.Agg = "last"
		}
	}
	if n.Window == 0 {
		n.Window = 30 * time.Second
	}
	if n.Op == "" {
		n.Op = ">"
	}
	if n.For == 0 {
		n.For = 1
	}
	if n.ResetAfter == 0 {
		n.ResetAfter = 1
	}
	if n.Scope == "" {
		n.Scope = "target"
	}
	if n.Absent == "" {
		n.Absent = "unknown"
	}
}

var (
	validProbeTypes   = set("http", "grpc", "tcp", "host", "docker", "log", "access_log", "db")
	validExecTypes    = set("symlink", "container", "vsphere", "nutanix", "kvm", "exec", "webhook")
	validTrafficTypes = set("nginx", "haproxy", "envoy", "f5", "aws_alb")
	validAggs         = set("last", "avg", "min", "max", "sum", "count", "rate", "p50", "p90", "p95", "p99")
	validOps          = set(">", ">=", "<", "<=", "==", "!=")
	validStepActions  = set("traffic.drain", "traffic.enable", "app.rollback", "app.verify", "probe.verify", "wait")
	validConnTypes    = set("ssh", "local", "none")
	validRuleActions  = set("rollback", "notify", "hold")
	validInconclusive = set("hold", "pass", "rollback")
	validPhases       = set("canary", "rolling", "full")
)

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// Validate checks types and every cross reference so that a bad config is
// rejected by `vigilante validate` in CI rather than at 3 a.m. mid-rollback.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	for name, rl := range map[string]*RateLimit{"api.rate_limit": c.API.RateLimit, "api.emergency_rate_limit": c.API.EmergencyRateLimit} {
		if rl != nil && (rl.Rate < 0 || rl.Burst < 0 || rl.Daily < 0 || (rl.Rate > 0 && rl.Burst < 1)) {
			bad("%s: rate, burst and daily must be >= 0, and burst >= 1 when rate > 0", name)
		}
	}
	if c.API.TokenTTL < 0 || c.API.TokenTTL > 24*time.Hour {
		bad("api.token_ttl must be between 1s and 24h")
	}

	targets := map[string]bool{}
	for _, t := range c.Targets {
		if t.Name == "" {
			bad("target with empty name")
			continue
		}
		if targets[t.Name] {
			bad("target %q: duplicate name", t.Name)
		}
		targets[t.Name] = true
		if !validConnTypes[t.Connection.Type] {
			bad("target %q: unknown connection type %q", t.Name, t.Connection.Type)
		}
		if t.Connection.Type == "ssh" {
			if t.Address == "" {
				bad("target %q: ssh connection needs address", t.Name)
			}
			cred, ok := c.Credentials[t.Connection.Credential]
			if !ok {
				bad("target %q: unknown credential %q", t.Name, t.Connection.Credential)
			} else if cred.Type != "ssh" {
				bad("target %q: credential %q is not of type ssh", t.Name, t.Connection.Credential)
			}
		}
	}
	for _, t := range c.Targets {
		if t.Connection.Bastion != "" && !targets[t.Connection.Bastion] {
			bad("target %q: unknown bastion %q", t.Name, t.Connection.Bastion)
		}
	}
	checkHosts := func(kind, name string, hosts []string) {
		if len(hosts) == 0 {
			bad("%s %q: hosts must list at least one target", kind, name)
		}
		for _, h := range hosts {
			if !targets[h] {
				bad("%s %q: unknown host target %q", kind, name, h)
			}
		}
	}
	checkCred := func(kind, name, cred string) {
		if cred == "" {
			return
		}
		if _, ok := c.Credentials[cred]; !ok {
			bad("%s %q: unknown credential %q", kind, name, cred)
		}
	}

	for name, tr := range c.Traffic {
		if !validTrafficTypes[tr.Type] {
			bad("traffic %q: unknown type %q", name, tr.Type)
			continue
		}
		switch tr.Type {
		case "nginx":
			if tr.Nginx == nil || tr.Nginx.UpstreamFile == "" {
				bad("traffic %q: nginx.upstream_file required", name)
			} else {
				checkHosts("traffic", name, tr.Nginx.Hosts)
			}
		case "haproxy":
			if tr.HAProxy == nil || tr.HAProxy.Backend == "" || (tr.HAProxy.Socket == "" && tr.HAProxy.Address == "") {
				bad("traffic %q: haproxy.backend and haproxy.socket|address required", name)
			} else if tr.HAProxy.Socket != "" {
				checkHosts("traffic", name, tr.HAProxy.Hosts)
			}
		case "envoy":
			if tr.Envoy == nil || tr.Envoy.EDSFile == "" {
				bad("traffic %q: envoy.eds_file required", name)
			} else {
				checkHosts("traffic", name, tr.Envoy.Hosts)
			}
		case "f5":
			if tr.F5 == nil || tr.F5.URL == "" || tr.F5.Pool == "" {
				bad("traffic %q: f5.url and f5.pool required", name)
			} else {
				checkCred("traffic", name, tr.F5.Credential)
			}
		case "aws_alb":
			if tr.AWSALB == nil || tr.AWSALB.TargetGroupARN == "" {
				bad("traffic %q: aws_alb.target_group_arn required", name)
			} else {
				checkCred("traffic", name, tr.AWSALB.Credential)
			}
		}
	}

	for name, ex := range c.Executors {
		if !validExecTypes[ex.Type] {
			bad("executor %q: unknown type %q", name, ex.Type)
			continue
		}
		switch ex.Type {
		case "symlink":
			if ex.Symlink == nil || ex.Symlink.Link == "" || ex.Symlink.ReleasesDir == "" {
				bad("executor %q: symlink.link and symlink.releases_dir required", name)
			} else if ex.Symlink.RestartCmd == "" && ex.Symlink.Init != "none" && ex.Symlink.Unit == "" {
				bad("executor %q: symlink.unit or symlink.restart_cmd required", name)
			}
		case "container":
			if ex.Container == nil || ex.Container.Name == "" {
				bad("executor %q: container.name required", name)
			}
		case "vsphere":
			if ex.VSphere == nil || ex.VSphere.URL == "" || ex.VSphere.VM == "" {
				bad("executor %q: vsphere.url and vsphere.vm required", name)
			} else {
				checkCred("executor", name, ex.VSphere.Credential)
			}
		case "nutanix":
			if ex.Nutanix == nil || ex.Nutanix.URL == "" || ex.Nutanix.VMUUID == "" {
				bad("executor %q: nutanix.url and nutanix.vm_uuid required", name)
			} else {
				checkCred("executor", name, ex.Nutanix.Credential)
			}
		case "kvm":
			if ex.KVM == nil || ex.KVM.Domain == "" || ex.KVM.Hypervisor == "" {
				bad("executor %q: kvm.hypervisor and kvm.domain required", name)
			} else if !targets[ex.KVM.Hypervisor] {
				bad("executor %q: unknown hypervisor target %q", name, ex.KVM.Hypervisor)
			}
		case "exec":
			if ex.Exec == nil || ex.Exec.Rollback == "" {
				bad("executor %q: exec.rollback required", name)
			} else if on := ex.Exec.OnHost; on != "" && on != "target" && on != "local" && !targets[on] {
				bad("executor %q: exec.on refers to unknown target %q", name, on)
			}
		case "webhook":
			if ex.Webhook == nil || ex.Webhook.URL == "" {
				bad("executor %q: webhook.url required", name)
			} else {
				checkCred("executor", name, ex.Webhook.Credential)
			}
		}
	}

	services := map[string]bool{}
	for _, s := range c.Services {
		if s.Name == "" {
			bad("service with empty name")
			continue
		}
		if services[s.Name] {
			bad("service %q: duplicate name", s.Name)
		}
		services[s.Name] = true
		if len(s.Targets) == 0 {
			bad("service %q: no targets", s.Name)
		}
		for _, t := range s.Targets {
			if !targets[t] {
				bad("service %q: unknown target %q", s.Name, t)
			}
		}
		for _, t := range s.ControlTargets {
			if t != "auto" && !targets[t] {
				bad("service %q: unknown control target %q", s.Name, t)
			}
		}
		probes := map[string]bool{}
		for _, p := range s.Probes {
			if p.ID == "" || strings.Contains(p.ID, ".") {
				bad("service %q: probe id %q must be non-empty and contain no dots", s.Name, p.ID)
			}
			if probes[p.ID] {
				bad("service %q: duplicate probe id %q", s.Name, p.ID)
			}
			probes[p.ID] = true
			if err := validateProbe(p); err != nil {
				bad("service %q probe %q: %v", s.Name, p.ID, err)
			}
		}
		rules := map[string]bool{}
		rollbackRules := map[string]bool{}
		for _, r := range s.Rules {
			if rules[r.Name] {
				bad("service %q: duplicate rule %q", s.Name, r.Name)
			}
			rules[r.Name] = true
			if r.Action == "rollback" {
				rollbackRules[r.Name] = true
			}
			if !validRuleActions[r.Action] {
				bad("service %q rule %q: unknown action %q", s.Name, r.Name, r.Action)
			}
			for _, err := range validateNode(r.When, probes, "when") {
				bad("service %q rule %q: %v", s.Name, r.Name, err)
			}
		}
		// Without a rollback rule nothing can fail, so every deployment would PASS.
		if len(rollbackRules) == 0 {
			bad("service %q: at least one rule with action: rollback is required (without one every deployment passes)", s.Name)
		}
		for pname, pc := range s.Phases {
			if !validPhases[pname] {
				bad("service %q: unknown phase %q (canary|rolling|full)", s.Name, pname)
			}
			for _, t := range pc.Targets {
				if !contains(s.Targets, t) {
					bad("service %q phase %q: target %q is not a service target", s.Name, pname, t)
				}
			}
			phaseCanFail := false
			for _, rn := range pc.Rules {
				if !rules[rn] {
					bad("service %q phase %q: unknown rule %q", s.Name, pname, rn)
				}
				phaseCanFail = phaseCanFail || rollbackRules[rn]
			}
			if len(pc.Rules) > 0 && !phaseCanFail && len(rollbackRules) > 0 {
				bad("service %q phase %q: rules %v include no action: rollback rule (the phase could never fail)", s.Name, pname, pc.Rules)
			}
			if !validInconclusive[pc.OnInconclusive] {
				bad("service %q phase %q: on_inconclusive must be hold|pass|rollback", s.Name, pname)
			}
			if pc.Warmup >= pc.ObservationWindow {
				bad("service %q phase %q: warmup must be shorter than observation_window", s.Name, pname)
			}
		}
		rb := s.Rollback
		if rb.Executor == "" {
			bad("service %q: rollback.executor required", s.Name)
		} else if _, ok := c.Executors[rb.Executor]; !ok {
			bad("service %q: unknown rollback executor %q", s.Name, rb.Executor)
		}
		if rb.Traffic != "" {
			if _, ok := c.Traffic[rb.Traffic]; !ok {
				bad("service %q: unknown traffic controller %q", s.Name, rb.Traffic)
			}
		}
		if rb.Scope != "deployed" && rb.Scope != "failed" {
			bad("service %q: rollback.scope must be deployed|failed", s.Name)
		}
		for i, st := range rb.Plan {
			if !validStepActions[st.Action] {
				bad("service %q: rollback.plan[%d] unknown action %q", s.Name, i, st.Action)
			}
			if strings.HasPrefix(st.Action, "traffic.") && rb.Traffic == "" {
				bad("service %q: rollback.plan[%d] %s needs rollback.traffic", s.Name, i, st.Action)
			}
			if st.Action == "probe.verify" && !probes[st.Probe] {
				bad("service %q: rollback.plan[%d] unknown probe %q", s.Name, i, st.Probe)
			}
		}
		for i, e := range rb.Escalation {
			if _, ok := c.Executors[e.Executor]; !ok {
				bad("service %q: rollback.escalation[%d] unknown executor %q", s.Name, i, e.Executor)
			}
		}
	}

	switch st := c.Server.State; st.Backend {
	case "file":
	case "postgres":
		if st.DSN == "" && st.DSNEnv == "" && st.DSNRef == "" {
			bad("server.state: postgres needs dsn, dsn_env or dsn_ref")
		}
	default:
		bad("server.state.backend must be file|postgres, got %q", st.Backend)
	}
	if ha := c.Server.HA; ha.Enabled {
		if c.Server.State.Backend != "postgres" {
			bad("server.ha: requires server.state.backend: postgres (nodes must share state)")
		}
		if ha.AdvertiseURL == "" {
			bad("server.ha: advertise_url required (followers forward API calls to the leader at this URL)")
		}
		if ha.LeaseTTL < 3*time.Second {
			bad("server.ha.lease_ttl must be at least 3s")
		}
	}
	c.validateAuth(services, bad)
	c.validateSecrets(bad)
	if sl := c.Audit.Syslog; sl != nil {
		if n, a, ok := strings.Cut(sl.Address, "://"); !ok || (n != "tcp" && n != "udp") || a == "" {
			bad("audit.syslog.address must be tcp://host:port or udp://host:port")
		}
		if sl.Format != "" && sl.Format != "rfc5424" && sl.Format != "cef" {
			bad("audit.syslog.format must be rfc5424 or cef")
		}
	}
	if c.Agent.Failsafe != "hold" && c.Agent.Failsafe != "rollback" {
		bad("agent.failsafe must be hold|rollback")
	}
	for i, n := range c.Notify {
		if n.Type != "webhook" && n.Type != "slack" {
			bad("notify[%d]: unknown type %q", i, n.Type)
		}
		if n.URL == "" && n.URLEnv == "" {
			bad("notify[%d]: url or url_env required", i)
		}
	}
	return errors.Join(errs...)
}

func validateProbe(p Probe) error {
	if !validProbeTypes[p.Type] {
		return fmt.Errorf("unknown type %q", p.Type)
	}
	need := map[string]bool{
		"http": p.HTTP != nil && p.HTTP.URL != "", "grpc": p.GRPC != nil && p.GRPC.Address != "",
		"tcp": p.TCP != nil && p.TCP.Address != "", "host": true,
		"docker":     p.Docker != nil && p.Docker.Container != "",
		"log":        p.Log != nil && p.Log.Path != "" && len(p.Log.Patterns) > 0,
		"access_log": p.AccessLog != nil && p.AccessLog.Path != "",
		"db":         p.DB != nil && (p.DB.DSN != "" || p.DB.DSNEnv != "" || p.DB.DSNRef != "") && (p.DB.Driver == "postgres" || p.DB.Driver == "mysql"),
	}
	if !need[p.Type] {
		return fmt.Errorf("missing or invalid %q settings block", p.Type)
	}
	if p.Log != nil {
		for name, re := range p.Log.Patterns {
			if _, err := regexp.Compile(re); err != nil {
				return fmt.Errorf("pattern %q: %v", name, err)
			}
		}
	}
	if p.HTTP != nil && p.HTTP.BodyRegex != "" {
		if _, err := regexp.Compile(p.HTTP.BodyRegex); err != nil {
			return fmt.Errorf("body_regex: %v", err)
		}
	}
	return nil
}

func validateNode(n Node, probes map[string]bool, path string) []error {
	var errs []error
	kinds := 0
	if len(n.Any) > 0 {
		kinds++
	}
	if len(n.All) > 0 {
		kinds++
	}
	if n.Not != nil {
		kinds++
	}
	if n.Metric != "" {
		kinds++
	}
	if kinds != 1 {
		return []error{fmt.Errorf("%s: node must be exactly one of any/all/not/metric", path)}
	}
	for i, c := range n.Any {
		errs = append(errs, validateNode(c, probes, fmt.Sprintf("%s.any[%d]", path, i))...)
	}
	for i, c := range n.All {
		errs = append(errs, validateNode(c, probes, fmt.Sprintf("%s.all[%d]", path, i))...)
	}
	if n.Not != nil {
		errs = append(errs, validateNode(*n.Not, probes, path+".not")...)
	}
	if n.Metric == "" {
		return errs
	}
	checkMetric := func(m string) {
		probeID, _, ok := strings.Cut(m, ".")
		if !ok || !probes[probeID] {
			errs = append(errs, fmt.Errorf("%s: metric %q must be <probe-id>.<name> of a declared probe", path, m))
		}
	}
	checkMetric(n.Metric)
	if n.RatioOf != "" {
		checkMetric(n.RatioOf)
	}
	if !validAggs[n.Agg] {
		errs = append(errs, fmt.Errorf("%s: unknown agg %q", path, n.Agg))
	}
	if !validOps[n.Op] {
		errs = append(errs, fmt.Errorf("%s: unknown op %q", path, n.Op))
	}
	if (n.Value == nil) == (n.Baseline == nil) {
		errs = append(errs, fmt.Errorf("%s: exactly one of value or baseline required", path))
	}
	if n.Scope != "target" && n.Scope != "service" {
		errs = append(errs, fmt.Errorf("%s: scope must be target|service", path))
	}
	if n.Absent != "unknown" && n.Absent != "breach" && n.Absent != "ok" {
		errs = append(errs, fmt.Errorf("%s: absent must be unknown|breach|ok", path))
	}
	return errs
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

var validRoles = set("viewer", "deployer", "operator", "admin", "agent")

func (c *Config) validateAuth(services map[string]bool, bad func(string, ...any)) {
	a := c.Auth
	checkGrant := func(where, role, scope string) {
		if !validRoles[role] {
			bad("%s: unknown role %q (viewer|deployer|operator|admin|agent)", where, role)
		}
		switch k, v, _ := strings.Cut(scope, "="); {
		case scope == "" || scope == "*":
		case k == "service" && v != "":
			if !services[v] {
				bad("%s: scope %q names an unknown service", where, scope)
			}
		case k == "team" && v != "":
		default:
			bad("%s: scope %q must be *, team=<team> or service=<name>", where, scope)
		}
	}
	names := map[string]bool{}
	hashes := map[string]bool{}
	for i, sa := range a.ServiceAccounts {
		where := fmt.Sprintf("auth.service_accounts[%d] %q", i, sa.Name)
		if sa.Name == "" || names[sa.Name] {
			bad("%s: empty or duplicate name", where)
		}
		names[sa.Name] = true
		h := strings.ToLower(sa.TokenSHA256)
		if len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
			bad("%s: token_sha256 must be 64 hex characters (vigilante token create)", where)
		} else if hashes[h] {
			bad("%s: token_sha256 reused by another account", where)
		}
		hashes[h] = true
		if sa.Expires != "" {
			if _, err := time.Parse("2006-01-02", sa.Expires); err != nil {
				bad("%s: expires must be YYYY-MM-DD", where)
			}
		}
		if len(sa.Roles) == 0 {
			bad("%s: no roles", where)
		}
		for _, g := range sa.Roles {
			checkGrant(where, g.Role, g.Scope)
		}
	}
	for i, rb := range a.RoleBindings {
		where := fmt.Sprintf("auth.role_bindings[%d]", i)
		if (rb.Group == "") == (rb.User == "") {
			bad("%s: set exactly one of group or user", where)
		}
		checkGrant(where, rb.Role, rb.Scope)
	}
	if a.OIDC != nil && (a.OIDC.Issuer == "" || a.OIDC.Audience == "") {
		bad("auth.oidc: issuer and audience are required")
	}
	if len(a.RoleBindings) > 0 && a.OIDC == nil {
		bad("auth.role_bindings: map OIDC groups/users, so auth.oidc is required")
	}
}

// ValidRef reports whether s is a secret reference this build understands.
func ValidRef(s string) bool {
	switch {
	case strings.HasPrefix(s, "env:"):
		return len(s) > 4
	case strings.HasPrefix(s, "file:"):
		return len(s) > 5
	case strings.HasPrefix(s, "vault:"):
		path, key, ok := strings.Cut(strings.TrimPrefix(s, "vault:"), "#")
		mount, rest, ok2 := strings.Cut(path, "/")
		return ok && ok2 && key != "" && mount != "" && rest != ""
	}
	return false
}

func (c *Config) validateSecrets(bad func(string, ...any)) {
	usesVault := false
	check := func(where, ref string) {
		if ref == "" {
			return
		}
		if !ValidRef(ref) {
			bad("%s: %q must be vault:<mount>/<path>#<key>, env:NAME or file:/path", where, ref)
		}
		if strings.HasPrefix(ref, "vault:") {
			usesVault = true
		}
	}
	for name, cr := range c.Credentials {
		w := "credentials." + name
		check(w+".username_ref", cr.UsernameRef)
		check(w+".password_ref", cr.PasswordRef)
		check(w+".token_ref", cr.TokenRef)
		check(w+".passphrase_ref", cr.PassphraseRef)
		check(w+".private_key_ref", cr.PrivateKeyRef)
		if cr.SSHCA != nil {
			usesVault = true
			if cr.Type != "ssh" {
				bad("%s.ssh_ca: only for type ssh", w)
			}
			if cr.SSHCA.Mount == "" || cr.SSHCA.Role == "" {
				bad("%s.ssh_ca: mount and role are required", w)
			}
		}
	}
	check("server.state.dsn_ref", c.Server.State.DSNRef)
	check("api.webhook_signing_key_ref", c.API.WebhookSigningKeyRef)
	for _, s := range c.Services {
		for _, p := range s.Probes {
			if p.DB != nil {
				check("service "+s.Name+" probe "+p.ID+" dsn_ref", p.DB.DSNRef)
			}
		}
	}
	v := c.Secrets.Vault
	if usesVault && v == nil {
		bad("secrets.vault: required by vault: references or ssh_ca")
	}
	if v != nil {
		if v.Address == "" {
			bad("secrets.vault.address required")
		}
		switch v.Auth {
		case "", "token":
		case "approle":
			if v.RoleIDEnv == "" || v.SecretIDEnv == "" {
				bad("secrets.vault: approle needs role_id_env and secret_id_env")
			}
		case "kubernetes":
			if v.K8sRole == "" {
				bad("secrets.vault: kubernetes auth needs k8s_role")
			}
		default:
			bad("secrets.vault.auth must be token, approle or kubernetes")
		}
	}
}
