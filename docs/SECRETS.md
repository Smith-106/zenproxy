# 凭据纪律

这个仓库的历史里，凭据被写进日志/终端/对话**六次**。六次的原因都是同一个：
**脱敏靠「枚举我以为存在的模式」，而不是靠「结构 + 值形状」。** 这份文档把那六次
的形状记下来，并给出唯一被认可的做法。

## 一条规则

> **任何要读/打印/对比含密钥的文件或输出的操作，都必须经过 `scripts/redact.py`。
> 不要手写 `sed`/`grep`/正则做脱敏 —— 那正是六次事故的来源。**

```bash
# 文件（自动识别 JSON / YAML / .env / env dump / 纯文本）
sudo scripts/redact.py /opt/zenproxy/.env
sudo scripts/redact.py /opt/zenproxy/data/state.json
sudo scripts/redact.py /opt/cli-proxy-api/config.yaml

# 管道（任何产出密钥的命令都接上它）
sudo docker exec zenproxy env | scripts/redact.py -
sudo grep -E '^ZEN_KEY' /opt/zenproxy/.env | scripts/redact.py -

# 想要前几位做对照（默认只给长度）
scripts/redact.py --hint /opt/zenproxy/.env

# 自测：把历史泄露形状当用例跑
scripts/redact.py --test
```

它按**两个独立信号**判定：键名像凭据 **或** 值本身是凭据形状，任一命中即遮蔽。
所以「键名出乎意料」不再能让密钥漏出去。

## 六次事故的形状（都是真实发生过的）

| # | 做法 | 为什么漏 |
| :--- | :--- | :--- |
| 1 | 逐行正则脱敏 YAML | 只匹配了 `key: value` 单行形式，`api-keys:` 下的**列表项** `- sk-…` 原样通过 |
| 2 | **打印原始配置行** | CPA 的 `openai-compatibility` 是 **flow-style 整行**，一行里几十把 key，肉眼/正则都盖不住 |
| 3 | 自写 `len(v) > 12` 过滤 | 12 字符的密码直接放行 |
| 4 | `json.dumps(state.json)` 无脱敏 | `state.json` 里 `api_key` 是**明文**，整个账号列表被打印 |
| 5 | 正则 `s/(KEY\|TOKEN)=.*/` | 变量名是 `ZEN_KEY_01=`，`KEY` 与 `=` 之间隔着 `_01`，**模式没匹配上** |
| 6 | `sed -E 's/(sk-[A-Za-z0-9_-]{6})…'` 遮 `sk-` | 那一行里的 key 是 `nvapi-…`、`<hex>.<base62>`、base64、以及 URL 内嵌的 `socks5h://user:pass@host` —— **一个都不是 `sk-` 形状** |

第 2 次和第 6 次是同一个形状（打印 flow-style 行），中间隔了很久又犯了一次。

共同点：**每一次都以为自己知道密钥长什么样。**

## 这个部署里密钥都在哪

| 位置 | 内容 | 权限 |
| :--- | :--- | :--- |
| `/opt/zenproxy/.env` | `ZEN_ADMIN_TOKEN`、`RESIN_PROXY_TOKEN`、`ZEN_KEY_0N` | 0600 |
| `/opt/zenproxy/data/state.json` | 各账号 `api_key`（**明文**） | 0600 |
| `/opt/zenproxy/config.yaml` | 只有 `${VAR}` 引用，无明文 | 0640 |
| `/opt/cli-proxy-api/config.yaml` | `access.api-keys`（入站）+ `api-keys.openai-compatibility`（9 个 provider、60 把 key、含 SOCKS 密码） | 0600 |
| `docker exec <c> env` | 上面 `.env` 注入进去的值 | — |
| sub2api `proxies` 表 | `password` 列 | DB |
| CPA / sub2api 的 Postgres | 各类凭据 | DB |

**已知的好消息**：zenproxy 的管理接口**不返回明文 key** ——
`GET /admin/api/accounts` 给的是 `has_key` + `key_hint`，控制台 UI 也只取这些字段。
所以从管理接口/控制台复制输出是安全的；危险的是**直接读 `state.json`**。

## 万一还是漏了

按「能不能自己换」分两类，处理方式不同：

1. **能自己换的** —— 立刻轮换，并验证旧的确实失效：
   - CPA 入站密钥：`scripts/rotate-cpa-inbound-key.py`（定点行替换，保注释，带结构断言）
   - sub2api 管理员密码：`PUT /api/v1/user/password`，body `{"old_password","new_password"}`
     （`new_password` 有最短长度要求；哈希是 bcrypt，**不需要碰数据库**）
2. **换不了的** —— 只能交给持有方：
   - 上游账号 key（opencode 后台重新生成）
   - 第三方 provider key（各 provider 后台）
   - 外部 SOCKS 代理密码（代理提供方）

**不要做假动作**：把「我本地存的副本」改掉，而外部那把仍然有效，等于没轮换。
判断标准是**拿旧凭据去用，必须失败**。
