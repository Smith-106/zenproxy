# 把 zenproxy 接进 new-api

> 本文所有结论都在你的服务器上实测过，不是照抄文档：
> new-api 镜像实际是 QuantumNous 主线 **`v1.0.0-rc.41`**（镜像名虽为 `calciumion/new-api:latest`），
> 运行于 compose 项目 `new-api`（`/opt/new-api/docker-compose.yml`），容器网络 `new-api_default`（172.18.0.0/16），
> 端口发布在 `127.0.0.1:3000`；Resin 容器在网络 `resin_default`（172.30.0.0/16），别名 `resin`，容器内 2260 全接口可达。

---

## 一、先选接入点：两个都能用，但只有一个能真正"报错换 IP"

new-api 的渠道高级配置里有两个位置可以指向 zenproxy。它们**不是等价的**：

| | 填在**Base URL** | 填在**代理**(proxy) |
| :--- | :--- | :--- |
| 填什么 | `http://zenproxy:9000/zen-01` | `http://zen-01:x@zenproxy:9000` |
| zenproxy 看到什么 | 上游的**明文响应**（403 / 429 / 5xx 全部可见） | 一条**加密的 TLS 隧道**（`CONNECT` 之后全是密文） |
| 上游报错能否自动换 IP | ✅ 能，按状态码精确判定后换出口重试 | ❌ 不能，只有"连不上"才触发换出口 |
| 适合 | new-api / LiteLLM 这类走 HTTP 的网关 | 只能用代理字段、或需要原样透传的客户端 |

**结论：接 new-api 请填 Base URL。**

原因很具体：new-api 请求的是 `https://opencode.ai/...`，Go 的 `net/http` 对 https 目标一律先发 `CONNECT`
建隧道。填"代理"字段时，zenproxy 拿到的是一条已经加密的字节流——它**看不见里面的 403**，自然无从判断该不该换 IP。
填 Base URL 时 TLS 由 zenproxy 终止，响应是明文，`429` / `5xx` 就能被识别并触发换出口重试。

两种模式 zenproxy 都实现了，共用同一个监听端口（靠请求形态自动区分），所以上面两个填法填哪个都能通，
只是"代理"模式下你只得到可用性容灾，得不到错误驱动的换 IP。

---

## 二、拓扑

```
                    ┌──────────────────────────────┐
   浏览器 / 客户端 ──▶│  new-api  (172.18.0.3:3000)  │
                    │  渠道 Base URL =             │
                    │  http://zenproxy:9000/zen-01 │
                    └───────────────┬──────────────┘
                                    │  new-api_default 网络
                                    ▼
                    ┌──────────────────────────────┐
                    │  zenproxy  127.0.0.1:9000     │
                    │  · 账号池：每个账号绑一个出口  │
                    │  · 失败分类 → 换出口 → 重试    │
                    │  · 粘性亲和：同一会话不换 IP   │
                    └───────────────┬──────────────┘
                                    │  resin_default 网络
                                    ▼
                    ┌──────────────────────────────┐
                    │  resin  (resin:2260)         │
                    │  自建出口节点池                │
                    └──────────────────────────────┘
```

zenproxy 容器**同时接入 `new-api_default` 和 `resin_default` 两个已有网络**，
所以 new-api 能按名字找到它、它也能按名字找到 Resin —— 两边的现有 compose 文件一个都不用改。

---

## 三、部署

见 `deploy/README.md`，三步：起容器 → 填 `config.yaml` → 在控制台加账号。

最短路径：

```bash
cd /opt/zenproxy
sudo docker compose up -d
curl -s http://127.0.0.1:9000/healthz          # {"status":"ok"}
```

---

## 四、加账号

控制台：`http://127.0.0.1:9000/admin/?token=<你的 admin_token>`

或走接口：

```bash
curl -sS -X POST http://127.0.0.1:9000/admin/api/accounts \
  -H "X-Admin-Token: $ZEN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
        "id": "zen-01",
        "name": "示例账号",
        "upstream": "zen",
        "api_key": "sk-...",
        "proxy": "auto",
        "enabled": true
      }'
```

`proxy` 字段的取值：

| 值 | 含义 |
| :--- | :--- |
| `auto` | 从出口池里挑一个，并保持粘性；报错时自动换下一个。**推荐** |
| `direct` | 本机直连，不走出口池 |
| `<出口ID>` | 把这个出口作为**首选**。它健康就一直用；一旦被隔离就自动退回池子里挑，不会把你钉死在一个坏出口上 |

> 密钥可以留空：留空表示"不改动已存的密钥"。响应里只会返回类似 `sk-a…rect` 的提示串，**永远不回显完整密钥**。

---

## 五、建渠道

new-api → 渠道 → 新建（或编辑现有渠道）：

| new-api 字段 | 填什么 |
| :--- | :--- |
| 类型 | OpenAI 兼容（渠道表 `type = 1`） |
| Base URL | `http://zenproxy:9000/zen-01` ← **账号 ID 就是路径第一段** |
| 密钥 | **随便填一个占位串** |
| 模型 | 按需勾选；zenproxy 会把 `{Base URL}/v1/...` 折叠成上游的 `/zen/v1/...`，不用你手动对齐 |
| 代理 | 留空（不要同时填 —— 见第一节） |

密钥为什么可以随便填：zenproxy 会用**账号自己那把 key** 覆盖掉 `Authorization` 头，
所以 new-api 里那把只是占位。实测验证：客户端发 `Bearer sk-CLIENT-SHOULD-BE-REPLACED`，
上游收到的是 `Bearer sk-account-key-direct`。

模型名如果和上游不一致，用渠道的**模型映射**转换；zenproxy 不改写请求体。

---

## 六、验证

### 1. 确认链路通（不消耗额度）

```bash
curl -s http://127.0.0.1:9000/healthz
# {"status":"ok"}

curl -s -H "X-Admin-Token: $ZEN_ADMIN_TOKEN" \
  http://127.0.0.1:9000/admin/api/status
# 关注 exits / exits_healthy：健康出口数不能是 0
```

### 2. 确认出口池真的在做探活

```bash
curl -s -H "X-Admin-Token: $ZEN_ADMIN_TOKEN" \
  http://127.0.0.1:9000/admin/api/proxies
# 每个出口的 exit_ip 字段就是它的真实出口 IP，用于确认各出口确实不同
```

### 3. 确认"报错自动换 IP"真的生效

这是最该验的一条。做法：把某个账号强行绑到一个**死出口**上，然后发一次请求，
观察统计里 `rotations` 变成 1 而请求最终返回 200。

```bash
# 先看池子里有哪些出口 ID
curl -s -H "X-Admin-Token: $ZEN_ADMIN_TOKEN" \
  http://127.0.0.1:9000/admin/api/proxies | grep -o '"id":"[^"]*"'

# 把账号钉到一个坏出口（用一个没人监听的地址当出口，或直接拔掉某个出口）
curl -sS -X POST http://127.0.0.1:9000/admin/api/accounts/zen-01 \
  -H "X-Admin-Token: $ZEN_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"id":"zen-01","upstream":"zen","proxy":"<坏出口ID>","enabled":true}'

# 发一次请求
curl -s -o /dev/null -w '%{http_code}\n' \
  -X POST http://127.0.0.1:9000/zen-01/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"...","messages":[{"role":"user","content":"hi"}]}'
# 期望 200

curl -s -H "X-Admin-Token: $ZEN_ADMIN_TOKEN" \
  http://127.0.0.1:9000/admin/api/accounts | grep -A3 rotations
# 期望 rotations 为 1，bound_exit 已经变成另一个出口
```

本机已用真实 socket 跑过同样的场景，日志长这样：

```
level=WARN msg="upstream transport failure" account=acc-pinned-dead exit=static-01 attempt=1 class=transport
   err="... dial tcp 127.0.0.1:9: connectex: No connection could be made because the target machine actively refused it."
level=INFO msg=request method=POST path=/acc-pinned-dead/v1/chat/completions status=200
```

### 4. 从 new-api 侧端到端验一次

新建一个只挂 `zenproxy` 的渠道，模型填一个便宜的，从 new-api 的"测试"按钮或对话页发一条。
然后回 zenproxy 控制台看该账号的 `requests` 有没有 +1。

---

## 七、已知限制（请务必读）

### 1. `*-free` 模型：换出口没用，但要补信封

OpenCode Zen 对 `*-free` 系列返回：

```json
{"type":"error","error":{"type":"FreeTierError",
 "message":"OpenCode's free tier can only be used from within OpenCode"}}
```

这个判定依据是**客户端身份**，不是 IP、也不是账号：换出口、换账号都不会让它打开。

zenproxy 的处理是**把请求补齐成官方客户端的样子**（`free_tier`，默认开）：
User-Agent、`x-opencode-*` 身份头、强制流式、逐字复述客户端的 system prompt、
工具名补齐——判据与阈值逐条实测，见 [`FREE-TIER.md`](FREE-TIER.md)。
new-api 这边不需要任何特殊配置，照常按上面的「建渠道」一节接 `/<账号ID>` 即可；
调用免费模型时留意两点：

- **非流式调用会被折叠**：免费额度只回 SSE，zenproxy 会读完整条流再折回一个
  `chat.completion` 给 new-api。响应体因此可能比平时晚一点点出现，但内容一致。
- **每个套壳请求多花约 1600 字符的 prompt**，计入上下文与 token 消耗。

如果仍然看到 `last_class: tier_gate`，说明套壳没救回来（模板可能随客户端版本过期）。
这时不要去查出口或账号——那不是问题所在。按 [`FREE-TIER.md` 第四节](FREE-TIER.md#四客户端升级后怎么刷新常量)
重新抓包刷新常量即可；控制台 `/admin/api/status` 的 `free_tier.learned` 会列出
"靠被拒绝才学会"的模型，把它们加进 `free_tier.models` 能省掉每个模型的第一趟 403。

### 2. new-api 的流式超时会先于 zenproxy 掐断

new-api 的 `STREAMING_TIMEOUT` 默认 **120 秒**，而 zenproxy 的 `upstream_timeout` 默认 600 秒。
如果你的推理请求经常跑过 2 分钟，会被 new-api 先掐掉。在 new-api 的 compose 里放宽：

```yaml
environment:
  - STREAMING_TIMEOUT=600
```

zenproxy 自己另有一道 `stream_idle_timeout`（默认 5 分钟）：
上游收了流式请求之后**一个字节都不发**的情况会被它掐断，而不是一直挂着连接。

### 3. `auto_ban` 会把 zenproxy 的 502 记在渠道头上

出口池全挂时 zenproxy 返回 502。如果渠道开了 `auto_ban`，连续几次就可能被 new-api 自动禁用。
这是符合预期的保护，但要知道它存在——恢复出口池后记得回去把渠道重新启用。

### 4. 代理字段不支持 socks5

new-api 的"代理"字段走 Go 的 `http.Transport`，只认 `http://` / `https://` 代理。
zenproxy 的正向代理入口就是 HTTP 代理，匹配。但**不要**把 Resin 的 `socks5h://` 地址直接填进 new-api 的代理字段——那个位置是 zenproxy 的地址，不是 Resin 的。

### 5. 一个渠道 = 一个账号

因为 zenproxy 会用账号自己的 key 覆盖 `Authorization`，所以一个渠道只能对应一个账号。
多个账号请在 new-api 里建多个渠道（或用渠道的"多密钥"模式配合 zenproxy 的多账号支持）。

### 6. 两个模型列表会各自漂移，谁都不会报错

有两份列表，**长得很像但不通用**，而且**没有任何机制让它们保持同步**：

| 列表 | 位置 | 作用 |
| :--- | :--- | :--- |
| `free_tier.models` | zenproxy 的 `config.yaml` | 告诉 zenproxy **哪些模型要补免费额度信封** |
| 渠道的 `models` → `abilities` 表 | new-api | 决定 new-api **能不能路由到**这个模型 |

只加前者的后果是**静默的**：zenproxy 那边信封备好了，但 new-api 的 `abilities`
里没有这一行，客户端一问就是

```
503 {"error":{"code":"model_not_found","message":"No available channel for model muse-spark-1.3-contributor-free ..."}}
```

这个报错看起来像"渠道配错了"，实际是**登记漏了**——模型本身完全可用。

实测的脱节例子：`scripts/add-responses-only-models.sh` 把
`muse-spark-1.3/1.2-contributor-free` 加进了 `free_tier.models`，但没人同步渠道的
`models`，于是这两个模型**有很长一段时间是够不着但能用的状态**。补登记之后：

```
muse-spark-1.3-contributor-free  /v1/responses → 200 completed
muse-spark-1.2-contributor-free  /v1/responses → 200 completed
```

**这两个模型只在 `/responses` 上接单**，所以客户端必须走 `/v1/responses`。
走 `/v1/chat/completions` 会得到

```
400 ModelProtocolUnsupported  "Model does not support this protocol."
```

这是**明确且对称**的：`big-pickle` 反过来的情况一样——它在 `abilities` 里登记着，
但拿 `/v1/responses` 调它就是同一个错。所以登记不会制造模糊失败。

补登记时注意 new-api 的一个坑：**`PUT /api/channel/` 的 body 里只要带 `status`，
就恒定回 `{"message":"Invalid parameters","success":false}`**（`POST` 带 `status` 却正常），
症状与"字段写错了"完全一样。用最小字段集提交，`status` 不要带。

---

## 八、回滚

zenproxy 对 new-api 是**非侵入**的：它不碰 new-api 的数据库，不改它的镜像，只是被当成一个上游。

1. 渠道 → 高级配置 → 把 **Base URL 清空**（恢复默认官方地址）→ 保存。
2. `sudo docker compose -f /opt/zenproxy/docker-compose.yml down`
3. 状态文件留在 `/opt/zenproxy/data/state.json`，想彻底清理就删掉 `/opt/zenproxy`。

new-api 的渠道本身不会因为 zenproxy 下线而丢失，只会请求失败。

---

## 九、排障

| 症状 | 先看哪里 |
| :--- | :--- |
| new-api 报 502 / 连接被拒 | zenproxy 容器在不在 `new-api_default` 网络上：`docker inspect zenproxy --format '{{json .NetworkSettings.Networks}}'` |
| 所有出口都不健康 | `curl -H "X-Admin-Token: ..." .../admin/api/proxies`，看 `last_error`；Resin 的 token 对不对、`resin:2260` 通不通 |
| 一直是 401/403 且 `last_class: auth` | 账号密钥本身的问题，不是出口问题 |
| 一直是 403 且 `last_class: tier_gate` | 见第七节第 1 条，跟 IP 无关 |
| 流式响应中途断 | new-api 的 `STREAMING_TIMEOUT`；见第七节第 2 条 |
| 换了出口但出口 IP 没变 | Resin 的粘性身份：同一 `Platform.Account` 在节点存活期间会一直落在同一出口。要么加池子成员，要么在 Resin 侧调整 |
