// This is the operator UI. It polls the scheduler's status and the infra log every few
// seconds and shows them, read-only. Everything is built as DOM nodes with textContent, never as
// HTML, since names, reasons and reports come from the spec, the agents and an LLM.
'use strict';

(() => {
  const POLL_MS = 5000;
  const STALE_MS = 15000;
  const MAX_LOGS = 2000;
  const LOG_PAGE = 500;

  // The helpers below build DOM and SVG nodes.

  function h(tag, props, ...kids) {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(props || {})) {
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k === 'text') el.textContent = v;
      else if (k === 'style') for (const [p, val] of Object.entries(v)) el.style.setProperty(p, val);
      else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, v === true ? '' : v);
    }
    return add(el, kids);
  }

  function add(el, kids) {
    for (const k of kids.flat(Infinity)) {
      if (k == null || k === false) continue;
      el.append(k instanceof Node ? k : String(k));
    }
    return el;
  }

  const SVG = 'http://www.w3.org/2000/svg';
  function svg(tag, attrs) {
    const el = document.createElementNS(SVG, tag);
    for (const [k, v] of Object.entries(attrs || {})) el.setAttribute(k, v);
    return el;
  }

  // icon is a status icon: good, warning, serious, critical, info or muted.
  function icon(tone) {
    const el = svg('svg', { viewBox: '0 0 16 16', width: 16, height: 16, class: `icon tone-${tone}`, 'aria-hidden': 'true', focusable: 'false' });
    const shapes = {
      good: [['circle', { cx: 8, cy: 8, r: 7 }], 'M4.8 8.3l2.1 2.1 4.3-4.6'],
      warning: [['path', { d: 'M8 1.3L15.2 14.2H.8z' }], 'M8 6v3.6M8 11.8v.1'],
      serious: [['path', { d: 'M8 .6L15.4 8 8 15.4.6 8z' }], 'M8 4.8v3.8M8 10.9v.1'],
      critical: [['circle', { cx: 8, cy: 8, r: 7 }], 'M5.6 5.6l4.8 4.8M10.4 5.6l-4.8 4.8'],
      info: [['circle', { cx: 8, cy: 8, r: 7 }], 'M8 7.3v4.1M8 4.7v.1'],
      muted: [['circle', { cx: 8, cy: 8, r: 7 }], 'M5 8h6'],
    }[tone] || [['circle', { cx: 8, cy: 8, r: 7 }], ''];
    const [shape, glyph] = shapes;
    el.append(svg(shape[0], { ...shape[1], class: 'fill' }));
    if (glyph) el.append(svg('path', { d: glyph, class: 'glyph' }));
    return el;
  }

  // The functions below format numbers, sizes and times.

  const nf = new Intl.NumberFormat('en-US');
  const cpu = (m) => (m == null ? '—' : `${nf.format(Math.round(m))}m`);
  function mem(mi) {
    if (mi == null) return '—';
    if (Math.abs(mi) < 1024) return `${nf.format(Math.round(mi))}Mi`;
    const gi = mi / 1024;
    return `${Number.isInteger(gi) ? gi : gi.toFixed(1)}Gi`;
  }
  const short = (commit) => (commit ? commit.slice(0, 7) : '');
  const plural = (n, one, many) => `${nf.format(n)} ${n === 1 ? one : many || one + 's'}`;
  const valid = (t) => typeof t === 'string' && t !== '' && !t.startsWith('0001-');

  // Times are aged against the scheduler's clock, not the browser's.
  function serverNow() {
    return Date.now() - state.skew;
  }
  function ago(t) {
    if (!valid(t)) return '';
    const s = Math.max(0, Math.round((serverNow() - Date.parse(t)) / 1000));
    if (s < 60) return `${s}s ago`;
    if (s < 3600) return `${Math.floor(s / 60)}m ago`;
    if (s < 86400) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m ago`;
    return `${Math.floor(s / 86400)}d ago`;
  }
  function span(from, to) {
    if (!valid(from)) return '';
    const end = valid(to) ? Date.parse(to) : serverNow();
    const s = Math.max(0, Math.round((end - Date.parse(from)) / 1000));
    if (s < 60) return `${s}s`;
    if (s < 3600) return `${Math.floor(s / 60)}m ${s % 60}s`;
    return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
  }
  const utc = (t) => (valid(t) ? `${new Date(t).toISOString().slice(0, 19).replace('T', ' ')} UTC` : '');
  const clock = (t) => (valid(t) ? new Date(t).toISOString().slice(11, 19) : '');

  // state holds what the page has fetched and what the operator has picked.

  const state = {
    status: null,
    error: null,
    lastOk: 0,
    skew: 0,
    role: null,
    logs: { key: null, records: [], latest: 0, more: false, error: null, loading: false },
    reports: null,
    reportsError: null,
    reportCache: new Map(),
    ui: {
      expanded: new Set(),
      replicas: { component: '', state: '', q: '' },
      logs: { component: '', source: '', level: '', q: '', since: '1h', snapshots: false },
      report: '',
    },
  };

  // The functions below fetch from the scheduler.

  async function getJSON(url) {
    const resp = await fetch(url, { cache: 'no-store', headers: { Accept: 'application/json' } });
    if (!resp.ok) {
      let msg = '';
      try { msg = (await resp.json()).error; } catch { /* The body isn't JSON. */ }
      throw new Error(msg || `HTTP ${resp.status}`);
    }
    return resp.json();
  }

  async function poll() {
    try {
      const st = await getJSON('/api/status');
      if (state.status && st.started !== state.status.started) {
        // The scheduler restarted, and the infra log's sequence numbers with it.
        state.logs.key = null;
      }
      state.status = st;
      state.skew = Date.now() - Date.parse(st.now);
      state.error = null;
      state.lastOk = Date.now();
    } catch (err) {
      state.error = err.message || 'unreachable';
    }
    if (viewName() === 'logs') await refreshLogs();
    render();
  }

  // The functions below read the infra log through the logs API, the one agents use too.

  const SINCE = [['15m', 'Last 15 minutes'], ['1h', 'Last hour'], ['6h', 'Last 6 hours'], ['1d', 'Last day'], ['3d', 'Last 3 days'], ['7d', 'Last 7 days']];

  function logParams(f, extra) {
    const p = new URLSearchParams();
    if (f.component) p.set('component', f.component);
    if (f.source) p.set('source', f.source);
    if (f.level) p.set('level', f.level);
    if (f.q.trim()) p.set('q', f.q.trim());
    if (f.snapshots) p.set('snapshots', 'true');
    for (const [k, v] of Object.entries(extra || {})) p.set(k, v);
    return p;
  }
  const logKey = () => logParams(state.ui.logs, { since: state.ui.logs.since }).toString();

  // refreshLogs loads the records the filters match when they change, and after that asks
  // only for the records the scheduler got since.
  async function refreshLogs() {
    const l = state.logs;
    if (l.loading) return;
    l.loading = true;
    const f = state.ui.logs;
    const key = logKey();
    try {
      if (l.key !== key) {
        const res = await getJSON(`/api/logs?${logParams(f, { since: f.since, limit: LOG_PAGE })}`);
        Object.assign(l, { key, records: res.records, latest: res.latest, more: res.more, error: null });
      } else {
        const res = await getJSON(`/api/logs?${logParams(f, { after: l.latest, limit: LOG_PAGE })}`);
        if (res.latest < l.latest) {
          l.key = null; // The scheduler restarted.
        } else {
          l.records.push(...res.records);
          if (l.records.length > MAX_LOGS) {
            l.records.splice(0, l.records.length - MAX_LOGS);
            l.more = true;
          }
          Object.assign(l, { latest: res.latest, error: null });
        }
      }
    } catch (err) {
      l.error = err.message;
    } finally {
      l.loading = false;
    }
    // The filters changed while the request was out.
    if (!l.error && l.key !== logKey()) await refreshLogs();
  }

  // loop polls every few seconds while the page is visible, one poll at a time.
  let timer = 0;
  let polling = false;
  async function loop() {
    clearTimeout(timer);
    if (polling) return; // The poll in flight schedules the next one.
    polling = true;
    try {
      await poll();
    } finally {
      polling = false;
    }
    if (!document.hidden) timer = setTimeout(loop, POLL_MS);
  }
  document.addEventListener('visibilitychange', () => { if (!document.hidden) loop(); });

  // The functions below work out what the status says.

  function replicaState(r) {
    return r.stopping && r.state !== 'failed' && r.state !== 'succeeded' ? 'stopping' : r.state;
  }
  const isLive = (r) => !r.stopping && !['failed', 'succeeded', 'stopping'].includes(r.state);

  const STATES = {
    ready: { label: 'ready', tone: 'good' },
    starting: { label: 'starting', tone: 'info' },
    assigned: { label: 'assigned', tone: 'info' },
    pending: { label: 'waiting for a VM', tone: 'muted' },
    failed: { label: 'failed', tone: 'critical' },
    stopping: { label: 'stopping', tone: 'muted' },
    succeeded: { label: 'succeeded', tone: 'good' },
  };
  const ORDER = ['ready', 'starting', 'assigned', 'pending', 'failed', 'stopping', 'succeeded'];
  const stateLabel = (s) => (STATES[s] || { label: s }).label;

  function vmLabel(st, id) {
    const vm = st.vms.find((v) => v.id === id);
    return vm && vm.name && vm.name !== vm.id ? vm.name : id;
  }

  function vmState(vm, version) {
    if (vm.lost) return { label: 'silent', tone: 'critical', why: `its agent last synced ${ago(vm.last_sync)}: its replicas no longer count` };
    if (vm.unhealthy) return { label: 'unhealthy', tone: 'serious', why: 'marked unhealthy, for its group to replace' };
    if (vm.terminating) return { label: 'terminating', tone: 'serious', why: 'AWS is removing it: its replicas are moving off' };
    if (vm.cordoned) return { label: 'draining', tone: 'info', why: 'sizing wants fewer VMs: its replicas are moving off before it is removed' };
    if (vm.version && version && vm.version !== version) return { label: 'old agent', tone: 'warning', why: `its agent runs ${vm.version}, the scheduler ${version}` };
    return { label: 'ok', tone: 'good', why: '' };
  }

  // components are the spec's, plus the edge and telemetry, which ProtoStrap adds.
  function components(st) {
    const list = st.components.map((c) => ({ ...c }));
    const own = (kind, pool, desired) => {
      const reps = st.replicas.filter((r) => r.kind === kind);
      if (!reps.length) return;
      const live = reps.filter(isLive);
      list.push({
        name: kind, kind, pool, desired,
        ready: live.filter((r) => r.state === 'ready').length, starting: live.filter((r) => r.state === 'starting' || r.state === 'assigned').length,
        pending: live.filter((r) => r.state === 'pending').length, old: live.filter((r) => r.old).length,
        request: reps[0].request,
      });
    };
    own('edge', 'ingress', st.vms.filter((v) => v.pool === 'ingress' && !v.lost).length || 1);
    own('telemetry', 'data-1', 1);
    return list;
  }

  // issues is what needs attention, the worst first.
  function issues(st) {
    const out = [];
    const push = (tone, title, detail, view) => out.push({ tone, title, detail, view });
    const last = st.rollouts && st.rollouts.length ? st.rollouts[st.rollouts.length - 1] : null;
    if (last && last.state === 'failed') push('critical', `Release ${short(last.commit)} failed`, last.reason || '', 'overview');

    const failed = st.replicas.filter((r) => r.state === 'failed' && !r.stopping);
    const failedBy = groupBy(failed, (r) => r.component);
    for (const [comp, reps] of failedBy) {
      push('critical', `${plural(reps.length, 'replica')} of ${comp} failed`, uniq(reps.map((r) => r.reason).filter(Boolean)).join('; '), 'replicas');
    }
    for (const vm of st.vms.filter((v) => v.lost)) {
      push('critical', `${vm.name || vm.id} is silent`, `${vm.pool} VM; last sync ${ago(vm.last_sync)}. Its replicas are being replaced elsewhere.`, 'vms');
    }
    for (const c of components(st)) {
      if (c.kind === 'task') continue;
      if (c.desired > 0 && c.ready === 0) {
        push(c.kind === 'telemetry' ? 'serious' : 'critical', `${c.name} has no ready replica`,
          c.kind === 'edge' ? 'Requests can\'t reach the application.'
            : c.kind === 'telemetry' ? 'Services carry on, but their logs, traces and metrics wait, and Grafana can\'t be opened.'
              : `${c.desired} wanted; ${c.starting} starting, ${c.pending} waiting for a VM.`, 'components');
      }
    }
    for (const p of st.pools) {
      if (p.pending > 0) push('serious', `${plural(p.pending, 'replica')} waiting for a ${p.name} VM`, `The pool has ${plural(p.vms, 'VM')}${p.booting ? `, ${p.booting} booting` : ''}, and wants ${p.wanted}.`, 'vms');
    }
    const otherPending = st.replicas.filter((r) => r.state === 'pending' && !r.stopping && !st.pools.some((p) => p.name === r.pool));
    if (otherPending.length) push('serious', `${plural(otherPending.length, 'replica')} waiting for a VM`, uniq(otherPending.map((r) => r.component)).join(', '), 'replicas');
    for (const vm of st.vms.filter((v) => !v.lost && (v.unhealthy || v.terminating))) {
      push('serious', `${vm.name || vm.id} is ${vm.unhealthy ? 'unhealthy' : 'being terminated'}`, vmState(vm, st.version).why, 'vms');
    }
    for (const c of st.components) {
      if (c.configure && c.configure.state === 'failed') push('serious', `${c.name}'s configure task ${c.configure.task} failed`, [c.configure.failed, c.configure.waiting].filter(Boolean).join('. '), 'logs');
      if (c.config && c.config.failed) push('serious', `${c.name} can't take its new config files`, c.config.failed, 'components');
    }
    if (st.warming) push('warning', 'The scheduler is starting', 'It decides nothing until it has heard from the agents.', 'logs');
    for (const c of components(st)) {
      if (c.kind === 'task' || c.desired === 0 || c.ready === 0 || c.ready >= c.desired) continue;
      push('warning', `${c.name}: ${c.ready} of ${c.desired} ready`, [c.starting && `${c.starting} starting`, c.pending && `${c.pending} waiting for a VM`].filter(Boolean).join(', ') || 'Replicas are on their way.', 'components');
    }
    for (const c of st.components) {
      if (c.scale && c.scale.max > 0 && c.desired >= c.scale.max && (c.kind === 'service' || c.kind === 'worker')) {
        push('warning', `${c.name} is at its maximum, ${c.scale.max}`, c.scale.reason, 'components');
      }
    }
    const oldAgents = st.vms.filter((v) => v.version && v.version !== st.version && !v.lost);
    if (oldAgents.length) push('warning', `${plural(oldAgents.length, 'agent')} not on ${st.version}`, `${oldAgents.map((v) => `${v.name || v.id} (${v.version})`).join(', ')}. Agents update themselves on their next sync.`, 'vms');
    if (last && last.state === 'rolling out') push('info', `Rolling out ${short(last.commit)}`, `Started ${ago(last.started)}.`, 'components');
    for (const c of st.components) {
      if (c.configure && (c.configure.state === 'running' || c.configure.state === 'pending')) push('info', `${c.name}'s configure task ${c.configure.task} is ${c.configure.state}`, c.configure.waiting || '', 'components');
      if (c.config && c.config.behind && !c.config.failed) push('info', `${c.name} is taking new config files`, `${plural(c.config.behind, 'replica')} still to take them.`, 'components');
    }
    if (st.holding && !st.warming) push('info', 'The scheduler started recently', `Started ${ago(st.started)}: it scales nothing down until it has 5 minutes of signals.`, 'logs');
    for (const c of st.components) {
      if (c.scale && c.scale.reason && c.scale.reason.startsWith('limited')) push('info', `${c.name} is held back by a limit`, c.scale.reason, 'components');
    }
    for (const vm of st.vms.filter((v) => v.cordoned && !v.lost && !v.terminating)) push('info', `${vm.name || vm.id} is draining`, vmState(vm, st.version).why, 'vms');
    return out;
  }

  function groupBy(list, key) {
    const m = new Map();
    for (const x of list) {
      const k = key(x);
      if (!m.has(k)) m.set(k, []);
      m.get(k).push(x);
    }
    return m;
  }
  const uniq = (list) => [...new Set(list)];
  const natural = new Intl.Collator('en', { numeric: true });
  const byID = (a, b) => natural.compare(a.id, b.id);

  // The functions below build pieces that the views share.

  function card(title, note, ...body) {
    return h('section', { class: 'card' }, h('header', null, h('h2', { text: title }), note ? h('p', null, note) : null), body);
  }

  function stateTag(s) {
    const meta = STATES[s] || { label: s, tone: 'muted' };
    return h('span', { class: 'state' }, icon(meta.tone), meta.label);
  }

  function replicaTip(st, r) {
    const lines = [`${r.id} · ${r.component}`, `${stateLabel(replicaState(r))}${r.vm ? ` on ${vmLabel(st, r.vm)}` : ''}`];
    lines.push(`release ${short(r.release)}${r.old ? ' (older than the target: a rollout replaces it)' : ''}`);
    if (r.cpu != null) lines.push(`CPU ${cpu(r.cpu)} of ${cpu(r.request.cpu)} requested`);
    else lines.push(`requests ${cpu(r.request.cpu)}, ${mem(r.request.memory)}`);
    if (r.memory != null) lines.push(`memory ${mem(r.memory)} of ${mem(r.request.memory)}`);
    if (r.restarts) lines.push(`${plural(r.restarts, 'restart')}`);
    if (r.ephemeral) lines.push('ephemeral: a worker on a general VM, until compute has room');
    if (valid(r.created)) lines.push(`created ${ago(r.created)}`);
    if (r.reason) lines.push(r.reason);
    return lines.join('\n');
  }

  function slot(st, r) {
    const s = replicaState(r);
    return h('span', { class: `slot s-${s}${r.old ? ' old' : ''}`, 'data-tip': replicaTip(st, r), tabindex: 0, 'aria-label': `${r.id}: ${stateLabel(s)}` });
  }

  // strip is a component's replicas, one square each, and a square for each it still wants.
  function strip(st, c) {
    const reps = st.replicas
      .filter((r) => r.component === c.name && r.kind !== 'task' && !r.stopping && r.state !== 'succeeded')
      .sort((a, b) => ORDER.indexOf(a.state) - ORDER.indexOf(b.state) || byID(a, b));
    const counted = reps.filter((r) => r.state !== 'failed').length;
    const missing = Math.max(0, c.desired - counted);
    const max = 24;
    const squares = reps.slice(0, max).map((r) => slot(st, r));
    for (let i = 0; i < Math.min(missing, max - squares.length); i++) {
      squares.push(h('span', { class: 'slot missing', 'data-tip': `${c.name} wants ${c.desired}: not yet created`, tabindex: 0, 'aria-label': 'wanted, not yet created' }));
    }
    const extra = reps.length + missing - squares.length;
    const parts = [`${c.ready}/${c.desired} ready`];
    if (c.old) parts.push(`${c.old} old`);
    return h('div', { class: 'strip' }, h('span', { class: 'slots' }, squares, extra > 0 ? h('span', { class: 'more', text: `+${extra}` }) : null), h('span', { class: 'slot-count', text: parts.join(' · ') }));
  }

  function slotLegend() {
    const item = (cls, label) => h('span', null, h('span', { class: `slot ${cls}` }), label);
    return h('div', { class: 'legend' }, item('s-ready', 'ready'), item('s-starting', 'starting'), item('s-pending', 'waiting for a VM'), item('s-failed', 'failed'), item('missing', 'wanted'), item('s-ready old', 'older release'));
  }

  function taskRun(st, c) {
    const runs = st.replicas.filter((r) => r.component === c.name).sort((a, b) => (b.created || '').localeCompare(a.created || ''));
    if (!runs.length) return h('span', { class: 'muted', text: 'not run since the scheduler started' });
    const r = runs[0];
    return h('span', null, stateTag(replicaState(r)), h('span', { class: 'muted', text: ` · release ${short(r.release)}` }));
  }

  function range(c) {
    if (!c.scale) {
      if (c.kind === 'task') return h('span', { class: 'muted', text: 'once per release' });
      return h('span', { class: 'range' }, h('span', { class: 'desired', text: c.desired }), h('span', { class: 'of', text: c.kind === 'stateful' ? 'pinned' : 'fixed' }));
    }
    const atMax = c.desired >= c.scale.max && c.scale.max > 0;
    return h('span', { class: 'range' },
      h('span', { class: 'desired', text: c.desired }),
      h('span', { class: 'of', text: `of ${c.scale.min}–${c.scale.max}` }),
      atMax ? h('span', { class: 'badge tone-warning', text: 'at max' }) : null);
  }

  function meterRow(what, capacity, reserved, free, tip, val) {
    const used = Math.max(0, capacity - reserved - Math.max(free, 0));
    const over = Math.max(0, -free);
    const pct = (x) => `${Math.max(0, Math.min(100, (x / Math.max(capacity, 1)) * 100))}%`;
    return h('div', { class: 'meter-row' },
      h('span', { class: 'what', text: what }),
      h('div', { class: 'meter', 'data-tip': tip, tabindex: 0, role: 'img', 'aria-label': tip },
        h('span', { class: 'reserved', style: { width: pct(reserved) } }),
        h('span', { class: 'used', style: { width: pct(used) } }),
        over ? h('span', { class: 'over', style: { width: pct(over) } }) : null),
      h('span', { class: `val${free < 0 ? ' over' : ''}`, text: val }));
  }

  function emptyState(title, text) {
    return h('div', { class: 'empty' }, h('strong', { text: title }), text);
  }

  // Each tab has a view below, along with the cards and loaders it uses.

  const views = {};

  views.overview = {
    render(st) {
      const list = issues(st);
      const serious = list.filter((i) => i.tone !== 'info');
      const attention = card(serious.length ? 'Needs attention' : 'All clear', list.length ? `${plural(list.length, 'item')}, the worst first` : null,
        list.length
          ? h('ul', { class: 'issues' }, list.map((i) => h('li', { class: `tone-${i.tone}` }, icon(i.tone),
            h('div', null, h('strong', { text: i.title }), i.detail ? h('p', { text: i.detail }) : null),
            i.view ? h('a', { href: `#${i.view}`, 'data-key': `issue-${i.title}`, text: 'Show' }) : null)))
          : h('ul', { class: 'issues' }, h('li', { class: 'tone-good' }, icon('good'), h('div', null, h('strong', { text: 'Everything is running as desired' }),
            h('p', { text: `${plural(st.components.length, 'component')}, ${plural(st.vms.length, 'VM')}.` })))));

      const comps = components(st).filter((c) => c.kind !== 'task');
      const atDesired = comps.filter((c) => c.ready >= c.desired).length;
      const liveReps = st.replicas.filter(isLive);
      const ready = liveReps.filter((r) => r.state === 'ready').length;
      const pending = liveReps.filter((r) => r.state === 'pending').length;
      const booting = st.pools.reduce((n, p) => n + p.booting, 0);
      const last = st.rollouts && st.rollouts.length ? st.rollouts[st.rollouts.length - 1] : null;
      const tile = (label, value, sub) => h('div', { class: 'card tile' }, h('div', { class: 'label', text: label }), h('div', { class: 'value' }, value), h('div', { class: 'sub', text: sub || '' }));
      const tiles = h('div', { class: 'tiles' },
        tile('Release', h('span', { class: 'mono', text: short(st.target) || 'none' }), last ? `${last.state === 'done' ? 'rolled out' : last.state} ${ago(last.finished) || ago(last.started)}` : st.last_good ? `last good ${short(st.last_good)}` : 'nothing deployed yet'),
        tile('Components at desired', [String(atDesired), h('small', { text: ` / ${comps.length}` })], comps.length - atDesired ? `${comps.length - atDesired} short` : 'all of them'),
        tile('Replicas ready', [String(ready), h('small', { text: ` / ${liveReps.length}` })], `${liveReps.length - ready - pending} starting`),
        tile('Waiting for a VM', String(pending), pending ? 'pools are growing' : 'every replica is placed'),
        tile('VMs', String(st.vms.length), booting ? `${booting} booting` : uniq(st.vms.map((v) => v.pool)).map((p) => `${st.vms.filter((v) => v.pool === p).length} ${p}`).join(', ')));

      const rows = comps.map((c) => h('tr', { class: 'clickable', onclick: () => { state.ui.expanded.add(c.name); location.hash = '#components'; } },
        h('td', null, h('span', { class: 'name', text: c.name }), h('span', { class: 'kind', text: c.kind })),
        h('td', null, strip(st, c)),
        h('td', null, range(c)),
        h('td', { class: 'why', text: c.scale ? c.scale.reason : c.kind === 'edge' ? 'one per ingress VM' : c.kind === 'telemetry' ? 'one on data-1' : c.kind === 'stateful' ? `pinned to ${c.pool}` : '' })));
      const table = card('Components', 'Select one to see its signals and limits.',
        comps.length
          ? h('div', { class: 'table-wrap' }, h('table', { class: 'data' },
            h('thead', null, h('tr', null, h('th', { scope: 'col', text: 'Component' }), h('th', { scope: 'col', text: 'Replicas' }), h('th', { scope: 'col', text: 'Desired' }), h('th', { scope: 'col', text: 'Why' }))),
            h('tbody', null, rows)))
          : emptyState('Nothing deployed yet', 'The scheduler is waiting for its first release.'),
        comps.length ? slotLegend() : null);

      return [attention, tiles, h('div', { class: 'cols' }, table, h('div', { class: 'stack' }, releaseCard(st), poolsCard(st)))];
    },
  };

  function releaseCard(st) {
    const tone = { done: 'good', 'rolling out': 'info', failed: 'critical', superseded: 'muted' };
    const rollouts = [...(st.rollouts || [])].reverse();
    return card('Releases', `Target ${short(st.target) || 'none'} · last good ${short(st.last_good) || 'none'}`,
      rollouts.length
        ? h('div', { class: 'table-wrap' }, h('table', { class: 'data' },
          h('thead', null, h('tr', null, h('th', { scope: 'col', text: 'Release' }), h('th', { scope: 'col', text: 'State' }), h('th', { scope: 'col', text: 'Started' }), h('th', { scope: 'col', class: 'num', text: 'Took' }))),
          h('tbody', null, rollouts.map((r) => [
            h('tr', null,
              h('td', { class: 'mono', text: short(r.commit), title: r.commit }),
              h('td', null, h('span', { class: 'state' }, icon(tone[r.state] || 'muted'), r.state)),
              h('td', { class: 'nowrap', 'data-tip': utc(r.started), text: ago(r.started) }),
              h('td', { class: 'num', text: span(r.started, r.finished) })),
            r.reason ? h('tr', null, h('td', { colspan: 4, class: 'why', text: r.reason })) : null,
          ]))))
        : h('p', { class: 'muted', text: 'No rollouts since the scheduler started. Deploys come from the deploy workflow.' }));
  }

  function poolsCard(st) {
    const rows = [];
    for (const p of st.pools) {
      const per = p.scale ? p.scale.usable : null;
      const cap = per ? per.cpu * Math.max(p.vms, 1) : 0;
      const tip = per ? `${cpu(p.requested.cpu)} requested of ${cpu(per.cpu * p.vms)} usable at ${Math.round(p.scale.target * 100)}% on ${plural(p.vms, 'VM')} (${p.scale.vm_type})` : '';
      rows.push(h('div', { class: 'pool-meter' },
        h('div', { class: 'pool-head' }, h('h3', { text: p.name }),
          h('span', { class: 'facts', text: `${plural(p.vms, 'VM')}${p.booting ? ` + ${p.booting} booting` : ''} · wants ${p.wanted}${p.scale ? ` · allowed ${p.scale.min}–${p.scale.max}` : ''}` }),
          p.pending ? h('span', { class: 'badge tone-serious', text: `${p.pending} waiting` }) : null),
        per ? h('div', { class: 'meter-row' }, h('span', { class: 'what', text: 'CPU' }),
          h('div', { class: 'meter pool', 'data-tip': tip, tabindex: 0, role: 'img', 'aria-label': tip },
            h('span', { class: 'used', style: { width: `${Math.min(100, (p.requested.cpu / Math.max(cap, 1)) * 100)}%` } })),
          h('span', { class: 'val', text: `${cpu(p.requested.cpu)} / ${cpu(cap)}` })) : null));
    }
    for (const name of ['ingress', 'data']) {
      const n = st.vms.filter((v) => v.pool === name).length;
      if (n) rows.push(h('div', { class: 'pool-meter' }, h('div', { class: 'pool-head' }, h('h3', { text: name }), h('span', { class: 'facts', text: `${plural(n, 'VM')} · fixed` }))));
    }
    return card('Pools', 'Requested CPU against what sizing aims to use', rows.length ? rows : h('p', { class: 'muted', text: 'No pools sized yet.' }));
  }

  views.components = {
    render(st) {
      const comps = components(st);
      if (!comps.length) return [card('Components', null, emptyState('Nothing deployed yet', 'The scheduler is waiting for its first release.'))];
      const rows = [];
      for (const c of comps) {
        const open = state.ui.expanded.has(c.name);
        const toggle = () => { open ? state.ui.expanded.delete(c.name) : state.ui.expanded.add(c.name); render(); };
        rows.push(h('tr', { class: 'clickable', onclick: (e) => { if (!e.target.closest('[data-tip]')) toggle(); } },
          h('td', { class: 'nowrap' },
            h('button', { class: 'expander', type: 'button', 'data-key': `expand-${c.name}`, 'aria-expanded': String(open), 'aria-label': `${open ? 'Hide' : 'Show'} ${c.name}'s details`, onclick: (e) => { e.stopPropagation(); toggle(); } }, h('span', { class: 'chev', text: '▸' })),
            h('span', { class: 'name', text: c.name }), h('span', { class: 'kind', text: c.kind })),
          h('td', { class: 'secondary', text: c.pool || '' }),
          h('td', null, c.kind === 'task' ? taskRun(st, c) : strip(st, c)),
          h('td', null, range(c)),
          h('td', { class: 'why', text: c.scale ? c.scale.reason : '' })));
        if (open) rows.push(h('tr', { class: 'detail' }, h('td', { colspan: 5 }, componentDetail(st, c))));
      }
      return [card('Components', 'Desired = clamp(min(demand, limit cap), min, max), recounted every cycle',
        h('div', { class: 'table-wrap' }, h('table', { class: 'data' },
          h('thead', null, h('tr', null, ['Component', 'Pool', 'Replicas', 'Desired', 'Why'].map((t) => h('th', { scope: 'col', text: t })))),
          h('tbody', null, rows))),
        slotLegend())];
    },
  };

  function componentDetail(st, c) {
    const parts = [];
    if (c.scale) {
      parts.push(h('div', null, h('h3', { text: 'Why it wants ' + plural(c.desired, 'replica') }), h('p', { class: 'why-line', text: c.scale.reason })));
      parts.push(h('div', null, h('h3', { text: 'Demand signals' }), c.scale.signals && c.scale.signals.length
        ? h('table', { class: 'mini' },
          h('thead', null, h('tr', null, h('th', { text: 'Signal' }), h('th', { class: 'num', text: 'Asks for' }), h('th', { text: 'Reading' }))),
          h('tbody', null, c.scale.signals.map((s) => h('tr', null, h('td', { class: 'code', text: s.signal }), h('td', { class: 'num', text: s.demand == null ? '—' : s.demand }), h('td', { text: s.detail })))))
        : h('p', { class: 'muted', text: 'None: it runs at its minimum.' })));
      if (c.scale.limits && c.scale.limits.length) {
        parts.push(h('div', null, h('h3', { text: 'Limits' }), h('table', { class: 'mini' },
          h('thead', null, h('tr', null, h('th', { text: 'Limit' }), h('th', { class: 'num', text: 'Cap' }), h('th', { text: 'Reading' }))),
          h('tbody', null, c.scale.limits.map((l) => h('tr', null, h('td', { class: 'code', text: l.limit }), h('td', { class: 'num', text: l.cap == null ? 'none' : l.cap }), h('td', { text: l.detail })))))));
      }
    }
    const reps = st.replicas.filter((r) => r.component === c.name).sort((a, b) => byID(a, b));
    parts.push(h('div', null, h('h3', { text: `Replicas · each requests ${cpu(c.request && c.request.cpu)} and ${mem(c.request && c.request.memory)}` }),
      reps.length ? h('div', { class: 'chips' }, reps.map((r) => chip(st, r, true))) : h('p', { class: 'muted', text: 'None.' })));
    return h('div', { class: 'detail-grid' }, parts);
  }

  function chip(st, r, showVM) {
    const s = replicaState(r);
    const text = showVM ? `${r.id}${r.vm ? ` · ${vmLabel(st, r.vm)}` : ''}` : r.component;
    return h('span', { class: 'chip', 'data-tip': replicaTip(st, r), tabindex: 0 },
      h('span', { class: `slot s-${s}${r.old ? ' old' : ''}`, 'aria-hidden': 'true' }),
      h('span', { class: `label${showVM ? ' mono' : ''}`, text }),
      s !== 'ready' ? h('span', { class: 'tag', text: stateLabel(s) }) : null,
      r.ephemeral ? h('span', { class: 'tag', text: 'ephemeral' }) : null);
  }

  views.vms = {
    render(st) {
      const pools = uniq(['ingress', 'general', 'compute', 'data', ...st.vms.map((v) => v.pool), ...st.pools.map((p) => p.name)]);
      const out = [];
      for (const name of pools) {
        const vms = st.vms.filter((v) => v.pool === name);
        const p = st.pools.find((x) => x.name === name);
        if (!vms.length && !(p && (p.booting || p.pending))) continue;
        const facts = [plural(vms.length, 'VM')];
        if (p) {
          if (p.booting) facts.push(`${p.booting} booting`);
          facts.push(`wants ${p.wanted}`);
          if (p.scale) facts.push(`allowed ${p.scale.min}–${p.scale.max} ${p.scale.vm_type}`, `${p.scale.spare} spare`, `${Math.round(p.scale.target * 100)}% target`);
        } else facts.push('fixed');
        const cards = vms.map((vm) => vmCard(st, vm));
        for (let i = 0; p && i < p.booting; i++) cards.push(h('div', { class: 'vm booting' }, h('strong', { text: 'Booting' }), h('span', { text: 'counted as part of the pool, with no room yet' })));
        out.push(h('section', { class: 'card' },
          h('div', { class: 'pool-head' }, h('h2', { text: name }), h('span', { class: 'facts', text: facts.join(' · ') }),
            p && p.pending ? h('span', { class: 'badge tone-serious' }, `${p.pending} waiting for a VM`) : null),
          h('div', { class: 'vm-grid' }, cards)));
      }
      if (!out.length) return [card('VMs', null, emptyState('No VM has synced yet', 'Agents sync every 5 seconds once their VM boots.'))];
      out.push(h('div', { class: 'legend' }, h('span', null, h('span', { class: 'slot s-ready', style: { background: 'var(--reserved)', 'border-color': 'var(--reserved)' } }), 'reserved for the OS'),
        h('span', null, h('span', { class: 'slot', style: { background: 'var(--accent)', 'border-color': 'var(--accent)' } }), 'requested by replicas'),
        h('span', null, h('span', { class: 'slot', style: { background: 'var(--accent-track)', 'border-color': 'var(--accent-track)' } }), 'free')));
      return out;
    },
  };

  function vmCard(st, vm) {
    const reps = st.replicas.filter((r) => r.vm === vm.id && r.state !== 'succeeded').sort((a, b) => a.component.localeCompare(b.component) || byID(a, b));
    const vs = vmState(vm, st.version);
    const using = reps.reduce((n, r) => n + (r.cpu || 0), 0);
    const usingMem = reps.reduce((n, r) => n + (r.memory || 0), 0);
    const cpuReq = vm.capacity.cpu - vm.reserved.cpu - vm.free.cpu;
    const memReq = vm.capacity.memory - vm.reserved.memory - vm.free.memory;
    const cpuTip = `CPU ${cpu(vm.capacity.cpu)}\nreserved ${cpu(vm.reserved.cpu)} · requested ${cpu(cpuReq)} · free ${cpu(vm.free.cpu)}\nreplicas are using ${cpu(using)}`;
    const memTip = `Memory ${mem(vm.capacity.memory)}\nreserved ${mem(vm.reserved.memory)} · requested ${mem(memReq)} · free ${mem(vm.free.memory)}\nreplicas are using ${mem(usingMem)}`;
    return h('article', { class: `vm${vs.tone !== 'good' ? ` problem tone-${vs.tone}` : ''}` },
      h('div', { class: 'vm-title' }, h('span', { class: 'mono', text: vm.name || vm.id }),
        h('span', { class: 'state', 'data-tip': vs.why || null }, icon(vs.tone), vs.label)),
      h('div', { class: 'vm-sub' },
        vm.name && vm.name !== vm.id ? h('span', { class: 'mono', text: vm.id }) : null,
        vm.zone ? h('span', { text: vm.zone }) : null,
        vm.ip ? h('span', { class: 'mono', text: vm.ip }) : null,
        h('span', { text: `v${vm.version}` }),
        h('span', { 'data-tip': utc(vm.last_sync), text: `synced ${ago(vm.last_sync)}` })),
      meterRow('CPU', vm.capacity.cpu, vm.reserved.cpu, vm.free.cpu, cpuTip, vm.free.cpu < 0 ? `${cpu(-vm.free.cpu)} over` : `${cpu(vm.free.cpu)} free`),
      meterRow('Mem', vm.capacity.memory, vm.reserved.memory, vm.free.memory, memTip, vm.free.memory < 0 ? `${mem(-vm.free.memory)} over` : `${mem(vm.free.memory)} free`),
      reps.length ? h('div', { class: 'chips' }, reps.map((r) => chip(st, r, false))) : h('span', { class: 'muted', text: 'No replicas' }));
  }

  views.replicas = {
    mount() {
      const f = state.ui.replicas;
      this.component = h('select', { onchange: (e) => { f.component = e.target.value; render(); } });
      this.state = h('select', { onchange: (e) => { f.state = e.target.value; render(); } },
        h('option', { value: '', text: 'Any state' }), ['ready', 'starting', 'pending', 'failed', 'stopping'].map((s) => h('option', { value: s, text: stateLabel(s) })));
      this.state.value = f.state;
      this.q = h('input', { type: 'search', placeholder: 'Replica, VM or reason', value: f.q, oninput: (e) => { f.q = e.target.value; render(); } });
      this.shown = h('span', { class: 'shown' });
      this.results = h('div');
      return [h('section', { class: 'card' },
        h('header', null, h('h2', { text: 'Replicas' }), h('p', { text: 'Every replica the scheduler knows, with what its agent last reported' })),
        h('div', { class: 'filters' },
          h('label', null, 'Component', this.component), h('label', null, 'State', this.state),
          h('label', null, h('span', { class: 'visually-hidden', text: 'Search' }), this.q), this.shown),
        this.results)];
    },
    update(st) {
      const f = state.ui.replicas;
      const names = uniq(st.replicas.map((r) => r.component)).sort();
      const current = [...this.component.options].map((o) => o.value).join('|');
      if (current !== ['', ...names].join('|')) {
        this.component.replaceChildren(h('option', { value: '', text: 'All components' }), ...names.map((n) => h('option', { value: n, text: n })));
        this.component.value = names.includes(f.component) ? f.component : '';
      }
      const q = f.q.trim().toLowerCase();
      const all = [...st.replicas].sort((a, b) => a.component.localeCompare(b.component) || ORDER.indexOf(replicaState(a)) - ORDER.indexOf(replicaState(b)) || byID(a, b));
      const list = all.filter((r) => (!f.component || r.component === f.component) &&
        (!f.state || replicaState(r) === f.state || (f.state === 'starting' && r.state === 'assigned')) &&
        (!q || [r.id, r.vm, vmLabel(st, r.vm), r.reason, r.release].some((x) => x && x.toLowerCase().includes(q))));
      this.shown.textContent = `${nf.format(list.length)} of ${nf.format(all.length)}`;
      const usage = (used, req, fmt) => (used == null ? h('span', { class: 'muted', text: `— / ${fmt(req)}` }) : `${fmt(used)} / ${fmt(req)}`);
      this.results.replaceChildren(list.length
        ? h('div', { class: 'table-wrap' }, h('table', { class: 'data' },
          h('thead', null, h('tr', null, h('th', { text: 'Replica' }), h('th', { text: 'Component' }), h('th', { text: 'State' }), h('th', { text: 'VM' }), h('th', { text: 'Release' }),
            h('th', { class: 'num', text: 'CPU used / req' }), h('th', { class: 'num', text: 'Memory used / req' }), h('th', { class: 'num', text: 'Restarts' }), h('th', { class: 'num', text: 'Age' }), h('th', { text: 'Note' }))),
          h('tbody', null, list.map((r) => h('tr', null,
            h('td', { class: 'mono nowrap', text: r.id }),
            h('td', { text: r.component }),
            h('td', null, stateTag(replicaState(r))),
            h('td', { class: 'mono nowrap', text: r.vm ? vmLabel(st, r.vm) : '—' }),
            h('td', { class: 'mono nowrap' }, short(r.release), r.old ? h('span', { class: 'badge', text: 'old' }) : null),
            h('td', { class: 'num' }, usage(r.cpu, r.request.cpu, cpu)),
            h('td', { class: 'num' }, usage(r.memory, r.request.memory, mem)),
            h('td', { class: 'num', text: r.restarts || 0 }),
            h('td', { class: 'num', 'data-tip': utc(r.created), text: ago(r.created).replace(' ago', '') }),
            h('td', { class: 'why', text: [r.ephemeral && 'ephemeral', r.reason].filter(Boolean).join(' · ') }))))))
        : emptyState('No replicas match', 'Clear the filters to see them all.'));
    },
  };

  views.logs = {
    mount() {
      const f = state.ui.logs;
      const changed = () => { render(); refreshLogs().then(render); };
      const pick = (key, options) => {
        const el = h('select', { onchange: (e) => { f[key] = e.target.value; changed(); } }, options.map(([v, text]) => h('option', { value: v, text })));
        el.value = f[key];
        return el;
      };
      this.component = h('select', { onchange: (e) => { f.component = e.target.value; changed(); } });
      this.source = pick('source', [['', 'Scheduler and agents'], ['scheduler', 'Scheduler'], ['agent', 'Agents']]);
      this.level = pick('level', [['', 'Every level'], ['warn', 'Warnings and errors'], ['error', 'Errors']]);
      this.since = pick('since', SINCE);
      let typing = 0;
      this.q = h('input', { type: 'search', placeholder: 'Message, replica, reason…', value: f.q,
        oninput: (e) => { f.q = e.target.value; clearTimeout(typing); typing = setTimeout(changed, 300); } });
      this.snapshots = h('input', { type: 'checkbox', onchange: (e) => { f.snapshots = e.target.checked; changed(); } });
      this.snapshots.checked = f.snapshots;
      this.shown = h('span', { class: 'shown' });
      this.results = h('div');
      this.api = h('code');
      return [h('section', { class: 'card' },
        h('header', null, h('h2', { text: 'Logs' }), h('p', { text: 'The infra log: the scheduler\'s decisions with their reasons, and every agent\'s records of its VM and its replicas, newest first, in UTC. A service\'s own output, traces and metrics are in Grafana, through scripts/telemetry.sh.' })),
        h('div', { class: 'filters' },
          h('label', null, 'Component', this.component), h('label', null, 'From', this.source), h('label', null, 'Level', this.level),
          h('label', null, h('span', { class: 'visually-hidden', text: 'Window' }), this.since),
          h('label', null, h('span', { class: 'visually-hidden', text: 'Search' }), this.q),
          h('label', null, this.snapshots, 'Signal snapshots'),
          this.shown),
        this.results),
      h('section', { class: 'card' },
        h('header', null, h('h2', { text: 'For an agent' }), h('p', { text: 'An agent such as Claude can query the same log through this port forward. Its help says how:' })),
        h('pre', { class: 'api' }, h('code', { text: `curl -s ${location.origin}/api/help` })),
        h('p', { class: 'muted', text: 'These filters, as text:' }),
        h('pre', { class: 'api' }, this.api))];
    },
    update(st) {
      const f = state.ui.logs;
      const l = state.logs;
      const names = st.components.map((c) => c.name).sort();
      if (f.component && !names.includes(f.component)) names.push(f.component);
      const current = [...this.component.options].map((o) => o.value).join('|');
      if (current !== ['', ...names].join('|')) {
        this.component.replaceChildren(h('option', { value: '', text: 'All components' }), ...names.map((n) => h('option', { value: n, text: n })));
        this.component.value = f.component;
      }
      this.api.textContent = `curl -s '${location.origin}/api/logs?${logParams(f, { since: f.since, format: 'text' })}'`;
      if (l.key !== logKey()) {
        if (!l.loading) refreshLogs().then(render);
        this.shown.textContent = '';
        this.results.replaceChildren(l.error ? emptyState('The infra log can\'t be read', l.error) : h('p', { class: 'muted', text: 'Loading…' }));
        return;
      }
      const list = [...l.records].sort((a, b) => Date.parse(b.time) - Date.parse(a.time) || (b.seq || 0) - (a.seq || 0));
      this.shown.textContent = `${nf.format(list.length)}${l.more ? '+' : ''} ${list.length === 1 ? 'record' : 'records'}`;
      this.results.replaceChildren(list.length
        ? h('ol', { class: 'feed', reversed: true }, list.slice(0, LOG_PAGE).map((e) => {
          const attrs = e.attrs || [];
          const reason = attrs.find((a) => a.key === 'reason' || a.key === 'err');
          const from = e.source === 'agent' ? `agent · ${e.vm_name || e.vm}` : 'scheduler';
          return h('li', null,
            h('span', { class: 'time', 'data-tip': `${utc(e.time)}\n${ago(e.time)}`, text: clock(e.time) }),
            h('span', { class: `lvl ${e.level}`, text: e.level }),
            h('div', null,
              h('span', { class: 'msg', text: e.msg }),
              h('span', { class: 'src', text: from }),
              e.component ? h('span', { class: 'badge', text: e.component }) : null,
              reason ? h('p', { class: 'reason', text: reason.value }) : null,
              h('div', { class: 'attrs' }, attrs.filter((a) => a !== reason).map((a) => h('span', null, h('b', { text: `${a.key}=` }), a.value)))));
        }))
        : emptyState('No records match', l.error || 'Widen the window or clear the filters. The scheduler logs each decision with its reason, and each agent what it does.'));
    },
  };

  views.reports = {
    mount() {
      this.list = h('ul', { class: 'report-list' });
      this.body = h('article', { class: 'md' });
      this.shownReport = null;
      loadReports();
      return [h('section', { class: 'card' },
        h('header', null, h('h2', { text: 'Reports' }), h('p', { text: 'The reporter\'s daily reports, from the deploy bucket' })),
        h('div', { class: 'reports' }, h('nav', { 'aria-label': 'Reports' }, this.list), this.body))];
    },
    update() {
      if (state.reportsError) {
        this.list.replaceChildren();
        this.body.replaceChildren(emptyState('The reports can\'t be read', state.reportsError));
        return;
      }
      if (!state.reports) {
        this.body.replaceChildren(h('p', { class: 'muted', text: 'Loading…' }));
        return;
      }
      if (!state.reports.length) {
        this.list.replaceChildren();
        this.body.replaceChildren(emptyState('No reports yet', 'The reporter writes one a day, and after every release.'));
        return;
      }
      if (!state.ui.report || !state.reports.some((r) => r.date === state.ui.report)) state.ui.report = state.reports[0].date;
      this.list.replaceChildren(...state.reports.map((r) => h('li', null, h('button', { type: 'button', 'data-key': `report-${r.date}`, 'aria-current': String(r.date === state.ui.report), onclick: () => { state.ui.report = r.date; render(); } },
        h('span', { text: r.date }), h('span', { class: 'size', text: `${Math.max(1, Math.round(r.size / 1024))} KB` })))));
      const cached = state.reportCache.get(state.ui.report);
      if (!cached) {
        this.body.replaceChildren(h('p', { class: 'muted', text: 'Loading…' }));
        loadReport(state.ui.report);
      } else if (cached.error) {
        this.body.replaceChildren(emptyState(`The report for ${state.ui.report} can't be read`, cached.error));
      } else if (this.shownReport !== state.ui.report) {
        this.body.replaceChildren(...markdown(cached.text));
      }
      this.shownReport = cached && !cached.error ? state.ui.report : null;
    },
  };

  async function loadReports() {
    try {
      state.reports = (await getJSON('/api/reports')).reports;
      state.reportsError = null;
    } catch (err) {
      state.reportsError = err.message;
    }
    render();
  }

  const loading = new Set();
  async function loadReport(date) {
    if (loading.has(date)) return;
    loading.add(date);
    try {
      const resp = await fetch(`/api/reports/${date}`, { cache: 'no-store' });
      if (!resp.ok) {
        let msg = `HTTP ${resp.status}`;
        try { msg = (await resp.json()).error || msg; } catch { /* The body isn't JSON. */ }
        state.reportCache.set(date, { error: msg });
      } else {
        state.reportCache.set(date, { text: await resp.text() });
      }
    } catch (err) {
      state.reportCache.set(date, { error: err.message });
    } finally {
      loading.delete(date);
    }
    render();
  }

  // The functions below render a report's markdown: headings, paragraphs, lists, tables,
  // code, emphasis and links. They build nodes, so nothing in a report runs.

  function markdown(text) {
    const lines = text.replace(/\r\n?/g, '\n').split('\n');
    const out = [];
    let i = 0;
    const isTable = (l) => /^\s*\|.*\|\s*$/.test(l);
    const listItem = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/;
    while (i < lines.length) {
      const line = lines[i];
      if (!line.trim()) { i++; continue; }
      const fence = line.match(/^\s*```/);
      if (fence) {
        const code = [];
        for (i++; i < lines.length && !/^\s*```/.test(lines[i]); i++) code.push(lines[i]);
        i++;
        out.push(h('pre', null, h('code', { text: code.join('\n') })));
        continue;
      }
      const heading = line.match(/^(#{1,6})\s+(.*)$/);
      if (heading) {
        out.push(h(`h${Math.min(heading[1].length, 4)}`, null, inline(heading[2])));
        i++;
        continue;
      }
      if (isTable(line) && i + 1 < lines.length && /^\s*\|[\s:|-]+\|\s*$/.test(lines[i + 1])) {
        const cells = (l) => l.trim().replace(/^\||\|$/g, '').split('|').map((c) => c.trim());
        const head = cells(line);
        const body = [];
        for (i += 2; i < lines.length && isTable(lines[i]); i++) body.push(cells(lines[i]));
        out.push(h('div', { class: 'table-wrap' }, h('table', null,
          h('thead', null, h('tr', null, head.map((c) => h('th', null, inline(c))))),
          h('tbody', null, body.map((row) => h('tr', null, row.map((c) => h('td', null, inline(c)))))))));
        continue;
      }
      if (listItem.test(line)) {
        const [list, next] = parseList(lines, i, listItem);
        out.push(list);
        i = next;
        continue;
      }
      const para = [];
      while (i < lines.length && lines[i].trim() && !listItem.test(lines[i]) && !/^(#{1,6})\s/.test(lines[i]) && !/^\s*```/.test(lines[i]) && !isTable(lines[i])) {
        para.push(lines[i].trim());
        i++;
      }
      out.push(h('p', null, inline(para.join(' '))));
    }
    return out;
  }

  function parseList(lines, start, listItem) {
    const first = lines[start].match(listItem);
    const indent = first[1].length;
    const ordered = /\d/.test(first[2]);
    const list = h(ordered ? 'ol' : 'ul');
    let i = start;
    let li = null;
    while (i < lines.length) {
      const m = lines[i].match(listItem);
      if (m && m[1].length === indent) {
        li = h('li', null, inline(m[3]));
        list.append(li);
        i++;
      } else if (m && m[1].length > indent && li) {
        const [sub, next] = parseList(lines, i, listItem);
        li.append(sub);
        i = next;
      } else if (!m && lines[i].trim() && li && /^\s+/.test(lines[i])) {
        li.append(' ', ...inline(lines[i].trim()));
        i++;
      } else {
        break;
      }
    }
    return [list, i];
  }

  function inline(text) {
    const out = [];
    // Underscores inside a word, as in jobs_budget_cap, aren't emphasis.
    const re = /(`[^`]+`)|(\*\*[^*]+\*\*)|(\*[^*\s][^*]*\*|(?<![\w])_[^_\s][^_]*_(?![\w]))|(\[[^\]]+\]\([^)\s]+\))/g;
    let last = 0;
    let m;
    while ((m = re.exec(text))) {
      if (m.index > last) out.push(text.slice(last, m.index));
      const t = m[0];
      if (m[1]) out.push(h('code', { text: t.slice(1, -1) }));
      else if (m[2]) out.push(h('strong', null, inline(t.slice(2, -2))));
      else if (m[3]) out.push(h('em', null, inline(t.slice(1, -1))));
      else {
        const [, label, href] = t.match(/^\[([^\]]+)\]\(([^)\s]+)\)$/);
        out.push(/^https?:\/\//i.test(href) ? h('a', { href, rel: 'noopener noreferrer', target: '_blank' }, inline(label)) : label);
      }
      last = m.index + t.length;
    }
    if (last < text.length) out.push(text.slice(last));
    return out;
  }

  // The functions below draw the frame: the header, the tabs and the current view.

  const $ = (id) => document.getElementById(id);
  let current = { name: '', view: null, mounted: null };

  function viewName() {
    const name = location.hash.replace(/^#/, '');
    return views[name] ? name : 'overview';
  }

  // render redraws the header and the current view. A refresh keeps the keyboard focus, by
  // data-key, and the tooltip under a still mouse.
  function render() {
    const focused = document.activeElement && document.activeElement.dataset ? document.activeElement.dataset.key : null;
    draw();
    if (focused) {
      const el = document.querySelector(`[data-key="${CSS.escape(focused)}"]`);
      if (el && el !== document.activeElement) el.focus({ preventScroll: true });
    }
    if (mouse) {
      const el = document.elementFromPoint(mouse.x, mouse.y);
      const target = el && el.closest('[data-tip]');
      if (target && target.dataset.tip) showTip(target, mouse.x, mouse.y);
    }
  }

  function draw() {
    renderHeader();
    const name = viewName();
    const main = $('view');
    const st = state.status;
    if (current.name !== name) {
      current = { name, view: views[name], mounted: null };
      main.replaceChildren();
      for (const a of document.querySelectorAll('.tabs a')) {
        if (a.dataset.view === name) a.setAttribute('aria-current', 'page');
        else a.removeAttribute('aria-current');
      }
    }
    hideTip();
    if (!st) {
      main.replaceChildren(h('section', { class: 'card' }, state.error
        ? emptyState('Can\'t reach the scheduler', `${state.error}. If the SSM port forward closed, start it again: scripts/operator-ui.sh`)
        : emptyState('Connecting…', 'Asking the scheduler for its status.')));
      current.mounted = null;
      return;
    }
    const v = current.view;
    if (v.mount) {
      if (!current.mounted) {
        current.mounted = v.mount();
        main.replaceChildren(...current.mounted);
      }
      v.update(st);
    } else {
      const y = window.scrollY;
      main.replaceChildren(...v.render(st));
      window.scrollTo(0, y);
    }
  }

  function renderHeader() {
    const st = state.status;
    if (st) {
      $('app').textContent = st.app;
      const env = $('env');
      env.hidden = !st.environment;
      env.textContent = st.environment || '';
      const rel = $('release');
      rel.replaceChildren(st.target ? h('span', { class: 'pill', 'data-tip': `target ${st.target}\nlast good ${st.last_good || 'none'}\nProtoStrap ${st.version}` },
        'release ', h('span', { class: 'mono', text: short(st.target) })) : h('span', { class: 'pill', text: 'no release' }),
      h('span', { class: 'pill', text: `ProtoStrap ${st.version}` }));
      const role = $('role');
      role.hidden = !state.role;
      role.textContent = state.role === 'org_admin' ? 'Org Admin' : 'Platform Admin';
      role.dataset.tip = state.role === 'org_admin'
        ? 'This port is for Org Admins: scripts/operator-ui.sh --admin. It shows the same read-only view as Platform Admins\' for now.'
        : 'This port is the Platform Admins\' read-only view: scripts/operator-ui.sh.';
      const critical = issues(st).filter((i) => i.tone === 'critical').length;
      document.title = `${critical ? `(${critical}) ` : ''}${st.app}${st.environment ? ` ${st.environment}` : ''} · ProtoStrap`;
      const tab = document.querySelector('.tabs a[data-view="overview"]');
      tab.replaceChildren('Overview', critical ? h('span', { class: 'count', 'aria-label': `${critical} critical`, text: critical }) : '');
    }
    updateLive();
  }

  function updateLive() {
    const el = $('live');
    const text = $('live-text');
    const since = state.lastOk ? Math.round((Date.now() - state.lastOk) / 1000) : null;
    el.classList.toggle('down', !!state.error);
    document.body.classList.toggle('offline', !!state.error && !!state.status);
    el.classList.toggle('stale', !state.error && since != null && since * 1000 > STALE_MS);
    if (state.error) {
      text.textContent = since == null ? 'Can\'t reach the scheduler' : `Can't reach the scheduler · last update ${since}s ago`;
      el.dataset.tip = state.error;
    } else if (since != null) {
      text.textContent = `Live · updated ${since}s ago`;
      delete el.dataset.tip;
    }
  }

  // The tooltip shows for any element with data-tip, on hover or keyboard focus.

  const tip = () => $('tip');
  let tipFor = null;
  let mouse = null;
  function showTip(el, x, y) {
    const t = tip();
    t.textContent = el.dataset.tip;
    t.hidden = false;
    tipFor = el;
    const r = t.getBoundingClientRect();
    const left = Math.min(Math.max(8, x + 12), window.innerWidth - r.width - 8);
    const top = y + 16 + r.height > window.innerHeight ? y - r.height - 10 : y + 16;
    t.style.left = `${left}px`;
    t.style.top = `${Math.max(8, top)}px`;
  }
  function hideTip() {
    tipFor = null;
    const t = tip();
    if (t) t.hidden = true;
  }
  document.addEventListener('mousemove', (e) => {
    mouse = { x: e.clientX, y: e.clientY };
    const el = e.target.closest && e.target.closest('[data-tip]');
    if (el && el.dataset.tip) showTip(el, e.clientX, e.clientY);
    else if (tipFor) hideTip();
  });
  document.addEventListener('focusin', (e) => {
    const el = e.target.closest && e.target.closest('[data-tip]');
    if (el && el.dataset.tip) {
      const r = el.getBoundingClientRect();
      showTip(el, r.left, r.bottom - 8);
    }
  });
  document.addEventListener('focusout', hideTip);
  document.addEventListener('mouseleave', () => { mouse = null; hideTip(); });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') hideTip(); });
  window.addEventListener('scroll', hideTip, { passive: true });

  // The theme is the system's, unless the operator picked one.

  function applyTheme(theme) {
    if (theme) document.documentElement.dataset.theme = theme;
    else delete document.documentElement.dataset.theme;
    const dark = theme ? theme === 'dark' : window.matchMedia('(prefers-color-scheme: dark)').matches;
    $('theme').textContent = dark ? 'Light' : 'Dark';
  }
  function savedTheme() {
    try { return localStorage.getItem('protostrap-theme'); } catch { return null; }
  }

  // The page starts once the DOM is loaded.

  document.addEventListener('DOMContentLoaded', () => {
    applyTheme(savedTheme());
    $('theme').addEventListener('click', () => {
      const dark = document.documentElement.dataset.theme
        ? document.documentElement.dataset.theme === 'dark'
        : window.matchMedia('(prefers-color-scheme: dark)').matches;
      const next = dark ? 'light' : 'dark';
      try { localStorage.setItem('protostrap-theme', next); } catch { /* Private mode may block storage. */ }
      applyTheme(next);
    });
    window.addEventListener('hashchange', () => { render(); $('view').focus({ preventScroll: true }); window.scrollTo(0, 0); });
    setInterval(updateLive, 1000);
    getJSON('/api/whoami').then((w) => { state.role = w.role; render(); }).catch(() => { /* The status error says enough. */ });
    render();
    loop();
  });
})();
