package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	memory_storage "github.com/storage-lock/go-memory-storage"
	storage_lock "github.com/storage-lock/go-storage-lock"
	"github.com/stretchr/testify/require"
)

var testIPCounter atomic.Uint32

// nextTestIP 生成 127.x.y.z 形式的唯一回环地址。
func nextTestIP() string {
	n := testIPCounter.Add(1)
	return fmt.Sprintf("127.%d.%d.%d:1000", (n>>16)&0xff, (n>>8)&0xff, n&0xff)
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	key, err := newTenantKey("")
	require.NoError(t, err)
	s := newServer(memory_storage.NewMemoryStorage(), key,
		newLimiters(120, 600),
		storage_lock.StorageLockOptions{LeaseExpireAfter: 30 * time.Second},
		nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return s
}

// newTenant 调用签发接口拿一对 id/secret。
func newTenant(t *testing.T, srv *Server) (tenantID, secret string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/tenant", nil))
	require.Equal(t, http.StatusCreated, rec.Code)
	var body tenantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotEmpty(t, body.TenantID)
	require.NotEmpty(t, body.Secret)
	return body.TenantID, body.Secret
}

func do(srv *Server, method, path, secret, body string) (*httptest.ResponseRecorder, map[string]any) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	// 每个请求用不同的回环地址，避免测试之间互相触发 IP 限流；
	// 租户限流仍然生效（那是被测行为的一部分）。
	req.RemoteAddr = nextTestIP()
	if secret != "" {
		req.Header.Set(tenantHeader, secret)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// TestUnauthenticatedRejected 验证没有凭据就不能碰锁。
func TestUnauthenticatedRejected(t *testing.T) {
	srv := newTestServer(t)
	rec, _ := do(srv, http.MethodPost, "/api/v1/locks/a/acquire", "", `{"owner_id":"x"}`)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestForgedTenantIDIsIgnored 验证请求体里塞别人的 tenant_id 没有用——
// 服务端只认 secret 派生的 id。
func TestForgedTenantIDIsIgnored(t *testing.T) {
	srv := newTestServer(t)
	_, secretA := newTenant(t, srv)

	rec, _ := do(srv, http.MethodPost, "/api/v1/locks/a/acquire", secretA,
		`{"owner_id":"x","tenant_id":"victim"}`)
	// tenant_id 是未知字段，DisallowUnknownFields 会直接拒绝，避免"看起来生效了"。
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestCrossTenantIsolation 是最核心的安全用例：
// 两个租户用同一个 lock id，彼此的锁完全不可见、不可抢。
func TestCrossTenantIsolation(t *testing.T) {
	srv := newTestServer(t)
	_, secretA := newTenant(t, srv)
	_, secretB := newTenant(t, srv)

	// A 拿锁。
	rec, body := do(srv, http.MethodPost, "/api/v1/locks/shared/acquire", secretA, `{"owner_id":"owner-a"}`)
	require.Equal(t, http.StatusOK, rec.Code, body)

	// B 用同一个 lock id 也必须能拿到——两者在存储里是不同 key。
	rec, body = do(srv, http.MethodPost, "/api/v1/locks/shared/acquire", secretB, `{"owner_id":"owner-b"}`)
	require.Equal(t, http.StatusOK, rec.Code, body)

	// 各自 inspect 只能看到自己的持有者。
	rec, body = do(srv, http.MethodGet, "/api/v1/locks/shared", secretA, "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "owner-a", body["owner_id"])

	rec, body = do(srv, http.MethodGet, "/api/v1/locks/shared", secretB, "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "owner-b", body["owner_id"])

	// A 释放后，B 的锁不受影响。
	rec, _ = do(srv, http.MethodPost, "/api/v1/locks/shared/release", secretA, `{"owner_id":"owner-a"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	rec, body = do(srv, http.MethodGet, "/api/v1/locks/shared", secretB, "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "owner-b", body["owner_id"], "A 的释放不应影响 B")
}

// TestListDoesNotLeakAcrossTenants 验证列表接口不跨租户泄露。
func TestListDoesNotLeakAcrossTenants(t *testing.T) {
	srv := newTestServer(t)
	_, secretA := newTenant(t, srv)
	_, secretB := newTenant(t, srv)

	_, body := do(srv, http.MethodPost, "/api/v1/locks/a-lock/acquire", secretA, `{"owner_id":"a"}`)
	require.NotNil(t, body)
	_, body = do(srv, http.MethodPost, "/api/v1/locks/b-lock/acquire", secretB, `{"owner_id":"b"}`)
	require.NotNil(t, body)

	rec, _ := do(srv, http.MethodGet, "/api/v1/locks", secretA, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var list []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 1, "租户 A 只应看到自己的锁")
	require.Equal(t, "a-lock", list[0]["lock_id"])

	rec, _ = do(srv, http.MethodGet, "/api/v1/locks", secretB, "")
	var listB []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listB))
	require.Len(t, listB, 1)
	require.Equal(t, "b-lock", listB[0]["lock_id"])
}

// TestSameTenantMutualExclusion 确认同一租户内部仍然是真正的互斥。
func TestSameTenantMutualExclusion(t *testing.T) {
	srv := newTestServer(t)
	_, secret := newTenant(t, srv)

	rec, body := do(srv, http.MethodPost, "/api/v1/locks/k/acquire", secret, `{"owner_id":"a"}`)
	require.Equal(t, http.StatusOK, rec.Code, body)

	rec, body = do(srv, http.MethodPost, "/api/v1/locks/k/acquire", secret, `{"owner_id":"b"}`)
	require.Equal(t, http.StatusConflict, rec.Code, "同租户第二个 owner 必须忙")
	require.Equal(t, "lock_busy", body["code"])
}

// TestNonOwnerCannotRelease 验证不能释放别人的锁。
func TestNonOwnerCannotRelease(t *testing.T) {
	srv := newTestServer(t)
	_, secret := newTenant(t, srv)

	_, body := do(srv, http.MethodPost, "/api/v1/locks/k/acquire", secret, `{"owner_id":"real"}`)
	require.NotNil(t, body)

	rec, body := do(srv, http.MethodPost, "/api/v1/locks/k/release", secret, `{"owner_id":"impostor"}`)
	require.Equal(t, http.StatusForbidden, rec.Code, body)
}

// TestRenewExtendsLease 验证续租真的延长了租约。
func TestRenewExtendsLease(t *testing.T) {
	srv := newTestServer(t)
	_, secret := newTenant(t, srv)

	rec, body := do(srv, http.MethodPost, "/api/v1/locks/k/acquire", secret, `{"owner_id":"a","ttl":"5s"}`)
	require.Equal(t, http.StatusOK, rec.Code, body)

	before, _ := do(srv, http.MethodGet, "/api/v1/locks/k", secret, "")
	var viewA map[string]any
	_ = json.Unmarshal(before.Body.Bytes(), &viewA)

	rec, body = do(srv, http.MethodPost, "/api/v1/locks/k/renew", secret, `{"owner_id":"a","ttl":"10s"}`)
	require.Equal(t, http.StatusOK, rec.Code, body)

	after, _ := do(srv, http.MethodGet, "/api/v1/locks/k", secret, "")
	var viewB map[string]any
	_ = json.Unmarshal(after.Body.Bytes(), &viewB)
	expA, _ := viewA["lease_expire"].(string)
	expB, _ := viewB["lease_expire"].(string)
	require.NotEqual(t, expA, expB)
	ta, err := time.Parse(time.RFC3339Nano, expA)
	require.NoError(t, err)
	tb, err := time.Parse(time.RFC3339Nano, expB)
	require.NoError(t, err)
	require.True(t, tb.After(ta), "续租后过期时间应更晚")
}

// TestInvalidTTLRejected 验证 TTL 边界。
func TestInvalidTTLRejected(t *testing.T) {
	srv := newTestServer(t)
	_, secret := newTenant(t, srv)
	for _, ttl := range []string{"0s", "-1s", "abc", "100h"} {
		rec, _ := do(srv, http.MethodPost, "/api/v1/locks/k/acquire", secret,
			`{"owner_id":"a","ttl":"`+ttl+`"}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, "ttl=%s 应被拒绝", ttl)
	}
}

// TestOversizedBodyRejected 验证请求体上限。
func TestOversizedBodyRejected(t *testing.T) {
	srv := newTestServer(t)
	_, secret := newTenant(t, srv)
	big := `{"owner_id":"` + strings.Repeat("x", 20<<10) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/locks/k/acquire", bytes.NewBufferString(big))
	req.Header.Set(tenantHeader, secret)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestReadLockSharedWithinTenant 验证读锁在租户内共享。
func TestReadLockSharedWithinTenant(t *testing.T) {
	srv := newTestServer(t)
	_, secret := newTenant(t, srv)

	rec, body := do(srv, http.MethodPost, "/api/v1/locks/k/acquire", secret, `{"owner_id":"r1","mode":"read"}`)
	require.Equal(t, http.StatusOK, rec.Code, body)
	rec, body = do(srv, http.MethodPost, "/api/v1/locks/k/acquire", secret, `{"owner_id":"r2","mode":"read"}`)
	require.Equal(t, http.StatusOK, rec.Code, body)

	// 写者必须被挡住。
	rec, body = do(srv, http.MethodPost, "/api/v1/locks/k/acquire", secret, `{"owner_id":"w","mode":"write"}`)
	require.Equal(t, http.StatusConflict, rec.Code, body)
}

// TestTenantSecretIsNotStoredInPlaintext 是结构性断言：
// 服务端只持有 HMAC 密钥，tenant id 由 secret 派生；换个 secret 就是另一个租户。
func TestTenantSecretIsNotStoredInPlaintext(t *testing.T) {
	srv := newTestServer(t)
	idA, secretA := newTenant(t, srv)
	require.NotEqual(t, secretA, idA, "tenant id 不应等于 secret")
	require.False(t, strings.Contains(idA, secretA))
	require.Len(t, idA, 22, "派生 id 是 16 字节 base64url")

	// 同一个 secret 必须得到同一个租户 id（无状态）。
	require.Equal(t, idA, srv.key.derive(secretA))
	// 另一个 secret 得到另一个租户。
	_, secretB := newTenant(t, srv)
	require.NotEqual(t, idA, srv.key.derive(secretB))

	ctx := context.Background()
	_ = ctx
}

// TestTokenBucketRefills 验证令牌桶按时间恢复。
func TestTokenBucketRefills(t *testing.T) {
	l := newTokenBucketLimiter(2, 1, time.Minute) // 容量 2，每秒补 1
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }

	ok, _ := l.allow("k")
	require.True(t, ok)
	ok, _ = l.allow("k")
	require.True(t, ok)
	ok, wait := l.allow("k")
	require.False(t, ok)
	require.Greater(t, wait, time.Duration(0))

	now = now.Add(time.Second)
	ok, _ = l.allow("k")
	require.True(t, ok, "1 秒后应恢复一个令牌")
}

// TestBucketSweepReleasesMemory 验证长期不用的桶会被回收，
// 这是原实现（map 只增不减）的 OOM 问题的回归测试。
func TestBucketSweepReleasesMemory(t *testing.T) {
	l := newTokenBucketLimiter(10, 1, time.Minute)
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }

	for i := 0; i < 1000; i++ {
		l.allow(strings.Repeat("x", i%50) + string(rune('a'+i%26)))
	}
	require.Greater(t, len(l.buckets), 0)

	now = now.Add(10 * time.Minute)
	l.sweep()
	require.Equal(t, 0, len(l.buckets), "空闲桶应被清理")
}

// TestClientIPIgnoresForgedXFF 验证直连客户端伪造 X-Forwarded-For 无效。
func TestClientIPIgnoresForgedXFF(t *testing.T) {
	trusted, err := parseCIDRs("127.0.0.0/8")
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:1234" // 非受信任来源
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	require.Equal(t, "203.0.113.7", clientIP(req, trusted),
		"非受信任来源的 XFF 必须被忽略")

	// 来自本机 nginx 的请求才采信 XFF。
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.9")
	require.Equal(t, "198.51.100.9", clientIP(req, trusted))
}

// TestHealthEndpoint 验证健康检查。
func TestHealthEndpoint(t *testing.T) {
	srv := newTestServer(t)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	require.Equal(t, http.StatusOK, rec.Code)
}
