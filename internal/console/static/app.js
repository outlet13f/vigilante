// Vigilante console: a small single-page app on the public v2 API.
// Every piece of server data is inserted as text (never as HTML).
'use strict';

const $ = (sel) => document.querySelector(sel);
const view = () => $('#view');

// h builds an element: h('a', {href: '#/'}, 'text', child, ...).
function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = v;
    else el.setAttribute(k, v === true ? '' : String(v));
  }
  for (const kid of kids.flat()) {
    if (kid === undefined || kid === null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

// show replaces the page content, skipping absent (null/false) sections.
function show(...kids) {
  view().replaceChildren(...kids.flat().filter((k) => k !== null && k !== undefined && k !== false));
}

const state = {
  token: sessionStorage.getItem('vgl_token') || '',
  mode: 'token',
  me: null,
  teams: {},        // service -> team
  events: [],       // recent events for the ticker
  refreshTimer: 0,
};

function cookie(name) {
  const m = document.cookie.split('; ').find((c) => c.startsWith(name + '='));
  return m ? decodeURIComponent(m.slice(name.length + 1)) : '';
}

function uuid() {
  if (window.crypto && crypto.randomUUID) return crypto.randomUUID();
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
}

class ApiError extends Error {
  constructor(status, problem) {
    super((problem && (problem.detail || problem.title)) || ('HTTP ' + status));
    this.status = status;
    this.code = problem && problem.code;
  }
}

async function api(method, path, body) {
  const headers = { Accept: 'application/json' };
  if (state.token) headers.Authorization = 'Bearer ' + state.token;
  const opt = { method, headers, credentials: 'same-origin' };
  if (method !== 'GET') {
    headers['Idempotency-Key'] = uuid();
    const csrf = cookie('vgl_csrf');
    if (csrf) headers['X-CSRF-Token'] = csrf;
    if (body !== undefined) {
      headers['Content-Type'] = 'application/json';
      opt.body = JSON.stringify(body);
    }
  }
  const r = await fetch(path, opt);
  const text = await r.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch (_) { data = null; }
  if (r.status === 401) {
    showLogin();
    throw new ApiError(401, data);
  }
  if (!r.ok) throw new ApiError(r.status, data);
  return data;
}

// ---------------------------------------------------------------- formatting

function fmtTime(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d)) return iso;
  const p = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function ago(iso) {
  const s = (Date.now() - new Date(iso).getTime()) / 1000;
  if (!isFinite(s)) return '';
  if (s < 60) return Math.max(0, Math.round(s)) + '초 전';
  if (s < 3600) return Math.round(s / 60) + '분 전';
  if (s < 86400) return Math.round(s / 3600) + '시간 전';
  return Math.round(s / 86400) + '일 전';
}

const STATE = {
  PENDING: ['대기', 'info'], BASELINE: ['기준선', 'info'], OBSERVING: ['관측 중', 'info'],
  PROMOTED: ['단계 통과', 'ok'], SUCCEEDED: ['완료', 'ok'], HELD: ['보류', 'warn'],
  AWAITING_APPROVAL: ['승인 대기', 'warn'], ROLLING_BACK: ['롤백 중', 'warn'],
  ROLLED_BACK: ['롤백됨', 'warn'], ROLLBACK_FAILED: ['롤백 실패', 'bad'], ABORTED: ['중단', 'neutral'],
};

function badge(st) {
  const [label, cls] = STATE[st] || [st, 'neutral'];
  return h('span', { class: 'badge b-' + cls, title: st }, label);
}

function circuitBadge(st) {
  const m = { CLOSED: ['정상 (CLOSED)', 'ok'], OPEN: ['차단 (OPEN)', 'bad'], HALF_OPEN: ['시험 (HALF_OPEN)', 'warn'] }[st] || [st, 'neutral'];
  return h('span', { class: 'badge b-' + m[1] }, m[0]);
}

function toast(msg, err) {
  const el = h('div', { class: 'msg' + (err ? ' err' : '') }, msg);
  $('#toast').append(el);
  setTimeout(() => el.remove(), err ? 9000 : 5000);
}

// ---------------------------------------------------------------- permissions

const RANK = { viewer: 1, deployer: 2, operator: 3, admin: 4 };

// can reports whether a grant gives role on a service (scope * / team= /
// service=); without a service only a "*" grant counts (circuit, freezes).
// It only decides which buttons to show: the API checks every call, scopes
// of API keys included.
function can(role, service) {
  if (!state.me) return false;
  return (state.me.grants || []).some((g) => {
    const [r, scope] = g.split('@');
    if ((RANK[r] || 0) < RANK[role]) return false;
    if (scope === '*') return true;
    if (!service) return false;
    if (scope === 'service=' + service) return true;
    return !!state.teams[service] && scope === 'team=' + state.teams[service];
  });
}

// ---------------------------------------------------------------- dialogs

function ask(title, text, okLabel, danger) {
  return new Promise((resolve) => {
    const d = $('#ask');
    $('#ask-title').textContent = title;
    $('#ask-text').textContent = text || '';
    $('#ask-reason').value = '';
    const ok = $('#ask-ok');
    ok.textContent = okLabel || '확인';
    ok.className = danger ? 'danger' : '';
    d.onclose = () => resolve(d.returnValue === 'ok' ? $('#ask-reason').value.trim() : null);
    d.showModal();
    $('#ask-reason').focus();
  });
}

async function act(label, fn) {
  try {
    const res = await fn();
    toast(label + ' 요청을 보냈습니다' + (res && res.id && res.kind ? ` (작업 ${res.id})` : ''));
    scheduleRefresh(300);
  } catch (e) {
    toast(label + ' 실패: ' + e.message, true);
  }
}

// ---------------------------------------------------------------- login

async function showLogin() {
  stopLive();
  $('#logout').hidden = true;
  $('#me').textContent = '';
  let mode = 'token';
  try {
    const r = await fetch('/console/auth/mode', { credentials: 'same-origin' });
    mode = (await r.json()).mode;
  } catch (_) { /* keep token */ }
  state.mode = mode;
  const box = h('div', { class: 'panel login' }, h('h1', {}, 'Vigilante 콘솔'));
  if (mode === 'oidc') {
    box.append(h('p', { class: 'sub' }, '회사 계정으로 로그인합니다.'),
      h('button', { onclick: () => { location.href = '/console/auth/login'; } }, '회사 계정으로 로그인'));
  } else {
    const input = h('input', { type: 'password', id: 'token', placeholder: 'vgl_… / vgk_… / vat_…', autocomplete: 'off', style: 'width:100%' });
    box.append(
      h('p', { class: 'sub' }, '이 서버에는 SSO가 설정되지 않았습니다. 서비스 계정 토큰이나 API 키를 넣으십시오. 토큰은 이 탭에만 보관됩니다.'),
      h('label', { for: 'token' }, 'API 토큰'), input,
      h('div', { class: 'row end', style: 'margin-top:12px' }, h('button', {
        onclick: async () => {
          state.token = input.value.trim();
          sessionStorage.setItem('vgl_token', state.token);
          await start();
        },
      }, '로그인')));
  }
  show(box);
}

async function logout() {
  if (state.mode === 'oidc') {
    await fetch('/console/auth/logout', { method: 'POST', credentials: 'same-origin', headers: { 'X-CSRF-Token': cookie('vgl_csrf') } });
  }
  sessionStorage.removeItem('vgl_token');
  state.token = '';
  state.me = null;
  showLogin();
}

// ---------------------------------------------------------------- live events (SSE over fetch, so tokens work too)

let liveAbort = null;

function setLive(on) {
  const el = $('#live');
  el.className = 'live ' + (on ? 'on' : 'off');
  el.title = on ? '실시간 이벤트 수신 중' : '실시간 연결 끊김 (재시도 중)';
}

function stopLive() {
  if (liveAbort) liveAbort.abort();
  liveAbort = null;
  setLive(false);
}

async function live() {
  stopLive();
  const ctl = new AbortController();
  liveAbort = ctl;
  let last = '';
  while (!ctl.signal.aborted) {
    try {
      const headers = { Accept: 'text/event-stream' };
      if (state.token) headers.Authorization = 'Bearer ' + state.token;
      if (last) headers['Last-Event-ID'] = last;
      const r = await fetch('/v2/events', { headers, credentials: 'same-origin', signal: ctl.signal });
      if (!r.ok || !r.body) throw new Error('HTTP ' + r.status);
      setLive(true);
      const reader = r.body.getReader();
      const dec = new TextDecoder();
      let buf = '';
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let i;
        while ((i = buf.indexOf('\n\n')) >= 0) {
          const chunk = buf.slice(0, i);
          buf = buf.slice(i + 2);
          const ev = parseSSE(chunk);
          if (ev) {
            last = ev.id;
            onEvent(ev.data);
          }
        }
      }
    } catch (e) {
      if (ctl.signal.aborted) return;
    }
    setLive(false);
    await new Promise((res) => setTimeout(res, 3000));
  }
}

function parseSSE(chunk) {
  let id = '', data = '';
  for (const line of chunk.split('\n')) {
    if (line.startsWith('id: ')) id = line.slice(4);
    else if (line.startsWith('data: ')) data += line.slice(6);
  }
  if (!id || !data) return null;
  try { return { id, data: JSON.parse(data) }; } catch (_) { return null; }
}

const EVENT_LABEL = {
  'vigilante.deployment.created': '배포 등록', 'vigilante.deployment.marked_good': '정상 버전 등록',
  'vigilante.observation.started': '관측 시작', 'vigilante.observation.passed': '단계 통과',
  'vigilante.observation.failed': '단계 실패', 'vigilante.observation.held': '보류',
  'vigilante.observation.aborted': '관측 중단', 'vigilante.rollback.started': '롤백 시작',
  'vigilante.rollback.completed': '롤백 완료', 'vigilante.rollback.failed': '롤백 실패',
  'vigilante.approval.requested': '승인 요청', 'vigilante.approval.decided': '승인 결정',
  'vigilante.circuit.opened': '서킷 열림', 'vigilante.circuit.half_opened': '서킷 시험',
  'vigilante.circuit.closed': '서킷 닫힘', 'vigilante.agent.lost': '에이전트 끊김',
  'vigilante.webhook.disabled': '웹훅 중지', 'vigilante.ping': '테스트',
};

function onEvent(ev) {
  state.events.unshift(ev);
  state.events.length = Math.min(state.events.length, 30);
  if (ev.type === 'vigilante.approval.requested' || ev.type === 'vigilante.rollback.failed' || ev.type === 'vigilante.circuit.opened') {
    toast(`${EVENT_LABEL[ev.type] || ev.type}: ${ev.service || ''} ${ev.subject || ''}`, true);
  }
  const cur = location.hash;
  if (cur.startsWith('#/d/') && decodeURIComponent(cur.slice(4)) !== ev.subject && !ev.type.includes('circuit')) return;
  if (cur.startsWith('#/audit') || cur.startsWith('#/services')) return;
  scheduleRefresh(700);
}

function scheduleRefresh(ms) {
  clearTimeout(state.refreshTimer);
  state.refreshTimer = setTimeout(() => route(true), ms);
}

// ---------------------------------------------------------------- views

function deploymentRows(items) {
  if (!items.length) return h('p', { class: 'empty' }, '없음');
  return h('table', {},
    h('thead', {}, h('tr', {}, h('th', {}, '배포'), h('th', {}, '서비스'), h('th', {}, '버전'), h('th', {}, '상태'), h('th', {}, '이유'), h('th', {}, '갱신'))),
    h('tbody', {}, items.map((d) => h('tr', {},
      h('td', { class: 'mono' }, h('a', { href: '#/d/' + encodeURIComponent(d.id) }, d.id)),
      h('td', {}, d.service),
      h('td', { class: 'mono' }, `${d.previous_version || '?'} → ${d.version}`),
      h('td', {}, badge(d.state)),
      h('td', {}, (d.reason || '').slice(0, 140)),
      h('td', { class: 'num', title: fmtTime(d.updated_at) }, ago(d.updated_at))))));
}

async function overview() {
  const [circuit, freezes, deps] = await Promise.all([
    api('GET', '/v2/circuit'), api('GET', '/v2/freezes'), api('GET', '/v2/deployments?limit=200')]);
  const items = deps.items;
  const attention = items.filter((d) => ['AWAITING_APPROVAL', 'ROLLBACK_FAILED', 'HELD'].includes(d.state));
  const running = items.filter((d) => ['OBSERVING', 'ROLLING_BACK'].includes(d.state));
  const activeFreezes = freezes.items.filter((f) => f.active);

  const circuitPanel = h('div', { class: 'panel' + (circuit.state === 'OPEN' ? ' danger' : '') },
    h('div', { class: 'row' }, h('strong', {}, '서킷브레이커 '), circuitBadge(circuit.state),
      circuit.reason ? h('span', { class: 'sub', style: 'margin:0' }, ' ' + circuit.reason) : null),
    can('admin') ? h('div', { class: 'row', style: 'margin-top:10px' },
      circuit.state === 'CLOSED'
        ? h('button', { class: 'danger', onclick: async () => {
          const reason = await ask('서킷 열기 (비상 정지)', '모든 자동 롤백과 새 배포가 멈춥니다.', '서킷 열기', true);
          if (reason) act('서킷 열기', () => api('POST', '/v2/circuit/trip', { reason }));
        } }, '비상 정지 (서킷 열기)')
        : h('button', { onclick: async () => {
          const reason = await ask('서킷 닫기', '원인 조사가 끝났는지 확인하십시오. 자동화가 다시 동작합니다.', '서킷 닫기');
          if (reason) act('서킷 닫기', () => api('POST', '/v2/circuit/reset', { reason }));
        } }, '서킷 닫기')) : null);

  show(
    h('h1', {}, '현황'),
    h('p', { class: 'sub' }, '조치가 필요한 배포와 진행 중인 관측·롤백. 이벤트가 오면 자동으로 갱신됩니다.'),
    h('div', { class: 'grid' },
      h('div', { class: 'stat' }, h('div', { class: 'k' }, '서킷'), h('div', { class: 'v' }, circuitBadge(circuit.state))),
      h('div', { class: 'stat' }, h('div', { class: 'k' }, '조치 필요'), h('div', { class: 'v' }, attention.length)),
      h('div', { class: 'stat' }, h('div', { class: 'k' }, '진행 중'), h('div', { class: 'v' }, running.length)),
      h('div', { class: 'stat' }, h('div', { class: 'k' }, '변경 동결'), h('div', { class: 'v' }, activeFreezes.length ? activeFreezes.map((f) => f.name).join(', ') : '없음'))),
    circuitPanel,
    h('h2', {}, '조치 필요 (승인 대기 · 롤백 실패 · 보류)'), deploymentRows(attention),
    h('h2', {}, '진행 중'), deploymentRows(running),
    h('h2', {}, '최근 배포'), deploymentRows(items.slice(0, 20)),
    h('h2', {}, '최근 이벤트'),
    state.events.length ? h('div', { class: 'panel ticker' }, h('ul', { class: 'timeline' }, state.events.map((e) => h('li', {},
      h('span', { class: 'num' }, fmtTime(e.time)), h('span', {}, EVENT_LABEL[e.type] || e.type),
      h('span', {}, e.service || '', ' ', e.subject && e.subject !== 'circuit'
        ? h('a', { href: '#/d/' + encodeURIComponent(e.subject), class: 'mono' }, e.subject) : '')))))
      : h('p', { class: 'empty' }, '이 화면을 연 뒤 들어온 이벤트가 여기에 표시됩니다.'));
}

async function deploymentsView(params) {
  const q = new URLSearchParams({ limit: '50' });
  if (params.get('service')) q.set('service', params.get('service'));
  if (params.get('state')) q.set('state', params.get('state'));
  if (params.get('cursor')) q.set('cursor', params.get('cursor'));
  const deps = await api('GET', '/v2/deployments?' + q);
  const svc = h('input', { id: 'f-svc', value: params.get('service') || '', placeholder: '서비스' });
  const st = h('select', { id: 'f-state' }, h('option', { value: '' }, '모든 상태'),
    Object.keys(STATE).map((k) => h('option', { value: k, selected: params.get('state') === k }, STATE[k][0])));
  const go = (cursor) => {
    const p = new URLSearchParams();
    if (svc.value) p.set('service', svc.value);
    if (st.value) p.set('state', st.value);
    if (cursor) p.set('cursor', cursor);
    location.hash = '#/deployments?' + p;
  };
  show(h('h1', {}, '배포'),
    h('div', { class: 'filters' }, h('div', {}, h('label', { for: 'f-svc' }, '서비스'), svc), h('div', {}, h('label', { for: 'f-state' }, '상태'), st),
      h('button', { onclick: () => go() }, '조회')),
    deploymentRows(deps.items),
    deps.next_cursor ? h('div', { class: 'row end', style: 'margin-top:10px' }, h('button', { class: 'secondary', onclick: () => go(deps.next_cursor) }, '다음')) : null);
}

async function deploymentView(id) {
  const [d, ops] = await Promise.all([api('GET', '/v2/deployments/' + encodeURIComponent(id)),
    api('GET', '/v2/operations?limit=20&deployment_id=' + encodeURIComponent(id))]);
  const svc = d.service;
  const actions = h('div', { class: 'row' });
  if (d.state === 'OBSERVING' && can('deployer', svc)) {
    actions.append(h('button', { class: 'secondary', onclick: async () => {
      const reason = await ask('관측 중단', '판정 없이 관측을 멈춥니다. 롤백은 하지 않습니다.', '중단');
      if (reason) act('관측 중단', () => api('POST', `/v2/deployments/${encodeURIComponent(id)}/abort`, { reason }));
    } }, '관측 중단'));
  }
  if (!['ROLLING_BACK', 'ROLLED_BACK', 'AWAITING_APPROVAL'].includes(d.state) && can('operator', svc)) {
    actions.append(h('button', { class: 'danger', onclick: async () => {
      const reason = await ask('수동 롤백', `${d.service}를 ${d.previous_version}(으)로 되돌립니다.`, '롤백', true);
      if (reason) act('롤백', () => api('POST', `/v2/deployments/${encodeURIComponent(id)}/rollbacks`, { reason }));
    } }, '롤백'));
  }

  const pr = d.pending_rollback;
  let approval = null;
  if (d.state === 'AWAITING_APPROVAL') {
    const decide = (decision) => async () => {
      const title = decision === 'approve' ? '롤백 승인' : '롤백 거절';
      const text = decision === 'approve'
        ? `준비된 롤백(${(pr && pr.targets || []).join(', ')} → ${d.previous_version})을 실행합니다.`
        : '새 버전을 유지하고, 격리했던 대상을 트래픽에 다시 넣은 뒤 보류합니다.';
      const comment = await ask(title, text, title, decision === 'approve');
      if (comment) act(title, () => api('POST', `/v2/deployments/${encodeURIComponent(id)}/approvals`, { decision, comment }));
    };
    approval = h('div', { class: 'panel alert' },
      h('h2', { style: 'margin-top:0' }, pr ? '롤백이 승인을 기다립니다' : '상위 복구 단계가 승인을 기다립니다'),
      pr ? h('dl', { class: 'facts' },
        h('dt', {}, '대상'), h('dd', {}, (pr.targets || []).join(', ')),
        h('dt', {}, '격리됨'), h('dd', {}, (pr.drained || []).join(', ') || '없음'),
        h('dt', {}, '이유'), h('dd', {}, pr.reason),
        h('dt', {}, '만료'), h('dd', {}, fmtTime(pr.expires_at) + (pr.escalated ? ' (만료됨, 상위 호출함)' : ''))) : h('p', {}, d.reason),
      can('operator', svc) ? h('div', { class: 'row', style: 'margin-top:12px' },
        h('button', { class: 'danger', onclick: decide('approve') }, '승인 (롤백 실행)'),
        pr ? h('button', { class: 'secondary', onclick: decide('reject') }, '거절 (새 버전 유지)') : null)
        : h('p', { class: 'sub' }, 'operator 권한이 있어야 결정할 수 있습니다.'));
  }

  // Verdict assessment (pilot decision quality): was the verdict right?
  let assess = null;
  if (['SUCCEEDED', 'PROMOTED', 'HELD', 'ROLLED_BACK', 'ROLLBACK_FAILED', 'ABORTED'].includes(d.state)) {
    const failed = (d.events || []).some((e) => e.kind === 'verdict' && e.message.startsWith('FAIL:'));
    const fb = d.feedback;
    const LABEL = { correct: '판정이 맞음', false_positive: '오탐 (정상인데 FAIL)', false_negative: '미탐 (문제가 있었는데 통과)', unclear: '판단 불가' };
    const outcome = h('select', { id: 'fb-outcome' }, ['correct', failed ? 'false_positive' : 'false_negative', 'unclear']
      .map((o) => h('option', { value: o, selected: fb && fb.outcome === o }, LABEL[o])));
    const incident = h('input', { id: 'fb-incident', placeholder: 'INC0012345', value: (fb && fb.incident) || '' });
    const note = h('input', { id: 'fb-note', placeholder: '확인한 내용', value: (fb && fb.note) || '', style: 'min-width:280px' });
    assess = h('div', { class: 'panel' + (fb ? '' : ' alert') },
      h('h2', { style: 'margin-top:0' }, '판정 평가'),
      h('p', { class: 'sub' }, fb
        ? `${LABEL[fb.outcome] || fb.outcome} · ${fb.by} · ${fmtTime(fb.at)}${fb.incident ? ' · ' + fb.incident : ''}${fb.note ? ' · ' + fb.note : ''}`
        : '아직 평가하지 않았습니다. 판정 품질(오탐·미탐) 측정에 쓰입니다.'),
      can('deployer', svc) ? h('div', { class: 'filters' },
        h('div', {}, h('label', { for: 'fb-outcome' }, '평가'), outcome),
        h('div', {}, h('label', { for: 'fb-incident' }, '인시던트'), incident),
        h('div', {}, h('label', { for: 'fb-note' }, '메모'), note),
        h('button', { onclick: () => act('판정 평가', () => api('PUT', `/v2/deployments/${encodeURIComponent(id)}/feedback`,
          { outcome: outcome.value, incident: incident.value.trim(), note: note.value.trim() })) }, fb ? '고치기' : '저장')) : null);
  }

  const ev = d.last_evaluation;
  show(
    h('p', {}, h('a', { href: '#/deployments' }, '← 배포 목록')),
    h('div', { class: 'row' }, h('h1', {}, d.id), badge(d.state), h('span', { class: 'badge b-neutral', title: 'CI 종료 코드' }, 'exit ' + d.exit_code)),
    h('p', { class: 'sub' }, d.reason || ''),
    approval,
    h('div', { class: 'panel' }, h('dl', { class: 'facts' },
      h('dt', {}, '서비스'), h('dd', {}, svc + (state.teams[svc] ? ` (팀 ${state.teams[svc]})` : '')),
      h('dt', {}, '버전'), h('dd', { class: 'mono' }, `${d.previous_version || '?'} → ${d.version}`),
      h('dt', {}, '단계'), h('dd', {}, d.phase || '-'),
      h('dt', {}, '대상'), h('dd', {}, (d.targets || []).join(', ') || '-'),
      h('dt', {}, '등록'), h('dd', {}, `${fmtTime(d.created_at)} · ${d.created_by || ''}`),
      d.rollback_requested_by ? [h('dt', {}, '롤백 요청'), h('dd', {}, d.rollback_requested_by)] : null,
      d.approved_by ? [h('dt', {}, '승인'), h('dd', {}, d.approved_by)] : null,
      d.change_ticket ? [h('dt', {}, '변경 티켓'), h('dd', {}, d.change_ticket.number + (d.change_ticket.unverified ? ' (미검증)' : ''))] : null,
      d.freeze_override ? [h('dt', {}, '동결 예외'), h('dd', {}, d.freeze_override)] : null,
      d.dry_run ? [h('dt', {}, '드라이런'), h('dd', {}, '조치는 기록만 했습니다 (dry_run)')] : null,
      ev ? [h('dt', {}, '최근 평가'), h('dd', {}, `${fmtTime(ev.time)} · 위반 ${ev.failing}, 보류 ${ev.holding}, 경고 ${ev.warning} (판정 ${ev.known}/${ev.known + ev.unknown})`)] : null),
    actions.childElementCount ? h('div', { style: 'margin-top:12px' }, actions) : null),
    assess,
    h('h2', {}, '규칙 위반'),
    (d.breaches && d.breaches.length) ? h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, '규칙'), h('th', {}, '대상'), h('th', {}, '조치'), h('th', {}, '내용'))),
      h('tbody', {}, d.breaches.map((b) => h('tr', {}, h('td', {}, b.rule), h('td', {}, b.target), h('td', {}, b.action + (b.environmental ? ' (환경 요인)' : '')), h('td', { class: 'mono' }, b.detail || '')))))
      : h('p', { class: 'empty' }, '없음'),
    h('h2', {}, '작업'),
    ops.items.length ? h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, '작업'), h('th', {}, '종류'), h('th', {}, '상태'), h('th', {}, '결과'), h('th', {}, '요청'))),
      h('tbody', {}, ops.items.map((o) => h('tr', {}, h('td', { class: 'mono' }, o.id), h('td', {}, o.kind), h('td', {}, o.status),
        h('td', {}, o.result ? `${o.result.state || ''} exit ${o.result.exit_code ?? ''}` : (o.error || '')), h('td', {}, `${o.created_by || ''} ${ago(o.created_at)}`)))))
      : h('p', { class: 'empty' }, '없음'),
    h('h2', {}, '타임라인'),
    h('div', { class: 'panel' }, h('ul', { class: 'timeline' }, (d.events || []).slice().reverse().map((e) => h('li', {},
      h('span', { class: 'num' }, fmtTime(e.time)), h('span', {}, e.kind), h('span', {}, e.message))))));
}

async function servicesView() {
  const svcs = await api('GET', '/v2/services?limit=500');
  show(h('h1', {}, '서비스'),
    h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, '서비스'), h('th', {}, '팀'), h('th', {}, '정상 버전'), h('th', {}, '단계'), h('th', {}, '대상'), h('th', {}, '실행기 / 트래픽'), h('th', {}, '프리셋'))),
      h('tbody', {}, svcs.items.map((s) => h('tr', {},
        h('td', {}, h('a', { href: '#/deployments?service=' + encodeURIComponent(s.name) }, s.name)),
        h('td', {}, s.team || ''), h('td', { class: 'mono' }, s.last_good_version || '-'),
        h('td', {}, (s.phases || []).join(' → ')), h('td', {}, String((s.targets || []).length)),
        h('td', {}, s.executor + (s.traffic ? ' / ' + s.traffic : '')), h('td', {}, s.preset || ''))))));
}

async function freezesView() {
  const fr = await api('GET', '/v2/freezes');
  const rows = fr.items.length ? h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, '이름'), h('th', {}, '상태'), h('th', {}, '기간'), h('th', {}, '범위'), h('th', {}, '자동 롤백'), h('th', {}, ''))),
    h('tbody', {}, fr.items.map((f) => h('tr', {},
      h('td', {}, f.name, f.reason ? h('div', { class: 'sub', style: 'margin:0' }, f.reason) : null),
      h('td', {}, f.active ? h('span', { class: 'badge b-warn' }, `동결 중 (~${fmtTime(f.until)})`) : h('span', { class: 'badge b-neutral' }, '예정')),
      h('td', {}, f.weekly || `${fmtTime(f.starts_at)} ~ ${fmtTime(f.ends_at)}`),
      h('td', {}, [...(f.services || []), ...(f.teams || []).map((t) => '팀 ' + t)].join(', ') || '전체'),
      h('td', {}, f.allow_rollback ? '허용' : '금지'),
      h('td', {}, f.source === 'api' && can('admin') ? h('button', { class: 'secondary', onclick: async () => {
        const reason = await ask('동결 종료', `${f.name}을(를) 지금 끝냅니다.`, '종료');
        if (reason) act('동결 종료', () => api('DELETE', '/v2/freezes/' + encodeURIComponent(f.id)));
      } }, '종료') : (f.source === 'config' ? h('span', { class: 'sub', style: 'margin:0' }, '설정 파일') : ''))))))
    : h('p', { class: 'empty' }, '동결 없음');

  let form = null;
  if (can('admin')) {
    const name = h('input', { id: 'fz-name', placeholder: 'incident-123' });
    const reason = h('input', { id: 'fz-reason', placeholder: '장애 대응 중', style: 'min-width:240px' });
    const until = h('input', { id: 'fz-until', type: 'datetime-local' });
    const scope = h('input', { id: 'fz-teams', placeholder: 'payments, search (비우면 전체)' });
    const allow = h('input', { id: 'fz-rb', type: 'checkbox', checked: true });
    form = h('div', { class: 'panel' }, h('h2', { style: 'margin-top:0' }, '동결 선언'),
      h('div', { class: 'filters' },
        h('div', {}, h('label', { for: 'fz-name' }, '이름'), name), h('div', {}, h('label', { for: 'fz-reason' }, '사유'), reason),
        h('div', {}, h('label', { for: 'fz-until' }, '종료 시각'), until), h('div', {}, h('label', { for: 'fz-teams' }, '팀'), scope),
        h('div', {}, h('label', { for: 'fz-rb' }, '자동 롤백 허용'), allow),
        h('button', { onclick: () => {
          if (!name.value || !until.value || !reason.value) { toast('이름, 사유, 종료 시각을 넣으십시오', true); return; }
          const teams = scope.value.split(',').map((s) => s.trim()).filter(Boolean);
          act('동결 선언', () => api('POST', '/v2/freezes', { name: name.value, reason: reason.value,
            ends_at: new Date(until.value).toISOString(), teams, allow_rollback: allow.checked }));
        } }, '선언')));
  }
  show(h('h1', {}, '변경 동결'),
    h('p', { class: 'sub' }, '동결 중에는 새 배포와 단계 시작을 받지 않습니다. 자동 롤백은 기간 설정에 따릅니다.'), form, rows);
}

async function auditView(params) {
  // The trail is oldest first: without a start, show the last 24 hours.
  if (!params.get('since')) params.set('since', new Date(Date.now() - 86400000).toISOString().replace(/\.\d+Z$/, 'Z'));
  const q = new URLSearchParams({ limit: '100' });
  for (const k of ['actor', 'action', 'service', 'since', 'cursor']) if (params.get(k)) q.set(k, params.get(k));
  let page;
  try {
    page = await api('GET', '/v2/audit-events?' + q);
  } catch (e) {
    if (e.status === 403) {
      show(h('h1', {}, '감사 기록'), h('p', { class: 'panel' }, '감사 기록은 전체 범위(viewer@*) 권한이 있어야 볼 수 있습니다.'));
      return;
    }
    throw e;
  }
  const inputs = {};
  const field = (k, label, ph) => { inputs[k] = h('input', { id: 'a-' + k, value: params.get(k) || '', placeholder: ph }); return h('div', {}, h('label', { for: 'a-' + k }, label), inputs[k]); };
  const go = (cursor) => {
    const p = new URLSearchParams();
    for (const [k, el] of Object.entries(inputs)) if (el.value) p.set(k, el.value);
    if (cursor) p.set('cursor', cursor);
    location.hash = '#/audit?' + p;
  };
  show(h('h1', {}, '감사 기록'),
    h('p', { class: 'sub' }, '누가 무엇을 했는지. 기록은 해시 체인으로 묶여 있어 고치면 `vigilante audit verify`가 찾아냅니다.'),
    h('div', { class: 'filters' }, field('actor', '작업자', 'user:alice'), field('action', '동작', 'rollback.manual'),
      field('service', '서비스', 'order-api'), field('since', '시작 (RFC 3339)', '2026-10-01T00:00:00Z'), h('button', { onclick: () => go() }, '조회')),
    page.items.length ? h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, '시각'), h('th', {}, '작업자'), h('th', {}, '동작'), h('th', {}, '서비스 / 배포'), h('th', {}, '사유'), h('th', {}, '티켓'))),
      h('tbody', {}, page.items.map((a) => h('tr', {}, h('td', { class: 'num' }, fmtTime(a.time)), h('td', {}, a.actor || ''), h('td', { class: 'mono' }, a.action || a.kind),
        h('td', {}, a.service || '', a.deployment_id ? [' ', h('a', { href: '#/d/' + encodeURIComponent(a.deployment_id), class: 'mono' }, a.deployment_id)] : ''),
        h('td', {}, a.reason || ''), h('td', {}, a.ticket || '')))))
      : h('p', { class: 'empty' }, '없음'),
    page.next_cursor ? h('div', { class: 'row end', style: 'margin-top:10px' }, h('button', { class: 'secondary', onclick: () => go(page.next_cursor) }, '다음')) : null);
}

// ---------------------------------------------------------------- routing

async function route(quiet) {
  if (!state.me) return;
  const hash = location.hash || '#/';
  const [path, query] = hash.slice(1).split('?');
  const params = new URLSearchParams(query || '');
  const section = path.split('/')[1] || 'overview';
  for (const a of document.querySelectorAll('nav a')) {
    a.classList.toggle('active', a.dataset.nav === (section === 'd' ? 'deployments' : section || 'overview'));
  }
  try {
    if (path === '/' || path === '') await overview();
    else if (path === '/deployments') await deploymentsView(params);
    else if (path.startsWith('/d/')) await deploymentView(decodeURIComponent(path.slice(3)));
    else if (path === '/services') await servicesView();
    else if (path === '/freezes') await freezesView();
    else if (path === '/audit') await auditView(params);
    else show(h('p', { class: 'empty' }, '없는 화면입니다.'));
  } catch (e) {
    if (e.status === 401) return;
    if (!quiet) show(h('div', { class: 'panel danger' }, '불러오지 못했습니다: ' + e.message));
    else toast('갱신 실패: ' + e.message, true);
  }
}

async function start() {
  try {
    state.me = await api('GET', '/v2/me');
  } catch (e) {
    if (e.status !== 401) toast('로그인 확인 실패: ' + e.message, true);
    return;
  }
  try {
    const svcs = await api('GET', '/v2/services?limit=500');
    for (const s of svcs.items) state.teams[s.name] = s.team || '';
  } catch (_) { /* no services visible */ }
  const grants = (state.me.grants || []).join(', ');
  $('#me').textContent = state.me.id;
  $('#me').title = grants;
  $('#logout').hidden = false;
  live();
  route();
}

window.addEventListener('hashchange', () => route());
document.addEventListener('DOMContentLoaded', () => {
  $('#logout').addEventListener('click', logout);
  start();
});
