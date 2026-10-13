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

**探活必须做 TLS，不能只做 TCP。** `reachability` 在隧道成功后会对目标主机做
TLS 握手并校验证书。只做 TCP 时，MITM / 假证书 resin slot 会 `CONNECT` 成功被标
healthy，真请求再炸 `x509: certificate is not valid for any names`，再被
`failure_threshold` 挡几枪才隔离——这是 transport 失败量的主要放大器。证书毒
失败现在 **一枪隔离**、连续约 3 次 retire。

**地区全灭就 fail-fast。** `Pick` 不再把已知 `RegionError` 的出口当候选项发出去；
亲和解析（`ExitForModel`）也会跳过对本模型 region-blocked 的绑定。额度冷却仍由
`avoidSpentExit` 在发送前处理（全员冷却时不瞎换绑）。

**瞬时传输错误先粘出口再试一次。** `unexpected EOF` / `connection reset` 等噪声
在同出口重试 1 次再 `RotateFor`，避免长对话中途换 IP 把上游 `cached_tokens` 打掉。
证书毒与 `socks5: general failure` 不走这条粘性路径。

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

### 4.7 部署环境实测

线上跑一遍（24 个出口的 Resin 池，`muse-spark-1.3-contributor-free`）：

**① 三个出口各自读到 429，独立收敛到同一个重置时刻。**

```
resin-slot-04   until=2026-10-10T00:00:01Z
resin-slot-05   until=2026-10-10T00:00:00Z
resin-slot-07   until=2026-10-10T00:00:00Z
```

这是"存绝对时刻而不是存时长"这个决定的直接验证：三次拒绝发生在**不同秒**，
`Retry-After` 的秒数当时分别是 6504/6501/6500 左右，但换算出来的时刻**相差不到 1 秒**。
按"再等 N 秒"记账的话，三个出口会各自漂移出几秒的假差异。

**② 轮换永远不会选中冷却出口——`Pick` 的分层是真的在生效。**

实测：连续 rotate **30 次**，一次都没落到 3 个冷却出口中的任何一个上
（均匀随机的话 30 次里撞上的概率约 98%）。冷却出口被牢牢降级在健康出口之下。

**③ 账本确实改变了下一次请求的行为，而不只是记了一笔。**

要触发主动规避，账号得**真的停在**一个冷却出口上。生产里只有一种情况会造成这件事：
出口是在账号绑上去**之后**才被冷却的（通常是**另一个账号**的请求干的）。

用一次性的临时账号直接停在冷却出口上，发一个请求，日志：

```
msg="moving off an exit whose quota is spent, before sending"
account=zen-quota-probe from=resin-slot-05 to=resin-slot-12
until=2026-10-10T00:00:00Z reason=rate_limit
```

注意 `from` 是那个冷却出口，而请求**还没发出去**——这就是记账相对被动轮换省下的东西：
被动的话要先发一次、被拒、再挪；记账的话直接不发。

复现脚本：`scripts/verify-quota-ledger.sh`（自建自删临时账号，密钥从 `.env` 读且不打印）。

### 4.8 地区轴：第二条按出口的轴，但它不能按出口记账

验证额度账本时暴露出来的：账本把一个账号从冷却出口挪到了 `resin-slot-12`，紧接着那次
请求拿到 **403 `RegionError`**——`slot-12` 当时所在的国家被限了。

当时的判断是"地区应该和额度并列"，这个判断**对了一半**。做实测之后发现另一半是错的：

| 出口 | `muse-spark` / `/responses` | `big-pickle` / `/chat` |
| :--- | :--- | :--- |
| slot-09 (FR) | 400 invalid_request_error | **200** |
| slot-10 (RU) | **403 RegionError** | **200** |
| slot-13 (HK) | **403 RegionError** | **200** |

**同一个出口、同一秒钟，一个模型被拒、另一个模型 200。**

所以地区限制不是"按出口"的，而是**按「出口 × 模型」**的。这一条决定了整个实现形状：

> 如果按出口记账，就会因为一个模型在该国不可用，把一个**对其他模型完全正常**的出口
> 整个踢出轮换——这是错得**更贵**的那个方向。

**三处必须同时改，少任何一处这个修复都是空转：**

1. `403 RegionError` 必须从 `ClassAuth` 里分出来单独判型为 `ClassRegion`。
   `ClassAuth` 不是 `IPAttributable()`，所以原来**根本不轮换**——客户端被告知
   "你所在国家不可用"，而换个出口就能用。
2. `ClassRegion` 必须是 `IPAttributable()` 的，且**轮换时必须带上模型名**
   （`RotateFor(account, excludeExit, model)`）。不带模型地盲轮，很可能又落到
   另一个同样被限的出口上，白花一次尝试还被同样拒绝一遍。
3. `403` 必须在 `rotate_on_status` 里。`shouldRotate` 要求
   `IPAttributable()` **且**状态码在列表中——**只改前两处，默认配置下依然不轮换**。
   ⚠️ **升级注意**：默认值已加 403，但如果你自己的 `config.yaml` 里显式写了
   `rotate_on_status`，**必须手动加上 403**，否则这条修复不生效。

**记账的寿命是猜的，这一点必须说清楚。** 额度有上游给的回归时刻，地区没有——**没有任何
东西会通知你某个国家重新开放了**。所以地区记账带一个 **6 小时信任窗口**
（`regionBlockTTL`）。两个方向的错不对称：信太久，一个本来能用的出口继续被跳过；
信太短，每个请求都要白花一次尝试去重新发现已知的事实。实测这些限制跨小时稳定
（同样两个出口、同样一个模型，每次试都被拒），所以取长的那端，代价是**每次过期一次
尝试**，而不是**每个请求一次**。

**和额度账本的关系：完全分开。** 两本账互不冒充——清掉一个不会清掉另一个，有测试专门
钉住这件事。地区既不入 `QuotaCooled`（那会凭空造出一个时刻），也不因为 403 就设冷却。

**在哪看、怎么清：**

```bash
curl -s -H "X-Admin-Token: $TOKEN" http://127.0.0.1:9000/admin/api/status
# "region": { "blocked": [ {"exit_id":"resin-slot-10","model":"muse-spark-…",
#                           "blocked_at":"…","recheck_after":"5h58m"} ], "count": 1 }

curl -s -X POST -H "X-Admin-Token: $TOKEN" \
  http://127.0.0.1:9000/admin/api/proxies/resin-slot-10/clear-region
# {"exit":"resin-slot-10","cleared":1,"changed":true}
```

`GET /admin/api/proxies` 里每个出口多了 `region_blocked_for: ["模型名"]`，而
**`healthy` 仍然是 `true`**——这是故意的：被地区限制的出口是好的，报成"down"会让人
去排查一个完全正常的代理。

`clear-region` 清的是**整个出口**而不是单个模型：操作员的证据通常是"这个模型现在能用了"，
但一个国家重新开放是同时放开所有模型的，要求他逐个模型点名会把常见情况变成麻烦的那种。

#### 部署环境实测

出口 `resin-slot-10`(RU)、`resin-slot-13`(HK)，模型 `muse-spark-1.3-contributor-free`：

**① 直连 SOCKS 绕开 zenproxy，能看到上游确实在拦**（对照组）：

```
slot-13  muse-spark  /responses       403  RegionError
slot-13  big-pickle  /chat/completions 200  -
```

**② 同一个出口经过 zenproxy，调用方拿不到那个 403 了：**

```
客户端收到: HTTP 502（池子里其余出口已死或额度耗尽）
```

这正是修复的核心——403 被换出口消化掉了，而不是转给调用方。

**③ 记账是「出口 × 模型」：** 账本里只有
`resin-slot-10 ← muse-spark-1.3-contributor-free`、`resin-slot-13 ← muse-spark-…`，
`big-pickle` 一条都没有。

**④ 发包前就规避：**

```
msg="moving off an exit whose country is refused for this model, before sending"
account=zen-avoid-probe from=resin-slot-13 to=resin-slot-24 model=muse-spark-1.3-contributor-free
```

**⑤ 主动轮换途中撞上另一个被限出口，会当场记下来：**

```
msg="exit's country is refused for this model; remembering the pair"
exit=resin-slot-10 model=muse-spark-1.3-contributor-free status=403 class=region recheck_after=6h0m0s
```

注意这一条里的 `resin-slot-10` 是**上一步刚被 `clear-region` 清掉**的：清掉之后下一个请求
撞上它，立刻重新记上。**"清掉"是让代理重新去试一次，不是让上游改主意**——这条日志就是
这句话的证据。

复现：`scripts/probe-region-matrix.sh`（直连对照）、`scripts/verify-region-axis.sh`
（走 zenproxy，自建自删临时账号）。

> 顺带一个测试自身的教训：`verify-region-axis.sh` 第一版把成功条件写成"客户端应当收到
> 403"，结果**修复生效时它反而报失败**，还差点让我以为上游放开了。识别被限出口不能看
> 响应码——修复的全部意义就是那个码不再出现——只能看账本。

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

统一入口对外就是标准的 OpenAI 兼容上游，所以三类下游填的东西是一样的：
**一个 base URL + 一个 gateway key + 让它自己拉模型列表**。

> ★ **但「base URL 带不带 `/v1`」各家相反，这是接下游最容易踩的一步。**
> 原以为可以一句「统一不带 `/v1`」打发掉，实测发现不行——填错的后果是
> `/v1/v1/chat/completions` → 404，而聚合器通常把它归结成「上游不可用」，
> 于是你在冷却、换 key、查出口上白花时间。

| 下游 | base URL | 是否实测 |
| :--- | :--- | :--- |
| new-api | `http://zenproxy:9000` —— **不带 `/v1`** | 实测（渠道 16） |
| CLIProxyAPI `api-keys.openai-compatibility` | `http://zenproxy:9000/v1` —— **带 `/v1`** | 实测（本轮） |
| sub2api（作为上游被别的系统导入时同此） | `http://sub2api:8080/v1` —— 带 | 实测（它是 CPA 里已有的 provider） |
| LiteLLM `api_base` | `http://zenproxy:9000/v1` —— **带 `/v1`** | 实测（本轮） |

CPA 那条的依据是**它自己的十个 provider 全都是带 `/v1` 的**
（`https://integrate.api.nvidia.com/v1`、`https://ollama.com/v1`、
`http://windsurfapi:3003/v1`、`http://sub2api:8080/v1` …），
不是推理出来的。new-api 那条相反，见 [NEW-API.md](NEW-API.md)。
LiteLLM 那条是**跑起来测的**——它把 `api_base` 和 `/chat/completions` 直接拼起来，
自己不补 `/v1`，见下面「LiteLLM」一节。

#### 少写 `/v1` 为什么这么难查

因为症状完全没有指向性。缺前缀的请求**不会**得到「找不到路径」，而是掉进
反向入口（`/<账号ID>/<路径>`），第一个路径段被当成账号名：

```
POST /chat/completions  ->  404 {"message":"unknown or disabled account: chat"}
```

看到这句话的人会去查账号配置，而账号配置没有任何问题。同一个错误到三个系统里
有三副面孔——LiteLLM 包成 `NotFoundError`、CPA 包成 `500 server_error`、
new-api 表现为渠道测试一直失败——**它们没有一句会提到 base URL**。

多写一层同样错（`/v1/v1/chat/completions`），所以两个方向都得实测。部署前先跑：

```bash
scripts/check-api-base.sh http://zenproxy:9000/v1 "$ZENPROXY_KEY"
```

它拿 `<base>/models` 探一次：对就是 200，错就报出实际错误**并给出两个候选写法
各自的结果**，让你直接看出该用哪个。三种情形都实测过（带 `/v1` 200、
不带 404 `unknown or disabled account: models`、多写一层 400）。

### CLIProxyAPI：`openai-compatibility` 的最小可用填法

CPA v8 的 provider 不写在 `openai-compatibility` 顶层段，而是挂在 `api-keys` 下。
`base-url` 属于**组**而不是属于 key，组里可以放多条 key 做加权轮询：

```yaml
api-keys:
  openai-compatibility:
    - name: zenproxy
      base-url: "http://zenproxy:9000/v1"   # ★ 带 /v1
      keys:
        - api-key: "sk-..."                 # gateway key
      models:
        - name: big-pickle                  # 上游真名
          alias: big-pickle                 # 对客户端暴露的名字；省略则用 name
```

接入后 CPA 的日志会打印
`server clients and configuration updated: N clients (... + N OpenAI-compat)`，
数字加一就说明它认了这个 provider。

> **别名必须唯一，且请求要用别名。** 本轮验证时故意用了 `zenproxy-test` 这个
> 独立别名——不是为了好看，是因为**同名模型可能同时存在于别的 provider 上**，
> 用真名根本分不清请求到底走了哪条路。测试时要让「走到了 zenproxy」这件事
> 无法被别的上游冒充。

> **CPA 会把失败的凭据冷却，并从 `/v1/models` 里摘掉它。** 上游抖动导致几次
> 失败之后，该 provider 的模型会**从列表里消失**（本轮实测 46 → 45 个模型），
> 请求变成 `auth_unavailable: no auth available`。这不是配置错了，是 CPA 在保护
> 自己；`routing.cooldown.transient-error-cooldown-seconds` 只管瞬时错误，
> 认证类冷却长得多。排查时先看这句话，别去改 base URL。

### sub2api：两条接法（代理出口 / opencode_go 上游）

#### A. 只换出口 IP：走「代理」表（已实测）

sub2api 的普通上游账号（Claude/Gemini/…）没有「自定义 OpenAI 兼容 URL」字段，
但 `proxies` 表有 `protocol/host/port/username/password`，正好对应正向入口
`http://<账号ID>:x@zenproxy:9000`。

实测结果（生产，`proxies` 里一条指向 zenproxy 的记录）：

```
探针: success=True  "Proxy is accessible"     3/4 次（失败那次是已知的 Resin 抖动）
latency_ms = 7687
ip_address = 80.15.99.54                      ← 出口 IP，不是宿主 IP 217.142.224.178
```

**最后一行才是重点**：sub2api 的流量确实从 zenproxy 的出口池出去了。
填法：

```
protocol = http
host     = zenproxy           # 与 zenproxy 同一 docker 网络
port     = 9000
username = <zenproxy 的账号 ID，例如 zen-01>   # 正向入口用它选账号
password = 任意非空
```

三件必须同时成立的事，缺一件都会失败，而且失败信息都不指向原因：

1. **zenproxy 与 sub2api 必须在同一个 docker 网络。** 生产的 zenproxy 默认只接
   `new-api_default` + `resin_default`，而 sub2api 在 `llm-internal` 上——
   **不接网络的话容器名根本解析不了**，而报错是「连不上 zenproxy」，看起来像下游的问题。
   这一条对**任何**下游都成立，包括 CLIProxyAPI。

   ★ **网络要写进 compose 的 `networks:`，不要用 `docker network connect` 手工接。**
   手工接的网络不在 compose 的期望状态里，下一次 `docker compose up -d` 重建容器就没了，
   于是下游突然全部失败。仓库的 `deploy/docker-compose.yml` 已经把 `llm-internal`
   声明为 external 网络并加进 zenproxy 的服务 networks，照它部署即不会丢。
   （曾经就是靠手工 `docker network connect`，踩过这个坑。）
2. **`allowed_hosts` 要放行它的健康探针。** sub2api 用
   `http://api64.ipify.org?format=json` 探代理，zenproxy 默认只放行 `opencode.ai`，
   于是探针拿到 403、这条代理永远显示 `latency_status=failed`。
   实际转发是好的，但那个红色的 failed 会让人以为是坏的。
3. **正向入口的 CONNECT 曾经是坏的**（`v0.1.0` 及之前），见下面那节。
   https 上游全靠它，所以当时 sub2api 这类客户端**完全接不进来**。

顺带一个它自己的白名单，与上面无关但同样会静默拦掉集成：sub2api 有
`SECURITY_URL_ALLOWLIST_*` 一组环境变量，默认关闭，所以现在能用；但一旦打开，
私有主机名和 http 都会被拦，且报错不提白名单：

```
SECURITY_URL_ALLOWLIST_ENABLED=true
SECURITY_URL_ALLOWLIST_ALLOW_PRIVATE_HOSTS=true      # 容器名是私有主机
SECURITY_URL_ALLOWLIST_ALLOW_INSECURE_HTTP=true      # 内网是 http
SECURITY_URL_ALLOWLIST_UPSTREAM_HOSTS=zenproxy
```

它的上游存在 Postgres 里（`config.yaml` 只有 server/database/redis/jwt），
所以只能走它的管理面加，不能改文件。

#### B. 要用 opencode 免费模型：走 `opencode_go` 账号的 `base_url`（推荐）

`platform=opencode_go` / `type=apikey` 的账号**自带**协议分流。正确姿势不是钉死
`chat_completions`，而是用官方同款的 **`api_protocol=adaptive`** + `api_base_urls`，
再把地址指到 zenproxy：

```
api_protocol = adaptive
account_mode = zen
base_url     = http://zenproxy:9000/zen-01/v1
api_base_urls = {
  chat_completions: http://zenproxy:9000/zen-01/v1,
  responses:        http://zenproxy:9000/zen-01/v1,
  anthropic:        http://zenproxy:9000/zen-01
}
api_key = 任意非空占位（zenproxy 会用账号自己的上游 key 覆盖 Authorization）
pool_mode = true
model_mapping 列入要用的免费模型名
protocol_rules 可自定义；空则走内置 Zen 表（muse-spark-*/grok-*/gpt-* → responses …）
```

这和 sub2api 单测 `account_test_service_opencode_go_test.go` 一致：adaptive 下
账号「测试连接」会按模型选端点——`muse-spark-*` / `grok-*` 打上游 `/responses`，
`deepseek-*` 打 `/chat/completions`，`minimax-*` 打 `/messages`。若把
`api_protocol` 钉成 `chat_completions`，协议规则会被忽略，muse-spark 必 400。

三个 zenproxy 账号就建三条 `opencode_go` 账号（`zen-01/02/03`），都挂到同一个
Opencode 分组；`pool_mode` 会在上游 403/429 时在账号间重试。改完 DB 后**必须重启
sub2api**（或走管理面保存）。

客户端用该分组的 API Key 调 `http://<host>:8082/v1/...` 即可。

**陷阱：上游抖动被当成「账号坏了」→ 临时不可调度。**
sub2api `0.2.x` 把 `connection refused` 等 transport 错硬编码成 durable fault
（`openai_upstream_transport_error.go`），封禁 **10 分钟**；账号上的
`temp_unschedulable_enabled=false` **挡不住**。zen 三号共一个 zenproxy，上游
只是不稳定时也会整组摘调度。策略：**请求级 failover 保留，账号级 10 分钟封禁禁止。**
主机 `clear-zen-transport-unsched.timer`（每 10s）无条件清掉 zen-01/02/03 上任何
活跃的 `temp_unschedulable_*`。脚本：`deploy/scripts/clear_zen_transport_unsched.py`。

**别把 `session_binding_enabled` 当成账号粘性。** 那是登录态 IP/UA 防盗绑定，
不是上游账号 sticky。OpenAI/opencode 路径的粘性靠请求里的 `session_id` /
`conversation_id` / `prompt_cache_key`；CPA 若不带这些头，三号会继续均分，
前缀缓存命中率上不去。

**`health.max_latency` 别压太死。** `1s` 时优先出口常常只剩 3–5 条，免费额度
会打在同一小撮 IP 上，轮换暴涨、缓存更差。Opencode free 实测更合适 **3s**
（慢的仍降为备用，但不至于把池子挤干）。

**免费线路的会话粘性在 CPA 和 sub2api 之间补。** `session-sticky`（`deploy/session-sticky/`）
只挂在 CPA → `sub2api:8080` 这一条上。请求里没有 `session_id` /
`prompt_cache_key` 时，用**第一条 user 消息的前 1500 字**做稳定键，写入
`session_id`、`conversation_id`、`x-opencode-session` 和 `prompt_cache_key`。
同一段对话因此钉在同一个 zen 账号上；别的供应商不经过这个代理。

**zenproxy freegate 不得每轮新造 `ses_*`。** `ApplyHeaders` 以前每次都
`newClientID("ses_")`，上游把每一跳都当成新会话，账号粘住了缓存仍接近 0。
现在复用入站 session / conversation 键（必要时归一成 `ses_*` 形状）；
`x-opencode-request` 仍可每跳新造。

**会话 → 出口粘性对齐 CPA。** CPA 的 `SessionAffinitySelector` 是
`provider::session::model → auth`：显式 session 优先，命中复用，凭证不可用才
换绑（见 [CLIProxyAPI#3613](https://github.com/router-for-me/CLIProxyAPI/issues/3613)）。
zenproxy 对应做 `account::session::model → exit`（`policy.session_exit_ttl`，
默认 1h）。入站 `session_id` / `prompt_cache_key` / `x-opencode-session` 钉住
当前账号的 resin 出口；出口坏了才 `RotateForSession` 换绑。无 session 时仍走
原来的账号→出口亲和。

**muse 的 chat→responses 不得打散前缀缓存。** 两处实测坑：
1. 重写后丢掉 `prompt_cache_key`（session-sticky 白写）；
2. 单轮 `input` 用字符串、多轮改成数组 —— 形状一切换，上游
   `cache_read` 就卡在 instructions+tools 的 ~3–4k。现在始终发 message 数组，
   并透传 `prompt_cache_key` / `prompt_cache_retention`。

**gate tools 前置。** `bash`/`read` 固定排在 `tools` 数组最前（客户端已有则前移
复用，缺则补 stub）。免费门闸不看顺序，但 agent 客户端常抖工具列表；把稳定门闸
对钉在前头，至少能保住 instructions + bash/read 这一段前缀。

**TLS 毒槽加速退休。** MITM resin 口（x509 名不对）用独立的 `poisonConsec`
计数，**探针成功不归零**；满 2 次就 `retired`。以前靠 `consecFails`，探针偶发
握手成功会清零，slot-11/22 一类槽位反复隔离又回流，打散 session→exit。

**muse「丢失思考度」。** chat→responses 曾丢掉 `reasoning_effort`，也不请求
`reasoning.summary`；折回 chat 时又跳过 `output[].type=reasoning`。结果是
OpenCode 选了 xhigh 也像没开思考，思考面板空白。现在：`reasoning_effort` →
`reasoning.effort` + `summary=auto`；CPA/OpenCode 的 `auto` 常常**不带**
effort 字段（用量里是空的），桥接层按 CPA 模型配置默认补 `xhigh`；折回时写入
`reasoning_content` 并在 SSE 里带上。

**cache_read 钉在 ~3441 不一定是故障。** 抓包可见一类流量只有
`[system, user]` 两轮，且首条 user 前 1500 字不变、后半段每轮替换——`pfx_*`
账号粘性正确，但上游前缀无法在「对话历史」上增长，只能命中
instructions+gate tools 信封。真多轮（带 assistant/tool 历史）或自带
`prompt_cache_key`（`01a…`）的会话 cache 可长到数万～20 万。CPA 已开
`passthrough-headers: true`，有客户端 session 头时优先用真键。

**HTTP 499 + ~100s / ~5min。** 多半是**客户端先断开**（OpenCode≈100s，
Pi `httpIdleTimeoutMs` 默认 300000≈5min），不是 zenproxy 主动失败。muse 默认
xhigh + chat→responses 折叠期间原先对客户端静默，更容易被掐。现已：
CPA openresty `proxy_read/send_timeout=3600s`，zenproxy
`upstream_timeout=3600s` / `stream_idle_timeout=1800s`，sticky `urlopen=3600s`；
ReplyAsChat 流式折叠时每 15s 刷 SSE `: keepalive`。Pi 建议
`httpIdleTimeoutMs: 1800000`（30min）。

**全员 FreeUsageLimitError / 429。** resin 出口配额耗尽会冷却到 UTC 次日
00:00。以前 zenproxy 仍会轮询 maxAttempts，客户端空等到 timeout。现在若
没有任何未冷却出口，立刻 `429 + Retry-After`，不再空转。

**429 空等 2–3 分钟。** 来自 CPA `request-retry` × sub2api
`same_account_retry_limit=10`（配置键对本版本无效）。已：CPA
`request-retry: 0`；session-sticky 对长 `Retry-After` 熔断（默认最多 hold
10 分钟再探测），后续请求秒级 429，不再拖到 Pi 499。

**加删账号不要 `docker compose ... --force-recreate`。** 重建空窗里 sub2api 会打到
`connection refused`，三号一起被标成 10 分钟 `temp_unschedulable`，CPA 侧就是
十几毫秒的 HTTP 503。正确路径：

- 增改：`POST /admin/api/accounts`（或 `/opt/zenproxy/scripts/upsert_zen_account.sh`）
- 删除：`DELETE /admin/api/accounts/{id}`（或 `delete_zen_account.sh`）
- 同步改 `config.yaml` / `.env` 只为重启后还能恢复；**运行期以 admin API 为准**
- 只有换 compose 注入的环境变量名等不得不换进程时，才显式
  `recreate-zenproxy.sh recreate`（会有短暂空窗；靠 2s 看门狗尽快解封）

**兜底：** 即便仍有路径把 muse-spark 打成 chat，zenproxy 反向入口也有
chat→responses 桥；但主配置应走 sub2api 自己的 adaptive 协议分流。

### 正向入口的 CONNECT 曾经是坏的

`v0.1.0` 及之前，正向代理模式**只能处理明文 HTTP，https 全部 404**。而所有 LLM
上游都是 https，所以这个入口实际上等于没有。

原因是路由，不是代理实现：`http.ServeMux` 按 `r.URL.Path` 分发，而 CONNECT 请求
**没有路径**（请求行是 `CONNECT host:port`，net/http 解析出空 Path），任何模式都
匹配不上，于是 mux 自己的 `404 page not found` 先生效，实现隧道的那段代码根本没
被调用。

它错了很久没被发现，因为**症状没有任何指向性**：

- curl 说 `Received HTTP code 404 from proxy after CONNECT`——像目标的问题；
- 日志是 `method=CONNECT path="" status=404`，里面有账号名吗？没有——像普通的未知路径；
- 同一个监听上的反代入口和统一入口都正常——像只有代理那一块的问题。

而测试也抓不到：`internal/forward` 的 fixture 把 `ServeForward` 挂在**自己的**
server 上，CONNECT 从来没经过任何 mux。**是接第二个聚合器时才暴露的**——它的管理面
接受了这条代理记录，但探针一直失败，而失败既不是凭据、也不是白名单、也不是目标。

修法是在 mux 之前把 CONNECT 拦出来（`tunnelFirst`），只动 CONNECT 一个方法；
绝对形式请求保持原有优先级（`/admin/`、`/healthz`、`/metrics` 仍然优先于代理分支）。
对应的测试手工拨 TCP，因为 `http.Client` 不配代理不会发 CONNECT，并且**按响应的形状
而不是状态码**判断是谁答的：mux 回 `text/plain`，正向入口回 JSON。

### 导入之前先用契约脚本验一遍

聚合器掉一个上游，几乎从来不是因为「模型少了」，而是**某个响应缺了它要解析的字段**——
表现出来却是一句看不懂的 `no auth available`，或者模型悄悄从列表里消失。
所以先把「别人眼中的你」验一遍，再决定接不接：

```bash
scripts/verify-openai-compat-contract.sh http://127.0.0.1:9000 "$ZENPROXY_KEY"
```

它查的是形状不是模型：`/v1/models` 是不是 `object=list`、每项有没有
`id/object=model/created/owned_by`；对话返回有没有
`object=chat.completion` / `choices[].message` / `finish_reason` / `usage`；
流式是不是 `text/event-stream` 且以 `data: [DONE]` 收尾；错误 key 与未知模型
是不是规规矩矩的 4xx JSON 对象（聚合器靠这个决定冷却还是换 key）。
脚本可以指向**任何** OpenAI 兼容网关，不是 zenproxy 专用。

### LiteLLM：跑起来测出来的三条

在临时实例上真起了一个 LiteLLM 容器（`ghcr.io/berriai/litellm:main-latest`），
配 `api_base` 的两个变体各一条 deployment，同一时刻对比：

| 配置 | 结果 |
| :--- | :--- |
| `api_base: http://…/v1` 非流式 | **200**，`object=chat.completion`，`usage` 含 `prompt_tokens_details.cached_tokens`，`finish_reason=stop` |
| `api_base: http://…/v1` 流式 | **200**，`text/event-stream`，142 个事件，以 `data: [DONE]` 收尾 |
| `api_base: http://…/v1` + 名字映射 | **200**，zenproxy 侧 `model_list` 的 `upstream_model` 生效 |
| `api_base: http://…`（不带 `/v1`） | **404 `unknown or disabled account: chat`**，稳定复现 |

1. **`api_base` 必须带 `/v1`，LiteLLM 不会自己补。** 它把 `api_base` 和
   `/chat/completions` 直接拼接，于是少前缀的请求掉进反向入口。
   完整说明见上面「少写 `/v1` 为什么这么难查」。
2. **LiteLLM 的 cooldown 确实是按 deployment 记账的**——这条是模板里早有的论断，
   本轮实测到了。上游连续拒绝几次之后它直接回
   `No deployments available for selected model … All deployments for selected
   model are in cooldown`，期间**一个上游请求都不发**。
   这正是「路线 A 更好」的理由：某个账号被限流时，LiteLLM 只能把整个 deployment
   冷却掉，而 zenproxy 可以只换一个出口 IP 继续用。
3. **名字映射要在两边都写。** LiteLLM 的 `model:` 决定发给 zenproxy 的名字，
   zenproxy 的 `model_list` 再决定发给上游的真名。两层可以各自映射，也可以只有一层。

`deploy/litellm.config.yaml` 已按实测结果改正：**路线 A 现在是可直接编辑使用的
配置**（只改 `<模型名>` 一处），路线 B 保留为注释。

### LiteLLM：为什么统一入口比 N 条 deployment 好

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
      api_base: "http://zenproxy:9000/v1"   # ★ 带 /v1
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

配置校验只管**自己的**配置对不对，管不了「别人能不能把你接进去」。
后者用 `scripts/verify-openai-compat-contract.sh` 验，见 [六、接下游](#六接下游)。
两者的分工值得记一下：本轮实测时配置一路全绿，而网关面有约六成请求返回
上游的 `Invalid credential`——**配置校验永远不会告诉你上游此刻在拒你**，
那要靠契约脚本和真实调用。

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
