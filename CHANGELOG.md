# 变更日志

本文件记录每个发布版本的用户可见变化。格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 新增

- **地区限制：按「出口 × 模型」记账的第二条轴**。上游用 `403 RegionError` 说
  "这个模型在你这个出口所在的国家不可用"。以前它落到 `ClassAuth`，不是 IP 可归因的，
  于是**根本不轮换**——调用方直接收到那句 "not available in your country"，
  而实际上换个出口就能用。
  - **为什么不能按出口记账**（实测）：同一个出口、同一秒钟，`muse-spark` 回
    `403 RegionError` 而 `big-pickle` 回 `200`。所以地区属于**出口和模型这一对**。
    按出口记账会因为一个模型在该国不可用，把一个**对其他模型完全正常**的出口整个
    踢出轮换——错得更贵的那个方向。
  - 新增 `ClassRegion` 判型（`IPAttributable()` 为真），轮换时**带上模型名**
    （`RotateFor`），避免盲轮又落到另一个同样被限的出口。
  - 记账带 **6 小时信任窗口**：额度有上游给的回归时刻，地区**没有**——没有任何东西
    会通知你某个国家重新开放了。信太久会继续跳过本来能用的出口；信太短则每个请求
    白花一次尝试去重新发现已知事实。实测这些限制跨小时稳定，所以取长的那端。
  - 与额度账本**完全分开**：清一个不会清另一个。既不入 `QuotaCooled`（那会凭空造出
    一个时刻），也不因为 403 就设冷却。
  - 面板：`GET /admin/api/status` 新增 `region` 段；`GET /admin/api/proxies` 每个出口
    新增 `region_blocked_for`，而 **`healthy` 仍是 `true`**——被地区限制的出口是好的，
    报成 down 会让操作员去排查一个完全正常的代理。
  - 新增 `POST /admin/api/proxies/{id}/clear-region`（幂等，返回清掉几条）。
    地区没有截止时刻可等，所以只能靠窗口到期或操作员发话；没有这个端点的话，
    唯一的补救是重启进程。
- **统一入口：一个 URL，一个 Key**（`gateway` / `pools` / `model_list` 三段配置，默认关闭）。
  新增第三种入口形状 `POST /v1/chat/completions`、`POST /v1/messages`、`GET /v1/models`，
  用 `Authorization: Bearer <key>`（或 Anthropic 系的 `x-api-key`）鉴权。**key 绑的是池，不是账号**，
  所以调用方永远不需要知道账号叫什么。原有的 `/<账号ID>/` 反代与正向代理模式一行未改，三者并存。
  设计说明见 `docs/GATEWAY.md`。
  - **失败时自动换账号**：`pools[].on_rate_limit` 决定上游拒绝一个请求后切什么——
    `rotate-exit`（换出口 IP，应对按 IP 限额）、`rotate-account`（换账号，应对按 key 限额）、
    `rotate-both`（默认：先换 IP，同一账号烧掉 2 个出口后再换账号）。
    全部为**被动触发**（收到 429/5xx/传输错误之后），不做预算式提前切换——
     但上游主动告知的**额度重置时刻**会被记账并提前规避，见下面那条。
     一次请求最多 8 次上游尝试，避免大池子 + 慢上游把单次请求拖到几分钟。
  - `model_list`：LiteLLM 同名概念，`model_name` → `pool` + 可选 `upstream_model` 名字翻译，
    支持 `"*"` 通配（精确匹配永远优先）。**整段不写 = 所有模型名原样透传**，
    因为「一个 URL + 一个 Key，池子自己挑账号」才是最省事的用法。
  - `GET /v1/models` 报这个 key 的池能服务什么；池有通配时再去上游拉一次真实清单合并进去
    （best-effort，上游不通也照样返回配置里的名字），方便下游网关自动导入模型列表。
  - 启动期全量校验这几种「只会表现为请求时 401/404、原因完全看不见」的错：
    空 key（会匹配所有匿名请求 → 开放中继）、重复 key id、池不存在（报错里列出所有已知池名）、
    key 里还有未展开的 `${VAR}`、`gateway.enabled` 但没有任何 key、拼错的策略名。
  - **池是从账号推导的**：`Account` 新增 `pool` 字段，省略即回落到 upstream 名。
    因此写 `pool` 之前，每个 upstream 天然就是一个池——老配置一行都不用改。
  - 管理面板 `/admin/api/status` 新增 `gateway` 与 `pools` 两块视图（**不含 key**，只有 id/pool/name），
    账号视图新增 `pool` 字段。
  - 鉴权用常量时间比较：逐字节提前返回会让能测时间的攻击者一个字符一个字符地把 key 试出来。
- **额度账本：把上游自己说的额度重置时刻记下来，下次不再往那个出口发。**
  对按 IP 限额的上游，被动轮换有一个固定成本——**每次都要先把请求发出去，被拒了才知道
  这个出口不能用**，而且下一个请求还要重学一遍。真正按 IP 限额的上游会在拒绝时给出
  `Retry-After`，实测这个值是**指向同一个固定重置时刻**（24 个出口读到的数字一起递减），
  所以存的是绝对时刻而不是"再等 N 秒"：存时长的话每个出口会按被发现的时间各自漂移。
  - **额度冷却与健康隔离是两条独立的轴**，代码里分成两个字段、两套清理路径。额度耗尽的
    出口探测**会成功**（探的是出口通不通，不是上游收不收），所以一次 `MarkSuccess` 就把它
    放回池子、然后照旧被拒——有测试专门钉住 `MarkSuccess` 之后冷却必须还在。
  - **主动规避**：反向入口（调用方点了名，只能动出口）在首包前把该账号的出口挪走；
    网关入口（能换账号）优先挑出口没冷却的账号。**池子里所有出口都在冷却时不动**——
    换过去只是白churn一次绑定、花掉一次轮换，结果一模一样。
  - `Pick` 里冷却是**降级而非丢弃**，插在"健康"和"隔离"之间，冷却之间取**最早回来**的那个。
    冷却中的出口通常完全健康（上游拒绝时答得又快又礼貌），只看健康度会把它排最前面。
  - **不因为 403 记账**：`FreeTierError` 是请求形状的问题，跟 IP 无关，冷却它等于把好出口
    踢出池子并把真正的原因藏在一个永远修不好的轮换循环后面。
  - **不设上限、不新增配置项**：上游说多久就多久（日额度的上游返回一天是对的），
    要提前解除用新增的 `POST /admin/api/proxies/{id}/uncool`。
  - `/admin/api/status` 新增 `quota` 视图，`/admin/api/proxies` 每个出口新增
    `quota_until` / `quota_reason` / `quota_hits`，**与 `quarantined_until` 同时存在、
    互不掩饰**——这两个字段一起看才能回答「池子是坏了，还是只是用完了」。设计说明见
    `docs/GATEWAY.md` 第四节。
- `.dockerignore`：此前每次构建都会把 `build/` 与 `.git/` 一起打包送给 builder。
- 运维脚本（都在仓库 `scripts/` 下，部署机上 `sudo` 跑）：
  `zenproxy-status.sh`（一次性自检）、`probe-free-models.sh`（实测哪些免费模型真的可用）、
  `refresh-free-models.py`（换 `free_tier.models` 清单）、`probe-resin-slots.sh`（实测 Resin 可用率）、
  `set-resin-slots.py`（扩池子）、`e2e-free-tier.sh`（端到端验证套壳与 SSE 折叠）、
  `soak-rotation.sh`（连打多次看轮换）、以及 new-api 侧的接入与排障脚本
  `create-newapi-channel.sh` / `e2e-through-newapi.sh` / `verify-all-free-models-via-newapi.sh`。
- `create-newapi-channel.sh` 泛化为**任意 OpenAI 兼容上游**都能接（`CH_NAME` / `CH_BASE_URL` /
  `CH_KEY` 或 `CH_KEY_CMD` / `CH_MODELS` 或 `CH_MODELS_URL`），并新增
  `connect-newapi-to-llm-internal.sh`（打通 new-api 与其它容器的网络，且写进 compose）、
  `extract-cpa-inbound-key.py`、`verify-cliproxy-via-newapi.sh`。

### ⚠️ 升级注意

- **`rotate_on_status` 的默认值新增了 `403`。** 地区限制的轮换依赖它：
  `shouldRotate` 要求 `IPAttributable()` **且**状态码在列表里，所以只加判型不改列表的话，
  默认配置下**依然不会轮换**。
  **如果你自己的 `config.yaml` 里显式写了 `rotate_on_status`，请手动补上 `403`**，
  否则上面那条地区修复对你的部署不生效：
  ```yaml
  rotate_on_status: [403, 407, 429, 500, 502, 503, 504, 520, 522, 524]
  ```
  这个改动只影响两类响应，因为 `IPAttributable()` 先做了闸门：
  `403 RegionError`（本次想要的）和带 `Cf-Mitigated` 的 403（Cloudflare 挑战，
  换个 IP 本来就该试试）。**`FreeTierError` 依旧不轮换**——它由请求的客户端身份决定，
  烧出口只会浪费池子。

### 修复

- **`/responses` 端点现在也套免费额度信封——此前两个本来能用的模型被误判成废品。**
  免费额度门禁对请求形状是**逐端点**的，而此前只对 `/chat/completions` 套壳。
  后果不只是"少一个端点"：`muse-spark-1.3-contributor-free` 与
  `muse-spark-1.2-contributor-free` **只在 `/responses` 上接单**，在
  `/chat/completions` 上恒回 400 `ModelProtocolUnsupported`。于是走 chat 的自适应客户端
  先撞 400，回退去试 `/responses`，又因为没被套壳而拿到 **403 `FreeTierError`** ——
  一个读起来像"鉴权有问题"的响应，实际含义是"你没被套壳"。两个可用模型就这样变成了
  `learned` 里的两条永久废品记录。修复后实测（同一出口，`slot-07` NL）：
  `/responses` 从 403 `FreeTierError` 变成 **429 `FreeUsageLimitError`** ——
  过了门禁也过了协议检查，撞到的只是免费额度用尽，`retry-after` 指向约 2.3 小时后的重置。
  - `/responses` 的字段名和 chat 不同，所以是两个 builder：提示词进 `instructions`
    （**前缀**在调用方自己的 instructions 前面，不是替换），用户内容在 `input`。
  - **工具名补齐跟着调用方的形状走**：chat 用嵌套的
    `{"type":"function","function":{"name":…}}`，`/responses` 用扁平的
    `{"type":"function","name":…}`。读名字时两种都认——只认一种的话，
    `/responses` 的请求会被看成"没有工具"，从而拿到 title 模板，那是错的模板。
  - **只补头是不够的**：实测在 `/responses` 上，标识头完美但没有 prompt 的请求仍然 403。
- **SSE 折叠按端点分流，且 chat 折叠器不再把外来流读成空答案。**
  两个端点的流事件完全不同：chat 是 `choices[].delta`，`/responses` 是
  `response.output_text.delta` 和 `response.completed`。用错折叠器**不会报错**——
  它返回一个合法但为空的答案，客户端会以为成功了。这比报错更糟，所以：
  - `/responses` 的折叠优先**原字节透传** `response.completed` 里的 `response` 字段
    （那个字段本身就是非流式对象），流提前断掉时才从 delta 兜底重建；
  - 两条路都走不通时回错误信封，**绝不返回空答案**；
  - chat 折叠器也只把"带 `choices` 的事件"算作读到了东西——任何别的形状的流都能被
    JSON 解析，但匹配不到 `choices`，此前会被折叠成一个空答案。
- **免费模型验活脚本只看 `/chat/completions`，会漏掉只在别的端点上接单的模型。**
  新增 `scripts/probe-slots-by-protocol.sh`（逐出口 × 逐协议矩阵，一次把
  「地区被限 / 门禁没过 / 端点不对 / 额度用尽」四件事分开）与
  `scripts/probe-responses-slots.sh`（只看单一端点，带 `retry-after` 读数）、
  `scripts/gen-gate-bodies.mjs`（生成套壳后的请求体）、
  `scripts/gen-prodlike-responses-body.mjs`（生成**和线上产出一致**的
  `/responses` 请求体，用来验证上游是否真的认这个形状）。

- **`deploy/install.sh` 的就绪闸门三处误报**，全部会让「一条命令装完」装不动：
  - 占位符检查扫了整个 `.env`，连注释一起算——顶部那句说明文字和注释掉的 LiteLLM 变量里
    都有 `REPLACE_WITH`，于是**四个真实变量全填好也永远报「还有占位值没替换」**。现在只看真正的赋值行。
  - 变量抽取扫了 `config.yaml` 的注释行，把文档里举例的 `${VAR}` 当成必填变量，报「缺少 VAR」。
    现在先去掉注释行再抽。
  - 「变量有没有被 compose 注入」的 needle 找的是 `${VAR}`，而 compose 写的是 `${VAR:?未设置…}`，
    **永远匹配不上**，于是每个变量都被误报成「没注入」。现在按 `${VAR` 前缀匹配。
- **`deploy/install.sh` 在 git 里是 100644（无执行位）**，而文档写的是 `sudo ./install.sh`——
  任何人克隆后都会撞上 `command not found`。已改为 100755。
- **`deploy/config.example.yaml` 缺整个 `free_tier` 块**：照这个模板部署出来的实例，
  `*-free` 模型全部 403 `FreeTierError`——而这份模板正是服务器上实际生效的那份配置。
  已补上，并让 `TestExampleConfigStaysLoadable` 同时加载根模板与部署模板，防止再次漂移。

### 文档

- `docs/GATEWAY.md` 新增第四节「额度账本」：为什么按 IP 限额的上游值得记账、
  额度冷却与健康隔离为什么是**两条不能混的轴**（两个方向各自会错成什么样）、
  两个入口各自的主动规避方式、`Pick` 的分层顺序，以及**账本刻意不做的事**
  （不猜、不设上限、不因为 403 记账、不新增配置项）和各自的原因。
  另含实测小节与 4.8「地区轴」——地区为什么属于「出口 × 模型」而不是出口、
  三处必须同时改（判型 / 带模型的轮换 / `rotate_on_status`）、以及信任窗口为什么是猜的。
- 新增 **`docs/GATEWAY.md`**：统一入口的完整设计说明——为什么「key 绑池而不是绑账号」，
  `on_rate_limit` 三种策略分别对应上游按 IP 限额还是按 key 限额（含默认 2 个出口的账怎么算、
  为什么不是 1 也不是更多），`/v1/models` 报什么，四类下游（new-api / CLIProxyAPI / sub2api / LiteLLM）
  的填法，以及 **Base URL 不能带 `/v1`** 这个踩过的坑。
- `deploy/litellm.config.yaml` 拆成两条路线并说明怎么选：**路线 A（推荐）**一条 deployment，
  账号与出口切换都在 zenproxy 内；**路线 B**保留原来的一账号一 deployment。
  并写明为什么 A 更好：LiteLLM 的 cooldown 是**按 deployment 记账**的，
  「某个账号被限流」只能表达成「把这个 deployment 整体冷却掉」——冷却期间这个账号连试都不试，
  哪怕换一个出口 IP 本来就能用。
- `config.example.yaml` 与 `deploy/config.example.yaml` 都补上 `gateway` / `pools` / `model_list`
  三段（默认 `enabled: false`），并让 `TestExampleConfigStaysLoadable` 一并校验：
  gateway key 指向的池必须真的有账号，否则启动就该报错。
- `docs/FREE-TIER.md` 新增「失败类型速查」：把「列表里有名字」和「真能用」区分开——
  410 `ModelDeprecated`（已下线）、400 `ModelProtocolUnsupported`（名字在售但不接公开端点，
  `chat/completions` 与 `messages` 都不支持）、403 `RegionError`（出口 IP 国家被限）、
  402 `server_error`（付费模型无额度），四种都**换出口换账号都救不了**。
  更正了上一版把 `RegionError` 写成"换出口有机会成功"的说法：实测同一模型从住宅出口出去
  是 `ModelProtocolUnsupported`、从服务器机房 IP 出去才是 `RegionError`，看起来像换出口有用，
  其实只是卡点从地区检查挪到了协议检查。附「怎么把这几道门分开」的重放方法
  （手动补官方标识打任意路径，按响应类型判断走到了哪一步）。
- `deploy/README.md` 新增 §7.1/§7.2/§7.3：脚本化建渠道要处理的坑——为什么必须走管理 API
  （`abilities` 表才是路由，只插 `channels` 渠道永远选不中）、两个接口形状不同
  （`POST` 要包在 `channel` 字段里、`PUT` 是裸对象带 `id`**且不能带 `status`**，
  否则回 `Invalid parameters`）、系统访问令牌是 `character(32)`；
  验证脚本挑令牌时**必须带 `deleted_at is null`**（GORM 软删除，挑到死令牌会得到
  401 `Invalid token`，而服务端只报 `record not found`，极易误判成渠道问题）。
  另记录了接第二个上游（CLIProxyAPI）的三条硬经验：**base_url 绝不能带 `/v1`**
  （new-api 会追加整段 `/v1/chat/completions`，重复即上游 404，而 new-api 报成
  `bad_response_status_code`）、渠道缓存约 60 秒才刷新（刚建好必然是
  `503 No available channel`）、以及 CPA 配置里 `access.api-keys`（入站）与顶层
  `api-keys`（provider 凭据）同名不同义。
- `docs/RESIN.md` 新增「池子该开多大：实测」：两批各 12 个 slot 的可用率是 2/12 与 7/12，
  合计约 38% 且批次间波动极大；默认模板的 2 个成员平均不到 1 个活着，扩到 24 个后
  能稳定有 5–7 个真实出口，连打 10 次免费模型 10/10 成功。
- `docs/FREE-TIER.md` 新增第五节「清单会漂移」：模型名过期表现为 **400 `Model is unavailable`
  而不是 403**，所以门禁学习机制救不了它，必须人工换；附三步换法、推理模型需要更大 token 预算的
  判别方法，以及两种容易被误判成清单过期的失败（`RegionError` 按国家限制、`ModelProtocolUnsupported`）。
- `config.example.yaml` 与 `deploy/config.example.yaml` 的 `free_tier.models` 换成 2026-10-10 实测可用的
  7 个模型（旧清单里的 `ling-3.0-flash-free`、`mimo-v2.5-free` 等上游已全部改名/下线）。

## [v0.1.0] — 首次发布

出口轮换守护进程的第一个版本：把 opencode/zen 账号池接到一个自带探活、隔离与状态码驱动重试的出口层前面。

### 新增

- **两种入口形态，共用一个监听端口**：反代模式 `http://<host>:9000/<账号ID>/<路径>`（渠道填 Base URL，
  上游响应是明文，能看到 403/429/5xx）与正向代理模式 `http://<账号ID>:x@<host>:9000`（渠道填「代理」字段，
  CONNECT 之后是盲隧道）。
- **账号 ↔ 出口绑定与亲和**：一个账号粘在一个出口上，长会话不中途换 IP；绑定的出口健康恶化时自动退回池子里挑。
- **出口池健康管理**：后台探活、连续失败隔离、指数退避、`retire_after` 永久淘汰，以及「池子空了也不回 502」
  的降级取用（退回失败最少的已淘汰出口，并在日志里喊一嗓子）。
- **失败分类驱动的轮换**：`transport` / `proxy_auth` / `rate_limit` / `server` 才算出口的错；
  `auth` 是钥匙的问题，`tier_gate` 是客户端身份的问题——这两类不换 IP、不浪费重试预算。
- **免费额度套壳（free tier）**：把 `*-free` 模型的请求补齐成官方客户端的样子（UA、`x-opencode-*` 身份头、
  `stream: true`、系统提示与工具名配对），让免费额度可以直接使用。只碰列表内/已学会的模型，付费模型一个字节都不改。
- **非流式请求的 SSE 折叠**：免费额度只回 SSE，调用方要单个 JSON 时读完整条流再折回 `chat.completion`，
  保留 `content`、`tool_calls`、`usage`、`finish_reason`。
- **管理 API 与内嵌控制台**：`/admin/` 免构建步骤的单页控制台（总览、免费额度、运行配置只读区、出口表、账号表，
  支持编辑回填/启用禁用/换出口/退休恢复），`/healthz`、`/metrics`（Prometheus 文本），
  鉴权走 `X-Admin-Token` / `Authorization: Bearer` / `?token=`。接口永不回显完整密钥，只给 `sk-a…rect` 提示串。
- **配置即校验**：启动时就把不合规配置顶回去（未展开的 `${VAR}`、启用账号缺 key、引用不存在的上游、
  空的 admin token），并指名道姓说是哪一项。
- **容器部署**：`deploy/` 提供默认路线（挂到已跑着的 new-api 旁边，只加入两个已有 docker 网络）
  与 LiteLLM 备选路线，含幂等 `install.sh`、非 root + 只读根文件系统的 compose 加固。

### 文档

- `docs/FREE-TIER.md`：免费额度门禁的判据、阈值、复现方法与代价。
- `docs/NEW-API.md`：与 new-api 的接入方式与取舍。
- `docs/RESIN.md`：Resin 出口池的用法。
- `deploy/README.md`：部署路线、拓扑与远端安全查看控制台的方式。

[Unreleased]: https://github.com/Smith-106/zenproxy/compare/v0.1.0...HEAD
[v0.1.0]: https://github.com/Smith-106/zenproxy/releases/tag/v0.1.0

### 修复

- **`deploy/install.sh` 的就绪闸门三处误报**，全部会让「一条命令装完」装不动：
  - 占位符检查扫了整个 `.env`，连注释一起算——顶部那句说明文字和注释掉的 LiteLLM 变量里
    都有 `REPLACE_WITH`，于是**四个真实变量全填好也永远报「还有占位值没替换」**。现在只看真正的赋值行。
  - 变量抽取扫了 `config.yaml` 的注释行，把文档里举例的 `${VAR}` 当成必填变量，报「缺少 VAR」。
    现在先去掉注释行再抽。
  - 「变量有没有被 compose 注入」的 needle 找的是 `${VAR}`，而 compose 写的是 `${VAR:?未设置…}`，
    **永远匹配不上**，于是每个变量都被误报成「没注入」。现在按 `${VAR` 前缀匹配。
- **`deploy/install.sh` 在 git 里是 100644（无执行位）**，而文档写的是 `sudo ./install.sh`——
  任何人克隆后都会撞上 `command not found`。已改为 100755。
- **`deploy/config.example.yaml` 缺整个 `free_tier` 块**：照这个模板部署出来的实例，
  `*-free` 模型全部 403 `FreeTierError`——而这份模板正是服务器上实际生效的那份配置。
  已补上，并让 `TestExampleConfigStaysLoadable` 同时加载根模板与部署模板，防止再次漂移。
