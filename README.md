# Storage Lock Cloud API

公共的分布式锁云端 API 服务，无需认证，免费使用。

## 架构

```
用户 CLI → HTTPS API → 内部 Firebase → 数据隔离
```

## 特性

- ✅ 无需认证，开箱即用
- ✅ 基于 IP 的限流保护
- ✅ 用户数据隔离（通过租户 ID）
- ✅ RESTful API
- ✅ 免费使用

## API 端点

### 获取锁

```bash
POST /api/v1/lock
Content-Type: application/json

{
  "lock_id": "my-lock",
  "ttl": 30,
  "owner_id": "agent-1"
}
```

### 续租

```bash
POST /api/v1/renew
Content-Type: application/json

{
  "lock_id": "my-lock",
  "owner_id": "agent-1",
  "ttl": 30
}
```

### 释放锁

```bash
POST /api/v1/unlock
Content-Type: application/json

{
  "lock_id": "my-lock",
  "owner_id": "agent-1"
}
```

### 查询锁状态

```bash
GET /api/v1/status/{lock_id}
```

## 限流策略

- 每个 IP 每分钟最多 100 次请求
- 每个租户每分钟最多 1000 次请求

## 安全说明

⚠️ 此服务无需认证，任何人都能访问。数据隔离仅通过租户 ID，无法防止恶意用户。

适合开发测试，不适合生产环境。

## 部署

```bash
go build -o storage-lock-cloud-api .
./storage-lock-cloud-api
```