# zenproxy

[![CI](https://github.com/Smith-106/zenproxy/actions/workflows/ci.yml/badge.svg)](https://github.com/Smith-106/zenproxy/actions/workflows/ci.yml)

**给账号池配一个会自己换 IP 的出口层。**

new-api 和 LiteLLM 都能给一个渠道配一个代理，但都是一次性静态值：填什么就是什么。
出口一挂，整条渠道跟着挂，得手工去改。zenproxy 补的就是这块——它把出口做成一个池子，
探活、隔离、退避、报错自动换下一个，并且**保持会话粘性**（同一个账号在一次会话里不会中途换 IP）。

```
客户端 ──▶ new-api / LiteLLM ──▶ zenproxy ──▶ 出口池(Resin / 静态代理) ──▶ 上游 LLM
                                     │
                                     └─ 探活 · 隔离 · 退避 · 失败分类 · 换出口重试
```

它不代理模型、不做计费、不改写请求体。它只负责一件事：**把请求从一个可用的出口送出去，坏了就换。**

---

## 三个入口，一个端口

zenproxy 在同一个监听端口上同时干三件事，靠请求形态自动区分：

| 模式 | 怎么用 | 谁在用 |
| :--- | :--- | :--- |
| **统一入口** | `http://zenproxy:9000/v1/chat/completions` + `Authorization: Bearer <key>` | 任何 OpenAI 兼容客户端；LiteLLM / CLIProxyAPI / sub2api 直接当上游导入（`api_base` / `base-url` 写 `http://zenproxy:9000/v1`） |
| **反代** | `http://zenproxy:9000/<账号ID>/v1/chat/completions`（账号名写进路径） | 需要把请求固定到某个账号时。new-api 的渠道 Base URL 填 `http://zenproxy:9000/<账号ID>`——它自己会补 `/v1/...` |
| **正向代理** | `http://<账号ID>:x@zenproxy:9000` | new-api 的渠道「代理」字段、sub2api 的 `proxies` 记录、任何只支持代理设置的工具 |

> ★ **填 base URL 前先跑 `scripts/check-api-base.sh`。** 少写或多写一个 `/v1`
> 都不会得到「找不到路径」，而是掉进反向入口、报成「账号不存在」——
> 三个聚合器有三副面孔，没有一句会提到 base URL。见 [GATEWAY.md](docs/GATEWAY.md)。

**统一入口是唯一不需要调用方知道账号名的形状。** 它对外只有一个 URL 和一个 key，
账号与出口都由 zenproxy 在内部决定——所以「上游按 IP 限额就自动换」这件事不用调用方配合：

```
POST /v1/chat/completions  (Bearer <key>)
      │  key → 池
      ▼
   账号 A ─ 出口 X → 429   ← 上游说这个 IP 花完了
   账号 A ─ 出口 Y → 429   ← 还是它，那问题更像在账号上
   账号 B ─ 出口 Z → 200  ✓ 调用方只看到这个
```

反代模式下 zenproxy 终止 TLS，能看到上游的明文响应，因此能**按状态码**决定换出口；
正向代理模式下 https 目标是一条 `CONNECT` 隧道，它只能按"连不上"换出口。
**要"报错自动换 IP"就用统一入口或反代模式。** 详见 [`docs/GATEWAY.md`](docs/GATEWAY.md)
与 [`docs/NEW-API.md`](docs/NEW-API.md)。

---

## 本机快速开始

```bash
go build -o build/zenproxy ./cmd/zenproxy
cp config.example.yaml config.local.yaml    # 填好 ZEN_ADMIN_TOKEN / RESIN_PROXY_TOKEN
ZEN_ADMIN_TOKEN=dev-token ./build/zenproxy -config config.local.yaml
```

不需要真出口也能看到东西：

```bash
curl -s localhost:9000/healthz                                    # {"status":"ok"}
curl -s -H 'X-Admin-Token: dev-token' localhost:9000/admin/api/status
```

控制台在 `http://127.0.0.1:9000/admin/?token=dev-token`。

---

## 配置

完整字段见 [`config.example.yaml`](config.example.yaml)，`internal/config/config.go` 是权威定义。
配置里写 `${VAR}` 会从环境变量展开；**密钥不要写进配置文件**。

值得单独说的几个开关：

| 配置 | 默认 | 说明 |
| :--- | :--- | :--- |
| `policy.max_attempts` | 3 | 单次请求最多尝试几个出口 |
| `policy.rotate_on_status` | `[407,429,5xx…]` | 命中这些状态码就换出口重试 |
| `policy.rotate_on_transport` | true | 连不上就换出口 |
| `policy.allowed_hosts` | `[]` | 正向代理模式的目标白名单。上游域名自动加入，不用手写。留空只应出现在监听回环时 |
| `health.failure_threshold` | 3 | 连续失败几次开始隔离 |
| `health.quarantine` / `max_backoff` | 5m / 30m | 隔离时长与指数退避上限 |
| `health.max_latency` | 0（关闭） | 上一轮探活超过这个耗时的出口被降为**备用**：仍在池子里、仍算健康，但不再被优先选中。用公共/免费节点拼的池子建议设 `5s`——实测这类池子里"能用"的出口常常要 20 秒才回。未测量过的出口永远算合格，新成员不会被饿死 |
| `health.retire_after` | 20 | **永久淘汰**：连续失败到这个次数、中间一次都没成功过，就把出口彻底移出轮换。任何一次成功清零计数，所以"时好时坏但还能用"的不会被淘汰。`0` = 关闭 |
| `stream_idle_timeout` | 5m | 上游收了流式请求后一字节不发，多久掐断 |
| `upstream_timeout` | 10m | 单次上游请求硬上限 |

**启动即校验，不合规直接退出**，不会带着半个错误配置跑起来。缺 `${VAR}`、启用的账号没有 key、
账号引用了不存在的上游、admin token 为空——都会在启动时明确报出是**哪一个**变量/账号有问题。

---

## 账号与出口的关系

- **出口（exit）**：一个能出网的代理端点。池子成员来自 `proxies.resin` 和 `proxies.static`。
- **账号（account）**：一把上游密钥 + 出口策略（`auto` / `direct` / 指定出口 ID）。
- **绑定**：账号与出口的粘性关系，持久化在状态文件里，重启不丢。

几个刻意的设计取舍：

1. **亲和，不是身份槽位。** 一个账号绑定到一个出口，是为了让长会话不中途换 IP，
   而不是为了给每个账号伪造一个"独立家庭"身份。出口挂了、或用 `auto` 时从池子里挑，都是共享的可用性池。
2. **429 不判出口死刑。** 上游限流是**账号级配额**问题，出口把请求送达并带回了响应，说明出口是好的。
   所以这类失败只移动绑定（`soft_failures` 计数），不隔离出口。否则一次账号限流就能把整个 IP 池耗干。
3. **`tier_gate` 单独归类。** 上游按"客户端身份"拒绝时，换 IP 是无效的。
   这类响应既不算出口的错，也不浪费重试预算，原样透传给客户端，并在日志里明确说清"换出口解决不了"。
4. **指定出口是首选，不是无期徒刑。** `proxy: <出口ID>` 在该出口健康时生效；
   一旦它被隔离就自动退回池子里挑——否则最需要轮换的账号反而永远轮换不了。
5. **隔离是打盹，淘汰是出门。** 隔离有退避上限，到点自动复活——这适合"网络抖一下"。
   但对一个**已经死透**的出口，反复复活只是永远占着位置、永远浪费重试预算。
   所以 `retire_after` 一到就永久淘汰，只能手动恢复。清出来的是位置，不是数据：
   状态文件里仍留着它的历史，控制台上也看得到，只是不再参与轮换。
6. **池子空掉时降级，不是崩掉。** 所有出口都被淘汰时，`Pick` 会退回到**失败最少**的那个已淘汰出口，
   并在日志里喊一嗓子。宁可带着最不坏的出口苟着，也不要给网关回 502——
   但这一嗓子必须喊，否则运维会以为池子还好好的。

### 清理垃圾出口

控制台右上角有个**「清理垃圾出口」**按钮，对应 `POST /admin/api/proxies/prune`。
它把当前「隔离中 / 降级 / 延迟超过 `max_latency`」的出口一次性永久移出轮换，并返回做了什么：

```json
{"report":{"retired":["static-01"],"already_retired":null,"usable_after":2,"pool_empty":false}}
```

- **幂等**：再点一次不会重复动作，`already_retired` 会列出已淘汰的。
- **不手软**：如果清完池子是空的，它会照清不误，但 `pool_empty: true` 会明确告诉你这一点。
- **可逆**：误清了就在表格里点该行的「恢复」（`.../revive`），
  恢复会连失败计数一起清零，给它一个干净的起点而不是下一次抖动又被淘汰。
- 表格里状态列会预先标出「**可清理**」，所以点按钮前就能看到会动到谁。

---

## 管理接口

| 方法 | 路径 | 说明 |
| :--- | :--- | :--- |
| GET | `/healthz` | 存活探测，免鉴权 |
| GET | `/metrics` | Prometheus 文本格式 |
| GET | `/admin/` | 控制台（令牌可走 `?token=`）：总览卡、免费额度区、运行配置（只读）、出口表、账号表；账号支持编辑回填、启用/禁用、换出口、删；出口支持退休/恢复 |
| GET | `/admin/api/status` | 概览（含 `free_tier`、`policy`、`health` 只读块与 `exits_retired`） |
| GET/POST | `/admin/api/accounts` | 列出 / 新增或修改账号 |
| DELETE | `/admin/api/accounts/{id}` | 删除账号 |
| POST | `/admin/api/accounts/{id}/rotate` | 手动换出口 |
| GET | `/admin/api/proxies` | 出口健康、真实出口 IP、延迟、隔离/退休状态 |
| POST | `/admin/api/proxies/probe` | 触发一轮探活 |
| POST | `/admin/api/proxies/prune` | **一键清理**：把当前不可用的出口（隔离中 / 降级 / 延迟超 `max_latency`）永久移出轮换 |
| POST | `/admin/api/proxies/{id}/retire` | 手动退休一个出口 |
| POST | `/admin/api/proxies/{id}/revive` | 恢复一个已退休的出口 |

鉴权用 `X-Admin-Token`、`Authorization: Bearer` 或 `?token=`。
接口**永远不回显完整密钥**，只返回 `sk-a…rect` 这样的提示串。

---

## 免费额度（free tier）

opencode 的 `*-free` 模型只认官方客户端，别的调用方一律 403 `FreeTierError`。
zenproxy 会**把免费模型的请求补齐成官方客户端的样子**再发出去，于是这些额度能直接用：

```yaml
free_tier:
  enabled: true                # 默认开。关掉 = 一个字节都不改写
  models:                      # 预期在免费额度里的模型；列表外的模型第一次被拒后会自己学会
    - big-pickle
    - deepseek-v4-flash-free
  user_agent: ""               # 留空 = 镜像抓包得到的官方 UA
```

行为要点：

- **只碰免费模型。** 付费模型（列表外的）一个字节都不改，原样转发。
- **不写 `stream` 的请求会被折叠。** 免费额度只回 SSE，调用方要的是单个 JSON 时，
  代理读完整条流再折回一个 `chat.completion`（内容、`tool_calls`、usage 全部保留）。
- **带 `tools` 的请求也能过。** 调用方自己的工具照常可用，实测模型会正确调用它；
  为了过门禁，代理会在工具列表后面补几个"占位"名字。
- **第一次被拒不用管**：403 会被就地转成"补壳重试"，并把该模型记下来
  （`/admin/api/status` 的 `free_tier.learned`），下一次直接套壳。
- **正向代理模式改写不了**（CONNECT 是盲隧道），超大 body 同理。

判据、阈值、复现方法和代价见 [`docs/FREE-TIER.md`](docs/FREE-TIER.md)。
上游改了判据也不会静默失败：日志会打 `tier_gate` 警告并指向那篇文档。

---

## 测试

```bash
make test      # 单元测试 + 端到端
make vet
make smoke     # 只跑端到端
```

端到端测试会在 `httptest` 上起一个假上游和一个假出口，覆盖这些行为：

- 账号被钉在死出口上 → 传输失败 → 换到活出口 → 200，且 `rotations == 1`
- `tier_gate` 403 → 原样透传、**不**换出口、**不**消耗重试预算
- 429 → 换出口后成功，但**不**隔离出口本身
- SSE 逐块送达：上游发第一块后阻塞，客户端必须**在**阻塞解除前收到它（证明不是缓冲整包）
- 上游收了流式请求后静默 → 被 `stream_idle_timeout` 掐断，而不是挂到硬超时
- 正向代理模式：从 `Proxy-Authorization` 用户名解析账号；无凭据 407；白名单外 403
- SOCKS5 客户端对着一个真的 RFC1928 服务端跑（不是 mock 接口），含密码错误路径
- 免费额度：被拒的免费模型被就地补壳重试 → 200，且第二次调用**一次到位**（不再浪费一次 403）
- 免费额度：非流式调用拿到的是折叠好的单个 JSON；`accept-encoding` 不会把 `br` 透传给上游
  （透传会让 403 变成密文、错误分类失灵）

真实 socket 冒烟（需要 Node）：

```bash
node scripts/mock-upstream.js &     # 假上游 :28081
node scripts/mock-exit.js &         # 假出口 :28082
./build/zenproxy -config config.smoke.yaml
```

---

## 部署

见 [`deploy/README.md`](deploy/README.md) 与 [`docs/NEW-API.md`](docs/NEW-API.md)。

---

## 已知限制

接 new-api 时尤其要看 [`docs/NEW-API.md` 第七节](docs/NEW-API.md)：

- OpenCode Zen 的 `*-free` 模型按**客户端身份**拒绝（HTTP 403 `FreeTierError`），
  **换 IP、换账号都无法改变这个结果**——这一条 zenproxy 现在自己解决了：
  见 [免费额度](#免费额度free-tier)，实测判据与代价在 [`docs/FREE-TIER.md`](docs/FREE-TIER.md)。
- new-api 的 `STREAMING_TIMEOUT` 默认 120s，会先于 zenproxy 掐断长流式响应。
- 出口池全挂时返回 502，new-api 的 `auto_ban` 会把它记在渠道头上。
- **下游必须和 zenproxy 在同一个 docker 网络。** 它默认只接
  `new-api_default` + `resin_default`，而 CLIProxyAPI、sub2api 之类往往在别的网络上，
  不接的话容器名根本解析不了：`docker network connect <网络> zenproxy`。
  另外 `docker compose up -d` 重建容器会丢掉手工接的网络，要重接。
- **`allowed_hosts` 默认只放行 `opencode.ai`。** 有些下游用
  `api64.ipify.org` 之类做代理健康探针，不放行的话那条代理永远显示 failed，
  而实际转发是好的。

关于出口池本身的质量，有一组实测数据值得先看：[`docs/RESIN.md`](docs/RESIN.md)。
要点：**池子规模 ≠ 可用出口数量**。你服务器上的 Resin 动态池有 1435 个节点，
但实测 4 个成员里只有 1 个能通，而且它花了 20.8 秒。这类池子必须开
`health.max_latency`，并把 `max_attempts` 给够。同一篇文档里也记录了粘性身份的实测边界：
Account 到出口的映射**只在节点存活期间**稳定。

---

## 目录

```
cmd/zenproxy/       入口：路由分发、优雅退出、请求日志
internal/config/    配置加载、${VAR} 展开、启动期校验
internal/model/     共享类型：账号、上游、错误分类、统计
internal/proxypool/ 出口池：HTTP/SOCKS5 拨号、探活、隔离与退避
internal/registry/  账号注册表：绑定、轮换、统计、状态持久化
internal/gateway/   统一入口的凭据与模型映射解析（key→池、model_name→上游名）
internal/forward/   转发核心：统一入口、反代模式、正向代理模式、失败分类与轮换重试；免费额度套壳（freegate.go）与 SSE 折叠（ssefold.go）
internal/admin/     管理 API + 内嵌控制台
deploy/             容器部署产物
docs/               接入与运维文档
scripts/            本机冒烟用的假上游 / 假出口 + 部署与运维自检脚本
.github/workflows/  CI：gofmt / vet / test -race / 静态构建
CHANGELOG.md        版本变更记录
```

发布版本与二进制见 [Releases](https://github.com/Smith-106/zenproxy/releases)，
版本变更见 [`CHANGELOG.md`](CHANGELOG.md)。

### 部署后自检与运维脚本

`scripts/` 里这几个是给**已经跑起来的部署**用的，都在部署机上 `sudo` 执行：

| 脚本 | 干什么 |
| :--- | :--- |
| `zenproxy-status.sh` | 一次性打印容器状态、概览、出口池、账号绑定（不回显任何密钥） |
| `probe-free-models.sh` | 逐个实测免费模型是否真的可用；`MT=` 给 token 预算（推理模型要给大） |
| `refresh-free-models.py` | 把 `free_tier.models` 换成实测可用的清单（逐行改写，不动注释排版） |
| `probe-resin-slots.sh` | 实测 Resin 粘性身份的真实可用率，用来决定池子该开多大 |
| `set-resin-slots.py` | 把 `proxies.resin.accounts` 扩到 N 个 slot |
| `e2e-free-tier.sh` | 端到端：经代理调一次免费模型，验证套壳与 SSE 折叠 |
| `redact.py` | **读任何含密钥的文件/输出都必须过它**；`--test` 跑历史泄露形状 |
| `set-newapi-channel-status.sh` | 改 new-api 渠道启用状态，并列出每个模型还剩几条渠道 |
| `rotate-cpa-inbound-key.py` | 轮换 CLIProxyAPI 入站密钥（定点行替换，保注释，带结构断言） |
| `check-exit-spread.sh` | 查账号是否落在不同出口、有无幽灵占用（只读，多次采样，有冲突退出 1） |

免费模型名会随上游漂移，**清单过期必须人工换**（表现是 400 `Model is unavailable`，
不是 403），换法见 [`docs/FREE-TIER.md` 第五节](docs/FREE-TIER.md)。

### 凭据纪律（请先读这条）

**不要手写 `sed`/`grep` 正则去脱敏。** 这个仓库的历史里有六次凭据被打印进终端/日志，
六次都是同一个原因：脱敏者**枚举了自己以为存在的模式**——漏掉 YAML 列表项、
漏掉 `nvapi-`/`<hex>.<base64>`/base64/URL 内嵌密码这些形状、被 `ZEN_KEY_01=` 里的
`_01` 骗过、用 `len(v) > 12` 当过滤器。

唯一认可的做法是 `scripts/redact.py`（按**键名**与**值形状**两个独立信号判定，
支持 JSON/YAML/.env/env dump/纯文本，可接管道）：

```bash
sudo scripts/redact.py /opt/zenproxy/data/state.json
sudo docker exec zenproxy env | scripts/redact.py -
```

六次事故的具体形状、这个部署里密钥都在哪、以及漏了之后按「能不能自己换」分类的处理
步骤，见 [`docs/SECRETS.md`](docs/SECRETS.md)。
