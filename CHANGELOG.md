# Changelog

## v0.2.1

- 新增 `--remote` / `--remote-token`：接
  [go-remote-storage](https://github.com/storage-lock/go-remote-storage) 作为后端。
  优先级 remote > data > memory。
- 已在 VPS 实测完整链路：客户端 → 本服务（租户隔离）→ remote-storage → SQLite。

## v0.2.0

### 安全修复（破坏性：旧接口行为变化）

- **租户隔离此前实际不成立**。旧实现用客户端自报的 `owner_id` 加内存存储，
  所有用户共享一个命名空间，且进程重启数据全丢。现在：
  - 身份由 `HMAC(服务端主密钥, secret)` 派生，请求体里的 `tenant_id` 不被采信；
  - 所有 lock id 落库前加 `tenant/<id>/` 前缀，列表/查询/释放都按前缀限定。
- **列表接口跨租户泄露**：`handleList` 用空前缀 `ListLocks(storage, "")`，
  挂着 namespace 的 Service 会把所有租户的锁一起返回。（同时修复了
  `storage-lock-http-api` 的同款问题。）
- **限流可被绕过且会 OOM**：原实现按 `c.ClientIP()` 计数，直连客户端伪造
  `X-Forwarded-For` 即可换桶；`clients` map 只增不减，扫一遍 IP 段就爆内存。
  现在只采信受信任反代覆写的头，桶按 `lastSeen` 惰性淘汰 + 后台清理。
- 请求体上限、`DisallowUnknownFields`、单请求 recover、读写/空闲超时。

### 新增

- 免费、免注册的租户签发：`POST /api/v1/tenant`，服务端不留明文 secret。
- SQLite 持久化（WAL + busy_timeout + synchronous=FULL + txlock=immediate），
  重启后租户与锁都还在。
- 多节点就近接入：`/api/v1/endpoints` 公布共用同一存储的节点清单，
  客户端可本地测延迟选择（`client/client.py --fastest`）。
- Python 客户端 `client/client.py`：自动领取并保存 secret、后台续租、
  上下文管理器 `hold()`、401/403/409 映射成异常。
- 部署产物：`deploy/Dockerfile`、`deploy/systemd/*.service`、
  `deploy/nginx.conf`（TLS + HSTS + 覆写 XFF）、`deploy/install.sh`。

### 移除

- 去掉 gin 依赖，改用标准库 `net/http`（更少的传递依赖，也更容易审计）。

## v0.1.0

- 初始版本：基于内存存储的 REST 锁 API。
