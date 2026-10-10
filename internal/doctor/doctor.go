// Package doctor runs read-only pre-flight checks (`vigilante doctor`): can
// we reach every target, read every log, find the previous release or image,
// and take members out of every load-balancer pool? Problems found here would
// otherwise surface in the middle of a rollback.
package doctor

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"vigilante/internal/config"
	"vigilante/internal/executor"
	"vigilante/internal/probe"
	"vigilante/internal/secrets"
	"vigilante/internal/sudoers"
	"vigilante/internal/tmpl"
	"vigilante/internal/transport"
)

type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Check is one result line.
type Check struct {
	Scope   string `json:"scope"`
	Subject string `json:"subject"`
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Detail  string `json:"detail,omitempty"`
	Hint    string `json:"hint,omitempty"`
}

// Scopes in report order.
const (
	ScopeCredential = "자격증명"
	ScopeTarget     = "대상"
	ScopeProbe      = "프로브"
	ScopeExecutor   = "실행기"
	ScopeTraffic    = "트래픽"
	ScopeSudo       = "sudo"
	ScopeCapacity   = "용량"
)

var scopeOrder = map[string]int{ScopeCredential: 0, ScopeTarget: 1, ScopeProbe: 2, ScopeExecutor: 3, ScopeTraffic: 4, ScopeSudo: 5, ScopeCapacity: 6}

type Options struct {
	Service     string // "" = every service
	Version     string
	Previous    string // lets executors check that the previous release/image exists
	Timeout     time.Duration
	Runners     func(target string) (transport.Runner, error)
	Log         *slog.Logger
	TrafficHTTP *http.Client
}

// Summary counts results by status.
func Summary(cs []Check) map[Status]int {
	m := map[Status]int{}
	for _, c := range cs {
		m[c.Status]++
	}
	return m
}

// Run executes every check (in parallel) and returns them in report order.
func Run(ctx context.Context, cfg *config.Config, opt Options) ([]Check, error) {
	if opt.Timeout == 0 {
		opt.Timeout = 15 * time.Second
	}
	if opt.Log == nil {
		opt.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	var services []*config.Service
	for i := range cfg.Services {
		if opt.Service == "" || cfg.Services[i].Name == opt.Service {
			services = append(services, &cfg.Services[i])
		}
	}
	if len(services) == 0 {
		return nil, fmt.Errorf("no service %q in config", opt.Service)
	}
	d := &run{cfg: cfg, opt: opt}
	var jobs []func(context.Context) []Check

	targets := d.involvedTargets(services)
	jobs = append(jobs, func(ctx context.Context) []Check { return d.credentials(ctx, targets, services) })
	for _, tn := range targets {
		tn := tn
		jobs = append(jobs, func(ctx context.Context) []Check { return d.target(ctx, tn) })
	}
	for _, s := range services {
		for _, tn := range s.Targets {
			t, _ := cfg.Target(tn)
			for _, p := range s.Probes {
				s, t, p := s, *t, p
				jobs = append(jobs, func(ctx context.Context) []Check { return d.probe(ctx, s, t, p) })
			}
			for _, name := range executorsOf(s) {
				s, t, name := s, *t, name
				jobs = append(jobs, func(ctx context.Context) []Check { return d.executor(ctx, s, t, name) })
			}
		}
	}
	seenTraffic := map[string]bool{}
	for _, s := range services {
		if s.Rollback.Traffic != "" {
			s, first := s, !seenTraffic[s.Rollback.Traffic]
			seenTraffic[s.Rollback.Traffic] = true
			jobs = append(jobs, func(ctx context.Context) []Check { return d.traffic(ctx, s, first) })
		}
	}
	jobs = append(jobs, func(context.Context) []Check { return d.capacity(services) })
	for _, job := range d.sudoJobs(services) {
		jobs = append(jobs, job)
	}

	results := make([][]Check, len(jobs))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		go func(i int, job func(context.Context) []Check) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, opt.Timeout)
			defer cancel()
			results[i] = job(cctx)
		}(i, job)
	}
	wg.Wait()
	var out []Check
	for _, r := range results {
		out = append(out, r...)
	}
	for i := range out {
		if out[i].Status == Fail && out[i].Hint == "" {
			out[i].Hint = Hint(out[i].Detail)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return scopeOrder[out[i].Scope] < scopeOrder[out[j].Scope] })
	return out, nil
}

type run struct {
	cfg *config.Config
	opt Options
}

func executorsOf(s *config.Service) []string {
	names := []string{s.Rollback.Executor}
	for _, e := range s.Rollback.Escalation {
		names = append(names, e.Executor)
	}
	return names
}

// involvedTargets = service targets + LB hosts + hypervisor / exec hosts.
func (d *run) involvedTargets(services []*config.Service) []string {
	seen := map[string]bool{}
	var out []string
	add := func(names ...string) {
		for _, n := range names {
			if n != "" && n != "target" && n != "local" && !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	for _, s := range services {
		add(s.Targets...)
		if tr, ok := d.cfg.Traffic[s.Rollback.Traffic]; ok {
			switch {
			case tr.Nginx != nil:
				add(tr.Nginx.Hosts...)
			case tr.HAProxy != nil && tr.HAProxy.Socket != "":
				add(tr.HAProxy.Hosts...)
			case tr.Envoy != nil:
				add(tr.Envoy.Hosts...)
			}
		}
		for _, name := range executorsOf(s) {
			ex := d.cfg.Executors[name]
			if ex.KVM != nil {
				add(ex.KVM.Hypervisor)
			}
			if ex.Exec != nil {
				add(ex.Exec.OnHost)
			}
		}
	}
	// Bastions are reached first, so they are checked too.
	for _, n := range append([]string(nil), out...) {
		if t, ok := d.cfg.Target(n); ok {
			add(t.Connection.Bastion)
		}
	}
	return out
}

func (d *run) credentials(ctx context.Context, targets []string, services []*config.Service) []Check {
	used := map[string]bool{}
	for _, tn := range targets {
		if t, ok := d.cfg.Target(tn); ok && t.Connection.Credential != "" {
			used[t.Connection.Credential] = true
		}
	}
	for _, s := range services {
		for _, name := range executorsOf(s) {
			ex := d.cfg.Executors[name]
			osCred := ""
			if ex.OpenStack != nil {
				osCred = ex.OpenStack.Credential
			}
			for _, c := range []string{credOf(ex.VSphere), credOfN(ex.Nutanix), credOfW(ex.Webhook), osCred} {
				if c != "" {
					used[c] = true
				}
			}
		}
		if tr, ok := d.cfg.Traffic[s.Rollback.Traffic]; ok {
			if tr.F5 != nil && tr.F5.Credential != "" {
				used[tr.F5.Credential] = true
			}
			if tr.AWSALB != nil && tr.AWSALB.Credential != "" {
				used[tr.AWSALB.Credential] = true
			}
			if tr.Octavia != nil && tr.Octavia.Credential != "" {
				used[tr.Octavia.Credential] = true
			}
		}
	}
	var names []string
	for n := range used {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []Check
	for _, n := range names {
		c := d.cfg.Credentials[n]
		var problems []string
		for _, env := range []string{c.PasswordEnv, c.UsernameEnv, c.TokenEnv, c.PassphraseEnv} {
			if env != "" && os.Getenv(env) == "" {
				problems = append(problems, "환경변수 "+env+" 비어 있음")
			}
		}
		for _, ref := range []string{c.UsernameRef, c.PasswordRef, c.TokenRef, c.PassphraseRef, c.PrivateKeyRef, c.ApplicationCredentialSecretRef} {
			if ref == "" {
				continue
			}
			if _, err := secrets.Resolve(ctx, ref); err != nil {
				problems = append(problems, err.Error())
			}
		}
		if c.SSHCA != nil {
			if err := checkSSHCA(ctx, *c.SSHCA, c.User); err != nil {
				problems = append(problems, err.Error())
			}
		}
		if c.PrivateKeyFile != "" {
			if _, err := os.Stat(expandHome(c.PrivateKeyFile)); err != nil {
				problems = append(problems, "키 파일 "+c.PrivateKeyFile+" 없음")
			}
		}
		if c.Type == "ssh" && !c.InsecureIgnoreKey {
			kh := c.KnownHostsFile
			if kh == "" {
				kh = "~/.ssh/known_hosts"
			}
			if _, err := os.Stat(expandHome(kh)); err != nil {
				problems = append(problems, "known_hosts "+kh+" 없음")
			}
		}
		if len(problems) > 0 {
			out = append(out, Check{Scope: ScopeCredential, Subject: n, Name: "자격증명 값", Status: Fail, Detail: strings.Join(problems, ", "),
				Hint: "비밀값은 설정 파일이 아니라 Vault(*_ref) 또는 실행 환경(CI 시크릿, 서비스 환경파일)에 넣어야 합니다. Vault 오류면 정책이 해당 경로 read(SSH CA는 sign/<role> update)를 허용하는지 확인하세요"})
		} else {
			out = append(out, Check{Scope: ScopeCredential, Subject: n, Name: "자격증명 값", Status: OK, Detail: c.Type})
		}
	}
	return out
}

// checkSSHCA signs a throwaway key: proves the Vault policy allows the role
// without touching any target.
func checkSSHCA(ctx context.Context, ca config.SSHCA, user string) error {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return err
	}
	_, err = secrets.SignSSHKey(ctx, ca, user, s.PublicKey())
	return err
}

func credOf(v *config.VSphereExec) string {
	if v == nil {
		return ""
	}
	return v.Credential
}
func credOfN(v *config.NutanixExec) string {
	if v == nil {
		return ""
	}
	return v.Credential
}
func credOfW(v *config.WebhookExec) string {
	if v == nil {
		return ""
	}
	return v.Credential
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + p[1:]
		}
	}
	return p
}

func (d *run) target(ctx context.Context, name string) []Check {
	t, _ := d.cfg.Target(name)
	c := Check{Scope: ScopeTarget, Subject: name}
	switch t.Connection.Type {
	case "none":
		c.Name, c.Status, c.Detail = "접속", Skip, "connection: none (API 전용 대상)"
		return []Check{c}
	case "local":
		c.Name, c.Status, c.Detail = "접속", OK, "로컬"
		return []Check{c}
	}
	c.Name = "SSH 접속"
	if t.Connection.Bastion != "" {
		c.Name += " (" + t.Connection.Bastion + " 경유)"
	}
	if t.Connection.Sudo {
		c.Name += " · sudo -n"
	}
	r, err := d.opt.Runners(name)
	if err == nil {
		var out string
		out, err = r.Run(ctx, "echo vigilante-ok", nil)
		if err == nil && !strings.Contains(out, "vigilante-ok") {
			err = fmt.Errorf("unexpected output %q", strings.TrimSpace(out))
		}
	}
	if err != nil {
		c.Status, c.Detail = Fail, err.Error()
	} else {
		c.Status, c.Detail = OK, t.Address
	}
	return []Check{c}
}

func (d *run) data(s *config.Service, t config.Target) tmpl.Data {
	data := tmpl.ForTarget(t)
	data.Service, data.Version, data.PreviousVersion = s.Name, d.opt.Version, d.opt.Previous
	return data
}

func (d *run) runner(name string) transport.Runner {
	r, err := d.opt.Runners(name)
	if err != nil {
		return nil
	}
	return r
}

func (d *run) probe(ctx context.Context, s *config.Service, t config.Target, p config.Probe) []Check {
	c := Check{Scope: ScopeProbe, Subject: t.Name, Name: s.Name + "/" + p.ID + " (" + p.Type + ")"}
	switch p.Type {
	case "log", "access_log":
		return []Check{d.logProbe(ctx, c, s, t, p)}
	}
	col := &probe.Collector{Runners: d.opt.Runners, Log: d.opt.Log, Data: func(t config.Target) tmpl.Data { return d.data(s, t) }}
	pr, err := col.Build(probe.Job{Target: t, Spec: p})
	if err != nil {
		c.Status, c.Detail = Fail, err.Error()
		return []Check{c}
	}
	if cl, ok := pr.(io.Closer); ok {
		defer cl.Close()
	}
	ch, ok := pr.(probe.Checker)
	if !ok {
		c.Status, c.Detail = Skip, "1회 점검을 지원하지 않는 프로브"
		return []Check{c}
	}
	if err := ch.Check(ctx); err != nil {
		c.Status, c.Detail = Fail, err.Error()
	} else {
		c.Status, c.Detail = OK, "응답 정상"
	}
	return []Check{c}
}

// logProbe checks that the file is readable and, for access logs, that its
// format parses (a format mismatch means the 5xx rule would never fire).
func (d *run) logProbe(ctx context.Context, c Check, s *config.Service, t config.Target, p config.Probe) Check {
	pathTmpl := ""
	if p.Log != nil {
		pathTmpl = p.Log.Path
	} else {
		pathTmpl = p.AccessLog.Path
	}
	path, err := tmpl.Render(pathTmpl, d.data(s, t))
	if err == nil {
		c.Name += " " + path
	}
	r := d.runner(t.Name)
	if r == nil {
		c.Status, c.Detail = Fail, "로그 프로브에는 ssh 또는 local 연결이 필요함"
		return c
	}
	lines, err := tailLines(ctx, r, path, 200)
	if err != nil {
		c.Status, c.Detail = Fail, err.Error()
		return c
	}
	if p.AccessLog == nil {
		c.Status, c.Detail = OK, fmt.Sprintf("읽기 가능, 최근 %d줄", len(lines))
		return c
	}
	if len(lines) == 0 {
		c.Status, c.Detail = Warn, "읽기 가능하지만 아직 기록이 없어 형식을 확인하지 못함"
		return c
	}
	parsed := 0
	for _, l := range lines {
		if _, _, ok := probe.ParseAccessLine(p.AccessLog, l); ok {
			parsed++
		}
	}
	ratio := float64(parsed) / float64(len(lines))
	c.Detail = fmt.Sprintf("최근 %d줄 중 %d줄 해석 (%s 형식)", len(lines), parsed, p.AccessLog.Format)
	switch {
	case ratio >= 0.9:
		c.Status = OK
	case parsed == 0:
		c.Status = Fail
		c.Hint = "로그 형식이 설정과 다릅니다. format(combined/json)과 json 필드명(status_field 등)을 확인하십시오. 이대로면 5xx 규칙이 동작하지 않습니다"
	default:
		c.Status = Warn
		c.Hint = "일부 줄의 형식이 다릅니다. 로그 형식이 섞여 있는지 확인하십시오"
	}
	return c
}

func tailLines(ctx context.Context, r transport.Runner, path string, n int) ([]string, error) {
	var text string
	if _, local := r.(*transport.Local); local {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if st, err := f.Stat(); err == nil && st.Size() > 256<<10 {
			_, _ = f.Seek(-256<<10, io.SeekEnd)
		}
		b, err := io.ReadAll(f)
		if err != nil {
			return nil, err
		}
		text = string(b)
	} else {
		out, err := r.Run(ctx, "tail -n "+fmt.Sprint(n)+" "+transport.ShellQuote(path), nil)
		if err != nil {
			return nil, err
		}
		text = out
	}
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if l := strings.TrimRight(sc.Text(), "\r"); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

func (d *run) executor(ctx context.Context, s *config.Service, t config.Target, name string) []Check {
	ex, err := executor.New(name, d.cfg.Executors[name])
	base := Check{Scope: ScopeExecutor, Subject: t.Name}
	if err != nil {
		base.Name, base.Status, base.Detail = name, Fail, err.Error()
		return []Check{base}
	}
	dg, ok := ex.(executor.Diagnoser)
	if !ok {
		base.Name, base.Status, base.Detail = name+" ("+d.cfg.Executors[name].Type+")", Skip, "변경 없이 점검할 수 없는 실행기 (실제 동작은 dry-run으로 확인)"
		return []Check{base}
	}
	data := d.data(s, t)
	rc := &executor.RunContext{Target: t, Data: data, Runner: d.runner(t.Name), Runners: d.opt.Runners,
		Creds: d.cfg.Credentials, Checkpoint: data.Checkpoint, Log: d.opt.Log}
	var out []Check
	for _, f := range dg.Diagnose(ctx, rc) {
		c := base
		c.Name, c.Status, c.Detail = f.Name, Status(f.Status), f.Detail
		out = append(out, c)
	}
	return out
}

func (d *run) traffic(ctx context.Context, s *config.Service, first bool) []Check {
	name := s.Rollback.Traffic
	base := Check{Scope: ScopeTraffic, Subject: name}
	tc, err := executor.NewTraffic(name, d.cfg.Traffic[name], executor.TrafficEnv{
		Runners: d.opt.Runners, Creds: d.cfg.Credentials, Log: d.opt.Log, HTTP: d.opt.TrafficHTTP})
	if err != nil {
		base.Name, base.Status, base.Detail = "제어기 생성", Fail, err.Error()
		return []Check{base}
	}
	var out []Check
	pool, err := tc.Pool(ctx)
	if err != nil {
		base.Name, base.Status, base.Detail = "풀 조회", Fail, err.Error()
		return []Check{base}
	}
	enabled := 0
	ids := map[string]bool{}
	for _, m := range pool {
		ids[m.ID] = true
		if m.Enabled {
			enabled++
		}
	}
	out = append(out, Check{Scope: ScopeTraffic, Subject: name, Name: "풀 조회", Status: OK, Detail: fmt.Sprintf("멤버 %d개 중 %d개 활성", len(pool), enabled)})
	var missing []string
	var members []executor.Member
	for _, tn := range s.Targets {
		t, _ := d.cfg.Target(tn)
		m := executor.Member{Target: tn, Data: d.data(s, *t)}
		members = append(members, m)
		id, err := tc.MemberID(m)
		if err != nil || !ids[id] {
			missing = append(missing, fmt.Sprintf("%s(%s)", tn, id))
		}
	}
	if len(missing) > 0 {
		out = append(out, Check{Scope: ScopeTraffic, Subject: name, Name: s.Name + " 대상이 풀에 있는지", Status: Fail,
			Detail: "풀에 없음: " + strings.Join(missing, ", "),
			Hint:   "member_format(또는 server_name, target_id)이 LB에 등록된 이름·주소와 같은지 확인하십시오. 다르면 드레인이 실패합니다"})
	} else {
		out = append(out, Check{Scope: ScopeTraffic, Subject: name, Name: s.Name + " 대상이 풀에 있는지", Status: OK, Detail: fmt.Sprintf("%d개 모두 있음", len(s.Targets))})
	}
	if td, ok := tc.(executor.TrafficDiagnoser); ok && first {
		for _, f := range td.Diagnose(ctx, members) {
			out = append(out, Check{Scope: ScopeTraffic, Subject: name, Name: f.Name, Status: Status(f.Status), Detail: f.Detail})
		}
	}
	return out
}

// sudoJobs checks, per host with sudo_scope: changes, that every command
// Vigilante will run with sudo is allowed (`sudo -n -l`, which runs nothing).
// Hosts with plain connection.sudo get a warning: that needs unrestricted sudo.
func (d *run) sudoJobs(services []*config.Service) []func(context.Context) []Check {
	selected := map[string]bool{}
	for _, s := range services {
		selected[s.Name] = true
	}
	rules, _ := sudoers.Rules(d.cfg)
	var mine []sudoers.Rule
	for _, r := range rules {
		if selected[r.Service] {
			mine = append(mine, r)
		}
	}
	var jobs []func(context.Context) []Check
	for _, h := range sudoers.Group(d.cfg, mine) {
		h := h
		if !h.Sudo {
			continue // commands run as the login user: file permissions decide
		}
		if h.Scope != "changes" {
			jobs = append(jobs, func(context.Context) []Check {
				return []Check{{Scope: ScopeSudo, Subject: h.Name, Name: "sudo 범위", Status: Warn,
					Detail: "connection.sudo가 모든 명령을 `sudo -n sh -c`로 실행합니다 (무제한 sudo 필요)",
					Hint:   "connection.sudo_scope: changes로 바꾸고 `vigilante sudoers --target " + h.Name + "`가 만든 규칙만 허용하십시오"}}
			})
			continue
		}
		jobs = append(jobs, func(ctx context.Context) []Check {
			r, err := d.opt.Runners(h.Name)
			if err != nil {
				return []Check{{Scope: ScopeSudo, Subject: h.Name, Name: "sudo 규칙", Status: Fail, Detail: err.Error()}}
			}
			var out []Check
			for _, rule := range h.Rules {
				c := Check{Scope: ScopeSudo, Subject: h.Name, Name: rule.Command + " " + rule.Args}
				path, found := sudoers.Resolve(ctx, r, rule.Command)
				args := strings.Fields(sudoers.Example(rule.Args))
				for i := range args {
					args[i] = transport.ShellQuote(args[i])
				}
				_, err := r.Run(ctx, "sudo -n -l "+transport.ShellQuote(path)+" "+strings.Join(args, " "), nil)
				switch {
				case err == nil:
					c.Status, c.Detail = OK, path+" 허용됨"
				case !found:
					c.Status, c.Detail = Fail, rule.Command+"를 찾지 못했습니다 ("+rule.Why+")"
				default:
					c.Status, c.Detail = Fail, "sudo가 허용하지 않습니다: "+rule.Why
					c.Hint = "`vigilante sudoers --target " + h.Name + "`가 만든 규칙을 /etc/sudoers.d에 설치하십시오"
				}
				out = append(out, c)
			}
			return out
		})
	}
	return jobs
}

// OpenSSH's default MaxSessions is 10 sessions per connection.
const sshMaxSessions = 10

// capacity warns about load the probes put on targets and databases.
func (d *run) capacity(services []*config.Service) []Check {
	var out []Check
	streams := map[string]int{}
	polls := map[string]int{}
	for _, s := range services {
		for _, tn := range s.Targets {
			for _, p := range s.Probes {
				switch p.Type {
				case "log", "access_log":
					streams[tn]++
				case "host":
					polls[tn]++
				}
			}
		}
	}
	var names []string
	for tn := range streams {
		names = append(names, tn)
	}
	sort.Strings(names)
	for _, tn := range names {
		t, _ := d.cfg.Target(tn)
		if t.Connection.Type != "ssh" {
			continue
		}
		budget, reserved := t.Connection.MaxSessions, transport.DefaultReservedSessions
		if budget <= 0 {
			budget = transport.DefaultMaxSessions
		}
		if t.Connection.ReservedSessions != nil {
			reserved = *t.Connection.ReservedSessions
		}
		share := budget - reserved
		c := Check{Scope: ScopeCapacity, Subject: tn, Name: "SSH 세션 수",
			Detail: fmt.Sprintf("상시 로그 스트림 %d + 주기 명령 %d / 수집 몫 %d (세션 한도 %d 중 롤백 예약 %d)", streams[tn], polls[tn], share, budget, reserved)}
		switch {
		case streams[tn] >= share:
			c.Status, c.Hint = Fail, "로그 스트림이 수집 몫을 다 차지해 일부 로그 프로브와 주기 명령이 세션을 얻지 못합니다. 이 서버는 에이전트 모드를 쓰거나, sshd MaxSessions와 함께 connection.max_sessions를 늘리십시오"
		case streams[tn]+polls[tn] > share:
			c.Status, c.Hint = Warn, "주기 명령이 세션을 기다리게 되어 수집 간격이 늘어날 수 있습니다(롤백용 예약 세션은 그대로). 에이전트 모드나 더 큰 max_sessions를 고려하십시오"
		case budget > sshMaxSessions:
			c.Status, c.Hint = Warn, fmt.Sprintf("max_sessions %d가 OpenSSH 기본 MaxSessions %d보다 큽니다. sshd_config의 MaxSessions도 함께 올리십시오", budget, sshMaxSessions)
		default:
			c.Status = OK
		}
		out = append(out, c)
	}
	for _, s := range services {
		for _, p := range s.Probes {
			if p.Type != "db" || p.DB == nil || p.Interval <= 0 {
				continue
			}
			perSec := float64(p.DB.PoolSize) / p.Interval.Seconds() * float64(len(s.Targets))
			c := Check{Scope: ScopeCapacity, Subject: s.Name, Name: "DB 프로브 " + p.ID + " 새 커넥션",
				Detail: fmt.Sprintf("초당 %.1f개 (pool_size %d × 대상 %d / %s)", perSec, p.DB.PoolSize, len(s.Targets), p.Interval)}
			if perSec > 1 {
				c.Status, c.Hint = Warn, "DB 인증·접속 부하가 됩니다. interval을 늘리거나 pool_size를 줄이십시오"
			} else {
				c.Status = OK
			}
			out = append(out, c)
		}
	}
	return out
}

// Hint maps common failure messages to a remediation in plain words.
func Hint(detail string) string {
	d := strings.ToLower(detail)
	switch {
	case strings.Contains(d, "unable to authenticate"), strings.Contains(d, "no supported methods remain"):
		return "SSH 키가 대상 계정의 authorized_keys에 등록되어 있는지, credential의 user가 맞는지 확인하십시오"
	case strings.Contains(d, "key mismatch"):
		return "대상의 호스트 키가 바뀌었습니다. 서버 교체 여부를 확인한 뒤 known_hosts를 갱신하십시오"
	case strings.Contains(d, "knownhosts: key is unknown"):
		return "known_hosts에 대상 호스트 키를 등록하십시오 (ssh-keyscan으로 받은 뒤 지문 확인)"
	case strings.Contains(d, "sudo:") && (strings.Contains(d, "password is required") || strings.Contains(d, "terminal is required")):
		return "sudoers에 배포 계정의 NOPASSWD 규칙을 추가하십시오 (필요한 명령만 허용 권장)"
	case strings.Contains(d, "connection refused"), strings.Contains(d, "actively refused"):
		return "대상 포트가 닫혀 있습니다. 서비스 기동 여부와 방화벽을 확인하십시오"
	case strings.Contains(d, "i/o timeout"), strings.Contains(d, "deadline exceeded"), strings.Contains(d, "timed out"):
		return "응답이 없습니다. 네트워크 경로(방화벽, bastion, 보안 그룹)를 확인하십시오"
	case strings.Contains(d, "no such host"):
		return "주소를 찾을 수 없습니다. DNS 또는 address 값을 확인하십시오"
	case strings.Contains(d, "no such file"), strings.Contains(d, "cannot find"), strings.Contains(d, "cannot access"):
		return "경로가 없습니다. 설정의 경로와 대상 서버의 실제 배치를 확인하십시오"
	case strings.Contains(d, "permission denied"):
		return "실행 계정에 권한이 없습니다. 파일 권한이나 sudo 설정을 확인하십시오"
	case strings.Contains(d, " 401"), strings.Contains(d, "unauthorized"):
		return "인증에 실패했습니다. 자격증명 값을 확인하십시오"
	case strings.Contains(d, " 403"), strings.Contains(d, "forbidden"):
		return "권한이 부족합니다. 장비·클라우드 계정의 역할을 확인하십시오"
	case strings.Contains(d, "x509"), strings.Contains(d, "certificate"):
		return "TLS 인증서를 신뢰할 수 없습니다. CA를 등록하거나 사설 장비면 tls_skip_verify를 검토하십시오"
	case strings.Contains(d, "env ") && strings.Contains(d, "empty"):
		return "필요한 환경변수가 비어 있습니다"
	}
	return ""
}

// ErrFailed signals that at least one check failed (CLI exit code 1).
var ErrFailed = errors.New("doctor: failing checks")
