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
> **2026-10-14 傍晚更新（affinity 头禁令 + 工具门全 lane 收紧）**：16/16 交替对照
> （同一信封、只翻转 `x-session-affinity`）钉死两条新规则：
> ① **`x-session-affinity` 头携带非空 ses 值 ⇒ 403 FreeTierError**——旧 26 字符形与
> 1.19 的 64-hex 形皆拒，与 x-opencode-session 同值/异值皆拒，空值或缺席则放行；
> directory 头无害。zenproxy 信封从不发这个头，但要防下游把入站 affinity 带进来：
> ApplyHeaders 现在在消费完入站会话键后把**所有原始拼法删除**（x-opencode-session/
> -id 由盖章值收编，`session_id`/`session-id`/`conversation_id`/`x-session-id`/
> `x-session-affinity` 一律剥除），否则 buildRequest 逐字上送＝保证 403。
> ② **逐字工具门已扩到全部 lane 与全部面**：同日 /zen/v1 与 /inference/v1 的
> chat 与 responses 四象限，stub 定义全灭、逐字对全过（以 429 FreeUsageLimitError=
> 门放行、403 FreeTierError=身份拒 判读）。第三方案（FishBottle7/opencode2dsh，
> 154★/36fork）10-11 的"schemas uninspected"+发 affinity 头的配方**已双双过期**，
> 现网每发必 403；其门禁判据史（09-16 UA→会话形状、09-18 流式+bash/read、
> 10-09/11 names-only、10-13 #62 时钟位反码、计数 [0,63]、高 36 位不校验）与我们的
> 实测链条完全互证，可作旁证库。muse 系在其 10-10 矩阵中 RegionError 全灭，同日
> 我们侧 200——再次证实 region 维度是出口 IP 的事。
>
> **2026-10-14 午后观察（红窗与重试自续期）**：逐字定义信封上线当天，同一形状的请求在
> 分钟级时间里 403 与 200 交替（40 分钟内两次风暴，两次又全绿；919KB 无工具真身
> 会话在风暴后原样 200）。起初疑为双副本状态分裂，取证后修正：双副本共享
> `data/` 挂载设计本就是原子替换、纯重建性状态，而当日 15:03–15:23 的合并日志显示
> 红相位的真相是**拒绝会被上游记忆并自我延续**——一个副本因重试轰炸连吃约 220 条
> 403（直连与 resin 槽位同时拒，证明判定不按出口 IP），另一个副本同期混发 429/403，
> 轰炸一停几分钟内双双转绿。对照组还证伪了连接级污染：在同一条 keep-alive TLS 连接
> 上先吃 403 再发好请求，立刻恢复 429/200。结论：**上游在红窗内按请求形态给拒绝打
> 时间戳，重试越凶红窗越长**；zenproxy 侧的正确姿势是不烧预算——tier 拒绝在
> 信封重试 + 一次换出口之后立即把上游原话回传给调用方（`forward.go` 的
> `tierGateFails` 上限），把"12 次连烧拖成超时"变成"快速失败"，红窗交给时间自愈。
> 遇到间歇 403 tier_gate 且换出口/账号都拒绝时，先怀疑红窗，别急着改信封。
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

### 1.4 2026-10-17 复核：整批 `-contributor-free` 别名被下架

上游目录（`inference/v1` 85 + `zen/go/v1` 37，去重 100 个 ID）里几乎看不到 `-free` 结尾的
名字了，但**目录不等于可用集**：老的 `-contributor-free` 别名有的还能打（1.2），有的直接
410，有的换成 `-contributor`（无 `-free`）后要求付费账号。唯一可靠的判据是两协议面各打一遍：

```bash
sudo python3 scripts/probe-free-models-both-faces.py --out /tmp/model-probe.json
```

2026-10-17 的实测结果（`200 = 该面可用`）：

| 模型 | `/chat/completions` | `/responses` | 处置 |
| :--- | :--- | :--- | :--- |
| `beam-free`、`big-pickle`、`deepseek-v4.1-flash-free` | 200 | 400 协议面不收 | 留 |
| `longcat-2.5-preview-free`、`nemotron-3.5-super-vl-free`、`space-bunny-free` | 200 | 400 协议面不收 | 留（长思考，30~50s） |
| `glm-5.3-flash-free`、`mimo-v2.6-flash-free` | 200 | 400 协议面不收 | 留 |
| `nemotron-3.5-lightning-free` | 200（慢） | 400 协议面不收 | 留 |
| `muse-spark-1.2-contributor-free` | 200 | **200** | 留 —— 两个面都接单 |
| `muse-spark-1.3-contributor-free` | 410 `ModelDeprecated` | 410 | 删（上游已弃用） |
| `muse-spark-1.2/1.3-contributor`、`gemini-4-argon-contributor(-free)` | 400 `Model is unavailable` | 400 | 删（要付费账号） |
| `ling-3.1-flash-free`、`step-5-preview-free` | 400 `Model is unavailable` | 400 | 删（已换名/下线） |
| `jev-1.13-free` | 400 `ModelProtocolUnsupported` | 400 | 删（两个面都不收） |

三点值得记下来：

1. **目录里查不到的名字仍可能可用**（1.2 的 `-free` 别名）；**目录里在的名字也可能不可用**
   （`*-contributor` 在目录里，但免费账号一律 400）。所以只信实测。
2. **只看 `/chat/completions` 会误删好模型**：`muse-spark-*-contributor-free` 在 chat 面回
   400 `ModelProtocolUnsupported`，看起来和废模型一模一样，其实它在 `/responses` 上完全正常
   —— 而这正是 pi / opencode 实际走的那个面。
3. **换池那一瞬间会看到零星的 403 `OpenCode's free tier can only be used from withi…`**。
   新出口刚上线时命中，同一条请求串行重试 10/10 全 200；不属于模型问题，别据此改名单。

收敛到两处名单（缺一处就是"目录看得见、请求打不通"）：

```bash
sudo python3 scripts/converge-free-models.py --models-file /tmp/model-probe.json \
    --alias muse-spark-1.3-contributor-free     # 老客户端还在用旧名 -> 靠 model_mapping 改写
```

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
| `muse-spark-1.2-contributor-free` | 结构门后 200 | 结构门后 200 |
| 多数 `*-free`（`big-pickle`、`beam-free`、`space-bunny-free` 等） | 结构门后 200 | 400 `ModelProtocolUnsupported` |
| `jev-1.13-free` | 400 `ModelProtocolUnsupported` | 400 `ModelProtocolUnsupported` |

（`muse-spark-1.3-contributor-free` 已在上游弃用，410；`step-5-preview-free` 已下线，见 1.4。）

`zenproxy` 给 `/chat/completions` 和 `/responses` 都套结构壳。
**没有套壳的端点永远只会回 403 `FreeTierError`**——那个 403 是"你没被套壳"，
和模型能力无关。

---

## 六、运维注意

- 日志里 `template=tools` / `template=responses-tools` 表示本次走了结构门。
- 非流式客户端仍会看到折叠后的 JSON；上游始终收到 `stream: true`。
- 若弱模型再次出现标题化回答，先确认部署的是否为**去掉 prompt 注入**的版本，
  再查上游是否重新引入了 prompt 匹配。

### 6.1 出口停用时长是这个池子的可用性旋钮（`free_tier.block_duration`）

出口被闸门拒绝后会被停用一段时间（日志 `egress blocked for free-tier refusal`）。
**探活永远清不掉这个停用** —— 探活走的是 ipify/opencode.ai，不触发闸门，所以
"IP 能通"不是"这道门能过"的证据；停用期间轮换也不会选它。**只有时钟能把它放回来。**

因此这个时长直接决定"闸门重新放开之后多久能恢复"。2026-10-17 实测：

- 10 分钟前：zen-01 连打 8/8 全 200，1.5–3.4 秒；
- 10 分钟后：四个账号 20/20 全 403，3 分钟内 130 次请求无一成功；
- 同一时间 zenproxy 自己还在不停制造停用：3 分钟新增 64 条，60 分钟累计 347 条、
  涉及 573 个出口。

一波拒绝 ≈ 一分钟上百次请求 × 每次最多停用 2 个出口（`tierGateFails <= 2`），
用原来硬编码的 15 分钟，池子会在**一波之内**被清空，然后在闸门已经放开之后
继续瘫十几分钟 —— 客户端看到的就是 `403 FreeTierError` 或 `503 auth_unavailable`，
和"模型下线""密钥失效"完全同形。现在改成可配置、默认仍 15m、生产上设 `2m`：

```yaml
free_tier:
  block_duration: 2m
```

判据：一次普遍拒绝之后，如果 `status=200` 迟迟不回来，先看
`docker logs zenproxy | grep "egress blocked for free-tier refusal"` 的速率——
还在刷就是闸门真关着，不刷了却仍 403 就是池子被自己的停用拖住了（等 `block_duration` 到点，
或按 `deploy/scripts` 的注入纪律清掉状态文件里的停用再滚动重建）。

### 6.2 闸门拒绝是**按出口**的，不是按负载 —— 池子必须记住"谁能过"

2026-10-17 三小时实测（两副本合计 10,192 次封装后的上游尝试、220 次放行）：

| 出口 | 放行 | 被拒 |
| :--- | ---: | ---: |
| `resin-slot-199` | 126 | 1 |
| `static-161` | 104 | 0 |
| `static-1040` | 39 | 0 |
| `static-897` | 15 | 0 |
| `resin-slot-386` | 13 | 4 |
| `static-44` | 8 | 0 |

**220 次放行全部来自这 6 个出口**，另有 **804 个出口被拒 ≥5 次、一次都没被放行**。
而且放行率与负载无关（某分钟 124 次尝试→2 次放行，另一分钟 330 次→10 次），
所以这不是"我们打太猛把自己打死"，是**上游按 IP 划分了一个被标记的人群，里面夹着几个干净 IP**。

在此之前 `PickForAccount` 对"谁能过闸门"没有任何记忆：探活不走闸门（§6.1），
所有出口在它眼里同样健康，于是每个免费请求都在 1,613 个出口里均匀抽签 ——
0.4% 的概率撞到那 6 个，这正是池子整体 2.2% 放行率的来源。

现在补上这一层（`member.gatePassUntil` + `MarkGatePass`）：

- 封装后的免费请求拿到 2xx → `MarkGatePass(exit)`，该出口进"过闸名单"（TTL 30 分钟）；
- `PickForAccount` 把过闸名单排在**所有健康出口之前**（且不受"别的账号正在用"这个
  平票规则影响：共用好出口只损失一点 per-IP 额度，从好出口挪开则整条请求报废）；
- 被拒立刻 `BlockFreeTier`（2 分钟），同时**撤销过闸记忆并把质量分 -3**。
  普通流量完全不选停用出口；门控请求只在「没有干净过闸出口」时才退到
  仍被证明过、但正被停用的出口。分数掉到 8 以下就不再优先重试，
  所以翻脸的出口（`resin-slot-199` 那种旧分 16、现在次次被拒）不会被无限重试；
- 名单、次数、质量分写进 `exits-state.json`（`gate_pass_until` / `gate_passes` / `gate_score`），
  重启不用重新抽签；
- 健康面板每条出口多了 `gate_pass_until` / `gate_passes`，
  "现在哪些出口能服务免费模型"可以直接看，不必翻日志。

**会话粘性也必须让路**（`ExitForSessionGated`）。会话表是**落盘并在重启后恢复**的，
里面全是"池子还不知道谁过闸"时建立的绑定。粘性优先于是意味着：这些会话会永久钉在
闸门拒绝的出口上。播种后第一次实测就是这个形状 —— 346 次被拒里 **342 次来自名单外出口**，
而 8 次成功全部来自名单内。所以对免费请求：绑定只在它仍是**过闸出口**时才保留，
否则改绑到当前名单内的出口（代价是一次上游 prompt cache，收益是这一轮有回答）。

复现这条判据（任意窗口都可跑）：

```bash
# 按出口统计放行 / 被拒
docker logs --since 180m zenproxy-zenproxy-a-1 2>&1 | grep -oE 'exit=[A-Za-z0-9:_-]+' | ...
# 放行看 "upstream cache usage"，被拒看 "upstream refused this free-tier request"
```

三条容易踩错的推论：

1. **别把 2.2% 当成"闸门关着"**。它不是开关，是一个小人群里的抽样；关窗时正确动作
   是换到名单内的出口，而不是加更多重试。
2. **别用探活结果判断"出口好不好"**。所有 1,613 个出口探活都健康，
   794 个在闸门面前一次都没过。
3. **停用时长（§6.1）和过闸名单是配套的**：停太久会把刚证明可用的出口也关掉，
   2 分钟的停用 + 30 分钟的过闸记忆才是这套组合的正确取值。

### 6.3 探活隔离会掐死过闸出口 —— 两个判据测的不是同一条链路

过闸名单上线后再实测，发现还有一处反向踩踏：**探活（ipify/opencode.ai:443）把这 6 个
过闸出口里的 4 个同时隔离了**（`consec_fails` 3–4，隔离 5 分钟），而闸门当时仍在放行它们。
名单瞬间只剩 2 个可用，池子掉回对非过闸出口的 2% 抽样，那一窗口内**每一个请求都被拒**。

两种判据的强度不对称：探活失败只说"这条隧道到不了探活主机"，过闸记录说"这条隧道**把我们
真正关心的那个请求**送到了上游并拿回 2xx"。所以对**门控请求**（`PickForAccountGated`），
被隔离但仍在过闸名单里的出口保留为候选，排位紧跟在"干净过闸出口"之后；普通流量完全不受影响
（`PickForAccount` 仍是原来的排序，付费模型继续回避被隔离的出口）。免费拒绝（`BlockFreeTier`）
仍然优先于这条豁免 —— 停用的出口一个都不给。

判定口径固定为：

| 场景 | 排序 |
| :--- | :--- |
| 门控请求 | 干净过闸（按质量加权） → 过闸但探活降级/隔离 → 过闸已到期但历史分高 → 已过闸但正被停用 → 健康未过闸 → … 原序列 |
| 普通请求 | 健康 → 降级 → 冷却/停用 → 隔离 → 退役（完全不看过闸名单） |

### 6.4 "过闸"不等于"能跑完"—— 名单内出口质量差三倍

过闸名单只回答"闸门让不让过"，不回答"过完之后这条隧道撑不撑得住长连接"。实测同一批放行里
断流比例差三倍（`the upstream stream ended without a completed response`）：

| 出口 | 放行 | 断流 | 直觉 |
| :--- | ---: | ---: | :--- |
| `static-161` | 131 | 32 | 好隧道 |
| `static-1040` | 69 | 70 | 一半一半 |
| `static-44` | 53 | 94 | 断的比成的多 |
| `static-897` | 38 | 45 | 一半一半 |

而当时的过闸层对这四个**一视同仁、均匀轮转** —— 于是好隧道和坏隧道拿到同样的份额，
三分之一的尝试要重掷，用户看到的是多余的几十秒。修法是一条有界、会自愈的质量分：

```
gateScore: 每次过闸 +2，每次断流 -3，上下限 [+16, -8]
份额:      score >= 8 → 3 份   0..7 → 2 份   负分 → 1 份
```

为什么用"有界分"而不是"放行/断流比"：比值会被一辈子拖着走（`static-1040` 开局 39/70，
之后一直翻不了身），而有界分在一次坏窗口之后，几次干净放行就能走回中性 —— 下一个请求
应该相信最近的事实。为什么断流扣得比放行加得多：两种证据不等价，漏掉一次放行的代价是
"多花一次尝试"，放过一次断流的代价是"用户白等一轮"。

两个配套细节：

1. **过闸已到期但历史分高的出口仍会被重试**（`gatePassersLapsed`）。过闸记忆只有 30 分钟，
   而干净 IP 的寿命远长于此 —— `resin-slot-199`（126 次放行）和 `resin-slot-386`（13 次）
   就是这样静悄悄掉出名单，然后再没被碰过（它们只在"名单内全被停用"时才有机会被抽到，
   而恰恰是那一次尝试才能把它们重新证明出来）。现在这类出口排在"从未过闸出口"之前先试一次：
   代价是一次尝试，命中率远高于 0.4% 的盲抽。
2. **只在出口"曾经被放行过"时才记断流**。给 1,600 个从未过闸的出口累计断流没有意义，
   只会让面板那一列变成噪声。

配套地，风暴兜底（连续传输失败后强制 `direct`）对门控请求**只在 `direct` 本身过闸时**才生效：
`direct` 曾经是这个池子里被拒最多的出口，而每一次这样的兜底都等于提前判这次请求死刑。


