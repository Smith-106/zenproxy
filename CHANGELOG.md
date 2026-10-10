# 变更日志

本文件记录每个发布版本的用户可见变化。格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 新增

- **新增 `scripts/redact.py` 与 `docs/SECRETS.md`：把脱敏从「手写正则」改成「过工具」。**
  这个仓库的历史里有**六次**凭据被打印进终端/日志，六次同一个原因：脱敏者枚举了自己
  以为存在的模式。新增的通用脱敏器按**键名**与**值形状**两个独立信号判定（任一命中即
  遮蔽），支持 JSON / YAML / `.env` / `env` dump / 纯文本，可读文件也可接管道；
  `--test` 把历史泄露形状当用例跑（7/7 通过）。已在真实文件上验证：`.env`（5 个密钥）、
  `state.json`（明文 `api_key`）、`docker exec env` 全部遮蔽且**程序化确认零漏出**，
  同时对正常输出的**误伤为零**（status JSON 20 个字段原样保留）。
  `docs/SECRETS.md` 记下六次事故各自的具体形状（含「打印 flow-style 整行」这个犯过两次
  的坑）、这个部署里密钥都在哪、以及漏了之后按「能不能自己换」分类的处理步骤——
  并明确点出**改本地副本而外部仍有效等于没轮换**，判据是拿旧凭据必须失败。
- **新增 `scripts/rotate-cpa-inbound-key.py`：轮换 CLIProxyAPI 入站密钥。**
  刻意做成定点行替换而不是 YAML load/dump 往返——配置是约 25 KB 的手写文件，
  `safe_dump` 会把注释全部丢掉。脚本进出一路断言：`access:` 块唯一、列表项恰好一条、
  写回后除该 key 外**结构完全一致**、旧 key 无残留。实测只改动了一行（24745→24747 字节，
  差值是给值加的两个引号），并在 CPA 上验证：不带 key 401 `Missing API key`、
  旧 key 401 `Invalid API key`、新 key 200。
- **文档补上 sub2api 改密码的真实端点。** 探测下来 `PUT /api/v1/user/password`
  （body `{"old_password","new_password"}`，`new_password` 有最短长度要求），
  哈希是 bcrypt —— **不需要碰数据库**。此前在 `/api/v1/admin/users/*`、`/api/v1/auth/*`
  一路 404，容易误判成「只能去控制台点」。

- **文档补上「哪些故障 IP 轮换治不了，只能靠账号冗余」**（`docs/NEW-API.md`）。
  界线是：zenproxy 的轮换只对「可归因于出口 IP」的失败生效（按 IP 限额、地区封锁）。
  上游回「这个模型现在不可用」时换出口毫无意义——那是模型的状态，不是 IP 的状态。
  实测 `step-5-preview-free`（`ling-3.1-flash-free` 同样）会间歇返回
  `403 Upstream request failed: Endpoint is unavailable.`，而该模型在三个账号的
  `/v1/models` 里都存在、权限没问题；同一时刻直连是 zen-01 失败、zen-02/zen-03 成功。
  **换账号有用，换 IP 没用**——new-api 的渠道级重试正好补上这一层（日志里渠道 16
  403、渠道 19 成功，客户端拿到 200；单发可能撞上，连打 5 次得到 5/5）。
  附了一张「故障类型 × 哪一层能治」的对照表，并点明推论：**只挂一个账号时那 403
  会直接漏给客户端**，因为单渠道没有第二个可重试的选择。

- **新增 `scripts/set-newapi-channel-status.sh`：改 new-api 渠道的启用状态。**
  起因是一个很容易被误判成「只能去控制台点」的不对称：**`PUT /api/channel/` 改不了状态**，
  body 里只要有 `status` 就回 `{"message":"Invalid parameters","success":false}`，
  而 `POST` 建渠道时带 `status` 却完全正常。正确端点是独立的一条
  `POST /api/channel/{id}/status`，body `{"status":1|2|3}`
  （1 启用 / 2 手动禁用 / 3 自动禁用）。脚本打印改动前后状态，并列出每个模型
  还剩几个启用渠道——**只有 1 个就说明那个模型不冗余**。
  ⚠ 该端点**不是只读的**：探测它就会真的改状态。
- **文档补上「只暴露免费模型」和「一个模型要有多个渠道才是冗余」两节**（`docs/NEW-API.md`）。
  实测生产里 26 个非免费模型**全部来自单个渠道**，其余渠道本来就是禁用的，
  所以关掉这一个渠道即可，不必逐个去关；请求非免费模型会得到
  `503 model_not_found` + `No available channel for model ...`。
  冗余那节记录了实测数据：`big-pickle` 挂两条渠道时 30 次请求 **15/15 均匀分流**，
  临时禁用其中一条后 **6/6 全部由另一条承接**。
  同时记下一条硬约束：**渠道的模型清单必须和各账号的真实权限对齐**——实测三个账号的
  免费模型权限并不相同（zen-02 没有 `big-pickle`），所以那条渠道的清单要去掉它，
  否则 new-api 会把 `big-pickle` 分给没有该权限的账号、拿到 `403 Model access is disabled`。
  别从账号数量上假设等价，要用 `/v1/models` 逐个比对实际权限集。

- **文档补上「`docker restart` 与 `docker compose up -d` 不能互换」**。判据是改的东西
  在容器的环境里还是在挂载的文件里：改 `config.yaml`（bind mount）用 `restart` 就够；
  改 `docker-compose.yml` 的 `environment:` 或 `.env` 里被 compose 引用的变量
  **必须用 `up -d` 重建**——`restart` 不会重新注入环境变量，容器仍用创建时的旧 env。
  实测踩过：往 `.env` 加 `ZEN_KEY_02`、compose 加引用行之后用了 `restart`，变量没进容器，
  zenproxy 的启动闸门直接拒绝启动（`api_key still contains an unexpanded ${ZEN_KEY_02}`），
  容器进入 `restarting (unhealthy)` 循环、**整个入口下线约一分钟**。
  这个闸门是刻意的（宁可起不来，也不把字面量当 key 发给上游），恢复动作是
  `docker compose up -d --force-recreate`。`docs/RESIN.md` 里补了对照表与一行自查命令。
  同时更正 `docs/GATEWAY.md` 里已经过时的一句：`llm-internal` 现在声明在 compose 里，
  重建不会再丢，不必再手工 `docker network connect`。

- **文档警告：在 config.yaml 里删账号是无效的**。实测手删 zen-02 后重启，
  启动日志照旧 \ccounts=2\、\/admin/api/accounts\ 里 zen-02 依然在——因为
  egistry.Load()\ 先把 \state.json\ 里的账号**全部**恢复，egistry.Seed()  才补配置里 state 还没有的，且遇到同 ID **直接跳过**（源码注释：Existing state
  always wins, so an operator editing the file by hand cannot clobber keys that were
  rotated through the admin API）。所以状态文件对账号是权威的、配置只是播种。
  唯一正确的删除路径是 \DELETE /admin/api/accounts/{id}\（先 eg.Delete()\ 再
  eg.Save()\ 重写状态，实测删除后重启不复活）。两份 \config.example.yaml  都在 \state_file\ 上方补了这段警告，并说明手改配置只应作为「让配置与状态保持一致」
  的配合动作——否则哪天 state.json 被删掉重建，那个账号会被重新播种出来。

- **补回两个「能用但够不着」的免费模型**：muse-spark-1.3-contributor-free 与
  muse-spark-1.2-contributor-free 只在 \/responses\ 上接单，早就被
  \scripts/add-responses-only-models.sh\ 加进了 zenproxy 的 \ree_tier.models\，
  但**没人同步 new-api 渠道的模型列表**，于是 \bilities\ 表里没有这两行，
  客户端一问就是 Ń model_not_found\——看着像渠道配错，其实是登记漏了。
  实测这两个模型本身完全可用（直打 zenproxy 的 \/zen-01/v1/responses\ 返回 200
  \status=completed\），补登记后经 new-api 发送「你好」两个都第一次就 200。
  文档新增一节说明**两份列表（zenproxy 的 \ree_tier.models\ 与 new-api 的
  \bilities\）不会自动同步、漂移时双方都不报错**，并记下 new-api 那个坑：
  \PUT /api/channel/\ 的 body 里只要带 \status\ 就恒定回
  \Invalid parameters\，与「字段写错」无法区分。

- **sub2api 接入实测通过，并把两处会静默拦住集成的坑写进了文档**。
  sub2api 里能接 zenproxy 的是**代理**那一层而不是上游那一层——它没有「自定义
  OpenAI 兼容上游」的概念（`channels` 表只有 `name/model_mapping/pricing`，没有 URL
  字段），但 `proxies` 表有 `protocol/host/port/username/password`，正好对应正向入口。
  实测：探针 `success=True "Proxy is accessible"`，`latency_ms=7687`，
  且**返回的出口 IP 是 `80.15.99.54` 而不是宿主 IP**——最后这条才说明流量确实
  从 zenproxy 的出口池出去了。
  两处坑：① **下游必须与 zenproxy 在同一 docker 网络**（生产默认只接
  `new-api_default` + `resin_default`，而 CPA 在 `llm-internal`、sub2api 在自己的网络上，
  不接网络容器名解析不了；`docker compose up -d` 重建还会丢掉手工接的网络）；
  ② **`allowed_hosts` 要放行下游的健康探针**（sub2api 探 `api64.ipify.org`，
  zenproxy 默认只放行 `opencode.ai`，不放行则那条代理永远显示 failed）。
  这两条对任何下游都成立，之前没写过。
- **LiteLLM 兼容改为实测，并纠正了模板里一处会让人查错方向的错误**。
  `deploy/litellm.config.yaml` 原本写着「api_base 不带 `/v1`」，**这是错的**：
  LiteLLM 的 `openai/` provider 把 `api_base` 和 `/chat/completions` **直接拼接**，
  它自己不补 `/v1`。
  - 错误写法的症状**没有指向性**：不是「找不到路径」，而是掉进 zenproxy 的反向入口、
    第一个路径段被当成账号名，得到 `404 unknown or disabled account: chat`。
    看到这句话的人会去查账号配置，而账号配置没有问题。
  - 真起了一个 LiteLLM 容器把两个变体并排测：带 `/v1` 非流式 200
    （`object=chat.completion`、`usage` 含 `cached_tokens`、`finish_reason=stop`）、
    流式 200（`text/event-stream`、142 个事件、以 `[DONE]` 收尾）、
    经 zenproxy `model_list` 改名后同样 200；不带 `/v1` 则稳定 404。
  - `deploy/litellm.config.yaml` 的**路线 A 现在是可直接编辑使用的配置**（只改
    `<模型名>` 一处），路线 B 保留为注释；反代模式的 `api_base` 也补上了 `/v1`。
  - 顺带实测到模板里早有的论断：**LiteLLM 的 cooldown 确实按 deployment 记账**——
    上游连续拒绝后它回 `No deployments available for selected model … All
    deployments for selected model are in cooldown`，期间一个上游请求都不发。
    这正是「统一入口优于 N 条 deployment」的理由。
- **`scripts/check-api-base.sh`：填 base URL 之前的自检**。
  少写或多写一个 `/v1` **在两个方向上都错得起**（LiteLLM/CPA 要带、new-api 不能带），
  而症状在三套系统里有三副面孔且都不提 base URL。脚本拿 `<base>/models` 探一次，
  错时报出实际错误**并列出两个候选写法各自的结果**。三种情形实测过：
  带 `/v1` → 200；不带 → 404 `unknown or disabled account: models`；
  多写一层 → 400。
- **测试：`/v1` 前缀的边界被钉住了**。
  新增 `TestAPathWithoutV1IsAnAccountLookupNotAGatewayRoute`——断言缺前缀的请求
  **不会**被网关服务，且错误里必须提到账号。后者是有意的：那句「账号」正是调用方
  唯一能据以认出误读的线索，也是防止有人日后「顺手修好」`/chat/completions`
  （账号真有可能就叫 chat，那样两个入口就无法区分了）。
- **下游导入实测：CLIProxyAPI 已打通，并纠正了文档里一处会让人白查半天的错误**。
  统一入口对外就是一个 OpenAI 兼容上游，但**「base URL 带不带 `/v1`」各家相反**。
  - 原文档写「统一不要带 `/v1`」，并把 new-api / CPA / sub2api 列成同一个填法。
    **对 CPA 是错的**：它自己的十个 provider 全部带 `/v1`
    （`https://integrate.api.nvidia.com/v1`、`http://sub2api:8080/v1` …），
    填成不带就是 `/v1/v1/chat/completions` → 404，而聚合器通常把这归结成
    「上游不可用」，于是你去查冷却、换 key、查出口，全查错方向。
  - 本轮改用**独立别名**（`zenproxy-test`）验证 CPA 接入，因为同名模型可能同时
    存在于别的 provider 上，用真名无法证明请求走了 zenproxy。
    实测链路：CPA 载入 provider（57 → 58 个 client）→ 别名出现在 `/v1/models`
    → 请求转发到网关 → 网关认证并选账号/出口 → 上游返回 `PONG`。
  - 记录了两条**非显然的运维现象**：① CPA 会把失败的凭据冷却，并把它
    **从 `/v1/models` 里摘掉**（实测 46 → 45 个模型），请求变成
    `auth_unavailable`——这不是配置错；② sub2api 有 `SECURITY_URL_ALLOWLIST_*`，
    默认关闭所以能直接用，但一旦打开，私有主机名和 http 都会被拦，且报错不提及白名单。
- **`scripts/verify-openai-compat-contract.sh`：导入前的对外契约验证**。
  聚合器掉一个上游几乎从来不是因为模型少了，而是某个响应缺了它要解析的字段，
  表现出来却是一句看不懂的 `no auth available`。脚本查形状不查模型：
  `/v1/models` 是不是 `object=list`、每项有没有 `id/object=model/created/owned_by`；
  对话有没有 `object=chat.completion`/`choices[].message`/`finish_reason`/`usage`；
  流式是不是 `text/event-stream` 且以 `data: [DONE]` 收尾；错误 key 与未知模型
  是不是规规矩矩的 4xx JSON。可指向**任何** OpenAI 兼容网关，不是 zenproxy 专用。
  在临时实例上实测 18/18 通过。
- **`scripts/redact-config.py`：按结构递归脱敏的配置打印器**。
  上一轮我用按行的正则读 CPA 配置，漏掉了 YAML 列表项，把一整条入站密钥打进了会话记录。
  这次换成正则匹配**键名**、并让键名沿列表**向下传递**，所以 `keys:` 下的每一条都会被
  替换而不是只有第一条。实测在 CPA 配置上正确遮蔽了 41 条 nvidia key 等全部凭据。
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

- **正向代理模式的 CONNECT 在真实二进制里从来没到达处理器**——https 上游全部 404，
  而所有 LLM 上游都是 https，所以这个入口实际上等于没有。
  原因是路由不是代理实现：`http.ServeMux` 按 `r.URL.Path` 分发，CONNECT 请求
  **没有路径**（请求行是 `CONNECT host:port`），任何模式都匹配不上，mux 自己的
  `404 page not found` 先生效，实现隧道的代码根本没被调用。
  它错了很久没被发现，因为症状完全没有指向性：curl 说
  `Received HTTP code 404 from proxy after CONNECT`（像目标的问题）、日志是
  `method=CONNECT path="" status=404` 且不含账号名（像普通的未知路径）、而同一监听上的
  反代入口与统一入口都正常。**测试也抓不到**：`internal/forward` 的 fixture 把
  `ServeForward` 挂在它自己的 server 上，CONNECT 从来没经过任何 mux。
  是接第二个聚合器时才暴露的——它的管理面接受了代理记录，但探针一直失败，
  而失败既不是凭据、也不是白名单、也不是目标。
  修法是在 mux 之前把 CONNECT 拦出来（`tunnelFirst`），只动 CONNECT 一个方法。
  实测对照：修复前 `status=404`，修复后 `status=200`，经正向入口取
  `https://opencode.ai/zen/v1/models` 返回 200。
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
