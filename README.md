# Storage Lock Cloud API

把 `go-storage-lock` 暴露成**免费、免注册、按租户隔离**的公共锁服务。你有一台 VPS 就能跑。

## 它解决什么

你想让多个进程/多台机器/多个 Agent 共享同一把分布式锁，但不想为每个调用方搭
Redis/etcd/数据库。这个服务把锁的全部语义（租约、fencing token、读写锁、公平排队）
放在服务端，客户端只做 HTTP 调用。

## 三条设计原则

### 1. 免注册 —— 但身份是真的

客户端第一次调用 `POST /api/v1/tenant` 领取一对 `{tenant_id, secret}`：

```bash
curl -X POST https://locks.example.com/api/v1/tenant
# {"tenant_id":"...","secret":"...","note":"请把 secret 保存在本地…"}
```

- `tenant_id = HMAC(服务端主密钥, secret)` 的截断值。
- 服务端**不留明文 secret**，只持有 HMAC 主密钥。
- 之后每个请求带 `X-Tenant-Secret`；服务端重新算出 `tenant_id` 并比对。
- **请求体里的 `tenant_id` 一律不采信**（带了会被直接 400 拒绝）。

所以：知道别人的 `tenant_id` 没用，你必须拿到对方的 `secret`。
丢了 secret 就回不到原租户——这是免注册的代价，也避免了任何账号体系。

### 2. 真隔离 —— 不只是换个名字

所有 lock id 落库前加 `tenant/<tenant_id>/` 前缀：

- 租户 A 和租户 B 用同一个 lock id `shared`，在存储里是两个不同的 key，互不干扰；
- `GET /api/v1/locks` 只返回本租户前缀的锁（这一条曾经有 bug，见 CHANGELOG）；
- 没有跨租户的枚举、查询、释放路径。

### 3. 免费 + 安全 —— 不牺牲任何一边

| 风险 | 做法 |
|---|---|
| 明文传输 secret | `deploy/nginx.conf` 强制 TLS + HSTS；网关只监听 `127.0.0.1` |
| 有人扫 IP 段打爆内存 | 令牌桶按 `lastSeen` 惰性淘汰 + 后台清理（原实现 map 只增不减） |
| 伪造 `X-Forwarded-For` 绕过限流 | 只采信受信任反代（默认仅回环）覆写的头 |
| 请求体过大 / JSON 注入 | `MaxBytesReader` 8KB + `DisallowUnknownFields` |
| 单个 panic 拖垮进程 | 每请求 recover，返回 500 |
| 慢客户端占住连接 | Read/Write/Idle 超时 + 单次存储操作超时 |
| 资源耗尽 | systemd `MemoryMax=256M`、低权限用户、`ProtectSystem=strict` |

## 三种存储后端

| 模式 | 参数 | 适用 |
|---|---|---|
| 远程存储服务（推荐） | `--remote https://... --remote-token ...` | 多机共用一份锁存储 |
| 本地 SQLite | `--data /var/lib/.../locks.db` | 单机部署 |
| 内存 | `--data ""` | 本地试用，重启即丢 |

`--remote` 指向 [go-remote-storage](https://github.com/storage-lock/go-remote-storage)
起的服务。这样多个进程、多台机器都能共用同一个事实来源，而不用各自运维数据库。

## 快速开始（本机）

```bash
go build -o cloud-api .
./cloud-api --addr 127.0.0.1:8080 --data ./locks.db
# 或接远程存储服务
./cloud-api --addr 127.0.0.1:8080 --remote http://127.0.0.1:8443 --remote-token "$TOKEN"
```

领取租户、抢锁：

```bash
SECRET=$(curl -s -X POST http://127.0.0.1:8080/api/v1/tenant \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["secret"])')

curl -X POST http://127.0.0.1:8080/api/v1/locks/order-42/acquire \
  -H "X-Tenant-Secret: $SECRET" \
  -d '{"owner_id":"worker-1","ttl":"30s"}'
```

## 部署到 VPS

```bash
sudo ./deploy/install.sh
```

脚本会：建专用系统用户 → 生成 `/etc/storage-lock/cloud-api.env` 里的主密钥 →
编译安装 → 装 systemd unit 并启动 → 打印 nginx/TLS 下一步。

然后按 `deploy/nginx.conf` 配 nginx + `certbot --nginx -d locks.example.com`。

Docker 方式：`docker build -f deploy/Dockerfile -t storage-lock-cloud .`，
数据挂卷到 `/data`，环境变量 `STORAGE_LOCK_TENANT_KEY` 必须固定。

### 必须固定的环境变量

```
STORAGE_LOCK_TENANT_KEY=<base64，至少 32 字节>
```

**没设置时服务每次启动随机生成**，重启后所有既有租户全部失效——只适合本地试用。
生成方式：`head -c 32 /dev/urandom | base64`。

## 多节点：就近接入

一批节点跑同一份服务、**共用同一个存储后端**（否则同一 lock id 在不同节点上是
两把不同的锁，互斥直接失效）。每个节点公布完整清单：

```bash
./cloud-api \
  --public-url https://cn.example.com \
  --endpoints "https://cn.example.com|cn-east|上海,https://sg.example.com|ap-sg,https://us.example.com|us-west"
```

```bash
curl https://cn.example.com/api/v1/endpoints
# {"endpoints":[{"url":"https://cn.example.com","region":"cn-east",...},...],
#  "shared_storage":true,
#  "note":"这些节点共用同一份锁存储；客户端应选延迟最低的一个。"}
```

客户端自己测延迟选点（比服务端代选更准——服务端看到的延迟不是客户端看到的）：

```bash
python3 client/client.py --fastest endpoints
```

Python 客户端会自动做这件事，某个节点挂了会跳过。

**存储前提**：SQLite 只能单机。多节点必须换成 PostgreSQL/MySQL/Redis 等
真正的网络存储（`go-postgresql-storage` 等，能力声明都是线性一致 CAS）。

## API

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/tenant` | 领取租户，**唯一不需要凭据的接口** |
| GET | `/api/v1/endpoints` | 节点清单 |
| POST | `/api/v1/locks/{id}/acquire` | 获取锁，body `{"owner_id","mode","ttl","wait"}` |
| POST | `/api/v1/locks/{id}/renew` | 续租 |
| POST | `/api/v1/locks/{id}/release` | 释放（或 `DELETE /api/v1/locks/{id}?owner_id=`） |
| GET | `/api/v1/locks/{id}` | 查看持有者、租约、等待队列 |
| GET | `/api/v1/locks/{id}/waiters` | 等待队列 |
| GET | `/api/v1/locks` | 列出本租户的锁 |
| GET | `/health` | 健康检查（不鉴权、不限流） |

状态码：`401` 无凭据 / `403` 非持有者 / `409` 锁忙或租期已过 /
`429` 限流（带 `Retry-After`）/ `400` 请求体问题。

`ttl` 必须在 **4s ~ 24h** 之间（核心库要求租期大于 3s 的安全余量）。

## 客户端

Python（`client/client.py`）：

```python
from client import CloudLock

lock = CloudLock("https://locks.example.com")
with lock.hold("order-42", ttl="30s") as token:
    # token 是 fencing token；资源侧必须用它做原子护栏
    write_resource_if_fence_is_current(token)
```

`hold()` 自动获取、后台续租、退出释放；续租失败（锁被抢）会抛 `NotOwner`。

## 限流

默认每 IP 每分钟 120 次、每租户每分钟 600 次，可用参数调整。
桶容量取一分钟额度——NAT/公司出口后面很多用户共享 IP，容量太小会互相挤掉。

## 边界

- 单点 SQLite 部署没有高可用。节点挂了服务不可用，但数据不丢。
- 锁服务失联时应按不可用处理，**不能**把缓存或旧副本的状态当成授权。
- 租约过期后旧进程仍可能在跑。服务端返回的 fencing token 必须在资源侧原子校验，
  否则锁正确也不等于业务安全。
- 免费服务不承诺 SLA。secret 丢失无法恢复。
