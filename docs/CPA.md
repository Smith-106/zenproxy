# CPA 路由（CLIProxyAPI v8）

> 这份文档只讲一件事：**客户端说「模型不可用」时，怎么在三次请求内确定是哪一跳的锅，
> 以及 CPA 这一跳有哪些反直觉语义。** 2026-10-17 的一次 `400 unknown provider` 把下面
> 全部踩了一遍。

拓扑（免费面）：

```
客户端 → cpa.nikoniko.tech ─┬─ /v1/*        → CPA (8317) → openai-compat provider
                            └─ /zenapi/v1/*  → new-api (3000) → zenproxy (9000) → upstream
```

两条路都通，但**是两套独立实现**：`/v1` 由 CPA 自己按 provider 路由，`/zenapi/v1` 由 new-api 路由。

## 1. 先确定是哪一跳在说话

把同一个请求按跳打到每一层，比读日志快得多。CPA、new-api、zenproxy 的错误格式互不相同：

| 返回 | 出自 | 含义 |
| :--- | :--- | :--- |
| `400 {"message":"unknown provider for model X","type":"invalid_request_error","code":"model_not_found"}` | **CPA** | CPA 的模型→provider 索引里没有 X（**注意：X 可能仍出现在 `/v1/models` 里**，见 §2） |
| `503 {"message":"auth_unavailable: no auth available (providers=…, model=X; last upstream error: …)"}` | **CPA** | CPA 认为该 (provider, model) 不可用；`last upstream error` 是**缓存下来的旧错误**，含决定性的时间戳（见 §3） |
| `503 {"code":"model_not_found","message":"No available channel for model X under group default"}` | **new-api** | new-api 的 abilities 里没有 X（或渠道被禁用） |
| `410 {"type":"ModelDeprecated","message":"Model X has been deprecated."}` | **上游** | 模型已下线；new-api 会原样转出 |
| `403 {"type":"FreeTierError","message":"OpenCode's free tier can only be used from within…"}` | **上游** | 该**出口 IP** 此刻不在免费放行批次（分钟级翻转，不是配置错） |
| `400 {"message":"Model X price not configured…"}` | **new-api** | `options.ModelRatio` 缺这个模型名（别名也要单独配，见 §5） |

诊断脚本（密钥从配置里读进变量，不回显）：

```bash
CFG=/opt/cli-proxy-api/config.yaml
CPAK=$(sudo python3 -c "import yaml;print(yaml.safe_load(open('$CFG'))['access']['api-keys'][0])")
# 同一个 body 打 CPA / new-api / zenproxy，比对返回体
```

## 2. `alias` 才是客户端可用名（本仓库最大的坑）

CPA v8.0.15 的 model 条目有两个名字：

```yaml
{"name": <转发给上游的名字>, "alias": <客户端可用名>}
```

- `alias` 为空 → 暴露 `name`，客户端按 `name` 请求正常；
- `alias` 非空 → **只暴露 `alias`**，`name` 变成纯上游名；
- **但 `/v1/models` 仍然只列 `name`。**

于是出现「列表里有、请求 400 unknown provider」：

```
/models       → muse-spark-1.2-contributor-free   ← 看得见
POST /v1/chat/completions {"model":"muse-spark-1.2-contributor-free"}
              → 400 unknown provider for model muse-spark-1.2-contributor-free
```

实测对照（同 provider 同 key）：`glm-5.3-flash`(alias) → 200，`glm-5.3-flash-free`(name) → 400。

**结论：免费模型一律 `alias: ""` 挂载**；历史客户端名（如 `muse-spark-1.3-contributor-free`）
作为**独立条目**保留，把改名交给 new-api 的 `model_mapping`，不要在 CPA 这层再做一次映射。

工具：`scripts/cpa-expose-models.py --provider new-api --from-newapi [--apply]`

## 3. 冷却放大器：一次抖动 = 十分钟硬锁

```yaml
routing:
  cooldown:
    save-cooldown-status: true              # 冷却落盘 → docker restart 也不清
    transient-error-cooldown-seconds: 15
  retry:
    request-retry: 0                        # 一次失败就结束
    max-retry-credentials: 0
```

免费面单发成功率本来就只有 ~50%（上游按出口 IP 信誉放行）。`request-retry: 0`
意味着一次掷骰定生死，失败即进入长冷却 → 偶发抖动被放大成「该模型整段不可用」。

**判据（别猜，看时间戳）**：错误里 `request id: 202610171130307451868248268d9d6…` 是
`20261017` + `113030`（11:30:30 UTC）。如果此刻 `date -u` 已经是 11:40，而错误里的
时间戳还是 11:30，说明 CPA 在复用旧错误，**根本没有再打上游** —— 那就是冷却，不是上游坏。

调优（`scripts/cpa-routing-tuning.py --provider new-api --apply`）：

| 项 | 值 | 理由 |
| :--- | :--- | :--- |
| `routing.retry.request-retry` | `2` | 每次请求多两次独立掷骰，吃掉 ~50% 的单发失败 |
| `routing.retry.max-retry-credentials` | `1` | 允许换凭据重试 |
| provider `disable-cooling` | `true`（仅 new-api） | 上游 new-api 自己会跨 zen-01/02/03/05 重试；在 CPA 这层再冷却等于把它的重试成果作废 |

**不要全局关冷却**：对真正不可用的上游（欠费/鉴权错），冷却是有效限流。

**验收方式不是看配置，是连打 10 次**：改前同模型 3 次全 503，改后 `200=10/10`。

## 4. 模型索引只在启动时构建

改完 `config.yaml` 里的 `models` **必须** `sudo docker restart cli-proxy-api`
（实测热改文件不生效：新增的模型名仍然 400）。重启约 70 s，期间 `/v1/models` 不通。

冷却状态是**落盘**的，所以「重启清冷却」这个经验只在 `save-cooldown-status: false`
时成立；`save-cooldown-status: true` 下重启不会解锁已被冷却的模型。

## 5. new-api 侧：一个别名要三样齐备

想让 new-api 也认一个**历史名**（例：`muse-spark-1.3-contributor-free` → 1.2、
`glm-5.3-flash` → `glm-5.3-flash-free`），三样缺一不可：

1. `channels.models`（CSV）里含该名字 —— 否则 `No available channel for model X`；
2. `abilities` 有对应行且 `enabled = true` —— 否则同样没有渠道；
3. `channels.model_mapping`（JSON）把它指向真模型 —— 否则名字原样打到上游吃 `410`。

并且 `options.ModelRatio` / `options.CompletionRatio` 里要有该名字（别名不继承原名的
价格配置），否则 `400 Model X price not configured`。免费模型填 `0`。

`abilities` 的分组列名是保留字，SQL 必须写 `"group"`：

```sql
INSERT INTO abilities ("group", model, channel_id, enabled, priority, weight) …
ON CONFLICT ("group", model, channel_id) DO UPDATE SET enabled = true;
```

改完 `redis-cli FLUSHALL` + `docker restart new-api`（abilities 走内存缓存）。

## 6. 2026-10-17 收口时的实测矩阵

免费 10 模型（+2 个历史名）经 CPA：

```
🟢 muse-spark-1.2-contributor-free    🟢 muse-spark-1.3-contributor-free
🟢 glm-5.3-flash-free                🟢 glm-5.3-flash
🟢 big-pickle                        🟢 deepseek-v4.1-flash-free
🟢 mimo-v2.6-flash-free              🟢 space-bunny-free
🟢 beam-free                         🟢 longcat-2.5-preview-free
🟢 nemotron-3.5-lightning-free       🟢 nemotron-3.5-super-vl-free
→ 12/12
```

公网三条路径（`muse-spark-1.2-contributor-free`，客户端原始报错就是这个模型）：

| 路径 | 结果 |
| :--- | :--- |
| `POST https://cpa.nikoniko.tech/v1/chat/completions` | 200 |
| `POST https://cpa.nikoniko.tech/v1/responses` | 200（`status: completed`） |
| `POST https://cpa.nikoniko.tech/zenapi/v1/chat/completions`（new-api token） | 200 |
| `POST …/zenapi/v1/responses`（`stream:true`） | 10 个 SSE 事件，含 `delta` |

稳定性：CPA 连打同模型 10 次 → `200=10/10`。

## 7. 两个动手时会踩的小坑

- **`docker run … curlimages/curl -H "Authorization: Bearer $K"` 不会展开 `$K`** ——
  没有 shell，变量被字面传给 curl，得到一个假的 `401 Invalid API key`（很容易误判成密钥错）。
  正确写法是显式走 shell：`docker run --rm -e K="$K" … sh -c 'curl -H "Authorization: Bearer $K" …'`。
- **别用 `grep`/`sed -n 'Np'` 碰 `/opt/cli-proxy-api/config.yaml`**：`openai-compatibility`
  整个数组是**一行** flow-style，按内容匹配就是把几十把 key 一次打出来（见 `docs/SECRETS.md`）。

## 8. 回滚

每次写盘都留时间戳备份：

```bash
ls -1 /opt/cli-proxy-api/config.yaml.bak-*
sudo cp /opt/cli-proxy-api/config.yaml.bak-expose-<ts> /opt/cli-proxy-api/config.yaml.bak-models-<ts> /opt/cli-proxy-api/config.yaml
sudo docker restart cli-proxy-api
```

## 9. 相关脚本

| 脚本 | 作用 |
| :--- | :--- |
| `scripts/cpa-expose-models.py` | 清点/修复「列表里有、打不通」的模型暴露名（§2） |
| `scripts/cpa-routing-tuning.py` | 重试次数与 provider 级关冷却（§3） |
| `scripts/cpa-model-prices.py` | CPA 定价（`cap-token-usage-tracker`）同步 |
| `scripts/converge-free-models.py` | zenproxy + new-api 两侧免费模型清单收敛 |
| `scripts/probe-free-models-both-faces.py` | 两面（chat / responses）探测模型存活 |
| `scripts/redact.py` | 任何含密钥输出的唯一允许通道 |
