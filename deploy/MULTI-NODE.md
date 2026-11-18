# 多节点部署

## 先说结论

**多节点必须共用同一份存储。** 否则同一个 lock id 在不同节点上是两把不同的锁，
互斥直接失效——这比单点更危险，因为它看起来在工作。

```
        ┌──────────────┐   ┌──────────────┐   ┌──────────────┐
客户端 →│ 北京节点     │   │ 新加坡节点    │   │ 美西节点      │
        │ (只是接入)   │   │ (只是接入)    │   │ (只是接入)    │
        └──────┬───────┘   └──────┬───────┘   └──────┬───────┘
               └───────────────┬──┴──────────────────┘
                               ▼
                    ┌─────────────────────┐
                    │ 唯一的事实来源       │
                    │ PostgreSQL / Redis  │
                    └─────────────────────┘
```

SQLite 只能单机（文件锁跨不了主机）。多节点请换用：

| 后端 | 需要的 go-storage 实现 | 前提 |
|---|---|---|
| PostgreSQL | `go-postgresql-storage` | 单个主库；`UPDATE ... WHERE version=` 原子 |
| MySQL / MariaDB | `go-mysql-storage` / `go-mariadb-storage` | 同上 |
| Redis | `go-redis-storage` | 单实例或 Sentinel；**不能**用 cluster 分片（同一 key 必须落在同一节点，否则 CAS 失效） |
| etcd | `go-etcd-storage` | 天然多副本，适合跨地域 |

本仓库当前实现用 SQLite。要接网络存储，改 `openStorage()` 里的构造即可，
`storage.Storage` 接口一致，其余代码不用动。

## 步骤

### 1. 一台机器先起存储

```bash
# 以 PostgreSQL 为例（各节点都连它）
docker run -d --name lockdb \
  -e POSTGRES_PASSWORD=<强密码> \
  -e POSTGRES_DB=storage_lock \
  -p 127.0.0.1:5432:5432 postgres:16-alpine
```

跨地域时建议在数据库侧做 TLS + 只放行各节点 IP。

### 2. 每个节点用同一个主密钥启动

`STORAGE_LOCK_TENANT_KEY` **必须所有节点一致**——它决定 tenant_id 的派生，
不一致意味着同一个 secret 在不同节点是不同的租户，锁就分叉了。

```bash
# 只在一台机器生成一次，然后安全地分发到所有节点
head -c 32 /dev/urandom | base64
```

### 3. 每个节点公布完整节点清单

```bash
cloud-api \
  --addr 127.0.0.1:8080 \
  --public-url https://cn.example.com \
  --endpoints "https://cn.example.com|cn-east|上海,https://sg.example.com|ap-sg,https://us.example.com|us-west"
```

清单要在每个节点上保持一致（内容可以一样）。`/api/v1/endpoints` 会原样返回它。

### 4. 客户端就近选点

```python
lock = CloudLock("https://cn.example.com")
lock.base_url = lock.pick_fastest()   # 测 /health 延迟，跳过挂掉的节点
```

**为什么让客户端选而不是服务端**：服务端测到的是节点间延迟，不是客户端到节点的延迟。
跨地域时两者差别很大。

DNS 层面也可以做（GeoDNS / 分线路解析），但那需要域名控制权与 DNS 服务商支持；
客户端主动测速更通用，也允许用户手动指定。

### 5. 健康检查与故障转移

任何节点不可用时，客户端换下一个即可——锁状态在共享存储里，不会随节点丢失。
不要去"复制"锁数据到另一个节点，那不是故障转移，那是制造两个事实来源。

## 不建议做的事

- **不要**把每个节点配一个独立的 SQLite/Redis 然后宣称"多节点高可用"。
- **不要**用 Redis Cluster 分片存锁记录（同一 lock id 的 CAS 需要落在同一分片）。
- **不要**在节点间做异步复制锁记录——异步复制窗口内两边都能拿到锁。
- **不要**用多个独立后端做 quorum（RedLock 类方案）；本项目明确不提供，
  理由见 [go-storage-lock 的能力边界](https://github.com/storage-lock/go-storage-lock#能力边界)。

## 当前 VPS 实测状态

单节点部署已在 `leetcode-001`（103.236.93.62）验证：

- 容器 `storage-lock-cloud` 运行中，只监听 `127.0.0.1:18000`
- SQLite 落在 `/srv/storage-lock/data/locks.db`
- 已验证：20 并发抢锁 1 成功 / 19 冲突；容器重启后数据完整；跨租户隔离成立

**TLS 入口已验证可用**：在同一台机器上用 `nginx:1.27-alpine`（`--network host`）
挂 `deploy/nginx.conf`（自签证书替代 certbot），实测：

- 明文端口 301 跳 HTTPS
- 经 TLS 领取租户 → 抢锁 → 列表，全链路正常
- 网关收到的 `X-Forwarded-For` 来自 nginx 覆写，限流按真实客户端 IP

正式上线只需把自签证书换成 `certbot --nginx -d <域名>` 签发的正式证书。

注意：nginx 若跑在**容器里**必须用 `--network host`，否则容器内的
`127.0.0.1:18000` 指的是容器自己而不是宿主机网关。直接装在宿主机上则不受此限。
