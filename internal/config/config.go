// Package config defines the YAML schema of vigilante.yaml, applies defaults
// and validates cross references (target -> credential, service -> probe,
// rollback plan -> executor / traffic controller).
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version     string                `yaml:"version"`
	Server      Server                `yaml:"server"`
	Agent       Agent                 `yaml:"agent"`
	Credentials map[string]Credential `yaml:"credentials"`
	Targets     []Target              `yaml:"targets"`
	Traffic     map[string]Traffic    `yaml:"traffic"`
	Executors   map[string]Executor   `yaml:"executors"`
	Services    []Service             `yaml:"services"`
	Safety      Safety                `yaml:"safety"`
	Notify      []Notifier            `yaml:"notify"`
	// PresetDirs holds organisation presets (*.yaml), relative to this file.
	PresetDirs []string `yaml:"preset_dirs"`
	Auth       Auth     `yaml:"auth"`
}

// Auth configures who may call the API and what they may do.
//
// Roles, lowest to highest: viewer (read), deployer (create deployments, run
// phases, abort, baselines, mark-good), operator (manual rollback, approve),
// admin (circuit reset/trip and everything else). The agent role may only
// push samples and heartbeats. A scope limits a grant: "*" (default),
// "team=<team>" or "service=<name>".
type Auth struct {
	OIDC            *OIDC            `yaml:"oidc"`
	ServiceAccounts []ServiceAccount `yaml:"service_accounts"`
	RoleBindings    []RoleBinding    `yaml:"role_bindings"`
	// FourEyes: whoever created a deployment or requested its rollback may
	// not also approve its gated escalation.
	FourEyes bool `yaml:"four_eyes"`
}

// OIDC validates bearer JWTs from the company identity provider.
type OIDC struct {
	Issuer        string `yaml:"issuer"`
	Audience      string `yaml:"audience"`       // expected client ID
	UsernameClaim string `yaml:"username_claim"` // default preferred_username
	GroupsClaim   string `yaml:"groups_claim"`   // default groups
}

// ServiceAccount is a machine identity (CI job, agent fleet). Only the
// SHA-256 of its token is stored; create one with `vigilante token create`.
type ServiceAccount struct {
	Name        string  `yaml:"name"`
	TokenSHA256 string  `yaml:"token_sha256"`
	Expires     string  `yaml:"expires"` // YYYY-MM-DD, optional
	Roles       []Grant `yaml:"roles"`
}

type Grant struct {
	Role  string `yaml:"role"`
	Scope string `yaml:"scope"`
}

// RoleBinding grants a role to an OIDC group or user.
type RoleBinding struct {
	Group string `yaml:"group"`
	User  string `yaml:"user"`
	Role  string `yaml:"role"`
	Scope string `yaml:"scope"`
}

type Server struct {
	Listen        string `yaml:"listen"`
	AuthTokenEnv  string `yaml:"auth_token_env"`
	JournalPath   string `yaml:"journal_path"`
	DryRun        bool   `yaml:"dry_run"`
	WebhookSecret string `yaml:"webhook_secret_env"`
	State         State  `yaml:"state"`
	HA            HA     `yaml:"ha"`
}

// State selects where decisions, rollback progress, circuit state and locks
// are kept. "file" (default) is the JSONL journal at journal_path, for a single
// node or CI. "postgres" is shared by every node and enables HA.
type State struct {
	Backend string `yaml:"backend"` // file | postgres
	DSN     string `yaml:"dsn"`     // avoid inline passwords; prefer dsn_env
	DSNEnv  string `yaml:"dsn_env"`
}

// HA runs several `vigilante server` nodes against one postgres state store:
// one leader acts, the others forward API calls to it and take over when its
// lease expires.
type HA struct {
	Enabled      bool          `yaml:"enabled"`
	AdvertiseURL string        `yaml:"advertise_url"` // how other nodes reach this node's API
	NodeID       string        `yaml:"node_id"`       // default: hostname
	LeaseTTL     time.Duration `yaml:"lease_ttl"`
}

// Agent configures the optional push agent (`vigilante agent`).
type Agent struct {
	PushInterval      time.Duration `yaml:"push_interval"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	// FailsafeAfter: when the orchestrator is unreachable for this long while a
	// deployment is active on the agent's target, Failsafe policy applies.
	FailsafeAfter time.Duration `yaml:"failsafe_after"`
	Failsafe      string        `yaml:"failsafe"` // hold | rollback
}

// Credential is resolved lazily; secrets are always read from env vars or files,
// never stored inline in the YAML.
type Credential struct {
	Type              string `yaml:"type"` // ssh | basic | token | aws
	User              string `yaml:"user"`
	PrivateKeyFile    string `yaml:"private_key_file"`
	PassphraseEnv     string `yaml:"passphrase_env"`
	PasswordEnv       string `yaml:"password_env"`
	UsernameEnv       string `yaml:"username_env"`
	TokenEnv          string `yaml:"token_env"`
	KnownHostsFile    string `yaml:"known_hosts_file"`
	InsecureIgnoreKey bool   `yaml:"insecure_ignore_host_key"`
	UseAgent          bool   `yaml:"use_ssh_agent"`
	Region            string `yaml:"region"`
	Profile           string `yaml:"profile"`
}

// Target is one machine or container host the engine observes or controls.
type Target struct {
	Name       string            `yaml:"name"`
	Kind       string            `yaml:"kind"` // baremetal | vm | cloud_vm | container_host
	Address    string            `yaml:"address"`
	Labels     map[string]string `yaml:"labels"`
	Connection Connection        `yaml:"connection"`
}

type Connection struct {
	Type       string        `yaml:"type"` // ssh | local | none
	Credential string        `yaml:"credential"`
	Port       int           `yaml:"port"`
	Bastion    string        `yaml:"bastion"` // name of another target used as jump host
	Sudo       bool          `yaml:"sudo"`
	Timeout    time.Duration `yaml:"timeout"`
}

// Traffic is a traffic control layer (software LB, hardware ADC, cloud LB).
type Traffic struct {
	Type    string          `yaml:"type"` // nginx | haproxy | envoy | f5 | aws_alb
	Nginx   *NginxTraffic   `yaml:"nginx,omitempty"`
	HAProxy *HAProxyTraffic `yaml:"haproxy,omitempty"`
	Envoy   *EnvoyTraffic   `yaml:"envoy,omitempty"`
	F5      *F5Traffic      `yaml:"f5,omitempty"`
	AWSALB  *AWSALBTraffic  `yaml:"aws_alb,omitempty"`
	// DrainWait is how long to wait after draining before touching the app.
	DrainWait time.Duration `yaml:"drain_wait"`
}

type NginxTraffic struct {
	Hosts        []string `yaml:"hosts"` // LB targets (reached over their connection)
	UpstreamFile string   `yaml:"upstream_file"`
	MemberFormat string   `yaml:"member_format"` // template, default "{{.Address}}:80"
	TestCmd      string   `yaml:"test_cmd"`      // default "nginx -t"
	ReloadCmd    string   `yaml:"reload_cmd"`    // default "nginx -s reload"
}

type HAProxyTraffic struct {
	Hosts        []string `yaml:"hosts"`   // LB targets; runtime socket reached over SSH stream-local forwarding
	Socket       string   `yaml:"socket"`  // unix socket path on the LB host, e.g. /run/haproxy/admin.sock
	Address      string   `yaml:"address"` // alternative: tcp runtime API "10.0.0.5:9999"
	Backend      string   `yaml:"backend"`
	ServerName   string   `yaml:"server_name"` // template, default "{{.Name}}"
	DrainToMaint bool     `yaml:"drain_to_maint"`
}

type EnvoyTraffic struct {
	Hosts        []string `yaml:"hosts"`
	EDSFile      string   `yaml:"eds_file"` // file-based EDS (path_config_source) JSON
	MemberFormat string   `yaml:"member_format"`
}

type F5Traffic struct {
	URL           string `yaml:"url"` // https://bigip.example.com
	Credential    string `yaml:"credential"`
	Pool          string `yaml:"pool"` // /Common/order_pool
	MemberFormat  string `yaml:"member_format"`
	ForceOffline  bool   `yaml:"force_offline"` // user-down instead of user-disabled (drops persistent sessions)
	TokenAuth     bool   `yaml:"token_auth"`
	TLSSkipVerify bool   `yaml:"tls_skip_verify"`
}

type AWSALBTraffic struct {
	Credential     string        `yaml:"credential"`
	TargetGroupARN string        `yaml:"target_group_arn"`
	TargetID       string        `yaml:"target_id"` // template, default "{{.Labels.instance_id}}"
	Port           int32         `yaml:"port"`
	WaitTimeout    time.Duration `yaml:"wait_timeout"`
}

// Executor is a rollback strategy instance.
type Executor struct {
	Type      string         `yaml:"type"` // symlink | container | vsphere | nutanix | kvm | exec | webhook
	Symlink   *SymlinkExec   `yaml:"symlink,omitempty"`
	Container *ContainerExec `yaml:"container,omitempty"`
	VSphere   *VSphereExec   `yaml:"vsphere,omitempty"`
	Nutanix   *NutanixExec   `yaml:"nutanix,omitempty"`
	KVM       *KVMExec       `yaml:"kvm,omitempty"`
	Exec      *ExecExec      `yaml:"exec,omitempty"`
	Webhook   *WebhookExec   `yaml:"webhook,omitempty"`
}

type SymlinkExec struct {
	Link        string `yaml:"link"`         // /opt/order/current
	ReleasesDir string `yaml:"releases_dir"` // /opt/order/releases
	Release     string `yaml:"release"`      // template, default "{{.PreviousVersion}}"
	Init        string `yaml:"init"`         // systemd | sysv | none
	Unit        string `yaml:"unit"`         // service name
	RestartCmd  string `yaml:"restart_cmd"`  // overrides init
	Atomic      *bool  `yaml:"atomic"`       // GNU mv -T rename; false for AIX/Solaris
}

type ContainerExec struct {
	Socket         string `yaml:"socket"`     // /var/run/docker.sock or /run/podman/podman.sock
	Host           string `yaml:"host"`       // alternatively tcp://host:2375
	Name           string `yaml:"name"`       // template, container name
	ImageRepo      string `yaml:"image_repo"` // default: repo of the running image
	Tag            string `yaml:"tag"`        // template, default "{{.PreviousVersion}}"
	Pull           bool   `yaml:"pull"`
	StopTimeoutSec int    `yaml:"stop_timeout_sec"`
}

type VSphereExec struct {
	URL           string `yaml:"url"` // https://vcenter/sdk
	Credential    string `yaml:"credential"`
	VM            string `yaml:"vm"`       // template, VM name or inventory path
	Snapshot      string `yaml:"snapshot"` // template, default "vigilante-{{.DeploymentID}}"
	PowerOn       bool   `yaml:"power_on"`
	TLSSkipVerify bool   `yaml:"tls_skip_verify"`
}

type NutanixExec struct {
	URL           string `yaml:"url"` // https://prism:9440
	Credential    string `yaml:"credential"`
	VMUUID        string `yaml:"vm_uuid"`  // template
	Snapshot      string `yaml:"snapshot"` // template
	TLSSkipVerify bool   `yaml:"tls_skip_verify"`
}

type KVMExec struct {
	Hypervisor string `yaml:"hypervisor"` // target name of the KVM host (ssh)
	Domain     string `yaml:"domain"`     // template
	Snapshot   string `yaml:"snapshot"`   // template
}

type ExecExec struct {
	Prepare  string `yaml:"prepare"`
	Rollback string `yaml:"rollback"`
	Verify   string `yaml:"verify"`
	OnHost   string `yaml:"on"` // "target" (default) or a target name, or "local"
}

type WebhookExec struct {
	URL        string            `yaml:"url"` // template
	Method     string            `yaml:"method"`
	Headers    map[string]string `yaml:"headers"`
	Body       string            `yaml:"body"` // template
	VerifyURL  string            `yaml:"verify_url"`
	Credential string            `yaml:"credential"`
}

// Service ties targets, probes, rules, phases and the rollback plan together.
type Service struct {
	Name string `yaml:"name"`
	// Team owns the service; auth scopes "team=<team>" match it.
	Team string `yaml:"team"`
	// Preset ("java-web" or pinned "java-web@1") supplies probes, rules,
	// baseline and phases; Overrides fills its parameters. Probes, rules and
	// phases written on the service replace the preset's of the same id/name.
	Preset         string                 `yaml:"preset"`
	Overrides      map[string]any         `yaml:"overrides"`
	Targets        []string               `yaml:"targets"`
	ControlTargets []string               `yaml:"control_targets"`
	Probes         []Probe                `yaml:"probes"`
	Baseline       Baseline               `yaml:"baseline"`
	Rules          []Rule                 `yaml:"rules"`
	Phases         map[string]PhaseConfig `yaml:"phases"`
	Rollback       Rollback               `yaml:"rollback"`
}

type Probe struct {
	ID        string          `yaml:"id"`
	Type      string          `yaml:"type"` // http | grpc | tcp | host | docker | log | access_log | db
	Interval  time.Duration   `yaml:"interval"`
	Timeout   time.Duration   `yaml:"timeout"`
	HTTP      *HTTPProbe      `yaml:"http,omitempty"`
	GRPC      *GRPCProbe      `yaml:"grpc,omitempty"`
	TCP       *TCPProbe       `yaml:"tcp,omitempty"`
	Host      *HostProbe      `yaml:"host,omitempty"`
	Docker    *DockerProbe    `yaml:"docker,omitempty"`
	Log       *LogProbe       `yaml:"log,omitempty"`
	AccessLog *AccessLogProbe `yaml:"access_log,omitempty"`
	DB        *DBProbe        `yaml:"db,omitempty"`
}

type HTTPProbe struct {
	URL           string            `yaml:"url"` // template
	Method        string            `yaml:"method"`
	Headers       map[string]string `yaml:"headers"`
	ExpectStatus  []int             `yaml:"expect_status"`
	BodyRegex     string            `yaml:"body_regex"`
	JSONPath      string            `yaml:"json_path"`   // e.g. components.db.status (Spring actuator)
	JSONExpect    string            `yaml:"json_expect"` // e.g. UP
	TLSSkipVerify bool              `yaml:"tls_skip_verify"`
}

type GRPCProbe struct {
	Address string `yaml:"address"` // template host:port
	Service string `yaml:"service"`
	TLS     bool   `yaml:"tls"`
}

type TCPProbe struct {
	Address string `yaml:"address"` // template host:port
}

type HostProbe struct {
	Devices []string `yaml:"devices"` // disk devices to watch; empty = all sd*/vd*/nvme*/xvd*
}

type DockerProbe struct {
	Socket    string `yaml:"socket"`
	Host      string `yaml:"host"`
	Container string `yaml:"container"` // template
}

type LogProbe struct {
	Path     string            `yaml:"path"`     // template
	Patterns map[string]string `yaml:"patterns"` // name -> regex
}

type AccessLogProbe struct {
	Path   string `yaml:"path"`
	Format string `yaml:"format"` // combined | json
	// JSON field names (json format)
	StatusField  string `yaml:"status_field"`
	LatencyField string `yaml:"latency_field"`
	// LatencyUnit of the trailing request-time column (combined) or latency field (json): s | ms.
	LatencyUnit string `yaml:"latency_unit"`
}

type DBProbe struct {
	Driver   string `yaml:"driver"` // postgres | mysql
	DSN      string `yaml:"dsn"`    // template; may reference {{env "X"}}
	DSNEnv   string `yaml:"dsn_env"`
	PoolSize int    `yaml:"pool_size"`
	Query    string `yaml:"query"`
}

type Baseline struct {
	Window     time.Duration `yaml:"window"`
	MinSamples int           `yaml:"min_samples"`
}

type Rule struct {
	Name   string `yaml:"name"`
	Action string `yaml:"action"` // rollback | notify | hold
	When   Node   `yaml:"when"`
}

// Node is a boolean expression tree: either a combinator (any/all/not) or a leaf condition.
type Node struct {
	Any       []Node `yaml:"any,omitempty"`
	All       []Node `yaml:"all,omitempty"`
	Not       *Node  `yaml:"not,omitempty"`
	Condition `yaml:",inline"`
}

type Condition struct {
	Metric     string        `yaml:"metric,omitempty"`
	Agg        string        `yaml:"agg,omitempty"` // last | avg | min | max | sum | count | rate | p50 | p90 | p95 | p99
	Window     time.Duration `yaml:"window,omitempty"`
	RatioOf    string        `yaml:"ratio_of,omitempty"` // value = sum(metric)/sum(ratio_of)*100
	Op         string        `yaml:"op,omitempty"`
	Value      *float64      `yaml:"value,omitempty"`
	Baseline   *BaselineCmp  `yaml:"baseline,omitempty"`
	MinValue   *float64      `yaml:"min_value,omitempty"`   // absolute floor for baseline comparisons
	For        int           `yaml:"for,omitempty"`         // consecutive breaching evaluations required
	ResetAfter int           `yaml:"reset_after,omitempty"` // consecutive OK evaluations to reset the counter
	Scope      string        `yaml:"scope,omitempty"`       // target | service
	Absent     string        `yaml:"absent,omitempty"`      // unknown | breach | ok
}

type BaselineCmp struct {
	IncreasePct float64 `yaml:"increase_pct"` // breach when value > baseline*(1+pct/100)
}

type PhaseConfig struct {
	Targets           []string      `yaml:"targets"` // targets that receive the new version in this phase
	Percent           int           `yaml:"percent"` // alternative to Targets: first N% of service targets
	ObservationWindow time.Duration `yaml:"observation_window"`
	Warmup            time.Duration `yaml:"warmup"`
	EvalInterval      time.Duration `yaml:"eval_interval"`
	Rules             []string      `yaml:"rules"` // subset of service rules; empty = all
	MinSamples        int           `yaml:"min_samples"`
	OnInconclusive    string        `yaml:"on_inconclusive"` // hold | pass | rollback
}

type Rollback struct {
	Executor    string        `yaml:"executor"`
	Traffic     string        `yaml:"traffic"`
	Scope       string        `yaml:"scope"` // deployed | failed
	Parallelism int           `yaml:"parallelism"`
	StepTimeout time.Duration `yaml:"step_timeout"`
	Retry       Retry         `yaml:"retry"`
	Plan        []Step        `yaml:"plan"`
	Escalation  []Escalation  `yaml:"escalation"`
}

type Retry struct {
	Attempts   int           `yaml:"attempts"`
	Backoff    time.Duration `yaml:"backoff"`
	MaxBackoff time.Duration `yaml:"max_backoff"`
}

type Step struct {
	Action    string        `yaml:"action"` // traffic.drain | traffic.enable | app.rollback | app.verify | probe.verify | wait
	Probe     string        `yaml:"probe,omitempty"`
	Successes int           `yaml:"successes,omitempty"`
	Duration  time.Duration `yaml:"duration,omitempty"`
	Timeout   time.Duration `yaml:"timeout,omitempty"`
}

type Escalation struct {
	Executor        string `yaml:"executor"`
	RequireApproval bool   `yaml:"require_approval"`
}

type Safety struct {
	CircuitBreaker CircuitBreaker `yaml:"circuit_breaker"`
	BlastRadius    BlastRadius    `yaml:"blast_radius"`
	Flapping       Flapping       `yaml:"flapping"`
	ObserverQuorum bool           `yaml:"observer_quorum"`
}

type CircuitBreaker struct {
	FailureThreshold int           `yaml:"failure_threshold"`
	Window           time.Duration `yaml:"window"`
	// OpenDuration > 0 moves OPEN -> HALF_OPEN automatically; 0 = manual reset only.
	OpenDuration time.Duration `yaml:"open_duration"`
}

type BlastRadius struct {
	MinHealthy        int `yaml:"min_healthy"`
	MinHealthyPercent int `yaml:"min_healthy_percent"`
}

type Flapping struct {
	MaxRollbacksPerHour int           `yaml:"max_rollbacks_per_hour"`
	Cooldown            time.Duration `yaml:"cooldown"`
}

type Notifier struct {
	Type     string `yaml:"type"` // webhook | slack
	URL      string `yaml:"url"`
	URLEnv   string `yaml:"url_env"`
	MinLevel string `yaml:"min_level"` // info | warning | critical
}

// Load reads, decodes (strictly), defaults and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parse(raw, filepath.Dir(path))
}

// Parse decodes a config; relative preset_dirs resolve against the working directory.
func Parse(raw []byte) (*Config, error) { return parse(raw, ".") }

func parse(raw []byte, baseDir string) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := c.expandPresets(baseDir); err != nil {
		return nil, err
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Lookup helpers.

func (c *Config) Target(name string) (*Target, bool) {
	for i := range c.Targets {
		if c.Targets[i].Name == name {
			return &c.Targets[i], true
		}
	}
	return nil, false
}

func (c *Config) Service(name string) (*Service, bool) {
	for i := range c.Services {
		if c.Services[i].Name == name {
			return &c.Services[i], true
		}
	}
	return nil, false
}

func (s *Service) Probe(id string) (*Probe, bool) {
	for i := range s.Probes {
		if s.Probes[i].ID == id {
			return &s.Probes[i], true
		}
	}
	return nil, false
}

// PhaseTargets returns the targets running the new version once `phase` is
// active. Phases are cumulative: rolling includes the canary targets.
// Without explicit targets/percent: canary = first target, rolling = 50%,
// full = all targets.
func (s *Service) PhaseTargets(phase string) []string {
	defaults := map[string]int{"canary": 0, "rolling": 50, "full": 100}
	seen := map[string]bool{}
	var out []string
	add := func(names []string) {
		for _, n := range names {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	percentOf := func(pct int) []string {
		n := max(1, (len(s.Targets)*pct+99)/100)
		return s.Targets[:min(n, len(s.Targets))]
	}
	for _, p := range []string{"canary", "rolling", "full"} {
		pc, ok := s.Phases[p]
		switch {
		case ok && len(pc.Targets) > 0:
			add(pc.Targets)
		case ok && pc.Percent > 0:
			add(percentOf(pc.Percent))
		case ok || p == phase: // configured without a target set, or the requested phase itself
			add(percentOf(defaults[p]))
		}
		if p == phase {
			break
		}
	}
	return out
}

// Controls returns targets used as an untouched comparison group for `phase`.
// `control_targets: [auto]` means "every service target not yet deployed".
func (s *Service) Controls(phase string) []string {
	if len(s.ControlTargets) == 1 && s.ControlTargets[0] == "auto" {
		deployed := map[string]bool{}
		for _, t := range s.PhaseTargets(phase) {
			deployed[t] = true
		}
		var out []string
		for _, t := range s.Targets {
			if !deployed[t] {
				out = append(out, t)
			}
		}
		return out
	}
	return s.ControlTargets
}
