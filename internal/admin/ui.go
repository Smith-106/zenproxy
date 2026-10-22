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
  <button class="danger" id="prune_btn" onclick="prune()">清理垃圾出口</button>
  <button onclick="load()">刷新</button>
  <button class="danger" onclick="logout()">退出</button>
</header>
<main>
  <section>
    <h2>总览</h2>
    <div class="cards" id="stat"></div>
    <p class="muted" id="pool_shape"></p>
    <p class="muted" id="untimed_note"></p>
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
    <h2>账号池与统一入口</h2>
    <div id="pools"></div>
  </section>

  <section>
    <h2>出口池（IP 池）</h2>
    <p id="exit_filters"></p>
    <table id="exits"><thead><tr>
      <th>出口 ID</th><th>类型</th><th>代理地址</th><th>出口 IP</th><th>延迟</th>
      <th>状态</th><th>额度</th><th>地区</th><th>连续失败</th><th>软失败</th><th>隔离还剩</th><th>绑定账号</th><th>最近错误</th><th>操作</th>
    </tr></thead><tbody></tbody></table>
    <p class="muted" id="peer_ids"></p>
    <p class="muted" style="margin-top:8px">
      <b>清理垃圾出口</b>按这一页上的「可清理」计数，把这些出口<b>永久移出轮换</b>：
      隔离中的、不健康的、延迟超过上限的。偏慢但还在轮换里的也算。
      只是连续失败、还没隔离的（观察）不算。误清了用该行的「恢复」。
    </p>
  </section>

  <section>
    <h2>账号池</h2>
    <form onsubmit="return saveAccount(event)">
      <label>账号 ID<input id="f_id" placeholder="zen-01" required></label>
      <label>备注<input id="f_name" placeholder="空着保存会清空" title="和钥匙不一样：这里空着保存会把备注清掉。"></label>
      <label>上游
        <select id="f_upstream"></select>
      </label>
      <label>API Key<input id="f_key" placeholder="留空 = 不改动" autocomplete="off"></label>
      <label>出口
        <input id="f_proxy" placeholder="auto / direct / 出口ID" value="auto">
      </label>
      <label>权重<input id="f_weight" type="number" value="0" title="这个池轮流用账号，权重不参与挑选"> <span class="muted" id="weight_hint"></span></label>
      <label>自定义头（JSON）<input id="f_headers" placeholder="空着保存会清空" title="和钥匙不一样：这里空着保存会把自定义头清掉。钥匙留空才是不改。" autocomplete="off"></label>
      <label>启用
        <select id="f_enabled"><option value="true">是</option><option value="false">否</option></select>
      </label>
      <button class="primary" type="submit">保存账号</button>
    </form>
    <table id="accounts"><thead><tr>
      <th>账号</th><th>上游</th><th>Key</th><th>策略</th><th>出口</th><th>出口 IP</th><th>状态</th>
      <th>最近分类</th><th>本副本累计请求</th><th>本副本累计成功/请求</th><th title="每成功换一次绑定的出口记一笔。公网 IP 不一定变了">换出口</th><th>最近错误</th><th>操作</th>
    </tr></thead><tbody></tbody></table>
    <p class="muted" id="sticky_line" style="margin-top:8px"></p>
    <p class="muted">
      「编辑」把该账号回填到上面表单里，改完点保存即可更新（ID 本身也可以改，改了就是新增）。
      「策略」是账号的出口选择（Cred）：<code>auto</code> 从池子里挑并保持亲和，
      <code>direct</code> 直连，写出口 ID 则优先落到它。
      备注和自定义头空着保存都会清空。钥匙留空才是不改。
      「本副本累计成功/请求」的右边是这个副本开服至今的请求数，不是这一分钟，也不含另一个副本。尝试失败按每一次上游调用计，写在同一格下面：
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
const cache = { accounts: [], exits: [], exitFilter: 'all' };

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
    const accountsP = api('/accounts');
    const status = await api('/status');
    let exitList = cache.exits;
    let fetchedExits = false;
    if (!status.exits_fingerprint || status.exits_fingerprint !== cache.exitGen || !exitList) {
      const exits = await api('/proxies');
      exitList = exits.exits || [];
      fetchedExits = true;
      if (status.exits_fingerprint) cache.exitGen = status.exits_fingerprint;
    }
    const accounts = await accountsP;
    renderStat(status);
    renderFree(status);
    renderConfig(status);
    renderPools(status);
    fillUpstreams(status.upstreams || []);
    cache.poolUsesWeight = (status.pools || []).some(p => p.strategy === 'weighted');
    const weightHint = document.getElementById('weight_hint');
    const weightInput = document.getElementById('f_weight');
    if (weightHint) weightHint.textContent = cache.poolUsesWeight ? '按权重挑' : '不看权重';
    if (weightInput) weightInput.title = cache.poolUsesWeight ? '这个池按权重挑账号' : '这个池轮流用账号，权重不参与挑选';
    cache.failureThreshold = (status.health || {}).failure_threshold || 0;
    cache.maxLatencyMS = (status.health || {}).max_latency_ms || 0;
    cache.sessionPins = status.session_pins || {};
    document.getElementById('foot').innerHTML =
      '监听 <code>' + esc(status.listen || '') + '</code> · 上游 <code>' +
      esc((status.upstreams || []).join(', ')) + '</code> · 白名单 <code>' +
      esc((status.allowed_hosts || []).join(', ') || '（仅回环监听时可为空）') + '</code> · ' +
      replicaLine(status);
    const exitKey = JSON.stringify(exitList || []);
    const accountKey = JSON.stringify(accounts.accounts || []);
    const exitsSame = !fetchedExits || exitKey === cache.exitKey;
    const accountsSame = accountKey === cache.accountKey;
    if (fetchedExits) cache.exitKey = exitKey;
    cache.accountKey = accountKey;
    if (exitsSame) refreshTicks();
    else renderExits(exitList || []);
    if (!accountsSame) {
      cache.accounts = accounts.accounts || [];
      renderAccounts(cache.accounts);
    }
  } catch (e) { toast(e.message, true); }
}

function replicaLine(s) {
  const rows = s.replicas || [];
  if (!rows.length) return '状态文件 <code>' + esc(s.state_file || '未持久化') + '</code>';
  const line = rows.map(r => {
    const who = r.self ? '本副本' : '另一副本';
    const when = r.saved_at ? new Date(r.saved_at).toLocaleTimeString() : '—';
    return who + ' <code>' + esc(r.file || '') + '</code> 退役 ' + (r.retired || 0) +
      ' / ' + (r.exits || 0) + '，额度冷却 ' + (r.quota || 0) + '，写于 ' + esc(when);
  }).join(' · ') + accountReplicaLine(s);
  if (rows.length < 2) return line;
  const split = s.replica_split || {};
  const bits = [];
  if (split.retired_only_self) bits.push('本副本多退役 ' + split.retired_only_self);
  if (split.retired_only_peer) bits.push('另一副本多退役 ' + split.retired_only_peer);
  if (split.quota_only_self) bits.push('本副本多冷却 ' + split.quota_only_self);
  if (split.quota_only_peer) bits.push('另一副本多冷却 ' + split.quota_only_peer);
  if (!bits.length) return line;
  return line + ' · <span class="pill warn" title="两份状态文件不是同一份。这台页面只反映本副本">两边不一致：' +
    bits.join('，') + '</span>';
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
    ['够快', (s.exits_clear || 0) + ' / 可用 ' + s.exits_healthy, '这轮还没探到 ' + (s.exits_pending || 0) + '。探过但没有延迟 ' + (s.exits_unmeasured || 0) + '。观察 ' + (s.exits_watching || 0) + ' 已经在失败，偏慢 ' + (s.exits_slow || 0) + ' 超过延迟上限'],
    ['隔离中', (s.quarantine && s.quarantine.total) || 0],
    ['偏慢', s.exits_slow || 0, '延迟超过上限，挑选时靠后，没有退休'],
    ['可走免费', s.exits_gate || 0, '免费模型现在会挑这些出口，过一会儿这次准入就失效'],
    ['额度冷却', (s.quota && s.quota.count) || 0, '上游说过这些出口的额度用完了，到点自己回来。这不是出口坏了'],
    ['地区限制', (s.region && s.region.count) || 0, '一个出口加一个模型被地区拒绝。这个出口还能服务别的模型'],
    ['熔断', (s.outage_breaker && s.outage_breaker.open) ? ('开 · ' + (s.outage_breaker.consecutive_failures || 0) + '/' + (s.outage_breaker.threshold || 0)) : '关', '连续失败到阈值就打开，打开后不再因为大面积拨号失败而退休出口'],
    ['退休出口', s.exits_retired],
    ['本副本累计请求', s.requests || 0, '只算这个副本，从这些账号记数开始算，不是这一分钟'],
    ['本副本累计成功', (s.success || 0) + ' / ' + (s.requests || 0), '跟上面同一段，不含另一个副本。这一分钟成不成功，看账号行的最近分类'],
    ['尝试失败', (s.failures || 0) + ' 次', '一次请求换出口会记多次，不是失败的请求数'],
    ['换出口', (s.rotations || 0) + ' 次', '失败后自动改绑定记一笔，不是按钮点出来的。公网 IP 不一定变了'],
    ['在线', uptimeWord(s.uptime_seconds)],
  ];
  document.getElementById('stat').innerHTML = cards.map(c =>
    '<div class="card"' + (c[2] ? ' title="' + esc(c[2]) + '"' : '') + '><div class="k">' + esc(c[0]) + '</div><div class="v">' + esc(c[1]) + '</div></div>').join('');
  const q = s.quarantine || {};
  const bits = ['超时 ' + (q.timeout || 0), '连不上 ' + (q.unreachable || 0), 'TLS ' + (q.tls || 0)];
  if (q.other) bits.push('其他 ' + q.other);
  let shape = '隔离原因：' + bits.join(' · ') + '。观察 ' + (s.exits_watching || 0) +
    ' 行上写的是观察，不写健康。偏慢 ' + (s.exits_slow || 0) +
    ' 行上仍写健康，旁边另标偏慢。';
  if (s.exits_pending) {
    shape = '这轮还没探到 ' + s.exits_pending + ' 个。探活走完之前，够快会偏小，不是这些出口变慢了。' + shape;
  }
  if ((s.failures || 0) > (s.requests || 0)) {
    shape += '尝试失败 ' + s.failures + ' 次，比请求 ' + (s.requests || 0) + ' 个多。一次请求换出口会记多次，不是失败的请求数。';
  }
  if ((s.rotations || 0) > (s.requests || 0)) {
    shape += '换出口 ' + s.rotations + ' 次，比请求 ' + (s.requests || 0) + ' 个多。这是失败后自动改绑定，不是按钮点出来的，公网 IP 也不一定变了。';
  }
  const peerReq = (s.account_replicas || []).reduce((n, r) => n + (r.self ? 0 : (r.requests || 0)), 0);
  if (peerReq) shape += '另一副本另有请求 ' + peerReq + ' 个。上面的累计只算本副本。';
  document.getElementById('pool_shape').textContent = shape;
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
    row('retire_after（普通连续失败才用；正在被免费准入的不退休，0=关闭）', h.retire_after) +
    row('tls_poison_retire_after（证书类失败，探活成功也不清零）', h.tls_poison_retire_after) +
    '</tbody></table>' +
    '<p class="muted" style="margin-top:8px">这里只展示、不修改：要调参数改 YAML 后重启生效。</p>';
}

function renderPools(s) {
  const strat = {
    'round-robin': '轮流用账号',
    'weighted': '按权重挑账号',
  };
  const limit = {
    'rotate-exit': '限流时只换出口',
    'rotate-account': '限流时只换账号',
    'rotate-both': '限流时先换出口，再换账号',
  };
  const rows = (s.pools || []).map(p => {
    const who = (p.accounts || []).join(', ') || '没有账号';
    const named = (p.models || []).length;
    const models = p.wildcard && !named ? '未点名的模型也走这个池'
      : p.wildcard ? ('列出 ' + named + ' 个，其余也放行')
      : ('只放行 ' + ((p.models || []).join(', ') || '空名单'));
    return '<tr><td><code>' + esc(p.id) + '</code></td><td>' + esc(who) + '</td><td>' +
      esc(strat[p.strategy] || p.strategy || '—') + '</td><td>' +
      esc(limit[p.on_rate_limit] || p.on_rate_limit || '—') + '</td><td>' + esc(models) + '</td></tr>';
  }).join('') || '<tr><td colspan="5" class="muted">没有池子</td></tr>';
  const gw = s.gateway || {};
  const gate = gw.enabled
    ? ('<p>统一入口开着' + (gw.require_key ? '，必须带钥匙' : '') + '。钥匙：' +
        ((gw.keys || []).map(k => esc(k.id || k.name || '未命名') + ' → ' + esc(k.pool || '—')).join('，') || '没有钥匙') +
        '。</p>')
    : '<p class="muted">统一入口关着。下游现在还是按账号路径进来，不是一把钥匙挑整个池。</p>';
  document.getElementById('pools').innerHTML =
    '<table><thead><tr><th>池</th><th>账号</th><th>怎么挑</th><th>限流时</th><th>模型</th></tr></thead><tbody>' +
    rows + '</tbody></table>' + gate;
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

function exitInWatch(e) {
  const q = e.quarantined_until ? new Date(e.quarantined_until) : null;
  return !e.retired && e.healthy && e.consecutive_failures > 0 && !(q && q > new Date());
}
function exitInNap(e) {
  const q = e.quarantined_until ? new Date(e.quarantined_until) : null;
  return !e.retired && q && q > new Date();
}
function exitInGate(e) {
  return e.gate_pass_until && new Date(e.gate_pass_until) > new Date();
}
function exitFast(e) {
  return !e.retired && e.healthy && !exitInNap(e) && !e.consecutive_failures && !e.slow && !!e.latency_ms;
}
function exitNoLatency(e) {
  return !e.retired && e.healthy && !exitInNap(e) && !e.consecutive_failures && !e.slow && !e.latency_ms;
}
function exitPending(e) {
  return exitNoLatency(e) && !e.last_check;
}
function exitUntimed(e) {
  return exitNoLatency(e) && !!e.last_check;
}
function exitMatches(e, f) {
  if (f === 'fast') return exitFast(e);
  if (f === 'pending') return exitPending(e);
  if (f === 'untimed') return exitUntimed(e);
  if (f === 'slow') return !!e.slow;
  if (f === 'watch') return exitInWatch(e);
  if (f === 'nap') return exitInNap(e);
  if (f === 'prune') return !e.retired && !!e.prunable;
  if (f === 'split') return !!(e.peer_retired || e.peer_keeps || e.peer_quota || e.peer_quota_here);
  if (f === 'gate') return exitInGate(e);
  return true;
}
function exitFilterButtons(list) {
  const el = document.getElementById('exit_filters');
  if (!el) return;
  const n = {
    all: list.length,
    fast: list.filter(exitFast).length,
    pending: list.filter(exitPending).length,
    untimed: list.filter(exitUntimed).length,
    slow: list.filter(e => e.slow).length,
    watch: list.filter(exitInWatch).length,
    nap: list.filter(exitInNap).length,
    prune: list.filter(e => e.prunable && !e.retired).length,
    split: list.filter(e => e.peer_retired || e.peer_keeps || e.peer_quota || e.peer_quota_here).length,
    gate: list.filter(exitInGate).length,
  };
  const cur = cache.exitFilter || 'all';
  const btn = (id, label) => '<button type="button" onclick="setExitFilter(\'' + id + '\')"' +
    (cur === id ? ' style="font-weight:bold"' : '') + '>' + label + ' ' + n[id] + '</button>';
  el.innerHTML = [btn('all', '全部'), btn('fast', '够快'), btn('pending', '还没探到'), btn('untimed', '还没量过'), btn('slow', '偏慢'), btn('watch', '观察'), btn('nap', '隔离'),
    btn('prune', '可清理'), btn('split', '两边不一致'), btn('gate', '可走免费')].join(' ');
}
function setExitFilter(id) {
  cache.exitFilter = id;
  if (cache.exits) renderExits(cache.exits);
}
function renderExits(list) {
  cache.exits = list;
  if (cache.accounts) stickyLine(cache.accounts);
  const note = document.getElementById('untimed_note');
  if (note) {
    const pending = list.filter(exitPending).length;
    const untimed = list.filter(exitUntimed).length;
    note.textContent = pending
      ? ('这轮还没探到 ' + pending + ' 个。探活走完之前，不够快不是因为它们慢。')
      : (untimed ? ('有 ' + untimed + ' 个还没量过延迟，和上面的够快不是同一批。格子里写着「还没量过」，不是坏了。') : '');
  }
  exitFilterButtons(list);
  const peer = document.getElementById('peer_ids');
  if (peer) {
    const there = list.filter(e => e.peer_retired).length;
    const keeps = list.filter(e => e.peer_keeps).length;
    const quota = list.filter(e => e.peer_quota).length;
    const here = list.filter(e => e.peer_quota_here).length;
    const bits = [];
    if (there) bits.push('另一副本已退役 ' + there + ' 个');
    if (keeps) bits.push('另一副本还在用 ' + keeps + ' 个');
    if (quota) bits.push('另一副本在冷却、这边没有 ' + quota + ' 个');
    if (here) bits.push('这边在冷却、另一边没有 ' + here + ' 个');
    peer.textContent = bits.length
      ? bits.join('，') + '。用上面的「两边不一致」看具体哪几条，这里不再把出口 ID 铺开。'
      : '';
  }
  cache.pruneCount = list.filter(e => e.prunable && !e.retired).length;
  const btn = document.getElementById('prune_btn');
  if (btn) btn.textContent = '清理垃圾出口（' + cache.pruneCount + '）';
  const tb = document.querySelector('#exits tbody');
  if (!list.length) { paintRows(tb, '<tr><td colspan="14" class="muted">未配置出口（proxies.resin / proxies.static 均为空）</td></tr>'); return; }
  const shown = list.filter(e => exitMatches(e, cache.exitFilter || 'all'));
  if (!shown.length) { paintRows(tb, '<tr><td colspan="14" class="muted">这个筛选下没有出口</td></tr>'); return; }
  paintRows(tb, shown.map(e => {
    const q = e.quarantined_until ? new Date(e.quarantined_until) : null;
    let state;
    if (e.retired) {
      state = '<span class="pill bad" title="' + esc(retireWord(e.retired_reason || '')) + '">已退休</span>';
    } else if (q && q > new Date()) {
      const why = quarantineWord(e.quarantine_class);
      state = '<span class="pill bad" title="' + esc(why ? ('隔离原因：' + why) : '隔离中，原因还没探到') + '">隔离中' +
        (why ? ' · ' + esc(why) : '') + '</span>';
    } else if (e.healthy && e.consecutive_failures > 0) {
      const why = shortReason(e.last_error);
      const need = (cache.failureThreshold || 0) - e.consecutive_failures;
      const left = need > 0 ? ('再 ' + need + ' 次就隔离') : '再失败就隔离';
      state = '<span class="pill warn" title="还在轮换里。' + esc(left) + '">观察 · ' +
        (why && why !== '—' ? esc(why) + ' ' : '') + e.consecutive_failures + ' 次，' + left + '</span>';
    } else if (e.healthy) {
      state = '<span class="pill ok">健康</span>';
    } else {
      state = '<span class="pill warn">降级</span>';
    }
    if (e.slow) {
      const extra = overCap(e.latency_ms);
      state += ' <span class="pill warn" title="延迟超过上限，挑选时靠后，没有退休">偏慢' +
        (extra ? ' · ' + extra : '') + '</span>';
    }
    if (e.gate_pass_until && new Date(e.gate_pass_until) > new Date()) {
      state += ' <span class="pill ok" title="免费模型现在会挑它。分是挑中的机会，不是延迟。准入是这次进程里通过的次数">可走免费 · 分 ' +
        (e.gate_score || 0) + ' · 准入 ' + (e.gate_passes || 0) +
        (e.gate_drops ? ' · 丢流 ' + e.gate_drops : '') +
        ' · <span class="gate-tick" data-until="' + esc(e.gate_pass_until) + '">还剩 ' + timeLeft(new Date(e.gate_pass_until)) + '</span></span>';
    }
    if (!e.retired && e.prunable) state += ' <span class="pill warn" title="下一次清理会把它移出轮换">可清理</span>';
    if (e.peer_retired) state += ' <span class="pill warn" title="' + esc(peerRetiredTitle(e)) + '">另一副本已退役</span>';
    if (e.peer_keeps) state += ' <span class="pill warn" title="这边已经退役。另一边还没退役，挑选时会把它排在干净出口后面">另一副本还在用</span>';
    if (e.peer_quota) state += ' <span class="pill warn" title="另一边这个出口在额度冷却，这边没有">另一副本在冷却</span>';
    if (e.peer_quota_here) state += ' <span class="pill warn" title="这边这个出口在额度冷却，另一边没有">另一边没冷却</span>';
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
      '<td><code title="连这个代理的地址，不是流量出去时的公网 IP">' + esc(e.addr) + '</code></td>' +
      '<td>' + exitIPCell(e) + '</td>' +
      '<td>' + latencyCell(e) + '</td>' +
      '<td>' + state + '</td>' +
      '<td class="quota-tick"' + (quotaLeft(e) ? ' data-until="' + esc(e.quota_until) + '" data-reason="' + esc(e.quota_reason || '') + '"' : '') + '>' + quotaCell(e) + '</td>' +
      '<td>' + regionCell(e) + '</td>' +
      '<td>' + e.consecutive_failures + '</td>' +
      '<td class="muted" title="上游返回了响应（429/5xx）：出口本身没坏，只换了绑定">' + (e.soft_failures || 0) + '</td>' +
      '<td class="muted nap-tick"' + (q && q > new Date() ? ' data-until="' + esc(e.quarantined_until) + '"' : '') + '>' + (e.retired ? '—' : napLeft(q)) + '</td>' +
      '<td class="muted">' + esc((e.bound_accounts || []).join(', ') || '—') + '</td>' +
      '<td class="muted">' + esc(shortReason(e.last_error)) + '</td>' +
      '<td>' + act.join(' ') + '</td>' +
    '</tr>';
  }).join(''));
}
function refreshTicks() {
  document.querySelectorAll('#exits td.nap-tick[data-until]').forEach(td => {
    const html = napLeft(new Date(td.getAttribute('data-until')));
    if (td.getAttribute('data-shown') === html) return;
    td.innerHTML = html;
    td.setAttribute('data-shown', html);
  });
  document.querySelectorAll('#exits .gate-tick[data-until]').forEach(el => {
    const until = new Date(el.getAttribute('data-until'));
    const html = until > new Date() ? ('还剩 ' + timeLeft(until)) : '这次准入已过';
    if (el.getAttribute('data-shown') === html) return;
    el.textContent = html;
    el.setAttribute('data-shown', html);
  });
  document.querySelectorAll('#exits td.quota-tick[data-until]').forEach(td => {
    const html = quotaCell({ quota_until: td.getAttribute('data-until'), quota_reason: td.getAttribute('data-reason') || '' });
    if (td.getAttribute('data-shown') === html) return;
    td.innerHTML = html;
    td.setAttribute('data-shown', html);
  });
}
function paintRows(tb, html) {
  const y = window.scrollY;
  tb.innerHTML = html;
  if (window.scrollY !== y) window.scrollTo(0, y);
}
function openDetailIds() {
  const open = {};
  document.querySelectorAll('#accounts tr.detail').forEach(el => {
    if (el.style.display !== 'none') open[el.id] = true;
  });
  return open;
}

// quotaLeft returns the remaining quota cooldown, or null.
//
// Kept separate from health on purpose: an exit the upstream has spent is
// working perfectly, it just will not be served. Showing it as unwell would send
// an operator to debug a healthy proxy.
function latencyCell(e) {
  if (e.latency_ms) return e.latency_ms + ' ms';
  if (!e.retired && e.healthy) {
    return '<span class="muted" title="这次进程还没有一次成功计时">还没量过</span>';
  }
  return '—';
}
function overCap(ms) {
  const cap = cache.maxLatencyMS || 0;
  if (!ms || !cap || ms <= cap) return '';
  const extra = ms - cap;
  if (extra < 1000) return '超出 ' + extra + ' ms';
  return '超出 ' + (Math.round(extra / 100) / 10) + ' 秒';
}
function uptimeWord(sec) {
  sec = Math.floor(sec || 0);
  if (sec < 90) return sec + ' 秒';
  const min = Math.round(sec / 60);
  if (min < 90) return min + ' 分钟';
  const hr = Math.round(sec / 3600);
  if (hr < 48) return hr + ' 小时';
  return Math.round(sec / 86400) + ' 天';
}
function timeLeft(t) {
  const mins = Math.round((t - new Date()) / 60000);
  if (mins >= 90) return Math.round(mins / 60) + ' 小时';
  if (mins < 1) return '不到 1 分钟';
  return mins + ' 分钟';
}
function napLeft(q) {
  if (!q || q <= new Date()) return '—';
  return '<span title="到点自己回来：' + esc(q.toLocaleString()) + '">还剩 ' + timeLeft(q) + '</span>';
}
function quotaLeft(e) {
  if (!e.quota_until) return null;
  const t = new Date(e.quota_until);
  return t > new Date() ? t : null;
}

function quotaCell(e) {
  const t = quotaLeft(e);
  if (!t) return '<span class="muted">—</span>';
  const left = timeLeft(t);
  const known = CLASS_ZH[e.quota_reason];
  const why = known ? known[1] : '';
  return '<span class="pill warn" title="上游说的重置时刻 ' + esc(t.toLocaleString()) + '">' +
    (why ? esc(why) + ' · ' : '') + '还剩 ' + left + '</span>';
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
function boundFlags(a) {
  const bits = [];
  if (a.bound_slow) bits.push('<span class="pill warn" title="绑定的出口延迟超过上限，挑选时靠后">偏慢</span>');
  if (a.bound_gate) bits.push('<span class="pill ok" title="绑定的出口正在被免费模型使用">可走免费</span>');
  if (a.bound_pending) bits.push('<span class="pill warn" title="这轮探活还没走到这个出口。不是变慢了">这轮还没探到</span>');
  if (a.bound_untimed) bits.push('<span class="pill warn" title="探过了，但没有一次成功计时。不是坏了">还没量过</span>');
  if (a.bound_prunable) bits.push('<span class="pill warn" title="下一次清理会把这个账号正在用的出口移出轮换">可清理</span>');
  if (a.bound_quarantine) bits.push('<span class="pill bad" title="绑定的出口在隔离，现在不参与轮换">隔离中</span>');
  if (a.bound_peer_retired) bits.push('<span class="pill warn" title="绑定的出口在另一副本已退役，这边排在干净出口后面">另一副本已退役</span>');
  if (a.bound_share) bits.push('<span class="pill warn" title="可走免费的出口很少，共用一个是正常的，不会自动拆开">和另外 ' + a.bound_share + ' 个账号共用</span>');
  return bits.join(' ');
}
function accountIPCell(a) {
  if (!a.bound_exit) return '<code>—</code>';
  return exitIPCell({exit_ip: a.bound_exit_ip, exit_ip_note: a.bound_exit_ip_note});
}
function exitIPCell(e) {
  if (!e.exit_ip && !e.exit_ip_note) {
    return '<code title="还没问过出口地址">还没问过</code>';
  }
  const ip = e.exit_ip || '—';
  const note = exitIPWord(e.exit_ip_note);
  const title = note ? ' title="' + esc(note) + '"' : '';
  return '<code' + title + '>' + esc(ip) + '</code>';
}
function peerRetiredTitle(e) {
  const word = quarantineWord(e.peer_retired_class);
  const why = word ? '另一边记下的是' + word + '。' : '';
  if (e.quarantined_until) return why + '这边隔离中，现在不参与轮换';
  if (e.healthy) return why + '这边还健康，但排在干净出口后面';
  return why + '这边不健康，排在干净出口后面';
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
function retireWord(msg) {
  const raw = scrubReason(msg);
  let m = /^tls poison (\d+)$/.exec(raw);
  if (m) return 'TLS 累计 ' + m[1] + ' 次';
  m = /^consecutive (\d+)$/.exec(raw);
  if (m) return '连续失败 ' + m[1] + ' 次';
  return shortReason(raw);
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

function proxyCell(a) {
  const mode = a.proxy || 'auto';
  if (mode === 'auto') {
    const stuck = a.bound_exit
      ? ' <span class="muted" title="它还健康、还能服务当前模型时不换。不健康才重新挑">粘住</span>'
      : '';
    return '<code title="auto 不是每次重挑">' + esc(mode) + '</code>' + stuck;
  }
  if (mode === 'direct' || mode === 'none') {
    return '<code title="不走代理，直接连上游">' + esc(mode) + '</code>';
  }
  return '<code title="优先用这个出口。它不健康时才回池子里重挑">' + esc(mode) + '</code>';
}
function boundWord(a) {
  if (a.bound_exit) return a.bound_exit;
  const mode = a.proxy || 'auto';
  if (mode === 'direct' || mode === 'none') return '直连';
  return '还没粘住';
}
function waitVersusProbe(a) {
  const st = a.stats || {};
  const total = st.last_duration_ms || 0;
  const probe = a.bound_latency_ms || 0;
  if (total < 10000 || probe < 1 || total < probe * 3) return '';
  return '，探活 ' + durationWord(probe) + '，多出来的不是这条出口在拨号';
}
function headerNote(st) {
  const total = st.last_duration_ms || 0;
  const head = st.last_header_ms || 0;
  if (head < 1 || total < 10000 || head > total) return '';
  if (total - head >= 5000) return '，响应头 ' + durationWord(head) + '，剩下是在往下传';
  if (total >= 45000) return '，超过 45 秒还是结束才看到字，要的是整包';
  return '，第一个字也是这时候';
}
function durationWord(ms) {
  const s = ms / 1000;
  if (s < 10) return s.toFixed(1) + ' 秒';
  return Math.round(s) + ' 秒';
}
function renderAccounts(list) {
  const tb = document.querySelector('#accounts tbody');
  const open = openDetailIds();
  if (!list.length) { paintRows(tb, '<tr><td colspan="13" class="muted">还没有账号，用上面的表单加一个</td></tr>'); return; }
  paintRows(tb, list.map(a => {
    const st = a.stats || {};
    const detailId = 'detail_' + a.id;
    const rows = [];
    rows.push('<tr>' +
      '<td><code>' + esc(a.id) + '</code>' + (a.name ? ' <span class="muted">' + esc(a.name) + '</span>' : '') +
        (a.session_pins ? '<div class="muted" title="这些对话下次说话还在各自的出口上。不在免费出口上的对话会改钉到一条免费出口。账号已经坐在免费出口上时，座位不动。点换出口会把它们一起改到新出口">' + a.session_pins + ' 个对话钉在 ' + a.session_exits + ' 个出口' +
          (a.session_not_gate ? '，其中 ' + a.session_not_gate + ' 个不在可走免费的出口上，免费请求会改钉，座位不动' : '') + '</div>' : '') +
        (a.has_key ? '' : ' <span class="pill warn" title="没有 key，这个账号发出去的请求上游会直接拒收">无 key</span>') + '</td>' +
      '<td>' + esc(a.upstream) + '</td>' +
      '<td><code>' + esc(a.key_hint || '未设置') + '</code></td>' +
      '<td>' + proxyCell(a) + '</td>' +
      '<td><code>' + esc(boundWord(a)) + '</code> ' + boundFlags(a) + '</td>' +
      '<td>' + accountIPCell(a) + '</td>' +
      '<td>' + (a.enabled ? '<span class="pill ok">启用</span>' : '<span class="pill bad">禁用</span>') + '</td>' +
      '<td>' + classPill(st.last_class) +
        (st.last_duration_ms ? '<div class="muted" title="客户端等完这一次的时间。响应头是第一个字什么时候出来。剩下是在往下传，不是又换了出口">最近一次 ' + durationWord(st.last_duration_ms) +
          (st.last_attempts > 1 ? '，试了 ' + st.last_attempts + ' 次' : '') +
          (st.last_attempts === 1 && st.last_duration_ms >= 10000 ? '，一次就通' : '') +
          (!st.last_attempts ? '，次数没记上' : '') +
          headerNote(st) +
          (st.last_duration_ms >= 10000 && st.last_attempts >= 1 && !(st.last_header_ms > 0) ? '，第一个字的时间没记上' : '') +
          waitVersusProbe(a) +
          '</div>' : '') +
        '</td>' +
      '<td>' + (st.requests || 0) + '</td>' +
      '<td title="左边是完成的请求，右边是请求总数。下面的尝试失败按换出口计，不能拿来当除数">' +
        (st.success || 0) + ' / ' + (st.requests || 0) +
        '<div class="muted">尝试失败 ' + (st.failures || 0) + '</div></td>' +
      '<td>' + (st.rotations || 0) + '</td>' +
      '<td class="muted">' + esc(shortReason(st.last_error)) + '</td>' +
      '<td style="white-space:normal">' +
        '<button onclick="fillAccount(\'' + esc(a.id) + '\')">编辑</button> ' +
        '<button onclick="toggleAccount(\'' + esc(a.id) + '\')">' + (a.enabled ? '禁用' : '启用') + '</button> ' +
        '<button onclick="rotate(\'' + esc(a.id) + '\')" title="把这个账号上的对话钉一起改到新出口。公网 IP 不一定变了">换出口</button> ' +
        '<button onclick="toggleDetail(\'' + esc(a.id) + '\')">详情</button> ' +
        '<button class="danger" onclick="del(\'' + esc(a.id) + '\')">删</button>' +
      '</td>' +
    '</tr>');
    const hdrs = a.headers || a.Headers;
    rows.push('<tr class="detail" id="' + esc(detailId) + '" style="display:' + (open[esc(detailId)] ? '' : 'none') + '"><td colspan="13">' +
      '权重 <code>' + esc(a.weight || 0) + '</code>' +
        (cache.poolUsesWeight ? '' : ' <span class="muted">不看权重</span>') + ' · ' +
      '自定义头 <code>' + esc(hdrs && Object.keys(hdrs).length ? JSON.stringify(hdrs) : '无') + '</code> · ' +
      '绑定出口 IP ' + accountIPCell(a) + ' · ' +
      '成功 ' + (st.success || 0) + ' · 上游调用 ' + (st.upstream_calls || 0) + ' · ' +
      '最近使用 ' + esc(fmtTime(st.last_used)) + ' · 最近成功 ' + esc(fmtTime(st.last_success)) +
    '</td></tr>');
    return rows.join('');
  }).join(''));
  stickyLine(list);
}
function stickyLine(list) {
  const el = document.getElementById('sticky_line');
  if (!el) return;
  const counts = {};
  let bound = 0;
  for (const a of list) {
    if (!a.bound_exit) continue;
    bound++;
    counts[a.bound_exit] = (counts[a.bound_exit] || 0) + 1;
  }
  if (!bound) { el.textContent = ''; return; }
  const used = Object.keys(counts).length;
  let worst = 0;
  let least = 0;
  for (const n of Object.values(counts)) {
    if (n > worst) worst = n;
    if (!least || n < least) least = n;
  }
  const gateExits = (cache.exits || []).filter(exitInGate);
  const gate = gateExits.length;
  let gateEmpty = 0;
  for (const e of gateExits) if (!counts[e.id]) gateEmpty++;
  let text = bound + ' 个账号粘在 ' + used + ' 个出口上';
  if (worst > 1) {
    text += '。最挤的一个上面有 ' + worst + ' 个账号';
    if (least && least < worst) text += '，最空的一个上面有 ' + least + ' 个';
    text += '。不会自动拆开';
  }
  if (gateEmpty) {
    text += '。可走免费的出口有 ' + gate + ' 个，其中 ' + gateEmpty + ' 个上面没有账号';
    let occupiedScore = 0;
    for (const e of gateExits) if (counts[e.id]) occupiedScore = Math.max(occupiedScore, e.gate_score || 0);
    let emptyNotWorse = 0;
    for (const e of gateExits) if (!counts[e.id] && (e.gate_score || 0) >= occupiedScore) emptyNotWorse++;
    if (emptyNotWorse) text += '。空着的里面有 ' + emptyNotWorse + ' 条分数并不更低，账号是粘住的，不是因为它更差';
    let satLatency = 0;
    for (const e of gateExits) if (counts[e.id] && (e.latency_ms || 0) > satLatency) satLatency = e.latency_ms;
    let fasterEmpty = 0;
    let fasterMs = 0;
    for (const e of gateExits) {
      const lat = e.latency_ms || 0;
      if (counts[e.id] || lat < 1 || satLatency < 1 || lat >= satLatency) continue;
      if (satLatency - lat < 500 && lat * 2 > satLatency) continue;
      fasterEmpty++;
      if (!fasterMs || lat < fasterMs) fasterMs = lat;
    }
    if (fasterEmpty) text += '。空着的里面有 ' + fasterEmpty + ' 条探活更短，最短 ' + durationWord(fasterMs) + '，坐着的是 ' + durationWord(satLatency);
  }
  else if (gate > used) text += '。可走免费的出口有 ' + gate + ' 个，比粘住的多';
  else if (gate) text += '。可走免费的出口有 ' + gate + ' 个';
  const pins = cache.sessionPins || {};
  if (pins.live > 0) {
    text += '。另有 ' + pins.live + ' 个对话钉在 ' + pins.exits + ' 个出口上';
    if (pins.accounts_on_several_exits) text += '，其中 ' + pins.accounts_on_several_exits + ' 个账号被钉在不只一个出口';
    text += '。点换出口会把这个账号的对话钉一起改到新出口';
  }
  el.textContent = text;
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
        const moved = r.pins_moved ? '，并把 ' + r.pins_moved + ' 个对话钉改到同一出口' : '';
        toast(id + ' 已切换到 ' + boundWord(r.account) + moved); await load(); }
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
  const n = cache.pruneCount || 0;
  if (!n) { toast('现在没有可清理的出口'); return; }
  if (!confirm('把 ' + n + ' 个带「可清理」的出口永久移出轮换？\n偏慢但还在用的也算。误清了可以用该行的「恢复」放回来。')) return;
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
