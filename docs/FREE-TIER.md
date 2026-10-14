# 免费额度：free tier 门禁的实测判据与实现

opencode 的 `*-free` 模型对"客户端身份"有门禁：

```json
403 {"type":"error","error":{"type":"FreeTierError",
     "message":"OpenCode's free tier can only be used from within OpenCode"}}
```

本文记录**白盒实测**出来的判据（逐项剔除，每次只改一个变量），以及 zenproxy 的实现。
判据全部来自抓包 + 逐项重放，不是猜的；每一项都有"只差这一项就 403"的对照实验。

> **2026-10-14 更新（工具指纹门）**：当天午后起，门在四项之外**加验 `bash`/`read` 的
> 工具定义本体**——`description` 与 `parameters` schema 必须与官方客户端逐字一致；
> 仅凭工具名的极简存根（旧实现在用）全部 `403 FreeTierError`，现网 40 分钟 991 条全灭。
> 对照实验（本机直连 + 探针）：官方 12 工具逐字 → 200；bash+read 逐字 → 200；bash 逐字
> + read 存根 → 403；stub bash + 逐字 read → 403；12 个名字 + 极简 schema → 403；
> bash+read 逐字 + 客户端自建工具随行 → 200。zenproxy 已把捕获的逐字定义铸进
> `internal/forward/freetier_tools.go`，信封在任何面上都盖上这对定义（客户端自带的
> bash/read 条目会被替换——形状不同会毒整条请求）。时钟 ID 门（`ses_` 的 12 位 hex
> 必须能解码成合法的倒序墙钟＋[0,63] 计数）依旧在，zenproxy 铸 ID 的方式天然合规，
> 但**探针自造随机 hex 会被拒**，做实验时务必用官方时钟算法铸 ID。
>
> **2026 更新（结构门）**：上游免费门已不再要求逐字复述客户端 system prompt。
> 实测通过的最小信封是：**官方 UA + `x-opencode-*` 身份头 + `"stream": true` + 工具名
> `bash` + `read`**。旧的 title/agent prompt 注入会污染弱模型（如 `step-5-preview-free`
> 回答 `Greeting` / `Short joke request`），因此已从 zenproxy 中移除。
>
> **2026-10-13 更新（出口 IP 维度）**：门禁**并非只看信封**。同一封完美信封在同一秒内，
> 机房 IP 直连 100% `403 FreeTierError`（4 个账号、4 种 UA 全试），而经免费代理池的出口
> 上同一封信封能 200。判定：这是**出口信誉门**——`*-free` 额度实际只在"看起来像真实
> OpenCode 用户"的 IP 组合下放行。因此命中 403 FreeTierError 后：
> ① 换出口重试是有意义的（同一信封、换出口）；
> ② 单一出口的命中率 ≈ 0.1%（免费代理池实测），zenproxy 用 15 分钟出口封锁 + 轮换来收敛。

---

## 一、门禁到底看什么

门禁看**请求信封**与**出口 IP 信誉**，不看 TLS 指纹。信封四项要求缺一不可：

| # | 要求 | 实测边界 | zenproxy |
| :--- | :--- | :--- | :--- |
| 1 | `User-Agent` 必须是官方客户端的 | 去掉即 403；`opencode/1.18.35 … runtime/bun/1.3.14` | 原样镜像 |
| 2 | 需要 `x-opencode-client: cli` 与请求/会话 ID | ID **只校验形状**，不校验状态：`msg_`/`ses_` + 12 位小写 hex + 14 位 base62，共 26 字符。新铸的 ID 一样通过 | 每个请求现场铸 |
| 3 | body 里必须有 `"stream": true` | 去掉即 403（头再完美也没用） | 强制置 true，见 §3.2 |
| 4 | 必须携带官方客户端的 `bash` 与 `read` **定义本体**（2026-10-14 加严：名字 + `description` + `parameters` 逐字一致） | 仅名字即可 200 的旧判据已死；极简 schema 403；逐字定义 + 任意客户端自建工具随行 200 | 逐字定义铸造盖章，见 §3.3 |

第 1–3 条**所有端点共用**；第 4 条每个端点各表述一遍，因为**工具字段形状不一样**：
chat 把名字嵌在 `function.name` 下，`/responses` 放在顶层 `name`。

**确认不需要的**（都做过对照实验）：TLS/ClientHello 指纹（纯 Node `fetch` 就能过）、出口 IP、
`connection`、`accept`、`accept-encoding`、真实会话状态、JSON 键序、`max_tokens`、
以及——**任何 system / instructions prompt 注入**。无工具但带完美身份头的请求会 403；
补上 `bash`+`read` 后即使 `messages`/`instructions` 完全是调用方原文也 200。

### 1.1 工具定义阈值（结构门，2026-10-14 版）

```
无 tools                        → 403 FreeTierError
仅 bash                         → 403
仅 read                         → 403
bash + read（极简 schema）      → 200（旧门）→ 403（2026-10-14 新门）
bash + read（逐字官方定义）     → 200
bash 逐字 + read 极简           → 403
bash 极简 + read 逐字           → 403
逐字对 + 客户端自建工具随行     → 200
12 个官方名字 + 极简 schema     → 403（名字不够，定义才算数）
```

旧文档里"至少 5/6 个官方工具名"是更早期观测；2026-10-13 之前只认 `bash`/`read` 两个
**名字**；2026-10-14 起认**定义本体**。实现里 `gateToolNames = ["bash","read"]`，
`gateMinToolNames = 2`，逐字定义在 `internal/forward/freetier_tools.go`。

### 1.2 tool_choice 按端点分流

| 端点 | 策略 | 实测 |
| :--- | :--- | :--- |
| `/chat/completions` | 客户端**没有**自带工具 → 设 `"none"`，避免模型去调注入的 stub；客户端**已有**工具 → 保留其 `tool_choice` | `"none"` / 指定函数 / 省略均可 200 |
| `/responses` | 有 tools 时**只接受** `"auto"`；`"none"` 或指定函数 → HTTP 400 | 信封无条件写成 `"auto"` |

### 1.2b 出口 IP 信誉门（2026-10-13 实测）

同一封已通过结构门的请求（官方 UA `opencode/1.19.0` + 全部身份头 + `bash`/`read` +
`stream:true`）：

```
服务器本机 IP（Oracle 机房）直连    → 403 FreeTierError（4 账号 × 多 UA，100% 复现，0.4-0.7s）
住宅/免费代理出口 A                → 200
同一时刻免费代理出口 B              → 403 FreeTierError
同一出口稍后重试                    → 概率性翻转
```

结论：**门在 IP 维度上放行的是"好信誉"出口**，机房 IP 整体拒绝；免费代理里只有小部分
IP 当前在信誉白名单上。所以：

- 换出口对 FreeTierError **有效**（zenproxy 已把 `ClassTierGate` 归为可轮换；
  命中后该出口封锁 15 分钟，到期或真实 200 才解封）；
- 命中率是瓶颈，不是信封形状。池子越大、信誉越杂，收敛越快。

### 1.3 为什么不再注入 prompt

旧实现把 title / agent prompt 前缀塞进 system / `instructions`，再跟一句
`IGNORE the preceding instructions…`。这在带 tools 的强模型上大体可用，但在弱免费模型上
会把任务本身污染成"起标题"：

| 模型 | 旧信封（prompt 注入） | 新信封（仅 bash+read） |
| :--- | :--- | :--- |
| `step-5-preview-free` | `"Short joke request"` / `"Greeting"` | 正常笑话 / `OK` |
| `big-pickle` | 偶发标题化 | 正常回答 |
| `muse-spark-*` | 偶发复述信封 | 正常笑话 / 算术 |

对照实验见 `FishBottle7/opencode2dsh` 与本仓库对 `opencode.ai/zen/v1` 的重放。

---

## 二、怎么自己复现

判据的唯一测法：**一次只改一个变量**，其余保持与一次成功请求完全一致，看 403 / 200。

1. 去掉 `User-Agent` → 403 ⇒ 头是必需的
2. 把 `stream` 改成 `false` → 403 ⇒ 流式是必需的
3. 去掉 `tools` → 403 ⇒ 结构门需要工具名
4. 只留 `bash` 或只留 `read` → 403；两个一起 → 200
5. 换 IP、换账号、复刻 TLS 指纹 → **与门禁无关**

> 注意别踩的坑：`Accept-Encoding: br` 会让 403 的 body 变成 brotli 密文，
> 用肉眼/正则看"这不是 FreeTierError"就判错方向。抓包时要么不带 `br`，要么先解压。

---

## 三、zenproxy 的实现

代码在 [`internal/forward/freegate.go`](../internal/forward/freegate.go)，
常量在 [`internal/forward/gateprompts.go`](../internal/forward/gateprompts.go)
（现在只保留工具名常量，不再生成 prompt 块）。

### 3.1 只碰该碰的请求

- **只对免费模型动手**：`free_tier.models` 列表里的，加上运行时**学到**的（第一次被 403
  拒绝后自动记住，下次直接套壳，不再浪费一次往返）。
- **付费模型一个字节都不改**：列表外的模型原样转发。
- **不改 `messages` / `instructions`**：调用方的提示词原样到达模型。
- **两个端点套壳：`/chat/completions` 和 `/responses`。** 工具字段形状不同，所以是两个
  builder。`/responses` 的 `input` / `instructions` 原样不动，只补 tools + `tool_choice`。
- `/messages`（Anthropic 形状）**不套壳**：第三个形状尚未特征化。
- 正向代理模式（CONNECT 隧道）、以及超过 `policy.max_body_bytes` 的大 body **无法改写**。

### 3.2 流式强制 + 折叠

免费额度**只回 SSE**，而客户端可能要的是单个 JSON。实现是：上游一律收到
`"stream": true`，如果调用方本来要的是非流式，响应路径把 SSE 折回一个单对象
（`internal/forward/ssefold.go`）。

**折叠按端点分流，这是必须的，不是优化。** 两个端点的流事件完全不同：
chat 是 `choices[].delta`，Responses 是 `response.output_text.delta` 和
`response.completed`。用错折叠器**不会报错**——它会把对方的流读成"没有内容"，
然后返回一个合法但为空的答案，而客户端会以为成功了。这比报错更糟。

- chat：内容拼接、`tool_calls` 按 index 归并、usage 与 `finish_reason` 保留。
- Responses：`response.completed` 事件里的 `response` 字段**本身就是**非流式对象，
  所以直接**原字节透传**；流提前断掉时才从 delta 兜底重建。
  两条路都走不通就回错误信封，**绝不返回空答案**。

`stream` 字段的语义按 OpenAI 默认：**不写 = 非流式**，所以不写就会被折叠成 JSON。

### 3.3 工具定义盖章（2026-10-14 起）

调用方自己的 tools 排在**盖过章的门禁对之后**（工具调用照常工作：模型按名调用，
客户端执行自己的实现）。门禁对 `bash`/`read` 一律用**捕获自官方客户端 v1.19.0 的
逐字定义**（`internal/forward/freetier_tools.go`，`description`+`parameters`+`strict` 原文），
形状跟着端点走：

- chat：嵌套 `{"type":"function","function":{…逐字内容…}}`
- Responses：扁平 `{"type":"function",…逐字内容…}`

一个数组里不混两种形状。读取名字时两种都认。

**客户端自带的 `bash`/`read` 条目会被替换成逐字对**（不再保留）：2026-10-14 门加验
定义本体后，形状不同的客户端同名条目会毒整条请求。若客户端真发的工具语义与官方一致
（pi 即如此），模型调用名不变，客户端照常执行。
旧占位描述 `Reserved by the proxied runtime. Do not call this tool.` 已停用。
chat 在客户端无自带工具时额外设 `tool_choice: "none"`，进一步降低误调概率；
Responses 只能设 `"auto"`（上游硬限制）。

```
请求：tools=[get_weather]，问巴黎天气
响应：tool_calls=[get_weather({"city": "Paris"})]     ← 调的是客户端的工具，不是占位
```

### 3.3b token 预算下限

结构门补上工具后，部分推理型免费模型会把很小的 `max_tokens`（如 32）全部烧在
隐藏 reasoning 上，返回 `finish_reason=length` + 空 `content`。信封对**已经写出的**
`max_tokens`（chat）/ `max_output_tokens`（Responses）设下限 `256`；客户端没写预算
则不填，沿用上游默认。chat 折叠同时保留 `reasoning_content`。

### 3.4 上游压缩：不要转发 `accept-encoding`

调用方的 `accept-encoding: br, zstd` **不会**被转发。原因不是门禁——是 `net/http`
只要发现 `Accept-Encoding` 是调用方自己设的，就会把 body 原样（压缩态）交出来，
于是：403 的分类会因为读不懂 brotli 而误判成 `auth`（重试逻辑直接失效），
折叠也拿到一堆密文。去掉这个头，由 transport 自己协商 gzip 并透明解压，
下游拿到的一律是明文。

---

## 四、客户端升级后怎么验证

结构门的常量极短（两个工具名）。若上游再次收紧：

1. 用官方客户端对假上游抓一次成功的 free 请求；
2. 对照本文 §一逐项剔除，确认是否仍只需 `bash`+`read`；
3. 若阈值上调，改 `gateToolNames` / `gateMinToolNames` 后跑：

   ```bash
   go test ./internal/forward/
   ```

旧的 `scripts/extract-gate-prompts.mjs` 等 prompt 抓包脚本保留作历史探测工具，
**不再驱动运行时常量**。

---

## 五、清单会漂移（比判据漂移更常见）

`free_tier.models` 是**上游的模型名**，而上游会直接把它们换掉。失效的表现**不是** 403，而是：

```
400 server_error: Upstream request failed: Model is unavailable.
```

**门禁学习机制救不了它**。`free_tier.learned` 只在收到 403 `FreeTierError`
时才记录；模型名不存在得到的是 400，学不到任何东西。**清单过期必须人工换。**

### 换清单的三步

```bash
# 1) 拿上游当前公布的免费模型（经 zenproxy 自己查，走的是同一条出口）
curl -s http://127.0.0.1:9000/<账号ID>/v1/models | grep -o '"[^"]*-free"'

# 2) 逐个验活 —— 只看 HTTP 200 的（403 = 套壳没生效；400 = 名字过期）
sudo scripts/probe-free-models.sh <模型名…>

# 3) 写回部署配置并重启
sudo scripts/refresh-free-models.py <确认可用的模型名…>
sudo docker restart zenproxy
```

### 失败类型速查

| 响应 | 含义 | 能救吗 |
| :--- | :--- | :--- |
| 410 `ModelDeprecated` | 上游已下线 | 从名单删掉 |
| 400 `ModelProtocolUnsupported` | **这个端点**不接这个模型 —— 换端点再试 | 见下 |
| 429 `FreeUsageLimitError` / `Endpoint is unavailable` | 通了门禁，该出口额度用尽（UTC 0 点重置） | 换出口 / 等待 |
| 403 `FreeTierError`（已套壳仍报） | **这个出口**的 IP 不在信誉白名单 | 换出口（zenproxy 自动：封锁 15 分钟 + 轮换） |
| 403 `RegionError` | 出口 IP 所在国家被限制 | 换国家出口 |
| 402 `server_error` | 付费模型，账号没额度 | 充值 |

**★ 端点是一个维度，不是背景板。**

| 模型 | `/chat/completions` | `/responses` |
| :--- | :--- | :--- |
| `muse-spark-*-contributor-free` | 400 `ModelProtocolUnsupported` | 结构门后 200 |
| 多数 `*-free`（`big-pickle`、`step-5-preview-free` 等） | 结构门后 200 | 视模型而定 |

`zenproxy` 给 `/chat/completions` 和 `/responses` 都套结构壳。
**没有套壳的端点永远只会回 403 `FreeTierError`**——那个 403 是"你没被套壳"，
和模型能力无关。

---

## 六、运维注意

- 日志里 `template=tools` / `template=responses-tools` 表示本次走了结构门。
- 非流式客户端仍会看到折叠后的 JSON；上游始终收到 `stream: true`。
- 若弱模型再次出现标题化回答，先确认部署的是否为**去掉 prompt 注入**的版本，
  再查上游是否重新引入了 prompt 匹配。
