(() => {
  'use strict';

  const POLL_MS = 2000;
  const MAX_POINTS = 300;
  const SEGMENTS = 50;
  const SVG_NS = 'http://www.w3.org/2000/svg';
  const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

  const $ = (id) => document.getElementById(id);
  const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, Number.isFinite(v) ? v : 0));

  // Every dynamic string reaches the page through textContent or text nodes.
  // Container names, images and mount points are data from outside this page.
  function h(tag, props, ...kids) {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(props || {})) {
      if (v == null || v === false) continue;
      if (k === 'class') n.className = v;
      else if (k === 'text') n.textContent = v;
      else n.setAttribute(k, v === true ? '' : String(v));
    }
    for (const kid of kids) if (kid != null) n.append(kid);
    return n;
  }
  function sv(tag, props) {
    const n = document.createElementNS(SVG_NS, tag);
    for (const [k, v] of Object.entries(props || {})) n.setAttribute(k, String(v));
    return n;
  }

  /* ---------- formatting ---------- */

  const UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  function fmtBytes(n) {
    if (!Number.isFinite(n) || n < 0) return '–';
    let i = 0;
    while (n >= 1024 && i < UNITS.length - 1) { n /= 1024; i++; }
    const txt = i === 0 || n >= 100 ? n.toFixed(0) : n >= 10 ? n.toFixed(1) : String(+n.toFixed(2));
    return txt + ' ' + UNITS[i];
  }
  const fmtRate = (n) => fmtBytes(n) + '/s';
  const fmtPct = (n) => (Number.isFinite(n) ? String(+n.toFixed(n >= 10 ? 0 : 1)) : '–') + '%';
  function fmtDuration(sec) {
    sec = Math.max(0, Math.floor(sec));
    const d = Math.floor(sec / 86400);
    const hr = Math.floor((sec % 86400) / 3600);
    const m = Math.floor((sec % 3600) / 60);
    if (d) return d + 'd ' + hr + 'h';
    if (hr) return hr + 'h ' + m + 'm';
    if (m) return m + 'm';
    return sec + 's';
  }
  const fmtTime = (ms) => new Date(ms).toLocaleTimeString([], { hour12: false });

  function niceMax(v) {
    const i = Math.max(0, Math.floor(Math.log(v) / Math.log(1024)));
    const base = Math.pow(1024, i);
    const sc = v / base;
    const e = Math.pow(10, Math.floor(Math.log10(sc)));
    const f = sc / e;
    const nice = f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10;
    return nice * e * base;
  }

  /* ---------- LED meter ---------- */

  function createMeter(track, label) {
    track.setAttribute('role', 'meter');
    track.setAttribute('aria-label', label);
    track.setAttribute('aria-valuemin', '0');
    track.setAttribute('aria-valuemax', '100');
    const segs = [];
    for (let i = 0; i < SEGMENTS; i++) {
      const zone = i < 30 ? 0 : i < 42 ? 1 : 2; // fixed zones: 0-59%, 60-84%, 85-100%
      const seg = h('div', { class: 'seg z' + zone });
      track.append(seg);
      segs.push(seg);
    }
    let first = true;
    return {
      set(pct) {
        const p = clamp(pct, 0, 100);
        const lit = Math.round((p / 100) * SEGMENTS);
        if (first && !reduceMotion) {
          // the one start-up moment: the lit segments power on left to right
          segs.forEach((seg, i) => { seg.style.transitionDelay = i * 12 + 'ms'; });
          setTimeout(() => segs.forEach((seg) => { seg.style.transitionDelay = ''; }), SEGMENTS * 12 + 400);
        }
        first = false;
        segs.forEach((seg, i) => seg.classList.toggle('on', i < lit));
        track.setAttribute('aria-valuenow', String(Math.round(p)));
        track.setAttribute('aria-valuetext', Math.round(p) + ' percent');
      },
    };
  }

  const meters = {
    cpu: createMeter($('cpu-bar'), 'Processor use'),
    mem: createMeter($('mem-bar'), 'Memory use'),
    disk: createMeter($('disk-bar'), 'Disk use'),
  };

  function setLines(node, lines) {
    node.replaceChildren(...lines.map((t) => h('span', { text: t })));
  }
  const levelClass = (pct, warn, crit) => (pct >= crit ? 'crit' : pct >= warn ? 'warn' : '');

  /* ---------- history store ---------- */

  const store = { interval: 2, t: [], cpu: [], mem: [], netRx: [], netTx: [], diskR: [], diskW: [] };
  const KEYS = ['cpu', 'mem', 'netRx', 'netTx', 'diskR', 'diskW'];

  function setHistory(hist) {
    store.interval = hist.interval || 2;
    store.t = (hist.t || []).slice(-MAX_POINTS);
    for (const k of KEYS) store[k] = (hist[k] || []).slice(-MAX_POINTS);
  }

  function pushSample(sys) {
    if (store.t.length && sys.time <= store.t[store.t.length - 1]) return;
    const sum = (arr, key) => (arr || []).reduce((a, x) => a + (x[key] || 0), 0);
    store.t.push(sys.time);
    store.cpu.push(sys.cpu.percent);
    store.mem.push(sys.mem.percent);
    store.netRx.push(sum(sys.net, 'rxBps'));
    store.netTx.push(sum(sys.net, 'txBps'));
    store.diskR.push(sum(sys.diskIo, 'readBps'));
    store.diskW.push(sum(sys.diskIo, 'writeBps'));
    if (store.t.length > MAX_POINTS) {
      store.t.shift();
      for (const k of KEYS) store[k].shift();
    }
  }

  /* ---------- line charts ---------- */

  function createChart(parent, cfg) {
    const multi = cfg.series.length > 1;
    const readouts = cfg.series.map((sr) => {
      const val = h('span', { class: 'ro-val', text: '–' });
      const node = h('span', { class: 'ro' },
        multi ? h('span', { class: 'linekey ' + sr.cls }) : null,
        multi ? h('span', { class: 'ro-name', text: sr.name }) : null,
        val);
      return { node, val };
    });

    const svg = sv('svg', { height: 150, role: 'img', tabindex: 0 });
    const tip = h('div', { class: 'tip', hidden: true });
    const plot = h('div', { class: 'plot' }, svg, tip);
    const root = h('section', { class: 'panel chart' },
      h('div', { class: 'chart-head' },
        h('h3', { text: cfg.title }),
        h('div', { class: 'readouts' }, ...readouts.map((r) => r.node))),
      plot,
      h('div', { class: 'chart-foot' }, h('span', { text: '10 minutes ago' }), h('span', { text: 'now' })));
    parent.append(root);

    const fmt = cfg.kind === 'pct' ? fmtPct : fmtRate;
    let data = null;
    let geom = null;
    let hovering = false;
    let kbIndex = -1;

    function draw() {
      if (!data) return;
      const W = Math.max(240, Math.floor(plot.clientWidth));
      const H = 150;
      const L = 48, R = 8, T = 8, B = 8;
      const pw = W - L - R;
      const ph = H - T - B;
      svg.setAttribute('viewBox', '0 0 ' + W + ' ' + H);
      svg.setAttribute('width', String(W));
      svg.replaceChildren();

      let max = 100;
      if (cfg.kind === 'rate') {
        let m = 0;
        for (const sr of cfg.series) for (const v of data[sr.key]) if (v > m) m = v;
        max = niceMax(Math.max(m, 1024));
      }
      const n = data.t.length;
      const tEnd = n ? data.t[n - 1] : Date.now();
      const span = data.interval * 1000 * MAX_POINTS;
      const x = (t) => L + pw * clamp(1 - (tEnd - t) / span, 0, 1);
      const y = (v) => T + ph - (Math.min(Math.max(v, 0), max) / max) * ph;

      for (const frac of [0, 0.5, 1]) {
        const yy = y(max * frac);
        svg.append(sv('line', { class: 'grid', x1: L, x2: W - R, y1: yy, y2: yy }));
        const label = cfg.kind === 'pct' ? Math.round(max * frac) + '%' : fmtBytes(max * frac);
        const txt = sv('text', { class: 'axis', x: L - 8, y: yy + 4, 'text-anchor': 'end' });
        txt.textContent = label;
        svg.append(txt);
      }

      if (n >= 2) {
        for (const sr of cfg.series) {
          const pts = data[sr.key].map((v, i) => x(data.t[i]).toFixed(1) + ',' + y(v).toFixed(1));
          if (!multi) {
            svg.append(sv('polygon', {
              class: 'area ' + sr.cls,
              points: x(data.t[0]).toFixed(1) + ',' + y(0) + ' ' + pts.join(' ') + ' ' + x(tEnd).toFixed(1) + ',' + y(0),
            }));
          }
          svg.append(sv('polyline', { class: 'line ' + sr.cls, points: pts.join(' ') }));
        }
      }

      const cross = sv('line', { class: 'cross', y1: T, y2: T + ph, visibility: 'hidden' });
      const dots = cfg.series.map((sr) => sv('circle', { class: 'dot-mark ' + sr.cls, r: 4, visibility: 'hidden' }));
      svg.append(cross, ...dots);
      geom = { W, L, pw, x, y, cross, dots };

      const sum = cfg.series.map((sr) => (n ? fmt(data[sr.key][n - 1]) : '–')).join(', ');
      svg.setAttribute('aria-label', cfg.title + ' over the last 10 minutes. Now ' + sum + '. Focus the chart and use the arrow keys to read earlier values.');
    }

    function hide() {
      tip.hidden = true;
      if (geom) {
        geom.cross.setAttribute('visibility', 'hidden');
        geom.dots.forEach((d) => d.setAttribute('visibility', 'hidden'));
      }
    }

    function showAt(i) {
      if (!data || !geom || i < 0 || i >= data.t.length) return hide();
      const px = geom.x(data.t[i]);
      geom.cross.setAttribute('x1', px);
      geom.cross.setAttribute('x2', px);
      geom.cross.setAttribute('visibility', 'visible');
      const rows = [h('div', { class: 'tip-time', text: fmtTime(data.t[i]) })];
      cfg.series.forEach((sr, k) => {
        const v = data[sr.key][i];
        geom.dots[k].setAttribute('cx', px);
        geom.dots[k].setAttribute('cy', geom.y(v));
        geom.dots[k].setAttribute('visibility', 'visible');
        rows.push(h('div', { class: 'tip-row' },
          h('span', { class: 'linekey ' + sr.cls }),
          h('strong', { text: fmt(v) }),
          multi ? h('span', { class: 'tip-name', text: sr.name }) : null));
      });
      tip.replaceChildren(...rows);
      tip.hidden = false;
      const w = tip.offsetWidth;
      const left = px > geom.W * 0.6 ? px - 12 - w : px + 12;
      tip.style.left = Math.max(0, left) + 'px';
    }

    function nearest(clientX) {
      const r = svg.getBoundingClientRect();
      const mx = clientX - r.left;
      let best = -1, bd = Infinity;
      for (let i = 0; i < data.t.length; i++) {
        const d = Math.abs(geom.x(data.t[i]) - mx);
        if (d < bd) { bd = d; best = i; }
      }
      return best;
    }

    svg.addEventListener('pointermove', (e) => {
      if (!data || !geom) return;
      hovering = true;
      showAt(nearest(e.clientX));
    });
    svg.addEventListener('pointerleave', () => { hovering = false; hide(); draw(); });
    svg.addEventListener('focus', () => {
      if (!data) return;
      kbIndex = data.t.length - 1;
      hovering = true;
      showAt(kbIndex);
    });
    svg.addEventListener('blur', () => { hovering = false; kbIndex = -1; hide(); draw(); });
    svg.addEventListener('keydown', (e) => {
      if (!data || !data.t.length) return;
      const last = data.t.length - 1;
      if (e.key === 'ArrowLeft') kbIndex = Math.max(0, (kbIndex < 0 ? last : kbIndex) - 1);
      else if (e.key === 'ArrowRight') kbIndex = Math.min(last, (kbIndex < 0 ? last : kbIndex) + 1);
      else if (e.key === 'Home') kbIndex = 0;
      else if (e.key === 'End') kbIndex = last;
      else if (e.key === 'Escape') { svg.blur(); return; }
      else return;
      e.preventDefault();
      showAt(kbIndex);
    });

    if (window.ResizeObserver) new ResizeObserver(() => { if (!hovering) draw(); }).observe(plot);

    return {
      update(d) {
        data = d;
        const n = d.t.length;
        cfg.series.forEach((sr, k) => {
          readouts[k].val.textContent = n ? fmt(d[sr.key][n - 1]) : '–';
        });
        if (!hovering) draw(); // freeze the plot while someone is reading it
      },
    };
  }

  const chartsRoot = $('charts');
  const charts = [
    createChart(chartsRoot, { title: 'Processor', kind: 'pct', series: [{ key: 'cpu', name: 'Processor use', cls: 's1' }] }),
    createChart(chartsRoot, { title: 'Memory', kind: 'pct', series: [{ key: 'mem', name: 'Memory in use', cls: 's1' }] }),
    createChart(chartsRoot, { title: 'Network', kind: 'rate', series: [{ key: 'netRx', name: 'In', cls: 's1' }, { key: 'netTx', name: 'Out', cls: 's2' }] }),
    createChart(chartsRoot, { title: 'Disk throughput', kind: 'rate', series: [{ key: 'diskR', name: 'Read', cls: 's1' }, { key: 'diskW', name: 'Write', cls: 's2' }] }),
  ];

  function renderDataTable() {
    const body = $('dataBody');
    const n = store.t.length;
    const rows = [];
    if (n) {
      const stride = Math.max(1, Math.floor(n / 12));
      for (let i = n - 1; i >= 0 && rows.length < 12; i -= stride) {
        rows.push(h('tr', null,
          h('td', { text: fmtTime(store.t[i]) }),
          h('td', { class: 'num', text: fmtPct(store.cpu[i]) }),
          h('td', { class: 'num', text: fmtPct(store.mem[i]) }),
          h('td', { class: 'num', text: fmtRate(store.netRx[i]) }),
          h('td', { class: 'num', text: fmtRate(store.netTx[i]) }),
          h('td', { class: 'num', text: fmtRate(store.diskR[i]) }),
          h('td', { class: 'num', text: fmtRate(store.diskW[i]) })));
      }
    }
    body.replaceChildren(...rows);
  }

  /* ---------- rendering ---------- */

  let coreEls = [];

  function renderCores(cores) {
    const host = $('cores');
    if (coreEls.length !== cores.length) {
      host.replaceChildren();
      coreEls = cores.map((_, i) => {
        const val = h('span', { text: '–' });
        const fill = h('div', { class: 'fill' });
        host.append(h('div', { class: 'core' },
          h('div', { class: 'core-row' }, h('span', { text: 'CPU ' + (i + 1) }), val),
          h('div', { class: 'track' }, fill)));
        return { val, fill };
      });
    }
    cores.forEach((p, i) => {
      const c = coreEls[i];
      c.val.textContent = fmtPct(p);
      c.fill.style.width = clamp(p, 0, 100) + '%';
      c.fill.className = 'fill ' + levelClass(p, 60, 85);
    });
  }

  function renderSystem(sys) {
    const info = sys.info || {};
    const name = info.hostname || 'server';
    $('host').textContent = name;
    document.title = name + ' – podmon';
    const items = [info.os, info.kernel ? 'Linux ' + info.kernel : '', info.cpuModel ? info.cpuModel + ', ' + info.cpuCount + ' CPUs' : ''].filter(Boolean);
    $('sysinfo').replaceChildren(...items.map((t) => h('span', { class: 'sys-item', text: t })));

    $('uptime').textContent = fmtDuration(sys.uptime);
    $('load').textContent = sys.load.map((v) => v.toFixed(2)).join('  ');
    const temps = sys.temps || [];
    $('tempBox').hidden = temps.length === 0;
    if (temps.length) $('temp').textContent = Math.round(Math.max(...temps.map((t) => t.celsius))) + ' °C';
    $('updated').textContent = fmtTime(sys.time);

    meters.cpu.set(sys.cpu.percent);
    $('cpu-val').textContent = String(Math.round(sys.cpu.percent));
    setLines($('cpu-detail'), [info.cpuCount + ' CPUs', 'Load ' + sys.load[0].toFixed(2)]);
    renderCores(sys.cpu.cores || []);

    const m = sys.mem;
    meters.mem.set(m.percent);
    $('mem-val').textContent = String(Math.round(m.percent));
    setLines($('mem-detail'), [
      fmtBytes(m.used) + ' of ' + fmtBytes(m.total) + ' in use',
      m.swapTotal ? 'Swap ' + fmtBytes(m.swapUsed) + ' of ' + fmtBytes(m.swapTotal) : 'No swap configured',
    ]);

    const disks = sys.disks || [];
    const root = disks.find((d) => d.mount === '/') || disks[0];
    if (root) {
      meters.disk.set(root.percent);
      $('disk-val').textContent = String(Math.round(root.percent));
      setLines($('disk-detail'), [fmtBytes(root.used) + ' of ' + fmtBytes(root.total) + ' used', root.mount + ', ' + fmtBytes(root.free) + ' free']);
    } else {
      meters.disk.set(0);
      $('disk-val').textContent = '–';
      setLines($('disk-detail'), ['No disk reported']);
    }

    renderStorage(disks, temps);
    renderNetwork(sys.net || []);
  }

  function renderNetwork(ifaces) {
    const body = $('net-body');
    if (!body) return;
    body.replaceChildren(...ifaces.map((n) => {
      const typeClass = (n.type || 'lan').toLowerCase().replace(/[^a-z0-9]/g, '');
      const badge = h('span', { class: 'net-badge ' + typeClass, text: n.type || 'LAN' });
      const ips = (n.ips || []).filter(ip => !ip.startsWith('fe80:')); // hide link-local ipv6
      const ipText = ips.length ? ips.join(', ') : '–';
      const nameCell = h('td', { class: 'cell-title ct-name', text: n.name });
      if (n.ssid) nameCell.append(h('span', { class: 'ct-sub', text: n.ssid }));
      return h('tr', null,
        nameCell,
        h('td', { 'data-label': 'Type' }, badge),
        h('td', { 'data-label': 'IP Address', text: ipText }),
        h('td', { 'data-label': 'MAC', text: n.mac || '–' }),
        h('td', { class: 'num', 'data-label': 'In', text: fmtRate(n.rxBps) }),
        h('td', { class: 'num', 'data-label': 'Out', text: fmtRate(n.txBps) }));
    }));
  }

  function renderStorage(disks, temps) {
    $('disk-body').replaceChildren(...disks.map((d) =>
      h('tr', null,
        h('td', { class: 'cell-title ct-name', text: d.mount }),
        h('td', { 'data-label': 'Device', text: d.device }),
        h('td', { 'data-label': 'Type', text: d.fsType }),
        h('td', { class: 'num', 'data-label': 'Used', text: fmtBytes(d.used) + ' of ' + fmtBytes(d.total) }),
        h('td', { class: 'num', 'data-label': 'Free', text: fmtBytes(d.free) }),
        h('td', { 'data-label': 'Usage' }, h('div', { class: 'usage' },
          h('div', { class: 'track' }, usageFill(d.percent)),
          h('span', { class: 'pct', text: fmtPct(d.percent) }))))));
    const list = $('temps');
    list.hidden = temps.length === 0;
    list.replaceChildren(...temps.map((t) => h('li', null, h('span', { text: t.label }), h('strong', { text: Math.round(t.celsius) + ' °C' }))));
  }

  function usageFill(pct) {
    const f = h('div', { class: 'fill ' + levelClass(pct, 70, 90) });
    f.style.width = clamp(pct, 0, 100) + '%';
    return f;
  }

  /* ---------- containers ---------- */

  const showStopped = $('showStopped');
  try { showStopped.checked = localStorage.getItem('podmon.showStopped') === '1'; } catch (e) { /* storage unavailable */ }
  let lastState = null;
  let podmanHasSocket = false;
  showStopped.addEventListener('change', () => {
    try { localStorage.setItem('podmon.showStopped', showStopped.checked ? '1' : '0'); } catch (e) { /* ignore */ }
    if (lastState) renderContainers(lastState);
  });

  function stateCell(c) {
    const kind = c.state === 'running' ? '' : c.state === 'exited' || c.state === 'stopped' || c.state === 'created' ? 'stopped' : 'other';
    const label = c.state ? c.state.charAt(0).toUpperCase() + c.state.slice(1) : 'Unknown';
    return h('span', { class: 'state' }, h('span', { class: 'dot ' + kind, 'aria-hidden': 'true' }), h('span', { text: label }));
  }

  function renderActions(c) {
    const state = c.state;
    let allowed = [];

    // Only show actions for non-infra containers when socket is available
    if (!c.infra && podmanHasSocket) {
      if (state === 'running') {
        allowed = ['pause', 'restart', 'stop', 'kill', 'remove'];
      } else if (state === 'paused') {
        allowed = ['unpause', 'restart', 'stop', 'kill', 'remove'];
      } else if (state === 'exited' || state === 'stopped' || state === 'created') {
        allowed = ['start', 'restart', 'remove'];
      }
    }

    if (allowed.length === 0) {
      return h('div', { class: 'act-menu' });
    }

    const menu = h('div', { class: 'act-menu' });
    const trigger = h('button', {
      class: 'act-trigger',
      type: 'button',
      'aria-label': 'Container actions',
      'aria-haspopup': 'true',
      'aria-expanded': 'false',
      text: '⋮'
    });
    const dropdown = h('div', { class: 'act-dropdown', role: 'menu' });

    allowed.forEach((act) => {
      const item = h('button', {
        class: 'act-item act-' + act,
        type: 'button',
        role: 'menuitem',
        'data-action': act,
        'data-id': c.id,
        text: act.charAt(0).toUpperCase() + act.slice(1)
      });
      item.addEventListener('click', async () => {
        // Disable all items in this dropdown
        dropdown.querySelectorAll('.act-item').forEach((b) => b.disabled = true);
        errNode.hidden = true;
        dropdown.classList.remove('open');
        trigger.setAttribute('aria-expanded', 'false');
        try {
          const res = await fetch('/api/containers/' + c.id + '/' + act, {
            method: 'POST',
            credentials: 'same-origin',
            cache: 'no-store'
          });
          if (!res.ok) {
            const txt = await res.text();
            throw new Error(txt || 'HTTP ' + res.status);
          }
          // Success: re-fetch state immediately
          const st = await fetch('/api/state', { cache: 'no-store', credentials: 'same-origin' }).then(r => r.json());
          render(st);
        } catch (e) {
          errNode.textContent = e.message;
          errNode.hidden = false;
          // Re-enable items on error
          dropdown.querySelectorAll('.act-item').forEach((b) => b.disabled = false);
        }
      });
      dropdown.append(item);
    });

    const errNode = h('span', { class: 'act-err', hidden: true });
    dropdown.append(errNode);

    // Toggle dropdown on trigger click
    trigger.addEventListener('click', (e) => {
      e.stopPropagation();
      const isOpen = dropdown.classList.toggle('open');
      trigger.setAttribute('aria-expanded', isOpen ? 'true' : 'false');
    });

    // Close dropdown on click outside
    document.addEventListener('click', (e) => {
      if (!menu.contains(e.target)) {
        dropdown.classList.remove('open');
        trigger.setAttribute('aria-expanded', 'false');
      }
    });

    // Close on Escape key
    trigger.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') {
        dropdown.classList.remove('open');
        trigger.setAttribute('aria-expanded', 'false');
        trigger.focus();
      }
    });

    menu.append(trigger, dropdown);
    return menu;
  }

  function renderContainers(state) {
    const p = state.podman;
    const notice = $('ct-notice');
    const empty = $('ct-empty');
    const hint = $('ct-hint');
    const table = $('ct-table');

    if (!p.available) {
      notice.hidden = false;
      notice.textContent = p.error || 'Podman is not reachable.';
      table.hidden = true;
      empty.hidden = true;
      hint.hidden = true;
      $('ct-summary').textContent = '';
      return;
    }
    notice.hidden = true;
    table.hidden = false;

    const stopped = p.total - p.running;
    $('ct-summary').textContent = p.running + ' running' + (stopped > 0 ? ', ' + stopped + ' stopped' : '');

    const memTotal = state.system ? state.system.mem.total : 0;
    const rows = p.containers
      .filter((c) => !c.infra && (showStopped.checked || c.state === 'running'))
      .map((c) => {
        const running = c.state === 'running';
        const up = running && c.startedAt > 0 ? 'Up ' + fmtDuration(state.serverTime / 1000 - c.startedAt) : c.status || '–';
        const nameCell = h('td', { class: 'cell-title' }, h('span', { class: 'ct-name', text: c.name }));
        if (c.pod) nameCell.append(h('span', { class: 'ct-sub', text: 'Pod ' + c.pod }));
        const portsCell = h('td', { class: 'ct-ports', 'data-label': 'Ports' });
        const portList = h('div', { class: 'port-list' });
        if (c.ports.length) c.ports.forEach((t) => portList.append(h('span', { text: t })));
        else portList.append(h('span', { class: 'muted', text: '–' }));
        portsCell.append(portList);
        const mem = c.hasStats && running ? fmtBytes(c.memUsage) : '–';
        const memTitle = c.hasStats && running && memTotal ? fmtPct((c.memUsage / memTotal) * 100) + ' of server memory' : null;
        const actions = renderActions(c);
        return h('tr', null,
          nameCell,
          h('td', { 'data-label': 'Image' }, h('span', { class: 'ct-image', text: c.image, title: c.image })),
          h('td', { 'data-label': 'State' }, stateCell(c)),
          h('td', { class: 'num', 'data-label': 'Processor', text: c.hasStats && running ? fmtPct(c.cpu) : '–' }),
          h('td', { class: 'num', 'data-label': 'Memory', text: mem, title: memTitle }),
          portsCell,
          h('td', { 'data-label': 'Status', text: up }),
          h('td', { class: 'ct-actions', 'data-label': 'Actions' }, actions));
      });
    $('ct-body').replaceChildren(...rows);
    table.hidden = rows.length === 0;

    empty.hidden = rows.length !== 0;
    if (rows.length === 0) {
      empty.textContent = showStopped.checked
        ? 'Podman has no containers.'
        : stopped > 0
          ? 'No containers are running. Turn on “Show stopped containers” to see the ' + stopped + ' that exist.'
          : 'No containers are running. Start one with podman run.';
    }
    hint.hidden = p.hasStats;
    if (!p.hasStats) hint.textContent = 'Per-container processor and memory use needs the Podman API socket. Enable it with: systemctl --user enable --now podman.socket';
  }

  /* ---------- connection state ---------- */

  function setStatus(kind) {
    const live = $('live');
    live.dataset.state = kind;
    live.setAttribute('aria-label', { live: 'Connected', offline: 'Connection lost', signedout: 'Signed out', connecting: 'Connecting' }[kind]);
    if (kind === 'offline') $('updated').textContent = 'Offline, retrying';
    if (kind === 'signedout') $('updated').textContent = 'Signed out. Reload to log in.';
  }

  function render(state) {
    lastState = state;
    if (!state.system) return;
    podmanHasSocket = state.podman.hasStats;
    renderSystem(state.system);
    pushSample(state.system);
    charts.forEach((c) => c.update(store));
    if ($('dataDetails').open) renderDataTable();
    renderContainers(state);
  }

  $('dataDetails').addEventListener('toggle', () => { if ($('dataDetails').open) renderDataTable(); });

  /* ---------- polling ---------- */

  let failures = 0;
  let timer = 0;

  async function getJSON(path) {
    const ctl = new AbortController();
    const to = setTimeout(() => ctl.abort(), 8000);
    try {
      const res = await fetch(path, { cache: 'no-store', credentials: 'same-origin', signal: ctl.signal });
      if (res.status === 401) { const e = new Error('unauthorized'); e.status = 401; throw e; }
      if (!res.ok) throw new Error('HTTP ' + res.status);
      return await res.json();
    } finally {
      clearTimeout(to);
    }
  }

  async function loadHistory() {
    try { setHistory(await getJSON('/api/history')); } catch (e) { /* the live poll fills the charts */ }
  }

  function schedule() {
    const delay = failures ? Math.min(30000, POLL_MS * Math.pow(2, Math.min(failures, 4))) : POLL_MS;
    timer = setTimeout(tick, delay);
  }

  async function tick() {
    if (document.hidden) { schedule(); return; }
    try {
      const state = await getJSON('/api/state');
      failures = 0;
      setStatus('live');
      render(state);
    } catch (e) {
      if (e && e.status === 401) { setStatus('signedout'); return; }
      failures++;
      setStatus('offline');
    }
    schedule();
  }

  document.addEventListener('visibilitychange', async () => {
    if (document.hidden) return;
    clearTimeout(timer);
    await loadHistory(); // fill the gap from while the tab was hidden
    tick();
  });

  loadHistory().then(tick);
})();
