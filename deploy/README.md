# zenproxy 部署说明

> **默认路线：把 zenproxy 挂到「已经跑着的 new-api」旁边。**
> 一个容器、两个已有的 docker 网络；new-api 与 resin 各自的 compose 文件一个字都不用改。
> 已经在用 LiteLLM 当网关、手上没有 new-api 的，走备选路线 `docker-compose.litellm.yml`（第十节）。

---

## 0. 职责边界

| 组件 | 负责什么 | 不负责什么 |
| :--- | :--- | :--- |
| `new-api` | 模型路由、渠道管理、用户/额度/计费、对外入口 | 出站网络路径 |
| `zenproxy` | 账号 → 出口 IP 的绑定、出口探活、坏出口隔离与退避、**按上游状态码换出口重试** | 模型路由、计费 |
| `resin` | 出口节点池、粘性身份（`Default.<slot>:<token>`） | 账号与上游密钥 |

zenproxy 对 new-api 是**非侵入**的：不碰它的数据库、不改它的镜像，只是在渠道里被当成一个上游地址。
下线 zenproxy 不会丢任何 new-api 数据，只会让指向它的渠道请求失败。

---

## 1. 文件清单

| 文件 | 用途 | 路线 |
| :--- | :--- | :--- |
| `docker-compose.yml` | ★ 单个 `zenproxy` 服务，加入两个已有网络 | **默认** |
| `config.example.yaml` | zenproxy 配置模板，已按本部署填好 | **默认** |
| `.env.example` | 4 个变量，全是占位值 | **默认** |
| `install.sh` | 幂等安装脚本：装文件 → 自检闸门 → 起容器 | **默认** |
| `docker-compose.litellm.yml` | zenproxy + LiteLLM + postgres 完整栈 | 备选 |
| `litellm.config.yaml` | LiteLLM 模板（每账号一个 deployment） | 备选 |
| `Dockerfile` | 构建 `zenproxy:latest` | 两条共用 |
| `.gitignore` | 挡掉 `.env` / `data/` | 两条共用 |

本目录**不再**提供 nginx vhost 与 systemd unit —— 原因见附录 A。

---

## 2. ★ 先选接入点：填 Base URL，不要填「代理」

new-api 渠道的高级配置里有两个位置可以指向 zenproxy。它们**不是等价的**：

| | 填在**Base URL**（推荐） | 填在**代理 / proxy** |
| :--- | :--- | :--- |
| 填什么 | `http://zenproxy:9000/<账号ID>` | `http://<账号ID>:x@zenproxy:9000` |
| zenproxy 看到什么 | 上游的**明文响应**（403 / 429 / 5xx 全部可见） | 一条**加密的 TLS 隧道**（`CONNECT` 之后全是密文） |
| 上游报错能否自动换 IP | ✅ 能：按状态码判定后换出口重试 | ❌ 不能：只有「连不上」才会换出口 |
| 适合 | 走 HTTP 的网关（new-api、LiteLLM） | 只能用代理字段的客户端 |

**结论：接 new-api 就填 Base URL。** 这不是「都能用，随便挑」—— 原因很具体：

new-api 请求的是 `https://opencode.ai/...`，Go 的 `net/http` 对 https 目标一律先发 `CONNECT` 建隧道。
填「代理」字段时 zenproxy 拿到的是一条**已经加密的字节流**：它看不见里面的 403/429/5xx，
自然无从判断该不该换 IP，于是你在 `policy.rotate_on_status` 里精心列的那些状态码**一个都用不上**，
只剩下「连接失败才换」。填 Base URL 时 TLS 由 zenproxy 终止，响应是明文，
`429` / `5xx` 才真正触发换出口重试。

> 两种模式 zenproxy 都实现了，共用同一个监听端口（靠请求形态自动区分），所以两种填法都能**通**。
> 但填「代理」你只得到可用性容灾，得不到错误驱动的换 IP。**要哪一个，自己清楚就行。**

---

## 3. 拓扑：接入两个「已经存在」的网络

```
        浏览器 / 客户端
             │
             ▼
┌─────────────────────────────────────────────────────┐
│ new-api 容器        (calciumion/new-api:latest)      │
│ 实际是 QuantumNous 主线 v1.0.0-rc.41                  │
│ 宿主发布：127.0.0.1:3000                             │
│ 渠道 → 高级配置 → Base URL：                          │
│     http://zenproxy:9000/zen-01                     │
└───────────────────┬─────────────────────────────────┘
                    │  docker 网络 new-api_default (172.18.0.0/16)
                    │  new-api 按容器别名 zenproxy 找到它
                    ▼
┌─────────────────────────────────────────────────────┐
│ zenproxy 容器                                        │
│ 容器内 0.0.0.0:9000        宿主 127.0.0.1:9000（仅回环）│
│ 账号池 / 出口绑定 / 探活隔离 / 失败分类 / 换出口重试    │
│ 状态：/opt/zenproxy/data/state.json  →  容器 /data    │
└───────────────────┬─────────────────────────────────┘
                    │  docker 网络 resin_default (172.30.0.0/16)
                    │  zenproxy 按容器别名 resin 找到它
                    ▼
┌─────────────────────────────────────────────────────┐
│ resin 容器          (ghcr.io/resinat/resin:latest)   │
│ 容器内 2260 全接口监听；宿主发布 127.0.0.1:2260       │
│ 粘性身份：socks5h://Default.<slot>:<token>@resin:2260 │
└───────────────────┬─────────────────────────────────┘
                    ▼
             opencode.ai/zen/v1
```

要点：

* zenproxy 容器**同时接入** `new-api_default` 与 `resin_default`，两个网络都写成
  `external: true` —— 只加入、不创建、不改属性。**不需要 `host.docker.internal`，
  不需要 host 网络，不需要动 new-api 或 resin 的 compose。**
* `proxies.resin.host` 因此写 `resin:2260`（不是 `127.0.0.1:2260`，也不是
  `host.docker.internal:2260`）。容器里的 `127.0.0.1` 是容器自己。
* 对宿主只发布 `127.0.0.1:9000`：给本机 `curl` 自检和 `/admin/` 控制台用。
  公网入口是 new-api，zenproxy 不直接对外。
* `server.listen` 必须是 `0.0.0.0:9000`：`127.0.0.1:9000` 只有容器自己看得到，new-api 连不上。

---

## 4. 安装（按顺序做）

### 4.1 前置检查（约 30 秒）

```bash
# 两个网络必须已存在
docker network ls --format '{{.Name}}' | grep -E '^(new-api_default|resin_default)$'
# 期望：两行都出现

# 两个既有容器在跑、端口也对
docker port new-api        # 期望 3000/tcp -> 127.0.0.1:3000
docker port resin          # 期望 2260/tcp -> 127.0.0.1:2260

# 9000 没被占
ss -ltn | grep -q ':9000' && echo "9000 被占用，先腾出来" || echo "9000 空闲"
```

网络名对不上时：以 `docker network ls` 的真实名字为准，改
`deploy/docker-compose.yml` 的 `networks:` 段（两处名字必须逐字一致）。

### 4.2 构建镜像

`/opt/zenproxy/` 下只有配置与 compose，没有源码，所以镜像要**先构建再放上去**，二选一：

```bash
# A. 就在这台机器上构建（仓库已经在机器上）
cd <仓库根>
docker build -f deploy/Dockerfile -t zenproxy:latest .

# B. 在别处构建再搬过来
docker save zenproxy:latest | gzip | ssh <宿主机> 'gunzip | docker load'
```

### 4.3 装文件

```bash
cd deploy
sudo ./install.sh
```

这一步会做好下面这些事，**而且可以反复执行**（幂等）：

| 产物 | 权限 | 说明 |
| :--- | :--- | :--- |
| `/opt/zenproxy/docker-compose.yml` | `0644 root:root` | 每次刷新成仓库里的版本 |
| `/opt/zenproxy/config.yaml` | `0640 root:10001` | 从模板生成；**已存在则原样保留，绝不覆盖** |
| `/opt/zenproxy/.env` | `0600 root:root` | 从模板生成；**已存在则原样保留，绝不覆盖** |
| `/opt/zenproxy/data/` | `0750 10001:10001` | 容器以非 root（uid 10001）运行，属主必须对上 |

模板里全是 `REPLACE_WITH_*` 占位值，所以第一次跑**不会**启动容器，只会提示你去填。
填完再跑一次 `sudo ./install.sh`（或直接 `docker compose up -d`）即可。

> 为什么 config.yaml 是 `root:10001 0640` 而不是 `root:root 0600`：容器非 root，
> 读不到就等于启动失败。`.env` 则是 `0600 root:root` —— 它只被宿主机上的 compose 读来做
> 变量插值，文件本身不进容器。

### 4.4 取 Resin token

Resin 的代理令牌是 **resin 容器自己生成/配置的那个值**，不是你 `openssl rand` 出来的。
目标宿主机上 resin 已经在跑，直接读它的环境变量：

```bash
docker inspect resin --format '{{range .Config.Env}}{{println .}}{{end}}' \
  | grep '^RESIN_PROXY_TOKEN='
# 输出形如： RESIN_PROXY_TOKEN=<一串字符>
```

同一份值也写在 resin 自己的 compose 文件里（`environment:` 段里的 `RESIN_PROXY_TOKEN:`），
不想读容器就去那儿看。

> ⚠ 上面这条命令的输出**就是密钥本身**，只应该出现在你自己的终端里。
> 不要贴进聊天、工单、截图、CI 日志。

顺手验证这个 token 真能用（在宿主机上，走 Resin 发布到回环的那个口）：

```bash
curl -s --proxy "socks5h://Default.slot-01:${RESIN_PROXY_TOKEN}@127.0.0.1:2260" https://api.ipify.org
# 期望：一个公网 IP，例如 203.0.113.7
# 407 / 空输出 → token 或 platform 名不对
# 连接被拒     → resin 没在 2260 上监听
```

### 4.5 填 `.env`

```bash
sudo vi /opt/zenproxy/.env
```

四个值，一个都不能留 `REPLACE_WITH_*`：

| 变量 | 怎么来 |
| :--- | :--- |
| `ZEN_ADMIN_TOKEN` | `openssl rand -hex 32` |
| `RESIN_PROXY_TOKEN` | 4.4 那条命令读出来的值 |
| `ZEN_KEY_01` / `ZEN_KEY_02` | 上游账号后台复制 |

`.env` 里**只有这 4 个变量**。`docker-compose.yml` 用 `environment:` 逐项注入、不用
`env_file`，所以这个文件不会被整个塞进容器 —— 将来往 `.env` 里加别的东西也不会顺带进 zenproxy。

### 4.6 核对 `config.yaml`

```bash
sudo vi /opt/zenproxy/config.yaml
```

模板已经填好了，正常只需要改两处：

| 字段 | 模板值 | 说明 |
| :--- | :--- | :--- |
| `server.listen` | `"0.0.0.0:9000"` | **别改成 127.0.0.1**，见 3 节 |
| `proxies.resin.host` | `"resin:2260"` | 默认路线的关键取值 |
| `state_file` | `"/data/state.json"` | 对应 compose 的 `./data:/data` |
| `accounts[].id` | `zen-01` / `zen-02` | 就是 Base URL 的路径第一段 |
| `accounts[].proxy` | `resin-slot-01` / `resin-slot-02` | **出口 ID**，不是槽位名 |

出口 ID 的真实取值用这条命令看（Resin 成员是 `resin-<槽位名>`，静态出口是 `static-01`）：

```bash
set -a; . /opt/zenproxy/.env; set +a
curl -s -H "X-Admin-Token: $ZEN_ADMIN_TOKEN" \
  http://127.0.0.1:9000/admin/api/proxies | grep -oE '"id": *"[^"]+"'
```

`accounts[].proxy` 填错了不会报错，只在日志里警告 `account references unknown exit`
然后退回池子里挑 —— 症状是「配置看着没问题，但账号跟出口的对应关系是随机的」。

### 4.7 起容器

```bash
sudo docker compose -f /opt/zenproxy/docker-compose.yml up -d
# 或再跑一次： cd deploy && sudo ./install.sh

set -a; . /opt/zenproxy/.env; set +a
curl -s http://127.0.0.1:9000/healthz
```

`install.sh` 在启动前会拦住这几种情况，不会把半成品推上线：
`.env` 里还有 `REPLACE_WITH_*`；`config.yaml` 引用了 `.env` 里没有的 `${VAR}`；
某个 `${VAR}` 没有被 compose 的 `environment:` 段注入；两个外部网络不存在；镜像不存在。

---

## 5. 怎么确认它真的在工作

先把 `.env` 载进当前 shell，下面命令都要用：

```bash
set -a; . /opt/zenproxy/.env; set +a
```

### 5.1 活着

```bash
curl -s http://127.0.0.1:9000/healthz
```
```json
{
  "status": "ok"
}
```
免鉴权。它只说明进程活着，**不说明出口可用**。

### 5.2 出口池真的在探活

```bash
curl -s -H "X-Admin-Token: $ZEN_ADMIN_TOKEN" \
  http://127.0.0.1:9000/admin/api/status
```
```json
{
  "accounts": 2,
  "accounts_enabled": 2,
  "exits": 2,
  "exits_healthy": 2,
  "listen": "0.0.0.0:9000",
  "state_file": "/data/state.json",
  ...
}
```

`exits_healthy` **不能是 0**。是 0 说明出口池一个都用不了 → 回到 4.4 查 token 与 `resin:2260`。

```bash
curl -s -H "X-Admin-Token: $ZEN_ADMIN_TOKEN" \
  http://127.0.0.1:9000/admin/api/proxies
```
```json
{
  "exits": [
    {
      "id": "resin-slot-01",
      "addr": "resin:2260",
      "scheme": "socks5h",
      "healthy": true,
      "exit_ip": "203.0.113.7",
      "latency_ms": 412,
      "bound_accounts": ["zen-01"]
    },
    ...
  ]
}
```

`exit_ip` 有值 = 这个出口真的出网了。首轮探活在容器启动时立刻跑一次，之后每
`health.interval`（默认 60s）一次。各槽位 IP 是否互不相同只是**期望**，不是判据：
Resin 完全可能把两个 slot 锚在同一节点上；`exit_ip` 为空或 `last_error` 有值才是问题。

想立刻重探一轮：

```bash
curl -sS -X POST -H "X-Admin-Token: $ZEN_ADMIN_TOKEN" \
  http://127.0.0.1:9000/admin/api/proxies/probe
# {"status": "probing"}
```

### 5.3 反代模式端到端

```bash
curl -sS -X POST http://127.0.0.1:9000/zen-01/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"<上游真实模型名>","messages":[{"role":"user","content":"ping"}],"max_tokens":8}'
```

拿到正常的 OpenAI 格式响应（`choices[0].message.content`）就说明
「账号 → 出口 → 上游 → 注入 key」这条链是通的。

* 账号 ID 写错 → `404`，body 是 `{"error":{"type":"zenproxy_error","message":"unknown or disabled account: xxx"}}`
* 上游自己返回的 401/403 → **原样透传**（body 就是上游的 body），不会变成 zenproxy 的错误格式
* 路径里的 `/v1` 会与上游 `base_url` 的 `/v1` 折叠：`/zen-01/v1/chat/completions` 和
  `/zen-01/chat/completions` 打到同一个上游地址

### 5.4 new-api 侧端到端

在 new-api 新建一个只挂 zenproxy 的渠道（填法见第 7 节），用「测试」按钮或对话页发一条，
然后回来看这个账号的计数有没有涨：

```bash
curl -s -H "X-Admin-Token: $ZEN_ADMIN_TOKEN" \
  http://127.0.0.1:9000/admin/api/accounts
# 该账号的 stats.requests 应该 +1
```

### 5.5 「报错自动换 IP」真的生效

这是最该验的一条。做法：先造一个**必然失败**的出口，把账号钉上去，再发一次请求，
期望请求最终仍然成功，且 `rotations` 涨了。

```bash
# 1) 在 /opt/zenproxy/config.yaml 的 proxies.static 里加一个指向空端口的出口
#      proxies:
#        static:
#          - "socks5h://127.0.0.1:9"     # 没人监听，必然连不上
#    然后 sudo docker compose -f /opt/zenproxy/docker-compose.yml restart zenproxy
#    它的出口 ID 是 static-01

# 2) 把 zen-01 钉到这个坏出口上（api_key 留空 = 不改动已存的密钥）
curl -sS -X POST http://127.0.0.1:9000/admin/api/accounts \
  -H "X-Admin-Token: $ZEN_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"id":"zen-01","upstream":"zen","proxy":"static-01","enabled":true}'

# 3) 发一次请求：期望仍然是 200 —— 坏出口被换掉了
curl -s -o /dev/null -w '%{http_code}\n' \
  -X POST http://127.0.0.1:9000/zen-01/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"<上游真实模型名>","messages":[{"role":"user","content":"hi"}],"max_tokens":8}'

# 4) 期望 rotations >= 1，且绑定已经离开 static-01
curl -s -H "X-Admin-Token: $ZEN_ADMIN_TOKEN" \
  http://127.0.0.1:9000/admin/api/accounts
```

日志里应当能看到对应的一条：

```
level=WARN msg="upstream transport failure" account=zen-01 exit=static-01 attempt=1 class=transport
level=INFO msg=request method=POST path=/zen-01/v1/chat/completions status=200
```

验完记得把 `static` 里那条删掉、把账号的 `proxy` 改回 `resin-slot-01`，再 restart 一次。

> 想直接看控制台也可以：`http://127.0.0.1:9000/admin/?token=<admin_token>`
> （控制台会把 token 挪进 localStorage 并从地址栏抹掉）。

---

## 6. 清理垃圾出口

出口池跑一阵子之后，一定会攒下一些**死透的**出口：隔离会退避到上限，然后到点自动复活、
再失败、再隔离……永远占着位置、永远浪费一次重试预算。要让它真正离开，用**永久淘汰**。

控制台右上角有个 **「清理垃圾出口」** 按钮（对应 `POST /admin/api/proxies/prune`），
它把当前「隔离中 / 降级 / 延迟超过 `max_latency`」的出口一次性永久移出轮换：

```bash
curl -sS -X POST -H "X-Admin-Token: $ZEN_ADMIN_TOKEN" \
  http://127.0.0.1:9000/admin/api/proxies/prune
```
```json
{"report":{"retired":["resin-slot-03"],"already_retired":null,"usable_after":3,"pool_empty":false}}
```

几点必须知道的：

- **可逆**。误清了就在控制台该行点「恢复」（`POST /admin/api/proxies/{id}/revive`）。
  恢复会连失败计数一起清零，给它一个干净的起点，而不是下一次抖动又被淘汰。
- **幂等**。再点一次不会重复动作，已淘汰的会列在 `already_retired` 里。
- **不手软**。如果清完池子是空的，它照清不误，但 `pool_empty: true` 会明确告诉你。
  这时流量会退回到**失败最少**的那个已淘汰出口（不会给 new-api 回 502），
  同时日志会打一条 WARN——看到它就该加出口或恢复了。
- **自动淘汰**是另一条并行路径：`health.retire_after`（默认 20）连续失败到这个次数、
  中间一次都没成功过，出口会自己退休。**任何一次成功都会清零计数**，
  所以"时好时坏但还能用"的出口不会被误伤。
- 表格里状态列会把够条件的标上「**可清理**」，点按钮前就能看到会动到谁。

> ⚠ 别把 `health.retire_after` 设得太小。出口池是 new-api 的强依赖，
> 一次性把好出口也淘汰掉会让所有渠道 502。20 是保守值，先跑一周看控制台再考虑收紧。

---

## 7. 接进 new-api

new-api → 渠道 → 新建（或编辑现有渠道）：

| new-api 字段 | 填什么 |
| :--- | :--- |
| 类型 | OpenAI 兼容（渠道表 `type = 1`） |
| **Base URL** | `http://zenproxy:9000/zen-01` ← 账号 ID 就是路径第一段 |
| 密钥 | **随便填一个非空占位串**（zenproxy 会用账号自己那把 key 覆盖 `Authorization`） |
| 模型 | 按需勾选；模型名与上游不一致时用渠道的「模型映射」转换，zenproxy 不改写请求体 |
| 代理 / proxy | **留空**（不要同时填，见第 2 节） |

一个渠道对应一个账号：因为 `Authorization` 被账号自己的 key 覆盖了，一个渠道塞多个
账号没有意义。多账号就建多个渠道，每个指向 `http://zenproxy:9000/<各自的账号ID>`。

### 7.1 用脚本建（免手点控制台）

`scripts/create-newapi-channel.sh` 会把上面这张表落成一条渠道，并在建完后核对路由：

```bash
sudo CH_NAME="zenproxy (opencode free)" ZEN_ACCOUNT=zen-01 scripts/create-newapi-channel.sh
```

它顺带处理了三件手点控制台时不会遇到、但脚本化时一定会踩的事：

| 坑 | 事实 |
| :--- | :--- |
| **为什么要走 API 而不是直接 `INSERT`** | new-api 靠 **`abilities` 表**做模型路由（主键 `group+model+channel_id`）。只往 `channels` 插一行，`abilities` 不会有对应记录，渠道建了也永远不会被选中。管理 API 会事务性地一起写。 |
| **请求体形状** | `POST /api/channel/` 绑定的是 `AddChannelRequest`，渠道必须**包在 `channel` 字段里**（顶层还有 `mode`）。直接发裸的 channel 对象会得到 `{"message":"channel cannot be empty","success":false}`——因为 `channel` 是 nil，校验在 `Key == ""` 那一句就挂了。而 `PUT /api/channel/` 绑的 `PatchChannel` 是**裸对象且要带 `id`**，两个方法形状不同。 |
| **管理 API 的认证** | 要「系统访问令牌」= `users.access_token`（`character(32)`，**正好 32 字符**）。root 用户没有的话脚本会生成一个写进去；撤销走控制台「设置 → 系统访问令牌 → 清除」。 |

建完自己核对一遍：

```bash
# 渠道在不在、对不对
sudo docker exec postgres psql -U root -d new-api -c \
  "select id,name,status,base_url,\"group\" from channels where name like 'zenproxy%';"
# 路由同步了没（关键：没有这些行渠道就是死的）
sudo docker exec postgres psql -U root -d new-api -c \
  "select a.model,a.channel_id,a.enabled from abilities a join channels c on c.id=a.channel_id where c.name like 'zenproxy%';"
```

万一 `abilities` 没跟上，new-api 自己有个重建接口：`POST /api/channel/fix`。

### 7.2 端到端验证

```bash
sudo scripts/e2e-through-newapi.sh big-pickle    # 单个模型，打印内容与用量
sudo scripts/verify-all-free-models-via-newapi.sh # 7 个免费模型全跑一遍
```

判断成功的硬证据是 **new-api 的日志里 `channel_id` 是这条新渠道的 id**：

```bash
sudo docker exec postgres psql -U root -d new-api -c \
  "select id, model_name, channel_id, use_time from logs where model_name<>'' order by id desc limit 5;"
```

> **验证脚本挑用户令牌时的坑**：new-api 用 GORM 软删除，被删掉的令牌在 `tokens` 表里**还在**。
> 拿令牌来测必须带上 `and deleted_at is null`，否则极容易挑到一条已删除的令牌，
> 然后拿到 `401 Invalid token`，而服务端日志只会说 `failed to get token: record not found`
> ——看起来像路由或渠道的问题，其实是挑了条死令牌。另外库里存的是**裸键**，
> 对外发请求要补回 `sk-` 前缀（中间件会 `TrimPrefix(key, "sk-")` 再查库）。

---

## 7.3 再接一个上游（以 CLIProxyAPI 为例）

`create-newapi-channel.sh` 是泛化的，任何 OpenAI 兼容上游都能用它接进来：

```bash
sudo CH_NAME="cli-proxy-api (CPA)" \
     CH_BASE_URL="http://cli-proxy-api:8317" \
     CH_MODELS_URL="http://127.0.0.1:8317/v1/models" \
     CH_KEY_CMD="python3 scripts/extract-cpa-inbound-key.py /opt/cli-proxy-api/config.yaml" \
     CH_REMARK="经 CLIProxyAPI 聚合的多家上游" \
     scripts/create-newapi-channel.sh
```

### ★ Base URL 千万不要带 `/v1`

这是最容易踩、而且报错最不像元凶的一个：

| 你填的 base_url | new-api 实际请求的路径 | 结果 |
| :--- | :--- | :--- |
| `http://cli-proxy-api:8317/v1` | `/v1/v1/chat/completions` | 上游 **404** |
| `http://cli-proxy-api:8317` | `/v1/chat/completions` | 正常 |

new-api 会把 **`/v1/chat/completions` 整段**追加到 base_url 后面。多写一个 `/v1` 就变成
`/v1/v1/...`，上游回 404，而 new-api 把这包装成
`bad_response_status_code: openai_error` —— 看着像上游挂了，其实是路径重复。
（对照：zenproxy 渠道写 `http://zenproxy:9000/zen-01`，也不带 `/v1`，就是这个原因。）

想确认上游到底收到了什么，看上游自己的访问日志最直接：

```bash
sudo grep -aE '404|chat/completions' /opt/cli-proxy-api/logs/main.log | tail -5
# 期望看到 POST "/v1/chat/completions"
```

### 其余三个必须知道的点

1. **网络要自己打通。** new-api 默认只在 `new-api_default` 上，够不到别的容器。
   `scripts/connect-newapi-to-llm-internal.sh` 会把它接进 `llm-internal`
   （与 `cli-proxy-api` / `sub2api` / `windsurfapi` 同网），**并且写进 compose** ——
   手工 `docker network connect` 只是给当前容器加了张网卡，容器一旦被 compose 重建就没了。
2. **`CH_MODELS_URL` 往往要单独给。** 渠道的 base_url 是给 new-api **容器**用的地址
   （容器名），而拉模型清单是脚本在**宿主机**上做的事，宿主机解析不了容器名。
   所以 base_url 写 `http://cli-proxy-api:8317`，清单地址写 `http://127.0.0.1:8317/v1/models`。
3. **建完等约 60 秒。** new-api 的渠道/路由是内存缓存，60 秒同步一次。
   刚建好的渠道会一直返回 `503 No available channel for model ...`，
   等一轮同步就好 —— **不要**因为看到 503 就以为渠道建错了。

### 更新已有渠道时：PUT 不能带 `status`

实测（new-api，2026-10-10）：同一份渠道 JSON，`POST` 带 `status: 1` 正常，
而 `PUT` 只要 body 里有 `status` 就回：

```json
{"message":"Invalid parameters","success":false}
```

去掉 `status` 后其余字段照常更新，而且已发布的启用状态会被保留 —— 正是更新时想要的语义。
脚本的更新分支因此会主动剥掉 `status`（见 `create-newapi-channel.sh` 里的注释）。

> 顺带一个教训：拿真实线上渠道做「试探性 PUT」来定位字段时，
> **校验通过的请求是会真的改数据的**。定位这类问题时要么用新建的临时渠道，
> 要么先记下原值（`select models, weight, auto_ban from channels where id=N`），
> 事后按记录恢复。

### CPA 的入站 key 藏在哪

`/opt/cli-proxy-api/config.yaml` 里有两个同名但完全不相关的字段：

| 字段 | 是什么 |
| :--- | :--- |
| `access.api-keys` | **入站 key** —— 客户端访问 CPA 时带的，接渠道要的就是它 |
| 顶层 `api-keys` | **provider 凭据**段（`codex` / `claude` / `xai` / `openai-compatibility`），里面是各家上游的 key |

`scripts/extract-cpa-inbound-key.py` 只取前者，且只打印第一条。

---

## 7.4 让下游服务的出站也走 Resin 出口池

**结论先说：不要让下游绕经 zenproxy，直接接 Resin。** 下面是两条路的对比和实测数据。

### 为什么不要走 zenproxy 的正向代理

`internal/forward/proxyface.go` 顶部注释已经把结构性限制写明了：下游若是 HTTPS 上游，
到达 zenproxy 时是**不透明的 CONNECT 隧道**，zenproxy **看不到上游的 403/429**，
因此 `shouldRotate` 永远不会触发——只有拨号失败才轮换。结果是**出口粘死在一个 IP 上**，
恰好失去轮换的意义。而且还要把每个上游域名加进 `allowed_hosts`。

（`forwardAbsolute` 那条路也一样：上游状态码原样透传，设计上就"由客户端自己负责重试语义"。）

### 为什么直接接 Resin 是对的

Resin 本身就是正向代理池（`ghcr.io/resinat/resin:latest`，监听 `:2260`），而 zenproxy
配置里的注释写明了它的**粘性身份格式**：

```
<Platform>.<Account>:<token>@<host>
# 同一个 Account 名在节点存活期间固定落在同一个出口
```

**身份名就是出口选择器。** 而下游服务（CPA、new-api 等）大多原生支持 `proxy-url`
且接受 `socks5h://`，所以只要给**每个 key 分配不同的 Account 名**，
就天然把「按 key 轮换」变成「按 IP 轮换」——不需要任何中间层。

```
proxy-url: socks5h://Default.slot-13:<RESIN_PROXY_TOKEN>@resin:2260
```

前置条件是网络要通：Resin 在 `resin_default` 上，CPA 在 `cli-proxy-api_default` +
`llm-internal` 上，**默认互相解析不了**。要么把下游接进 `resin_default`，
要么把 `resin` 接进共享网络（参考 §7.3 的 `connect-newapi-to-llm-internal.sh`，
注意同样要写进 compose，否则容器重建即失效）。

### 实测数据（2026-10-10）

**机制**：不同 slot → 不同出口 IP（`slot-13`→`154.37.216.186` HK、
`slot-15`→`146.70.76.58` JP，与 zenproxy 记录的完全一致）；同一 slot 打两次 → 同一个 IP，
**粘性成立**。

**24 个 slot 的出口并非 24 个 IP**，实测只有 **15 个不同出口 IP**，多个 slot 共用同一个：

| 出口 IP | 国家 | 占了几个 slot |
| :--- | :--- | :--- |
| `160.250.130.168` | VN | 3（04/05/06）|
| `84.17.47.126` | NL | 3（17/18/19）|
| `34.132.56.138` | US | 2（02/03）|
| `87.242.85.187` | RU | 2（10/11）|
| `146.70.76.58` | JP | 2（15/16）|
| `193.176.84.26` | RO | 2（20/21）|
| `139.59.1.14` | IN | 2（22/23）|
| 其余 8 个 | US/NL/FR/ID/HK | 各 1 |

**延迟波动极大，这是最大的问题。** 对 `api.ipify.org` 的往返：

| 延迟 | slot |
| :--- | :--- |
| < 1s | `slot-13`(HK) `slot-11`(RU) `slot-22`(IN) `slot-24`(US) |
| 1–2s | `slot-07`(NL) `slot-14`(ID) `slot-10`(RU) `slot-09`(FR) `slot-20`(RO) `slot-17`(NL) |
| 2–6s | `slot-08` `slot-12` `slot-15` `slot-18` `slot-23` `slot-06` `slot-01` |
| **> 6s，不可用** | `slot-02`(8.8s) `slot-04`(6.9s) `slot-05`(12.9s) `slot-16`(**20s**) `slot-19`(**20s**) `slot-21`(**20s**) `slot-03`(**20s**) |

真实 LLM 调用上更明显：同一个模型，**直连 TTFB 0.70s**，经 `slot-13` 是 **0.46s**（可用），
经 `slot-15` 是 **12.6s**、经 `slot-21` 是 **30.1s**（不可用）。

> **所以别全量切换。** 只给确实需要换 IP 的上游挂代理，并且先用
> `scripts/sweep-resin-slots.sh` 扫一遍，只挑延迟 < 2s 的 slot ——
> 实测只有 **9 个不同出口 IP** 同时满足「活 + 快」，这才是真实可用的池子大小。

---

## 8. 已知限制（务必读）

### 8.1 `*-free` 模型是客户端身份门槛，换出口解决不了

OpenCode Zen 对 `*-free` 系列返回 HTTP 403：

```json
{"type":"error","error":{"type":"FreeTierError",
 "message":"OpenCode's free tier can only be used from within OpenCode"}}
```

这个判定依据是**客户端身份**，不是 IP、也不是账号。所以：**无论换多少个出口、
多少个账号，这个结果都不会改变** —— 换 IP 对这个类别无效。

zenproxy **不伪造客户端身份**，这是有意为之的设计边界，不是漏做。
它解决的是「账号/出口会掉，要自动绕开」的可用性问题，不是「绕过额度门槛」的问题。
这类响应被单独归类为 `tier_gate`，与 `auth` 分开显示，并且**不会为它浪费出口**。
在控制台看到 `last_class: tier_gate` 时，不要去查出口或账号，那不是问题所在。

### 8.2 new-api 的流式超时会先于 zenproxy 掐断

new-api 的 `STREAMING_TIMEOUT` 默认 **120 秒**，而 zenproxy 的 `upstream_timeout` 默认
**600 秒**。跑得长的推理请求会先被 new-api 掐断。用长流就在 new-api 的 compose 里放宽：

```yaml
environment:
  - STREAMING_TIMEOUT=600
```

zenproxy 自己还有一道 `stream_idle_timeout`（默认 5 分钟）：上游收了流式请求之后
**一个字节都不发**才会被它掐断，正常输出的长流不受影响。

### 8.3 `auto_ban` 会把 zenproxy 的 502 记在渠道头上

出口池全挂时 zenproxy 返回 **502**。如果渠道开了 `auto_ban`，连续几次就可能被
new-api 自动禁用。这是符合预期的保护，但要知道它存在 —— 修好出口池之后，
记得回 new-api 把那个渠道重新启用。

### 8.4 代理字段不支持 socks5，而且那里该填 zenproxy 不是 Resin

new-api 的「代理」字段只认 `http://` / `https://` 代理。zenproxy 的正向代理入口就是
HTTP 代理，匹配。但**不要**把 Resin 的 `socks5h://...` 地址直接填进去 —— 那个位置要填的是
zenproxy 的地址，Resin 地址是 zenproxy 自己内部用的。

---

## 9. 回滚

zenproxy 对 new-api 非侵入，回滚很干净：

```bash
# 1) new-api 侧：渠道 → 高级配置 → 把 Base URL 清空（恢复官方默认地址）→ 保存
#    渠道本身不会丢，只是不再经过 zenproxy

# 2) 停掉 zenproxy（只停它，new-api 与 resin 完全不受影响）
sudo docker compose -f /opt/zenproxy/docker-compose.yml down

# 3) 状态文件留在 /opt/zenproxy/data/state.json（账号↔出口绑定）。
#    想彻底清干净：
sudo rm -rf /opt/zenproxy
docker rmi zenproxy:latest

# 4) 换回 LiteLLM 路线？见第十节，先 down 再 up，别两条一起跑
```

想「暂停」而不是「下线」：`sudo docker compose stop`，容器留着，数据也在。

---

## 10. 备选路线：zenproxy + LiteLLM + PostgreSQL

给**没有 new-api、本来就在用 LiteLLM**的人。文件是 `docker-compose.litellm.yml`
（外加 `litellm.config.yaml`），三个服务：zenproxy / litellm / postgres。

与默认路线的差异，务必看清：

| | 默认路线 | LiteLLM 路线 |
| :--- | :--- | :--- |
| 服务数 | 1（zenproxy） | 3（+ litellm + postgres） |
| zenproxy 的 `proxies.resin.host` | `resin:2260` | `host.docker.internal:2260`（需 `extra_hosts`） |
| 网络 | 加入两个已有网络 | 自建 `zen` 网络 |
| `.env` 额外变量 | 无 | `LITELLM_MASTER_KEY` / `POSTGRES_PASSWORD` / `DATABASE_URL`（详见该文件头部注释） |
| 网关侧接入点 | 渠道的 **Base URL** | LiteLLM 的 `api_base` |

两条路线**互斥**，不要同时起：都占宿主 `127.0.0.1:9000`，容器名都叫 `zenproxy`，
且各自的 `config.yaml` 内容不同（Resin 地址 + `state_file`）。
建议放**不同目录**（例如 `/opt/zenproxy-litellm/`），各自带自己的 `config.yaml` 与 `.env`。

---

## 11. 排障

先看这三条，多数问题一眼就定位：

```bash
sudo docker compose -f /opt/zenproxy/docker-compose.yml ps
sudo docker compose -f /opt/zenproxy/docker-compose.yml logs -f zenproxy
docker inspect zenproxy --format '{{json .NetworkSettings.Networks}}'
```

| 症状 | 多半是 |
| :--- | :--- |
| new-api 报 502 / 连接被拒 | zenproxy 不在 `new-api_default` 网络上 —— 看上面第三条命令 |
| 容器起不来，日志 `invalid config: ... unexpanded ${VAR}` | `.env` 里没那个变量，或 compose 的 `environment:` 段漏注入（`config.yaml` 只会读环境变量） |
| 容器起不来，日志 `server.admin_token is empty` | `ZEN_ADMIN_TOKEN` 为空 |
| 容器起不来，日志 `invalid config` + 权限/读文件错误 | `config.yaml` 属主不对：容器非 root，需要 `root:10001` 且可读（`sudo ./install.sh` 会设好） |
| 容器起不来，`error while creating mount source path` / 写不进去 | `data/` 属主不对，或宿主机上那个路径不存在 |
| `exits_healthy: 0` | Resin 不通：token 不对 / `resin:2260` 不通 / resin 不在 `resin_default` 网络上 |
| 请求能通，但出口 IP 从来不变 | 所有槽位被 Resin 锚在同一节点，或账号的 `proxy` 填了不存在的出口 ID（日志有 `account references unknown exit`） |
| `/healthz` 正常但容器 `unhealthy` | 镜像里没有 `wget`（换 distroless 时会出现），改用宿主侧探活 |
| 一直是 401/403 且 `last_class: auth` | 账号密钥本身的问题，不是出口问题 |
| 一直是 403 且 `last_class: tier_gate` | 见 7.1，跟 IP 无关 |
| 流式响应 2 分钟就断 | new-api 的 `STREAMING_TIMEOUT`，见 7.2 |
| 渠道用一阵子突然被禁用 | `auto_ban` 记了出口池全挂时的 502，见 7.3 |
| 改了 `config.yaml` 不生效 | 配置在启动时加载一次：`sudo docker compose restart zenproxy` |

---

## 12. 密钥纪律

* 所有凭据只存在于 `/opt/zenproxy/.env`（`0600 root:root`）。`config.yaml` 里一律写
  `${ZEN_KEY_01}` 这种引用，**任何 YAML 模板里都不出现字面量密钥**。
* 变量缺失时启动即失败并点名是哪一个，不静默降级 —— 这是刻意的。
* `.env` 与 `data/` 已在 `deploy/.gitignore` 里。
* zenproxy 的接口**永远不回显完整密钥**，只返回 `sk-a…rect` 这样的提示串。
* `.env` 不进容器：`docker-compose.yml` 用 `environment:` 逐项注入，只有 4 个变量会出现在
  容器的环境变量表里。核对方法：

  ```bash
  docker compose -f /opt/zenproxy/docker-compose.yml config | grep -A 6 'environment:'
  ```

---

## 附录 A：为什么没有 nginx vhost / systemd unit

这两样在默认路线里都是多余甚至有害的，所以上一版的 `nginx-zen.conf` 与
`zenproxy.service` 已删除：

* **nginx vhost**：公网入口是 new-api，zenproxy 只需要宿主回环上的 `9000`。
  远端看控制台请用 SSH 隧道（附录 B），**不要**为它开一个对外的 vhost ——
  `/admin/api/*` 只有一个共享 Bearer token，拿到就等于拿到全部账号的出口控制权。
* **systemd unit**：那是裸机路线（本地二进制 + unit）的产物。默认路线是容器，
  `install.sh` 不再安装二进制与 unit；留一个引用 `/opt/zenproxy/zenproxy` 的 unit
  只会和实际部署对不上。真要裸机跑：仓库根 `go build ./cmd/zenproxy`，
  `config.yaml` 里把 `proxies.resin.host` 改成 `127.0.0.1:2260`、
  `state_file` 改成 `./data/state.json`，并注意它和容器路线会抢宿主 9000 端口。

## 附录 B：远端安全地看控制台

```bash
# 在你自己的机器上开隧道，把远端回环的 9000 映射到本地 9000
ssh -N -L 9000:127.0.0.1:9000 <user>@<宿主机>

# 然后本地浏览器打开（token 会存进 localStorage 并从地址栏抹掉）
#   http://127.0.0.1:9000/admin/?token=<admin_token>
```
