# 凭据纪律

这个仓库的历史里，凭据被写进日志/终端/对话**十次**。十次的原因都是同一个：
**脱敏靠「枚举我以为存在的模式」，而不是靠「结构 + 值形状」。** 这份文档把那十次
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

## 事故的形状（都是真实发生过的）

| # | 做法 | 为什么漏 |
| :--- | :--- | :--- |
| 1 | 逐行正则脱敏 YAML | 只匹配了 `key: value` 单行形式，`api-keys:` 下的**列表项** `- sk-…` 原样通过 |
| 2 | **打印原始配置行** | CPA 的 `openai-compatibility` 是 **flow-style 整行**，一行里几十把 key，肉眼/正则都盖不住 |
| 3 | 自写 `len(v) > 12` 过滤 | 12 字符的密码直接放行 |
| 4 | `json.dumps(state.json)` 无脱敏 | `state.json` 里 `api_key` 是**明文**，整个账号列表被打印 |
| 5 | 正则 `s/(KEY\|TOKEN)=.*/` | 变量名是 `ZEN_KEY_01=`，`KEY` 与 `=` 之间隔着 `_01`，**模式没匹配上** |
| 6 | `sed -E 's/(sk-[A-Za-z0-9_-]{6})…'` 遮 `sk-` | 那一行里的 key 是 `nvapi-…`、`<hex>.<base62>`、base64、以及 URL 内嵌的 `socks5h://user:pass@host` —— **一个都不是 `sk-` 形状** |

| 7 | `select-string` 递归扫目录查一把 key | 命中的文件里有 6 个根、上万行，其中一行就是第 2 次那张 flow-style 表 |
| 8 | **`grep -n 'claude-opus-5-5'`** | 那行 flow-style 表**就是文件的一整行**，`grep` 按行输出，于是**整行**被打印：41 把 `nvapi-`、10 把 ollama、stepfun、weixin、siliconflow、windsurfapi、sub2api、黑煤窑的 key，外加 `socks5h://user:pass@host` 里的代理密码（出现两次） |
| 9 | **`Select-String -Pattern 'DEEPSEEK\|deepseek'`** 递归扫 `.dsh/**/*.yaml` | 想查"这个变量还有谁在用"，结果 `.credentials.yaml` 那一行本身被命中并原样打印。**而且漏出去的是活凭据**（见下） |
| 10 | **`sudo python3 -c` 打印配置里 `name: new-api` 附近 26 行**，现场手写 `re.sub(r"(api-key:\s*).*", …)` 遮密钥 | 这一行是 flow-style JSON，密钥写法是 `"api-key": "…"`（引号夹在中间），**正则匹配的是块状 `api-key: value`，一个都没命中** → 41 把 `nvapi-`、ollama×11、weixin、siliconflow、stepfun、windsurfapi、sub2api、new-api、黑煤窑的活 key 原样进对话 |

第 2、6、8、10 次是同一个形状（打印 flow-style 行），中间各隔了很久，各又犯了一次。
第 8 次尤其没道理：动手前刚写下「**绝不打印原始配置行**」，下一秒就用 `grep` 去按内容搜这个文件。
第 9 次同样没道理：**"找引用"和"读凭据文件"是同一个动作**，只要文件名出现在搜索范围里就必然中招。
第 10 次是最没道理的：`scripts/redact.py` 就在手边，它按「结构 + 值形状」双信号判定，
flow-style 的单行 `api-keys` 数组**本来就在覆盖范围内**（`--test` 里有这个用例）。
**却临时手搓了一个只认块状写法的正则** —— 等于把第 1、3、5 次的错误重新发明一遍。

共同点：**每一次都以为自己知道密钥长什么样。**

### 第 9 次为什么特殊：变量名不是证据

那次漏出去的 `DEEPSEEK_API_KEY`，看上去是"一把已经失效的 DeepSeek 官方 key"，当时
正准备删掉它。**两件事都是错的**：

- **它不是 DeepSeek 的 key。** 名字是历史遗留。它的值（`th-…`，32 字符）是
  `https://tavily.ivanli.cc/api/tavily` 的代理密钥，`cordis.patch.yml` 里的
  `web-search-tavily` 正在用 `apiKeyEnv: DEEPSEEK_API_KEY` 引用它。DSH 的联网搜索靠它。
- **它没有失效。** 拿它去问 `api.deepseek.com` 得到 401 是**必然的**——测错了服务。
  换成正确的服务是 `HTTP 200`，真返回搜索结果。

**差点把一个在用的凭据当成废品删掉。** 教训：确认一个凭据的死活，必须拿它去问
**它真正所属的那个服务**；变量名、文件名、以及"它看起来像什么"都不算证据。
（`docs/SECRETS.md` 的这一节之所以留着这段，是因为它比"又漏了一次"更值得记。）

### 怎么安全地查"这把凭据还有谁在用"

想要的是**引用关系**，不是**值**。所以先按名字筛文件，再只看命中行的键名：

```bash
# ✅ 只列哪些文件提到了这个变量名，不输出内容
grep -rl --include='*.yaml' --include='*.yml' --include='*.json' 'DEEPSEEK_API_KEY' ~/.dsh \
  | sed 's|^|  |'

# ✅ 要看命中处在哪一行，就把行号拿出来，再用 read/offset 定位到那一行——不要 cat
grep -rn --include='*.yaml' 'DEEPSEEK_API_KEY' ~/.dsh | cut -d: -f1,2

# ❌ 直接 grep 变量名并把结果打出来 —— 如果搜索范围里有 .credentials.yaml，
#    命中的那一行就是「变量名: 值」，值和名字一起被打印
```

`.credentials.yaml` 这类文件**只能整文件交给 `scripts/redact.py`** 读，
或者用 `--include` 明确把它排除掉。

### 那条最危险的结构性质

`/opt/cli-proxy-api/config.yaml` 的 `api-keys.openai-compatibility` **整个数组是一行**
（flow-style，9 个 provider 全挤在同一个物理行里）。所以：

**任何按内容匹配的 `grep` / `sed -n 'Np'` / `awk` 都会把全部 60 把 key 一次性打出来。**
不是"可能"，是必然——匹配到就是打印整行，没有中间状态。

这个文件只能这样碰：

```bash
# ✅ 用 YAML 解析，只报告结构，不打印内容
sudo python3 -c "
import yaml
d = yaml.safe_load(open('/opt/cli-proxy-api/config.yaml'))
for p in d['api-keys']['openai-compatibility']:
    print('  %-20s keys=%d models=%d' % (p['name'], len(p.get('keys') or []), len(p.get('models') or [])))
"

# ✅ 要改就定点字符串替换，断言命中次数，只打印计数
# ✅ 要比对密钥就只比长度 + 前缀 + 后缀

# ❌ sudo grep -n '<任何内容>' /opt/cli-proxy-api/config.yaml
# ❌ sudo sed -n '404p'   同一条路
```

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

---

## 轮换要两个方向都成立，只做一个方向等于没做

上面那条讲的是「只改本地、外部还活着」。**反过来同样成立，而且更阴**：只改服务端、客户端还留着旧副本。

真实事故：CLIProxyAPI 的入站密钥在服务端轮换了，**同时有三份客户端配置在另一台机器上没有被改**：

```
~/.dsh/.credentials.yaml          CPA_API_KEY
~/.pi/agent/models.json           providers.cpa-responses.apiKey
~/.pi/agent/auth.json             cpa-responses.key
```

症状是 `401 "Invalid API key"`。**这个症状指向的方向是错的**——它看起来像「代理坏了」，
而实际上代理好好的，是客户端手里那把已经是废的。当时的表现是：一次摘要压缩静默失败、
以及被判定成「CLIProxyAPI 不能正常工作」。

判断轮换是否完成，标准不是「我改过服务端了」，而是**「没有任何地方还留着旧值」**：

```bash
# --old / --new 指向存放这两个值的文件（不是值本身，免得进 shell 历史和进程表）
python3 scripts/sweep-stale-credential.py --old /tmp/old.key --new /tmp/new.key --apply
python3 scripts/sweep-stale-credential.py --old /tmp/old.key   # 复查，必须 0 命中
```

只报路径和行号，从不打印凭据。`--apply` 会改写并复查，有残留则退出码 1。

⚠ 一个坑：`--old` / `--new` 文件如果落在被扫描的目录里，**它们自己就会永远命中**，
于是工具变成一个永远报「有残留」的噪音源。脚本已显式跳过这两个文件（实测踩过，退出码
本该是 0 却一直是 1）。
