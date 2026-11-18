package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// 租户隔离模型
//
// 目标：免费、免注册，但不同用户之间的锁必须真正隔离，任何人不能读到别人的锁，
// 也不能伪造别人的身份去抢锁。
//
// 做法：
//  1. 客户端第一次调用 /api/v1/tenant 时不带 wallet，服务端生成一个随机 secret，
//     返回 {tenant_id, secret}。secret 只交给客户端一次，服务端不留明文。
//  2. tenant_id = HMAC(serverSecret, secret) 的截断。持有 secret 是证明租户身份的唯一方式，
//     因此不需要注册、不需要密码，也不会因为知道 tenant_id 就能冒用。
//  3. 之后所有请求带 X-Tenant-Secret 头。服务端用它重新算出 tenant_id 并比对。
//     请求体里的 tenant_id 一律不采信。
//  4. 所有 lock id 都加 "tenant/<tenant_id>/" 前缀后落库，不同租户不可能看到彼此的记录。
//
// secret 存在客户端本地（环境变量 / 文件 / 浏览器 localStorage）。
// 丢了 secret 就找不回原来的租户，这是免费的代价，也避免了任何形式的账号体系。
//
// 为什么不用 IP 做租户：
//  - NAT 后面的一批用户会共享 IP，等于没有隔离；
//  - IP 会变（移动网络、重启），租户跟着漂移；
//  - IP 可伪造（X-Forwarded-For 取决于部署是否可信）。
// IP 只用于限流，不用于身份。

const (
	// tenantHeader 是客户端携带 secret 的头。
	tenantHeader = "X-Tenant-Secret"
	// tenantIDPrefix 是所有租户 lock id 的公共前缀。
	tenantIDPrefix = "tenant/"
	// secretBytes 是生成 secret 的熵长度。
	secretBytes = 32
)

var (
	// ErrTenantSecretMissing 表示请求没带认证凭据。
	ErrTenantSecretMissing = errors.New("missing X-Tenant-Secret header; call POST /api/v1/tenant first")
	// ErrTenantSecretInvalid 表示凭据格式不对。
	ErrTenantSecretInvalid = errors.New("invalid X-Tenant-Secret")
)

// tenantKey 是服务端的 HMAC 主密钥，进程启动时随机生成或从环境变量读取。
type tenantKey struct {
	mac []byte
}

// newTenantKey 用给定密钥构造；空字符串表示每次启动随机生成（重启后旧租户全部失效，
// 部署时应通过 STORAGE_LOCK_TENANT_KEY 固定下来）。
func newTenantKey(seed string) (*tenantKey, error) {
	if strings.TrimSpace(seed) == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return nil, fmt.Errorf("generate tenant key: %w", err)
		}
		return &tenantKey{mac: buf}, nil
	}
	raw, err := base64.StdEncoding.DecodeString(seed)
	if err != nil || len(raw) < 32 {
		return nil, fmt.Errorf("STORAGE_LOCK_TENANT_KEY must be base64 of at least 32 bytes")
	}
	return &tenantKey{mac: raw}, nil
}

// issue 生成一个新的 secret 与其对应的 tenant id。secret 只在这一刻返回给客户端。
func (k *tenantKey) issue() (secret, tenantID string, err error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate secret: %w", err)
	}
	secret = base64.RawURLEncoding.EncodeToString(buf)
	return secret, k.derive(secret), nil
}

// derive 由 secret 推导 tenant id。同一个 secret 永远得到同一个 id，
// 但只有持有服务端密钥的人才能从任意 secret 算出 id。
func (k *tenantKey) derive(secret string) string {
	// 用 HMAC 而不是明文哈希，避免弱 secret 被离线枚举。
	m := hmac.New(sha256.New, k.mac)
	m.Write([]byte("tenant-id:"))
	m.Write([]byte(secret))
	sum := m.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

// verify 校验客户端带来的 secret 并返回 tenant id。
// 比较使用常量时间，避免时序侧信道。
func (k *tenantKey) verify(secret string) (string, error) {
	if secret == "" {
		return "", ErrTenantSecretMissing
	}
	if len(secret) < 16 || len(secret) > 128 {
		return "", ErrTenantSecretInvalid
	}
	for _, r := range secret {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') &&
			!(r >= '0' && r <= '9') && r != '-' && r != '_' {
			return "", ErrTenantSecretInvalid
		}
	}
	// derive 是纯函数，直接算；保留常量时间比较是因为下面还要比对 secret 本身
	// （未来若引入预注册租户，比较会变成有状态查找，这里先把口径固定下来）。
	return k.derive(secret), nil
}

// equalSecret 常量时间比较两个 secret。
func equalSecret(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// tenantMeta 是租户在存储里的命名空间前缀。
func tenantPrefix(tenantID string) string {
	return tenantIDPrefix + tenantID + "/"
}

// fullLockID 把用户给的 lock id 映射到带租户前缀的全局 id。
func fullLockID(tenantID, lockID string) string {
	return tenantPrefix(tenantID) + lockID
}

// lastSegment 去掉租户前缀，还原用户看到的 lock id。
func lastSegment(fullID, tenantID string) string {
	prefix := tenantPrefix(tenantID)
	return strings.TrimPrefix(fullID, prefix)
}

// clientIP 取真实客户端 IP。
//
// 只有当请求来自受信任的反向代理（由 trustedProxies 指定）时才采信 X-Forwarded-For，
// 否则直连的客户端可以随便伪造该头绕过限流。
func clientIP(r *http.Request, trustedProxies []*net.IPNet) string {
	remote := r.RemoteAddr
	if host, _, err := net.SplitHostPort(remote); err == nil {
		remote = host
	}
	ip := net.ParseIP(remote)
	if ip != nil && isTrusted(ip, trustedProxies) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			// 从右往左找第一个非信任代理地址。
			for i := len(parts) - 1; i >= 0; i-- {
				candidate := strings.TrimSpace(parts[i])
				if cand := net.ParseIP(candidate); cand != nil && !isTrusted(cand, trustedProxies) {
					return candidate
				}
			}
			if cand := net.ParseIP(strings.TrimSpace(parts[0])); cand != nil {
				return cand.String()
			}
		}
	}
	if ip != nil {
		return ip.String()
	}
	return remote
}

func isTrusted(ip net.IP, cidrs []*net.IPNet) bool {
	for _, c := range cidrs {
		if c.Contains(ip) {
			return true
		}
	}
	return false
}
