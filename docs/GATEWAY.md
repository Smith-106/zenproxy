# 统一入口（gateway）：一个 URL，一个 Key

> 这份文档讲的是 zenproxy 的第二种入口形状。
> 第一种是 **反向模式** `/<账号ID>/<路径>`（见 [README](../README.md)），
> 它要求调用方知道账号名；第二种就是这里说的统一入口，调用方什么都不知道。

---

## 一、它解决什么问题

原来的形状里，**账号名必须在 URL 里**：

```
http://zenproxy:9000/zen-01/v1/chat/completions
http://zenproxy:9000/zen-02/v1/chat/completions
```

于是「换账号」这件事只能由调用方做。LiteLLM 就得为此写两条 deployment
（见 [deploy/litellm.config.yaml](../deploy/litellm.config.yaml) 开头那段注释），
new-api 就得为此开两个渠道。**池子是有的，但池子的选择权在外面。**

统一入口把选择权收回 zenproxy：

```
http://zenproxy:9000/v1/chat/completions
Authorization: Bearer <gateway key>
```

一个 URL，一个 Key。账号、出口、模型映射全在里面决定。

---

## 二、配置

三块，都是**新增的**，不写就完全等于以前的行为。

```yaml
gateway:
  enabled: true
  keys:
    # 一个 key 绑一个池。这是关键：key 绑的是池，不是账号，
    # 所以调用方永远不需要知道账号叫什么。
    - id: main
      key: ${ZENPROXY_KEY}        # 必须走环境变量，见下文「校验」
      pool: zen
      name: 控制台                 # 面板上显示的名字，可省

pools:
  # 池本身是**从账号推导出来的**，不用在这里声明成员。
  # 这里只叠加策略。
  - id: zen
    strategy: round-robin         # round-robin | weighted
    on_rate_limit: rotate-both    # rotate-exit | rotate-account | rotate-both

model_list:
  # LiteLLM 同名概念，连 "*" 通配都一样。
  - {model_name: big-pickle, pool: zen}
  - {model_name: "*",        pool: zen}   # 通配放最后，精确匹配永远优先
```

### 池从哪来

**账号声明自己属于哪个池**，池不需要单独定义成员：

```yaml
accounts:
  - {id: zen-01, upstream: zen, api_key: ${KEY1}, enabled: true}
  - {id: zen-02, upstream: zen, api_key: ${KEY2}, enabled: true}
```

这两个账号没写 `pool:`，于是 `EffectivePool()` 回落到 upstream 名 `zen`，
它们就在同一个池里。**老配置一行都不用改。**

要跨 upstream 组池时，才需要显式写：

```yaml
accounts:
  - {id: free-a, upstream: zen,    pool: shared, api_key: ${K1}, enabled: true}
  - {id: free-b, upstream: zen-go, pool: shared, api_key: ${K2}, enabled: true}
```

### 不写 model_list 会怎样

**全部透传。** 这是刻意的：最常见的用法就是「一个 URL + 一个 Key，
池子自己挑账号」，逼着人先把模型名枚举一遍才能用，等于把简单场景做成难场景。
只有当你**写了** model_list 又没写 `*` 时，未列出的模型才会被 404。

---

## 三、`on_rate_limit`：自动切换到底切什么

这是整个设计的核心。上游拒绝一个请求时，**可能怪 IP，也可能怪账号**：

| 值 | 行为 | 适用 |
| :--- | :--- | :--- |
| `rotate-exit` | 换出口 IP，**不换账号** | 上游**按 IP** 限额（一个 IP 打满了） |
| `rotate-account` | 换账号，**不换 IP** | 上游**按 key** 限额（一把 key 打满了） |
| `rotate-both`（默认） | 先换 IP；同一账号烧掉 2 个出口后，换账号 | 分不清是哪个，让代价自己说话 |

`rotate-both` 的账是这么算的（默认配额：每账号 2 个出口）：

```
请求进来
  ├─ 账号 A / 出口 X  → 429
  │     exitsFor[A] = 1 < 2，所以再给 A 一个机会
  ├─ 账号 A / 出口 Y  → 429
  │     exitsFor[A] = 2 ≥ 2，判断「问题更像在账号上」
  ├─ 账号 B / 出口 Z  → 200  ✓ 调用方看到的是这个
  └─ 池里没别人了 → 回到 A 再换一个出口（有出口就还有希望）
```

**为什么是 2：** 1 的话一个倒霉 IP 就能判死一个账号；再多就是把池子里的好出口
浪费在一个可能真的坏掉的账号上。

**预算上限：** 一次请求最多 8 次上游尝试（`gatewayMaxAttempts`），
`rotate-account` 下上限是池内账号数。没有上限的话，大池子 + 慢上游 =
一次请求要等好几分钟才失败。

**触发时机有两类，下一节分开讲：** 上面这套是**被动**的——收到 429/5xx/传输错误之后
才切。真正按 IP 限额的上游会**明确告诉你额度什么时候回来**（`Retry-After`），那一句
话可以省掉整个"试了才知道"的循环，见下一节「额度账本」。

---

## 四、额度账本：把「试了才知道」变成「不用试」

### 4.1 为什么值得记账

被动轮换有一个固定成本：**每一次都要先把请求发出去，被拒了才知道这个出口不能用**。
一个请求如此，下一个请求还是如此——池子不会记住上一次学到的东西。

真正按 IP 限额的上游会给出一句关键信息：**拒绝时的 `Retry-After`**。

```
HTTP/2 429
retry-after: 7099
```

实测（24 个出口全扫一遍）：这个值**不是**每次重新计时的窗口，而是**指向同一个固定
重置时刻**——各出口读到的数字一起递减，约 2 小时后归零。

| 出口 | 国家 | 读到 retry-after |
| :--- | :--- | :--- |
| slot-01 | FR | 7099 |
| slot-08 | NL | 7037 |
| slot-12 | ID | 7004 |
| slot-20 | RO | 6938 |
| slot-24 | US | 6905 |

所以账本存的是**绝对时刻**，不是"再等 N 秒"。存时长的话，每个出口会按自己被发现的
先后各自漂移，而读这个头的全部意义就是**别再猜额度什么时候回来**。

同一轮实测里，24 个出口中 **4 个返回 200、其余多数 429**（还有几个地区被限、几个
连不上）。池子里"能用"和"额度已尽"是混在一起的——这正是记账能省下请求的场景。

### 4.2 额度冷却和健康隔离是两条轴，不能混

这是设计上最容易做错的地方，两个方向都会错：

| | `quarantine`（健康隔离） | `quota_until`（额度冷却） |
| :--- | :--- | :--- |
| 含义 | 这个出口**坏了** | 这个出口**好好的**，上游就是不收 |
| 谁造成 | 传输失败、TLS、407 | 上游的 429 + `Retry-After` |
| 怎么结束 | **探测成功就清掉** | **只有时间能清掉** |
| 表现 | 探测失败 | 连接快、响应快、一切正常 |

**把冷却做成一种隔离是错的**：额度耗尽的出口探测**会成功**（探的是出口本身能不能通，
不是上游收不收），于是一次探测就把它"修好"、放回池子，然后照旧被拒。

**把隔离做成一种冷却是错的**：坏掉的出口不该等一个上游根本没给的时刻。

代码里因此分成两个字段、两套清理路径，并有一条测试专门钉住这件事：
`TestQuotaCoolExpiresOnTheClockAndNothingElse` 里 `MarkSuccess` 之后冷却**必须还在**。

### 4.3 记账之后怎么用

记下来只是数据，省下请求靠两处主动规避：

| 入口 | 能不能换账号 | 规避方式 |
| :--- | :--- | :--- |
| 反向 `/<account-id>/<path>` | 不能（调用方点了名） | 首包前把**该账号的出口**挪走（`avoidCooledExit`） |
| 网关 `/v1/*` | 能 | 优先挑**出口没冷却**的账号（`pickAccount`） |

**没有更暖的出口时不动。** 如果池子里所有出口都在冷却，换过去只是白白churn一次绑定、
花掉一次轮换，结果一模一样。这时候留在原地更诚实——请求反正会被拒，而这个出口至少
知道自己的额度什么时候回来，日志里会明说：

```
every candidate exit is out of quota; the request will most likely be refused
```

### 4.4 `Pick` 里的分层

冷却的出口在选池时被**降级但不丢弃**，插在"健康"和"隔离"之间：

```
健康且不冷却 → 降级但不冷却 → 冷却中（取最快回来的）→ 隔离中 → 任何未退役 → 最不坏的退役
```

冷却的出口**通常完全健康**——上游拒绝一个额度耗尽的 IP 时答得又快又礼貌——所以只看
健康度会把它排在最前面，专门挑出那个**保证会被拒**的出口。

冷却之间**不轮询，取最早回来的那个**。理由是实测里它们的重置时刻是同一个，分散负载
没有收益；而取最早的意味着额度一恢复池子就在用，不用靠运气撞上。

**冷却绝不删除出口。** 额度回来的出口是池子里最值钱的东西，丢掉等于把马上就能用的
那个成员扔了。单出口池里它照样会被选中——比直接失败强。

### 4.5 账本不做的事

- **不猜。** 上游没给 `Retry-After` 就什么都不记，被动轮换照旧（这条路径在账本之前就
  存在，必须继续工作），有测试钉住。
- **不设上限。** 上游说多久就多久。看着离谱的值多半是你没理解那个上游——比如日额度的
  上游返回一天是**对的**。硬套一个"合理上限"是在用一个自己编的数字覆盖上游明确的
  数字，而且它一旦生效就没有任何症状。
- **不因为 403 记账。** `FreeTierError` 是**请求形状**的问题（见 `FREE-TIER.md`），
  跟 IP 无关，冷却它等于把好出口踢出池子，还会把真正的原因藏在一个永远修不好的轮换
  循环后面。有测试钉住：`TestATierGateNeverCoolsTheExit`。
- **不新增配置项。** 冷却时长来自上游，不需要人来调；要提前解除用
  `POST /admin/api/proxies/{id}/uncool`。

### 4.6 在哪看

```bash
# 哪些出口额度已尽、各自什么时候回来（按回来的先后排序）
curl -s -H "X-Admin-Token: $TOKEN" http://127.0.0.1:9000/admin/api/status | jq .quota
```

```json
{
  "cooled": [
    { "exit_id": "resin-slot-01", "until": "2026-10-10T00:32:11Z",
      "reason": "rate_limit", "after": "1h56m12s" }
  ],
  "count": 21,
  "total": 24
}
```

每个出口的 `quota_until` / `quota_reason` / `quota_hits` 也直接出现在
`GET /admin/api/proxies` 里，并且**与 `quarantined_until` 同时存在、互不掩饰**——
这两个字段一起看才能回答操作员真正要问的那个问题：**池子是坏了，还是只是用完了？**
这两种情况需要完全不同的应对，其中一种是"等"。

```bash
# 手动提前解除（刚充值、或确信那个 Retry-After 是错的）
curl -s -X POST -H "X-Admin-Token: $TOKEN" \
  http://127.0.0.1:9000/admin/api/proxies/resin-slot-01/uncool
# {"uncooled":"resin-slot-01","changed":true}
```

`changed` 是故意留的：第二次调用返回 `changed:false`（幂等，不是错误），而出口名写错
返回 404。不区分的话操作员分不清"没有可解除的"和"我叫错了名字"。

---

## 五、接口

| 方法 | 路径 | 说明 |
| :--- | :--- | :--- |
| POST | `/v1/chat/completions` | OpenAI 形状 |
| POST | `/v1/messages` | Anthropic 形状，鉴权同时接受 `x-api-key` |
| GET | `/v1/models` | 这个 key 的池能服务什么 |

鉴权两种头都认，因为同一个入口要同时伺候两种客户端：

```
Authorization: Bearer <key>     # OpenAI 系
x-api-key: <key>                # Anthropic 系
```

### `/v1/models` 报什么

1. model_list 里**属于这个 key 的池**的条目；
2. 如果这个池有通配（显式 `*`，或压根没写 model_list），再**去上游拉一次真实清单**
   合并进去。

第 2 条是为下游网关准备的：new-api / CPA / sub2api 导入渠道时会自动拉模型列表，
只报配置里写死的那几个名字，导入完还得手工补。

拉取是 **best-effort**：上游不通也照样返回配置里知道的名字，不 5xx。

---

## 六、接下游

统一入口对外就是标准的 OpenAI 兼容上游，所以三类下游都是同一个填法。

> ★ **Base URL 不要带 `/v1`。** new-api 会自己把 `/v1/chat/completions` 拼上去，
> 带了就变成 `/v1/v1/chat/completions` → 404。这条踩过，见 [NEW-API.md](NEW-API.md)。

| 下游 | Base URL | Key |
| :--- | :--- | :--- |
| new-api | `http://zenproxy:9000` | gateway key |
| CLIProxyAPI | `http://zenproxy:9000` | gateway key |
| sub2api | `http://zenproxy:9000` | gateway key |
| LiteLLM | `api_base: http://zenproxy:9000` | gateway key |

### LiteLLM：从 N 条 deployment 塌缩成 1 条

用统一入口之前，LiteLLM 必须**一个账号一条 deployment**，因为
「换账号」只能由 LiteLLM 的 cooldown/fallback 机制做：

```yaml
# 以前（每个账号一条，靠 simple-shuffle + cooldown 兜底）
model_list:
  - model_name: "big-pickle"
    litellm_params: {model: "openai/big-pickle", api_base: "http://zenproxy:9000/zen-01", api_key: "x"}
  - model_name: "big-pickle"
    litellm_params: {model: "openai/big-pickle", api_base: "http://zenproxy:9000/zen-02", api_key: "x"}
```

现在一条就够，账号切换和出口切换都在 zenproxy 内部发生：

```yaml
# 现在
model_list:
  - model_name: "big-pickle"
    litellm_params:
      model: "openai/big-pickle"
      api_base: "http://zenproxy:9000"
      api_key: "os.environ/ZENPROXY_KEY"
```

**注意重试不要乘起来。** 两层的重试是独立的：

| 层 | 换什么 | 解决什么 |
| :--- | :--- | :--- |
| zenproxy `policy.max_attempts` | 出口 IP | 「这个 IP 坏了」 |
| zenproxy `on_rate_limit` | 账号 | 「这个账号被限了」 |
| LiteLLM `num_retries` | deployment | 「这一整路都不行了」 |

用了统一入口之后，LiteLLM 那层其实只剩最后一个作用，`num_retries: 1`
通常就够；两边都设大，理论最大上游请求数是相乘的。

---

## 七、校验

配置在启动时**全量校验**，因为下面每一条出错都只会表现为请求时的 401/404，
原因完全看不见：

| 错误 | 为什么拦 |
| :--- | :--- |
| `key is empty` | 空 key 会匹配**所有**匿名请求——网关变成开放中继 |
| `duplicate id` | 两个同 id 的 key，其中一个永远不可达 |
| `pool is required` / `references unknown pool` | 报错里会**列出所有已知池名** |
| key 里还有 `${VAR}` | 变量没设，字面量 `${VAR}` 会被当凭据发出去 |
| `gateway.enabled` 但 `keys` 为空 | 谁都能进 |
| `unknown strategy` / `unknown on_rate_limit` | 拼错会静默退回默认行为 |

`require_key` 默认 **true**（只要配了 key）。要开匿名访问得显式写
`require_key: false`。

---

## 八、安全

- **key 只以常量时间比较。** 逐字节提前返回会让能测时间的攻击者一个字符一个字符地
  把 key 试出来。
- **`/admin/api/status` 的 gateway 视图不含 key**，只有 id / pool / name。
  面板需要知道有哪些 key，不需要知道 key 是什么。
- **key 不进 argv。** 配置里用 `${VAR}`，从环境变量读。这是仓库通用的凭据纪律。

---

## 九、和反向模式的关系

**并存，不是替换。** 反向模式一行没动：

- 已部署的 new-api 渠道 16 指向 `http://zenproxy:9000/zen-01`，继续工作；
- 反向模式仍然是唯一能**流式转发超大 body** 的入口（统一入口要把 body 读进内存
  才能解析模型名，所以超限时返回 413 并说明改用反向模式）；
- 反向模式永远不会换账号——账号名写在 URL 里，换掉就等于违背调用方的意图。

路由优先级在 `cmd/zenproxy/main.go` 里：`/v1/*` 在**拆账号之前**被拦下，
否则 `v1` 会被当成账号名，所有网关客户端都会收到 `unknown account: v1`。

---

## 十、面板

`GET /admin/api/status` 新增两块：

```json
{
  "gateway": {
    "enabled": true,
    "require_key": true,
    "keys": [{"id": "main", "pool": "zen", "name": "控制台"}],
    "model_list": [{"model_name": "big-pickle", "pool": "zen", "upstream_model": "big-pickle"}]
  },
  "pools": [
    {
      "id": "zen",
      "strategy": "round-robin",
      "on_rate_limit": "rotate-both",
      "accounts": ["zen-01", "zen-02"],
      "models": ["big-pickle"],
      "wildcard": false
    }
  ]
}
```

`accounts[]` 里每个账号也多了 `pool` 字段，能看到它属于哪个池。
