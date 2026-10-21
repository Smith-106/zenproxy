package admin

// dashboardHTML is a build-step-free admin panel. It talks to the JSON API
// with X-Admin-Token and never renders a full API key: the server only ever
// sends a hint.
const dashboardHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>zenproxy 出口池控制台</title>
<style>
  :root {
    --bg:#0e1116; --panel:#161b22; --line:#262d36; --fg:#e6edf3; --muted:#8b949e;
    --ok:#3fb950; --warn:#d29922; --bad:#f85149; --accent:#4493f8;
  }
  * { box-sizing:border-box }
  body { margin:0; background:var(--bg); color:var(--fg);
         font:14px/1.5 -apple-system,"Segoe UI","Microsoft YaHei",sans-serif }
  header { display:flex; align-items:center; gap:16px; padding:16px 24px;
           border-bottom:1px solid var(--line); background:var(--panel);
           position:sticky; top:0; z-index:5 }
  h1 { font-size:16px; margin:0; font-weight:600 }
  header .spacer { flex:1 }
  main { padding:24px; max-width:1240px; margin:0 auto }
  section { margin-bottom:32px }
  h2 { font-size:14px; margin:0 0 12px; color:var(--muted); font-weight:600;
       text-transform:uppercase; letter-spacing:.06em }
  table { width:100%; border-collapse:collapse; background:var(--panel);
          border:1px solid var(--line); border-radius:8px; overflow:hidden }
  th,td { padding:9px 12px; text-align:left; border-bottom:1px solid var(--line);
          font-size:13px; white-space:nowrap }
  th { background:#1b2129; color:var(--muted); font-weight:600 }
  tr:last-child td { border-bottom:none }
  tr:hover td { background:#1a2029 }
  code { font-family:ui-monospace,Consolas,monospace; font-size:12px }
  .pill { display:inline-block; padding:1px 8px; border-radius:10px; font-size:11px;
          border:1px solid transparent }
  .ok   { color:var(--ok);   border-color:color-mix(in srgb,var(--ok) 40%,transparent) }
  .warn { color:var(--warn); border-color:color-mix(in srgb,var(--warn) 40%,transparent) }
  .bad  { color:var(--bad);  border-color:color-mix(in srgb,var(--bad) 40%,transparent) }
  .muted { color:var(--muted) }
  button { background:#21262d; color:var(--fg); border:1px solid var(--line);
           padding:5px 11px; border-radius:6px; cursor:pointer; font-size:12px }
  button:hover { background:#2b323b; border-color:#3d444d }
  button.primary { background:var(--accent); border-color:var(--accent); color:#fff }
  button.danger:hover { background:#3d1a1a; border-color:var(--bad); color:var(--bad) }
  .cards { display:grid; grid-template-columns:repeat(auto-fit,minmax(150px,1fr)); gap:12px }
  .card { background:var(--panel); border:1px solid var(--line); border-radius:8px; padding:14px }
  .card .k { color:var(--muted); font-size:12px }
  .card .v { font-size:22px; font-weight:600; margin-top:4px }
  form { display:grid; grid-template-columns:repeat(auto-fit,minmax(160px,1fr));
         gap:10px; align-items:end; background:var(--panel); padding:16px;
         border:1px solid var(--line); border-radius:8px; margin-bottom:12px }
  label { display:flex; flex-direction:column; gap:5px; font-size:12px; color:var(--muted) }
  input,select { background:#0d1117; border:1px solid var(--line); color:var(--fg);
                 padding:7px 9px; border-radius:6px; font-size:13px }
  #gate { max-width:380px; margin:14vh auto; background:var(--panel); padding:24px;
          border:1px solid var(--line); border-radius:10px }
  #gate input { width:100%; margin-bottom:12px }
  #gate button { width:100%; padding:9px }
  #msg { position:fixed; bottom:20px; left:50%; transform:translateX(-50%);
         background:var(--panel); border:1px solid var(--line); padding:10px 18px;
         border-radius:8px; display:none; font-size:13px }
  tr.detail td { white-space:normal; background:#12161c; font-size:12px; color:var(--muted) }
  #free_lists { margin-top:10px }
  #free_lists .pill { margin:0 6px 6px 0 }
  #free_lists .grp { margin:2px 8px 6px 0; color:var(--muted); font-size:12px }
  #foot { max-width:1240px; margin:0 auto; padding:0 24px 28px; color:var(--muted);
         font-size:12px }
</style>
</head>
<body>
<form id="gate" onsubmit="saveToken(); return false;">
  <h1>zenproxy 控制台</h1>
  <p class="muted">填入 <code>server.admin_token</code>（或启动时的 <code>ZEN_ADMIN_TOKEN</code>）。令牌仅存本机 localStorage。</p>
  <input id="token" type="password" placeholder="admin token" autocomplete="off">
  <button class="primary" type="submit">进入</button>
</form>

<div id="app" style="display:none">
<header>
  <h1>zenproxy 出口池控制台</h1>
  <span class="spacer"></span>
  <button onclick="probe()">探活所有出口</button>
  <button class="danger" onclick="prune()">清理垃圾出口</button>
  <button onclick="load()">刷新</button>
  <button class="danger" onclick="logout()">退出</button>
</header>
<main>
  <section>
    <h2>总览</h2>
    <div class="cards" id="stat"></div>
    <p class="muted" id="pool_shape"></p>
  </section>

  <section>
    <h2>免费额度</h2>
    <div class="cards" id="free"></div>
    <div id="free_lists"></div>
    <p class="muted" style="margin-top:8px" id="free_hint"></p>
  </section>

  <section>
    <h2>运行配置（只读，改配置需重启）</h2>
    <div id="cfg"></div>
  </section>

  <section>
    <h2>出口池（IP 池）</h2>
    <table id="exits"><thead><tr>
      <th>出口 ID</th><th>类型</th><th>地址</th><th>出口 IP</th><th>延迟</th>
      <th>状态</th><th>额度</th><th>地区</th><th>连续失败</th><th>软失败</th><th>隔离至</th><th>绑定账号</th><th>最近错误</th><th>操作</th>
    </tr></thead><tbody></tbody></table>
    <p class="muted" style="margin-top:8px">
      <b>清理垃圾出口</b>会把「隔离中 / 降级 / 延迟超过 max_latency / 连续失败达 retire_after」的出口
      <b>永久移出轮换</b>——不像隔离那样到点自动复活。误清了就用「恢复」放回来。
    </p>
  </section>

  <section>
    <h2>账号池</h2>
    <form onsubmit="return saveAccount(event)">
      <label>账号 ID<input id="f_id" placeholder="zen-01" required></label>
      <label>备注<input id="f_name" placeholder="可选"></label>
      <label>上游
        <select id="f_upstream"></select>
      </label>
      <label>API Key<input id="f_key" placeholder="留空 = 不改动" autocomplete="off"></label>
      <label>出口
        <input id="f_proxy" placeholder="auto / direct / 出口ID" value="auto">
      </label>
      <label>权重<input id="f_weight" type="number" value="0" title="预留字段：当前轮换不按权重分配，0 = 默认"></label>
      <label>自定义头（JSON）<input id="f_headers" placeholder='{"X-Foo":"bar"}' autocomplete="off"></label>
      <label>启用
        <select id="f_enabled"><option value="true">是</option><option value="false">否</option></select>
      </label>
      <button class="primary" type="submit">保存账号</button>
    </form>
    <table id="accounts"><thead><tr>
      <th>账号</th><th>上游</th><th>Key</th><th>策略</th><th>出口</th><th>出口 IP</th><th>状态</th>
      <th>最近分类</th><th>请求</th><th>成功/请求</th><th>换 IP 次数</th><th>最近错误</th><th>操作</th>
    </tr></thead><tbody></tbody></table>
    <p class="muted" style="margin-top:8px">
      「编辑」把该账号回填到上面表单里，改完点保存即可更新（ID 本身也可以改，改了就是新增）。
      「策略」是账号的出口选择（Cred）：<code>auto</code> 从池子里挑并保持亲和，
      <code>direct</code> 直连，写出口 ID 则优先落到它。
      「成功/请求」的右边是请求数。尝试失败按每一次上游调用计，写在同一格下面：
      一次请求换几个出口就会记几笔，不能拿来当除数。
    </p>
  </section>
</main>
<footer id="foot"></footer>
</div>
<div id="msg"></div>

<script>
const LS_KEY = 'zenproxy_admin_token';
let token = localStorage.getItem(LS_KEY) || new URLSearchParams(location.search).get('token') || '';
const cache = { accounts: [] };

function saveToken() {
  token = document.getElementById('token').value.trim();
  localStorage.setItem(LS_KEY, token);
  if (location.search) history.replaceState(null, '', location.pathname);
  boot();
}
function logout() { localStorage.removeItem(LS_KEY); location.reload(); }

function toast(text, bad) {
  const el = document.getElementById('msg');
  el.textContent = text;
  el.style.display = 'block';
  el.style.borderColor = bad ? 'var(--bad)' : 'var(--line)';
  clearTimeout(el._t);
  el._t = setTimeout(() => el.style.display = 'none', 4000);
}

async function api(path, opts) {
  const res = await fetch('/admin/api' + path, Object.assign({}, opts, {
    headers: Object.assign({ 'X-Admin-Token': token, 'Content-Type': 'application/json' },
                           (opts && opts.headers) || {})
  }));
  const body = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(body.error || ('HTTP ' + res.status));
  return body;
}

function esc(v) {
  return String(v == null ? '' : v).replace(/[&<>"']/g, c =>
    ({ '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;' }[c]));
}

async function load() {
  try {
    const [status, exits, accounts] = await Promise.all([
      api('/status'), api('/proxies'), api('/accounts')
    ]);
    renderStat(status);
    renderFree(status);
    renderConfig(status);
    fillUpstreams(status.upstreams || []);
    document.getElementById('foot').innerHTML =
      '监听 <code>' + esc(status.listen || '') + '</code> · 上游 <code>' +
      esc((status.upstreams || []).join(', ')) + '</code> · 白名单 <code>' +
      esc((status.allowed_hosts || []).join(', ') || '（仅回环监听时可为空）') + '</code> · ' +
      replicaLine(status);
    renderExits(exits.exits || []);
    cache.accounts = accounts.accounts || [];
    renderAccounts(cache.accounts);
  } catch (e) { toast(e.message, true); }
}

function replicaLine(s) {
  const rows = s.replicas || [];
  if (!rows.length) return '状态文件 <code>' + esc(s.state_file || '未持久化') + '</code>';
  return rows.map(r => {
    const who = r.self ? '本副本' : '另一副本';
    const when = r.saved_at ? new Date(r.saved_at).toLocaleTimeString() : '—';
    return who + ' <code>' + esc(r.file || '') + '</code> 退役 ' + (r.retired || 0) +
      ' / ' + (r.exits || 0) + '，额度冷却 ' + (r.quota || 0) + '，写于 ' + esc(when);
  }).join(' · ') + accountReplicaLine(s);
}

function accountReplicaLine(s) {
  const rows = s.account_replicas || [];
  if (!rows.length) return '';
  return ' · ' + rows.map(r => {
    const who = r.self ? '本副本账号' : '另一副本账号';
    return who + ' <code>' + esc(r.name || '') + '</code> 请求 ' + (r.requests || 0) +
      ' 成功 ' + (r.success || 0) + ' 尝试失败 ' + (r.failures || 0);
  }).join(' · ');
}

function renderStat(s) {
  const cards = [
    ['账号', s.accounts + ' / 启用 ' + s.accounts_enabled],
    ['可用', s.exits_healthy + ' / ' + s.exits, '还在轮换里，含观察 ' + (s.exits_watching || 0) + '、偏慢 ' + (s.exits_slow || 0) + '。这两类行上另有标记，隔离和退休不算'],
    ['隔离中', (s.quarantine && s.quarantine.total) || 0],
    ['偏慢', s.exits_slow || 0, '延迟超过上限，挑选时靠后，没有退休'],
    ['可走免费', s.exits_gate || 0, '免费模型现在会挑这些出口，过一会儿这次准入就失效'],
    ['退休出口', s.exits_retired],
    ['请求', s.requests],
    ['成功', (s.success || 0) + ' / ' + (s.requests || 0)],
    ['尝试失败', s.failures, '一次请求换出口会记多次，不是失败的请求数'],
    ['换 IP 次数', s.rotations],
    ['在线', Math.floor(s.uptime_seconds) + 's'],
  ];
  document.getElementById('stat').innerHTML = cards.map(c =>
    '<div class="card"' + (c[2] ? ' title="' + esc(c[2]) + '"' : '') + '><div class="k">' + esc(c[0]) + '</div><div class="v">' + esc(c[1]) + '</div></div>').join('');
  const q = s.quarantine || {};
  const bits = ['超时 ' + (q.timeout || 0), '连不上 ' + (q.unreachable || 0), 'TLS ' + (q.tls || 0)];
  if (q.other) bits.push('其他 ' + q.other);
  document.getElementById('pool_shape').textContent =
    '隔离原因：' + bits.join(' · ') + '。可用里含观察 ' + (s.exits_watching || 0) + '、偏慢 ' + (s.exits_slow || 0) + '，这两类行上不是「健康」。';
}

function renderConfig(s) {
  const p = s.policy || {}, h = s.health || {};
  const row = (k, v) => '<tr><td><code>' + esc(k) + '</code></td><td><code>' + esc(v) + '</code></td></tr>';
  const maxMB = p.max_body_bytes == null ? '—' : (p.max_body_bytes / 1048576).toFixed(0) + ' MB';
  document.getElementById('cfg').innerHTML =
    '<table><thead><tr><th>轮换与重试（policy）</th><th>值</th></tr></thead><tbody>' +
    row('max_attempts（单请求最多试几个出口）', p.max_attempts) +
    row('rotate_on_status（命中就换出口）', (p.rotate_on_status || []).join(', ')) +
    row('rotate_on_transport（连不上就换）', p.rotate_on_transport) +
    row('max_body_bytes（超过则不缓冲、不可重试）', maxMB) +
    row('stream_idle_timeout（流式静默掐断）', p.stream_idle_timeout) +
    row('upstream_timeout（单请求硬上限）', p.upstream_timeout) +
    '</tbody></table><br>' +
    '<table><thead><tr><th>出口探活（health）</th><th>值</th></tr></thead><tbody>' +
    row('interval（探活间隔）', h.interval) +
    row('timeout（单次探活超时）', h.timeout) +
    row('failure_threshold（几次失败开始隔离）', h.failure_threshold) +
    row('quarantine → max_backoff（隔离退避）', h.quarantine + ' → ' + h.max_backoff) +
    row('max_latency（超过则降为备用）', h.max_latency) +
    row('retire_after（永久淘汰阈值，0=关闭）', h.retire_after) +
    '</tbody></table>' +
    '<p class="muted" style="margin-top:8px">这里只展示、不修改：要调参数改 YAML 后重启生效。</p>';
}

function renderFree(s) {
  const f = s.free_tier || {};
  document.getElementById('free').innerHTML =
    '<div class="card"><div class="k">套壳开关</div><div class="v">' +
    (f.enabled ? '<span class="pill ok">开启</span>' : '<span class="pill bad">关闭</span>') + '</div></div>' +
    '<div class="card"><div class="k">配置的模型</div><div class="v">' + ((f.models || []).length) + '</div></div>' +
    '<div class="card"><div class="k">靠被拒学会的</div><div class="v">' + ((f.learned || []).length) + '</div></div>';
  const box = document.getElementById('free_lists');
  const chips = (list, cls) => (list || []).map(m =>
    '<span class="pill ' + cls + '">' + esc(m) + '</span>').join('');
  box.innerHTML =
    '<span class="grp">配置：</span>' + (chips(f.models, '') || '<span class="muted">无</span>') + '<br>' +
    '<span class="grp">被拒学会：</span>' + (chips(f.learned, 'warn') || '<span class="muted">无</span>');
  document.getElementById('free_hint').innerHTML =
    '「被拒学会」是靠第一次 403 才发现的免费模型——写进配置文件的 <code>free_tier.models</code> 里，' +
    '下次连那一次 403 都不用浪费。套壳关闭时这里永远是空。';
}

function fillUpstreams(list) {
  const sel = document.getElementById('f_upstream');
  const cur = sel.value;
  const names = (list && list.length) ? list.slice().sort() : ['zen', 'zen-go'];
  sel.innerHTML = names.map(n => '<option value="' + esc(n) + '">' + esc(n) + '</option>').join('');
  if (names.indexOf(cur) >= 0) sel.value = cur;
}

function renderExits(list) {
  const tb = document.querySelector('#exits tbody');
  if (!list.length) { tb.innerHTML = '<tr><td colspan="14" class="muted">未配置出口（proxies.resin / proxies.static 均为空）</td></tr>'; return; }
  tb.innerHTML = list.map(e => {
    const q = e.quarantined_until ? new Date(e.quarantined_until) : null;
    let state;
    if (e.retired) {
      state = '<span class="pill bad" title="' + esc(shortReason(e.retired_reason || '')) + '">已退休</span>';
    } else if (q && q > new Date()) {
      const why = quarantineWord(e.quarantine_class);
      state = '<span class="pill bad" title="' + esc(why ? ('隔离原因：' + why) : '隔离中，原因还没探到') + '">隔离中' +
        (why ? ' · ' + esc(why) : '') + '</span>';
    } else if (e.healthy && e.consecutive_failures > 0) {
      const why = shortReason(e.last_error);
      state = '<span class="pill warn" title="还在轮换里，再连续失败几次才会隔离">观察 · ' +
        (why && why !== '—' ? esc(why) + ' ' : '') + e.consecutive_failures + ' 次</span>';
    } else if (e.healthy) {
      state = '<span class="pill ok">健康</span>';
    } else {
      state = '<span class="pill warn">降级</span>';
    }
    if (e.slow) state += ' <span class="pill warn" title="延迟超过上限，挑选时靠后，没有退休">偏慢</span>';
    if (e.gate_pass_until && new Date(e.gate_pass_until) > new Date()) {
      state += ' <span class="pill ok" title="免费模型现在会挑它。别的出口看起来一样健康，但这次还没被准入">可走免费</span>';
    }
    if (!e.retired && e.prunable) state += ' <span class="pill warn" title="下一次清理会把它移出轮换">可清理</span>';
    const act = [];
    if (e.retired) {
      act.push('<button onclick="reviveExit(\'' + esc(e.id) + '\')">恢复</button>');
    } else {
      act.push('<button onclick="retireExit(\'' + esc(e.id) + '\')">退休</button>');
    }
    if (quotaLeft(e)) {
      act.push('<button onclick="uncoolExit(\'' + esc(e.id) + '\')" title="上游说过它会回来；只有你确信那个时刻是错的才用">提前解除额度</button>');
    }
    if ((e.region_blocked_for || []).length) {
      act.push('<button onclick="clearRegion(\'' + esc(e.id) + '\')" title="地区限制没有到期时刻，只能靠这个或 6 小时信任窗口">解除地区限制</button>');
    }
    return '<tr>' +
      '<td><code>' + esc(e.id) + '</code></td>' +
      '<td>' + esc(e.scheme) + '</td>' +
      '<td><code>' + esc(e.addr) + '</code></td>' +
      '<td>' + exitIPCell(e) + '</td>' +
      '<td>' + (e.latency_ms ? e.latency_ms + ' ms' : '—') + '</td>' +
      '<td>' + state + '</td>' +
      '<td>' + quotaCell(e) + '</td>' +
      '<td>' + regionCell(e) + '</td>' +
      '<td>' + e.consecutive_failures + '</td>' +
      '<td class="muted" title="上游返回了响应（429/5xx）：出口本身没坏，只换了绑定">' + (e.soft_failures || 0) + '</td>' +
      '<td class="muted">' + (e.retired ? '—' : ((q && q > new Date()) ? q.toLocaleTimeString() : '—')) + '</td>' +
      '<td class="muted">' + esc((e.bound_accounts || []).join(', ') || '—') + '</td>' +
      '<td class="muted">' + esc(shortReason(e.last_error)) + '</td>' +
      '<td>' + act.join(' ') + '</td>' +
    '</tr>';
  }).join('');
}

// quotaLeft returns the remaining quota cooldown, or null.
//
// Kept separate from health on purpose: an exit the upstream has spent is
// working perfectly, it just will not be served. Showing it as unwell would send
// an operator to debug a healthy proxy.
function quotaLeft(e) {
  if (!e.quota_until) return null;
  const t = new Date(e.quota_until);
  return t > new Date() ? t : null;
}

function quotaCell(e) {
  const t = quotaLeft(e);
  if (!t) return '<span class="muted">—</span>';
  const mins = Math.round((t - new Date()) / 60000);
  const left = mins >= 90 ? Math.round(mins / 60) + ' 小时' : mins + ' 分钟';
  return '<span class="pill warn" title="上游说的重置时刻 ' + esc(t.toLocaleString()) + '">还剩 ' + left + '</span>';
}

function regionCell(e) {
  const models = e.region_blocked_for || [];
  if (!models.length) return '<span class="muted">—</span>';
  // The model names are the point: the exit is fine for everything else, and
  // reporting it as down would be the wrong conclusion to invite.
  return '<span class="pill warn" title="这些模型在该出口所在国家不可用；其他模型不受影响">' +
    models.length + ' 个模型受限</span>';
}

// 类别决定下一步，面板上的这句话要和代码一致：
// transport / proxy_auth / rate_limit / server / region / tier_gate / policy_gate 会换出口；
// auth 和 client 换多少个 IP 都没用。
const CLASS_ZH = {
  ok: ['ok', '成功'], transport: ['warn', '链路故障'], proxy_auth: ['bad', '出口 407'],
  rate_limit: ['warn', '被限流'], server: ['warn', '上游 5xx'], auth: ['bad', '钥匙无效'],
  tier_gate: ['bad', '额度门禁'], region: ['warn', '地区拒绝'], policy_gate: ['warn', '出口被风控'],
  client: ['bad', '请求有误'],
  cancelled: ['muted', '已取消'], unknown: ['muted', '未知'],
};
function classPill(cls) {
  if (!cls) return '<span class="muted">—</span>';
  const m = CLASS_ZH[cls] || ['muted', cls];
  const tip = cls === 'tier_gate'
    ? '免费门禁（FreeTierError）。套壳之后仍可能是某一个出口的问题，所以会再换一两个出口，然后才把上游原文交回去'
    : cls === 'auth'
    ? '401/403 且是钥匙的形状：检查这个账号的 key，不是出口的问题'
    : cls === 'region'
    ? '这个模型在该出口的国家不可用。只对这个模型换出口，出口本身还能服务别的模型'
    : cls === 'policy_gate'
    ? '上游按出口 IP 做了风控。换出口有用，连续几次会把这个出口隔离'
    : cls === 'rate_limit' ? '429：账号或出口额度问题。有重置时刻就等，否则换绑定' : '';
  return '<span class="pill ' + m[0] + '" title="' + esc(tip) + '">' + esc(m[1]) +
         ' <code>' + esc(cls) + '</code></span>';
}
function exitIPWord(note) {
  if (!note) return '';
  if (note === 'canceled') return '隧道是通的，这次地址探测被取消';
  if (note === 'timeout') return '隧道是通的，这次地址探测超时';
  if (note.indexOf('status ') === 0) return '隧道是通的，地址探测返回 ' + note.slice(7);
  return '隧道是通的，这次没问到出口地址';
}
function exitIPCell(e) {
  const ip = e.exit_ip || '—';
  const note = exitIPWord(e.exit_ip_note);
  const title = note ? ' title="' + esc(note) + '"' : '';
  return '<code' + title + '>' + esc(ip) + '</code>';
}
function quarantineWord(cls) {
  if (cls === 'timeout') return '超时';
  if (cls === 'unreachable') return '连不上';
  if (cls === 'tls') return 'TLS';
  if (cls === 'other') return '其他';
  return '';
}
function scrubReason(msg) {
  if (!msg) return '';
  return String(msg)
    .replace(/[^\s\/@]+:[^\s\/@]+@/g, '')
    .replace(/\b(?:\d{1,3}\.){3}\d{1,3}(?::\d+)?\b/g, '<exit>');
}
function shortReason(msg) {
  const raw = scrubReason(msg);
  if (!raw) return '—';
  if (raw === 'timeout') return '超时';
  if (raw === 'unreachable') return '连不上';
  if (raw === 'tls') return 'TLS 失败';
  if (raw === 'other') return '其他';
  const low = raw.toLowerCase();
  if (low.indexOf('proxyconnect') >= 0 || low.indexOf('socks5:') >= 0 || low.indexOf('i/o timeout') >= 0 || low.indexOf('connection refused') >= 0 || low.indexOf('no such host') >= 0)
    return '出口连不上游';
  if (low.indexOf('certificate') >= 0 || low.indexOf('tls:') >= 0)
    return '出口 TLS 失败';
  if (low.indexOf('freetiererror') >= 0 || low.indexOf('only be used from within opencode') >= 0)
    return '免费门禁';
  if (low.indexOf('policy violation') >= 0)
    return '出口被风控';
  if (low.indexOf('regionerror') >= 0 || low.indexOf('not available in your country') >= 0)
    return '地区拒绝';
  let s = raw.replace(/^Post "[^"]*":\s*/, '');
  if (s.length > 72) s = s.slice(0, 72) + '…';
  return s;
}

function fmtTime(v) {
  if (!v) return '—';
  const d = new Date(v);
  return isNaN(d) ? esc(v) : d.toLocaleString();
}

function renderAccounts(list) {
  const tb = document.querySelector('#accounts tbody');
  if (!list.length) { tb.innerHTML = '<tr><td colspan="13" class="muted">还没有账号，用上面的表单加一个</td></tr>'; return; }
  tb.innerHTML = list.map(a => {
    const st = a.stats || {};
    const detailId = 'detail_' + a.id;
    const rows = [];
    rows.push('<tr>' +
      '<td><code>' + esc(a.id) + '</code>' + (a.name ? ' <span class="muted">' + esc(a.name) + '</span>' : '') +
        (a.has_key ? '' : ' <span class="pill warn" title="没有 key，这个账号发出去的请求上游会直接拒收">无 key</span>') + '</td>' +
      '<td>' + esc(a.upstream) + '</td>' +
      '<td><code>' + esc(a.key_hint || '未设置') + '</code></td>' +
      '<td><code>' + esc(a.proxy || 'auto') + '</code></td>' +
      '<td><code>' + esc(a.bound_exit || '直连') + '</code></td>' +
      '<td><code>' + esc(a.bound_exit_ip || '—') + '</code></td>' +
      '<td>' + (a.enabled ? '<span class="pill ok">启用</span>' : '<span class="pill bad">禁用</span>') + '</td>' +
      '<td>' + classPill(st.last_class) + '</td>' +
      '<td>' + (st.requests || 0) + '</td>' +
      '<td title="左边是完成的请求，右边是请求总数。下面的尝试失败按换出口计，不能拿来当除数">' +
        (st.success || 0) + ' / ' + (st.requests || 0) +
        '<div class="muted">尝试失败 ' + (st.failures || 0) + '</div></td>' +
      '<td>' + (st.rotations || 0) + '</td>' +
      '<td class="muted">' + esc(shortReason(st.last_error)) + '</td>' +
      '<td style="white-space:normal">' +
        '<button onclick="fillAccount(\'' + esc(a.id) + '\')">编辑</button> ' +
        '<button onclick="toggleAccount(\'' + esc(a.id) + '\')">' + (a.enabled ? '禁用' : '启用') + '</button> ' +
        '<button onclick="rotate(\'' + esc(a.id) + '\')">换出口</button> ' +
        '<button onclick="toggleDetail(\'' + esc(a.id) + '\')">详情</button> ' +
        '<button class="danger" onclick="del(\'' + esc(a.id) + '\')">删</button>' +
      '</td>' +
    '</tr>');
    const hdrs = a.headers || a.Headers;
    rows.push('<tr class="detail" id="' + esc(detailId) + '" style="display:none"><td colspan="13">' +
      '权重 <code>' + esc(a.weight || 0) + '</code> · ' +
      '自定义头 <code>' + esc(hdrs && Object.keys(hdrs).length ? JSON.stringify(hdrs) : '无') + '</code> · ' +
      '绑定出口 IP <code>' + esc(a.bound_exit_ip || '—') + '</code> · ' +
      '成功 ' + (st.success || 0) + ' · 上游调用 ' + (st.upstream_calls || 0) + ' · ' +
      '最近使用 ' + esc(fmtTime(st.last_used)) + ' · 最近成功 ' + esc(fmtTime(st.last_success)) +
    '</td></tr>');
    return rows.join('');
  }).join('');
}

function toggleDetail(id) {
  const el = document.getElementById('detail_' + id);
  if (el) el.style.display = el.style.display === 'none' ? '' : 'none';
}

function fillAccount(id) {
  const a = cache.accounts.find(x => x.id === id);
  if (!a) return;
  document.getElementById('f_id').value = a.id;
  document.getElementById('f_name').value = a.name || '';
  document.getElementById('f_upstream').value = a.upstream || 'zen';
  document.getElementById('f_proxy').value = a.proxy || 'auto';
  document.getElementById('f_weight').value = a.weight || 0;
  const hdrs = a.headers || a.Headers;
  document.getElementById('f_headers').value = hdrs && Object.keys(hdrs).length ? JSON.stringify(hdrs) : '';
  document.getElementById('f_enabled').value = a.enabled ? 'true' : 'false';
  document.getElementById('f_key').value = '';
  document.getElementById('f_id').scrollIntoView({ behavior: 'smooth', block: 'center' });
  toast('已把 ' + id + ' 回填到表单，改完点保存账号（Key 留空 = 不改动）');
}

async function toggleAccount(id) {
  const a = cache.accounts.find(x => x.id === id);
  if (!a) return;
  try {
    await api('/accounts', { method: 'POST', body: JSON.stringify({ id: id, enabled: !a.enabled }) });
    toast(id + (a.enabled ? ' 已禁用' : ' 已启用'));
    await load();
  } catch (e) { toast(e.message, true); }
}

async function saveAccount(ev) {
  ev.preventDefault();
  const id = document.getElementById('f_id').value.trim();
  if (!id) return false;
  // 表单是"全量保存"语义：头字段是 {} 就表示清空（想保留就别动它，
  // 「编辑」回填时已经把旧值写进去了）。
  let headers = {};
  const hs = document.getElementById('f_headers').value.trim();
  if (hs) {
    try {
      headers = JSON.parse(hs);
      if (typeof headers !== 'object' || Array.isArray(headers)) throw new Error('need object');
    } catch (e) { toast('自定义头不是合法的 JSON 对象', true); return false; }
  }
  try {
    await api('/accounts', { method: 'POST', body: JSON.stringify({
      id: id,
      name: document.getElementById('f_name').value.trim(),
      upstream: document.getElementById('f_upstream').value,
      api_key: document.getElementById('f_key').value.trim(),
      proxy: document.getElementById('f_proxy').value.trim() || 'auto',
      weight: parseInt(document.getElementById('f_weight').value || '0', 10) || 0,
      headers: headers,
      enabled: document.getElementById('f_enabled').value === 'true',
    })});
    document.getElementById('f_key').value = '';
    toast('已保存 ' + id);
    await load();
  } catch (e) { toast(e.message, true); }
  return false;
}

async function rotate(id) {
  try { const r = await api('/accounts/' + encodeURIComponent(id) + '/rotate', { method: 'POST' });
        toast(id + ' 已切换到 ' + (r.account.bound_exit || '直连')); await load(); }
  catch (e) { toast(e.message, true); }
}

async function del(id) {
  if (!confirm('删除账号 ' + id + ' ？')) return;
  try { await api('/accounts/' + encodeURIComponent(id), { method: 'DELETE' }); toast('已删除 ' + id); await load(); }
  catch (e) { toast(e.message, true); }
}

async function probe() {
  try { await api('/proxies/probe', { method: 'POST' }); toast('探活已触发，几秒后刷新'); }
  catch (e) { toast(e.message, true); }
}

async function prune() {
  if (!confirm('把当前「隔离中 / 降级 / 延迟过高」的出口永久移出轮换？\n（误清了可以用该行的「恢复」放回来）')) return;
  try {
    const r = await api('/proxies/prune', { method: 'POST' });
    const rep = r.report || {};
    const n = (rep.retired || []).length;
    if (rep.pool_empty) {
      toast('已清理 ' + n + ' 个，但出口池现在空了——请加出口或点「恢复」', true);
    } else {
      toast('已清理 ' + n + ' 个出口，仍有 ' + rep.usable_after + ' 个可用');
    }
    await load();
  } catch (e) { toast(e.message, true); }
}

async function retireExit(id) {
  if (!confirm('把出口 ' + id + ' 永久移出轮换？')) return;
  try { await api('/proxies/' + encodeURIComponent(id) + '/retire', { method: 'POST' }); toast('已退休 ' + id); await load(); }
  catch (e) { toast(e.message, true); }
}

async function reviveExit(id) {
  try { await api('/proxies/' + encodeURIComponent(id) + '/revive', { method: 'POST' }); toast('已恢复 ' + id); await load(); }
  catch (e) { toast(e.message, true); }
}

// Both of these exist because the proxy cannot observe what they report. A quota
// cooldown ends on a clock the upstream named, and a region block ends when a
// country reopens — nothing announces either, so an operator who knows better
// needs a way to say so.

async function uncoolExit(id) {
  // Deliberately not a plain-action button: the upstream stated the deadline, so
  // second-guessing it puts traffic straight back into a refusal.
  if (!confirm('提前解除 ' + id + ' 的额度冷却？\n\n上游明确说过它什么时候回来。只有你刚充值、或确信那个时刻是错的才这么做。')) return;
  try { await api('/proxies/' + encodeURIComponent(id) + '/uncool', { method: 'POST' }); toast('已解除额度冷却 ' + id); await load(); }
  catch (e) { toast(e.message, true); }
}

async function clearRegion(id) {
  if (!confirm('解除 ' + id + ' 的所有地区限制记录？\n\n这不会改变上游的行为，只是让代理不再跳过它。下次再被拒就会重新记上。')) return;
  try {
    const r = await api('/proxies/' + encodeURIComponent(id) + '/clear-region', { method: 'POST' });
    toast('已清除 ' + (r.cleared || 0) + ' 条地区限制');
    await load();
  } catch (e) { toast(e.message, true); }
}

async function boot() {
  if (!token) return;
  try {
    await api('/status');
    document.getElementById('gate').style.display = 'none';
    document.getElementById('app').style.display = 'block';
    await load();
  } catch (e) {
    document.getElementById('gate').style.display = 'block';
    document.getElementById('app').style.display = 'none';
    if (token) toast('令牌无效：' + e.message, true);
  }
}
boot();
setInterval(() => { if (document.getElementById('app').style.display !== 'none') load(); }, 15000);
</script>
</body>
</html>`
