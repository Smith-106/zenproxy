# 变更日志

本文件记录每个发布版本的用户可见变化。格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

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

### 新增

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

### 文档

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
