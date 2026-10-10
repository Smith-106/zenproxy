# Resin 出口池实操

本文记录的是**在你服务器上实测得到**的结果，不是 Resin 官方文档的复述。
测试时间：2026-10-09，服务器 `oracle-ubuntu`。

---

## 一、zenproxy 怎么用 Resin

zenproxy 不管理 Resin 的节点，它只把 Resin 的**粘性身份**当成一个个出口端点的地址。

配置：

```yaml
proxies:
  resin:
    enabled: true
    scheme: "socks5h"
    host: "resin:2260"        # 同容器网络里的服务名；宿主机部署则填 127.0.0.1:2260
    platform: "Default"
    token: "${RESIN_PROXY_TOKEN}"
    accounts:
      - "slot-01"
      - "slot-02"
      - "slot-03"
```

`accounts` 里每一项会被展开成一个池子成员，ID 形如 `resin-slot-01`，
地址形如 `socks5h://Default.slot-01:<token>@resin:2260`。

**`accounts` 的数量决定并发出口的上限。** 它和你有几个上游账号没有关系——
多个账号可以共享同一个出口成员。

> Resin 的环境变量：`RESIN_PORT`、`RESIN_LISTEN_ADDRESS`、`RESIN_PROXY_TOKEN`、`RESIN_ADMIN_TOKEN`。
> 取 token（不打印内容）：
> ```bash
> docker inspect resin --format '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^RESIN_PROXY_TOKEN=//p'
> ```

---

## 二、粘性身份到底保证了什么（实测）

Resin 的身份格式是 `Platform.<Account>:<token>`。我用同一个 Account 连续请求了两次，
走的是两个**不同的** IP 回显服务：

```
slot-03  first=[50.6.207.27]  second=[50.6.207.27]     ← 两次同一个 IP
```

**结论：在同一个 Account 名下，短时间内出口 IP 是稳定的。** 这正是 zenproxy 需要的性质——
它保证了一次会话不会中途换 IP。

**但它不是永久的。** 大约 30 秒后再测，`slot-03` 已经失败了，换成 `slot-02` 成功：

```
slot-01  curl: (97) Can't complete SOCKS5 connection to api.ipify.org. (1)   [5.00s]
slot-02  34.132.56.138                                                        [20.84s]
slot-03  curl: (97) Can't complete SOCKS5 connection to api.ipify.org. (1)   [5.00s]
slot-04  curl: (97) Can't complete SOCKS5 connection to api.ipify.org. (1)   [5.00s]
```

所以准确的表述是：**只要被分配到的那个节点还活着，Account 到出口的映射就稳定；
节点死了，同一个 Account 会漂到别的节点上去。** 这正是 zenproxy 要在出口层做探活和轮换的原因。

---

## 三、池子的真实质量（重要）

直接查它的状态库（只读打开，不写入）。Resin 的池子落在宿主的
`/opt/resin/data/cache/cache.db`（SQLite），关键表是 `nodes_static`（节点定义）、
`nodes_dynamic`（每个节点的健康状态）、`node_latency`（延迟 EWMA）：

```bash
DB=/opt/resin/data/cache/cache.db
sudo sqlite3 "file:$DB?mode=ro" "select count(*) from nodes_dynamic;"                  -- 117374
sudo sqlite3 "file:$DB?mode=ro" "select count(*) from nodes_dynamic where failure_count>=3;"   -- 109799
sudo sqlite3 "file:$DB?mode=ro" "select count(*) from nodes_dynamic where circuit_open_since>0;" -- 110618
sudo sqlite3 "file:$DB?mode=ro" "select count(*) from nodes_dynamic where egress_ip<>'';"      -- 7878
```

实测结果：

| 指标 | 数量 | 占比 |
| :--- | ---: | ---: |
| 节点总数 | 117,374 | 100% |
| `failure_count ≥ 3` | 109,799 | **93.5%** |
| 当前熔断中（`circuit_open_since`） | 110,618 | **94.2%** |
| 有已知出口 IP（即真的能用过） | 7,878 | 6.7% |
| 延迟测量条数 | 23,901 | — |
| 其中 < 1 秒 | 19,300 | 80.7% |
| 其中 ≥ 10 秒 | 10 | 0.0% |

两个结论都是反直觉的：

**1. 池子规模 ≠ 可用出口数量，而且 94% 已经被 Resin 自己熔断了。**
`circuit_open_since` 就是它的熔断器。所以"把坏节点从 Resin 里删掉"基本是无效操作——
它们本来就不参与派发，而且删了之后 24 个订阅源（145,308 条绑定）下次同步就灌回来。

**2. 病不在延迟，在不稳定。**
23,901 条延迟测量里 80.7% 在 1 秒内，超过 10 秒的只有 10 条。
节点一旦活着通常是快的；问题是它**下一秒可能就死了**。
所以别去调延迟相关的参数，要调的是**淘汰**和**重试预算**。

### 该调什么

| 配置 | 建议值 | 为什么 |
| :--- | :--- | :--- |
| `health.retire_after` | `20` | 死透的出口要真的离开池子，而不是隔 30 分钟复活一次。**这是最关键的一条** |
| `health.max_latency` | `5s` | 少数"活着但很慢"的出口降为备用 |
| `health.failure_threshold` | `2` ~ `3` | 节点会猝死，别给太多次机会 |
| `health.interval` | `60s` | 池子变化快，探活要勤 |
| `health.reachability` | 你的上游 `host:port` | 比 ping 通用地址更有意义：验证的是"能不能到我要去的地方" |
| `policy.max_attempts` | `3` ~ `4` | 单成员成功率低，重试预算要够 |
| `proxies.resin.accounts` | 多给几个 | 单成员成功率低，池子太小的意义不大 |

`health.probe_url` 会被用来取每个出口的**真实出口 IP**（只有真的是 IP 形态的响应才会被采信），
显示在控制台里。用它来确认各成员确实处在不同 IP 上。
坏掉的出口可以在控制台点「清理垃圾出口」一次性永久移出轮换，详见仓库根 README 的「清理垃圾出口」一节。

### 池子该开多大：实测

2026-10-10 在 `oracle-ubuntu` 上连测两批（`scripts/probe-resin-slots.sh`）：

| 批次 | 可用 / 总数 | 备注 |
| :--- | ---: | :--- |
| `slot-01` … `slot-12` | **2 / 12** | 失败的基本都是 5 秒 SOCKS5 超时 |
| `slot-13` … `slot-24` | **7 / 12** | 同一套配置，几分钟后测，成功率差 3 倍 |

两批合计 ≈ 38%，**而且批次之间波动极大**——这正是「节点会猝死」的直接体现。

结论：**`accounts` 只写 2 个是不够的**。默认模板里写 `slot-01`/`slot-02` 只是示意，
按实测成功率算，2 个成员平均只有不到 1 个活着。把池子开到 24 个之后，
`/admin/api/proxies` 里能稳定看到 5–7 个成员持有真实出口 IP：

```bash
sudo scripts/set-resin-slots.py 24      # 扩到 24 个 slot
sudo docker restart zenproxy            # config.yaml 是 bind mount，改完要重启才生效
```

> ★ **`restart` 与 `up -d` 不能互相替代，选错会让服务起不来。** 判据是「改的东西在
> 容器的环境里，还是在挂载的文件里」：
>
> | 改了什么 | 命令 | 为什么 |
> | :--- | :--- | :--- |
> | `config.yaml`（bind mount） | `docker restart` | 进程重读挂载文件即可 |
> | `docker-compose.yml` 的 `environment:` | **`docker compose up -d`** | `restart` **不会重新注入环境变量**，容器仍用创建时的旧 env |
> | `.env` 里被 compose 引用的变量 | **`docker compose up -d`** | 同上——`.env` 是 compose 读取的，不是容器直接读的 |
>
> 上面第三行是最容易踩的：往 `.env` 里加一个 `ZEN_KEY_02`、又在 compose 的
> `environment:` 里加一行引用，然后 `docker restart` —— **变量根本没进容器**，
> 而 zenproxy 的启动闸门会直接拒绝启动：
>
> ```
> account "zen-02" api_key still contains an unexpanded ${ZEN_KEY_02} —
> set that environment variable, or the literal placeholder would be sent upstream as the key
> ```
>
> 容器于是进入 `restarting (unhealthy)` 循环，**整个入口下线**（实测踩过一次，
> 服务中断约一分钟）。这个闸门是刻意的——宁可起不来，也不要把字面量 `${ZEN_KEY_02}`
> 当 key 发给上游。正确的恢复动作就是补一条 `sudo docker compose up -d --force-recreate`。
>
> 自查一行：`docker exec zenproxy sh -c 'env | grep -c "^ZEN_KEY"'` 的数字应该等于
> 你启用的账号数。

扩完之后连打 10 次免费模型请求（`scripts/soak-rotation.sh 10`）的结果是 **10/10 成功**，
单次 1.3–3.5 秒；池子只有 2 个成员时失败率是肉眼可见的。

代价是探活更重：24 个成员 × 每个最多 5 秒超时，一轮探活要一分钟以上。
`health.interval` 保持 `60s`，别调更密。

---

## 四、验证

### 池子成员是否各自可用

```bash
TOK=$(docker inspect resin --format '{{range .Config.Env}}{{println .}}{{end}}' | sed -n 's/^RESIN_PROXY_TOKEN=//p')
for acct in slot-01 slot-02 slot-03; do
  printf '%-8s ' "$acct"
  curl -sS --max-time 25 -x "socks5h://Default.$acct:$TOK@127.0.0.1:2260" \
       -w ' [HTTP=%{http_code} %{time_total}s]\n' https://api.ipify.org 2>&1 | tail -1
done
```

（把 `127.0.0.1:2260` 换成 `resin:2260` 即可在容器内跑。）

### zenproxy 侧看到的出口 IP

```bash
curl -s -H "X-Admin-Token: $ZEN_ADMIN_TOKEN" \
  http://127.0.0.1:9000/admin/api/proxies
```

关注每个成员的 `exit_ip`、`latency_ms`、`healthy`、`quarantined_until`、`last_error`。
强制探活一轮：`curl -sX POST -H "X-Admin-Token: ..." .../admin/api/proxies/probe`。

---

## 五、排障

| 现象 | 原因与处理 |
| :--- | :--- |
| 所有成员都是 `SOCKS5 connection ... (1)`，约 5 秒返回 | Resin 找不到可用节点为这个 Account 提供服务。这是**池子侧的**问题，zenproxy 无能为力——多加成员、或给 Resin 配更好的节点源 |
| 成员时好时坏 | 正常。这就是要开探活和轮换的原因 |
| 换了出口但出口 IP 没变 | 同一个 Account 的粘性：只要它被分配到的节点还活着就不会换。要换 IP 得换 Account 名（即换池子成员） |
| `407` / 认证失败 | `RESIN_PROXY_TOKEN` 不对，或 token 里的特殊字符没做 URL 编码 |
| 出口 IP 是 `45.74.31.x` 这类地址 | 那是公共代理，质量不稳定。有条件的话给 Resin 配静态节点（`proxies.static` 也支持，zenproxy 会把它和 Resin 成员放进同一个池子） |

---

## 六、和静态出口混用

zenproxy 允许把 Resin 成员和自己手上的静态代理放进**同一个池子**，
轮换和探活逻辑完全一致：

```yaml
proxies:
  resin:
    enabled: true
    host: "resin:2260"
    token: "${RESIN_PROXY_TOKEN}"
    accounts: ["slot-01", "slot-02"]
  static:
    - "socks5h://user:pass@1.2.3.4:1080"
    - "http://user:pass@5.6.7.8:8080"
```

在出口质量差异很大时这是最实用的配置：静态出口当主力，Resin 成员兜底，
`max_latency` 会把慢的自动降到备用位。
