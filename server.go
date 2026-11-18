package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/storage-lock/go-storage"
	storage_lock "github.com/storage-lock/go-storage-lock"
)

// Server 是面向公网、免注册的锁服务。
//
// 与 storage-lock-http-api 的 admin.Service 的区别：
//   - 租户身份由 HMAC(secret) 派生，请求体里的 tenant 一律不采信；
//   - 所有 lock id 落库前加 tenant 前缀，列表/查询都按前缀过滤；
//   - 每次请求临时按租户构造一个 admin.Service 语义的路径，避免共享可变状态。
//
// 这里没有复用 admin.Service 本身，是因为它按 namespace 绑定一个 Handler，
// 而本服务需要"一个端口服务任意租户"，租户集合是动态的。
type Server struct {
	store    storage.Storage
	key      *tenantKey
	limits   *limiters
	options  storage_lock.StorageLockOptions
	trusted  []*net.IPNet
	maxBody  int64
	logger   *slog.Logger
	nowFn     func() time.Time
	healthFn  func() error
	endpoints *endpointRegistry
}

func newServer(store storage.Storage, key *tenantKey, limits *limiters,
	options storage_lock.StorageLockOptions, trusted []*net.IPNet, logger *slog.Logger) *Server {
	// 无状态 HTTP 服务：租约按 TTL 自然过期，客户端自己续租。
	options.DisableWatchDog = true
	return &Server{
		store: store, key: key, limits: limits, options: options, trusted: trusted,
		maxBody: 8 << 10, logger: logger, nowFn: time.Now,
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/api/v1/tenant", s.handleTenant)
	mux.HandleFunc("/api/v1/tenants", s.handleTenant) // 兼容复数拼写
	mux.HandleFunc("/api/v1/endpoints", s.handleEndpoints)
	mux.HandleFunc("/api/v1/endpoints/best", s.handleBestEndpoint)
	mux.HandleFunc("/api/v1/locks", s.withTenant(s.handleList))
	mux.HandleFunc("/api/v1/locks/", s.withTenant(s.handleLock))
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown endpoint")
	})
	return s.recoverPanic(s.limitByIP(mux))
}

// ---- 中间件 ----

// recoverPanic 保证单个请求里的 panic 不会带走整个进程。
func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic recovered", "path", r.URL.Path, "panic", rec)
				writeErr(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// limitByIP 按真实客户端 IP 限流。
func (s *Server) limitByIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, s.trusted)
		if ok, wait := s.limits.perIP.allow(ip); !ok {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(wait.Seconds())+1))
			writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many requests from this address")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withTenant 校验凭据、按租户限流，并把 tenant id 放进 context。
func (s *Server) withTenant(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret := r.Header.Get(tenantHeader)
		if secret == "" {
			// 兼容：允许用 Authorization: Bearer <secret>。
			if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				secret = strings.TrimPrefix(auth, "Bearer ")
			}
		}
		tenantID, err := s.key.verify(secret)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", err.Error())
			return
		}
		if ok, wait := s.limits.perUser.allow(tenantID); !ok {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(wait.Seconds())+1))
			writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many requests for this tenant")
			return
		}
		next(w, r.WithContext(withTenantID(r.Context(), tenantID)))
	}
}

// ---- 端点 ----

type tenantResponse struct {
	TenantID string `json:"tenant_id"`
	Secret   string `json:"secret"`
	Note     string `json:"note"`
}

// handleTenant 签发新租户。无状态、无注册：调用一次就得到一对 id/secret。
func (s *Server) handleTenant(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	secret, tenantID, err := s.key.issue()
	if err != nil {
		s.logger.Error("issue tenant", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal", "could not issue tenant")
		return
	}
	writeJSON(w, http.StatusCreated, tenantResponse{
		TenantID: tenantID,
		Secret:   secret,
		Note:     "请把 secret 保存在本地，服务端不保留明文；丢失后无法找回原租户。",
	})
}

// handleEndpoints 公布所有共用同一存储的节点，客户端据此就近选择。
// 这个接口不需要租户凭据：选节点发生在拿到 secret 之前。
func (s *Server) handleEndpoints(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	if s.endpoints == nil {
		writeErr(w, http.StatusNotFound, "not_found", "endpoint discovery is not configured")
		return
	}
	writeJSON(w, http.StatusOK, s.endpoints.list())
}

// handleBestEndpoint 由服务端测一次延迟并给出建议。
// 注意：服务端看到的延迟不等于客户端看到的延迟，客户端应优先自己测。
func (s *Server) handleBestEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	if s.endpoints == nil {
		writeErr(w, http.StatusNotFound, "not_found", "endpoint discovery is not configured")
		return
	}
	resp := s.endpoints.list()
	best := s.endpoints.pickFastest(resp.Endpoints)
	writeJSON(w, http.StatusOK, best)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	if s.healthFn != nil {
		if err := s.healthFn(); err != nil {
			writeErr(w, http.StatusServiceUnavailable, "unhealthy", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type lockView struct {
	LockID          string    `json:"lock_id"`
	OwnerID         string    `json:"owner_id,omitempty"`
	Held            bool      `json:"held"`
	ReadLock        bool      `json:"read_lock,omitempty"`
	LockCount       int       `json:"lock_count,omitempty"`
	Version         uint64    `json:"version,omitempty"`
	LeaseExpire     time.Time `json:"lease_expire,omitempty"`
	RemainingLease  string    `json:"remaining_lease,omitempty"`
	WriteIntentOwn  string    `json:"write_intent_owner,omitempty"`
	Waiters         int       `json:"waiters,omitempty"`
}

// handleLock 处理 /api/v1/locks/{id}[/{action}]。
func (s *Server) handleLock(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFrom(r.Context())
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/locks/")
	id, action := splitAction(path)
	if id == "" || strings.Contains(id, "/") {
		writeErr(w, http.StatusNotFound, "not_found", "lock id required")
		return
	}
	// 限制 id 长度与字符集，避免把超长键打进存储。
	if len(id) > 200 {
		writeErr(w, http.StatusBadRequest, "bad_request", "lock id too long (max 200)")
		return
	}

	switch action {
	case "":
		switch r.Method {
		case http.MethodGet:
			s.inspect(w, r, tenantID, id)
		case http.MethodDelete:
			s.release(w, r, tenantID, id)
		default:
			w.Header().Set("Allow", "GET, DELETE")
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		}
	case "acquire":
		s.requirePost(w, r, func() { s.acquire(w, r, tenantID, id) })
	case "renew":
		s.requirePost(w, r, func() { s.renew(w, r, tenantID, id) })
	case "release":
		s.requirePost(w, r, func() { s.release(w, r, tenantID, id) })
	case "waiters":
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
			return
		}
		s.waiters(w, r, tenantID, id)
	default:
		writeErr(w, http.StatusNotFound, "not_found", "unknown action")
	}
}

func (s *Server) requirePost(w http.ResponseWriter, r *http.Request, fn func()) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	fn()
}

type acquireRequest struct {
	OwnerID string `json:"owner_id,omitempty"`
	Mode    string `json:"mode,omitempty"`
	TTL     string `json:"ttl,omitempty"`
	Wait    bool   `json:"wait,omitempty"`
}

type acquireResult struct {
	LockID       string    `json:"lock_id"`
	OwnerID      string    `json:"owner_id"`
	FencingToken uint64    `json:"fencing_token"`
	LeaseExpire  time.Time `json:"lease_expire,omitempty"`
	Remaining    string    `json:"remaining_lease,omitempty"`
}

func (s *Server) lockFor(tenantID, id string, ttl time.Duration) (*storage_lock.StorageLock, error) {
	options := s.options
	options.LockId = fullLockID(tenantID, id)
	if ttl > 0 {
		options.LeaseExpireAfter = ttl
	}
	return storage_lock.NewStorageLockWithOptions(s.store, &options)
}

func (s *Server) acquire(w http.ResponseWriter, r *http.Request, tenantID, id string) {
	var req acquireRequest
	if !decodeBody(w, r, s.maxBody, &req) {
		return
	}
	ttl, ok := parseTTL(req.TTL, s.defaultLease())
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid ttl")
		return
	}
	lock, err := s.lockFor(tenantID, id, ttl)
	if err != nil {
		s.writeLockError(w, err)
		return
	}
	owner := strings.TrimSpace(req.OwnerID)
	if owner == "" {
		owner = storage_lock.NewOwnerIdGenerator().GenOwnerId()
	}
	if len(owner) > 200 {
		writeErr(w, http.StatusBadRequest, "bad_request", "owner_id too long (max 200)")
		return
	}

	ctx := r.Context()
	readMode := strings.EqualFold(req.Mode, "read")
	if !req.Wait {
		// 立即返回，避免一个慢请求占住服务端资源。
		ctx, cancel := timeoutCtx(ctx, 5*time.Second)
		defer cancel()
		if readMode {
			err = lock.TryRLock(ctx, owner)
		} else {
			err = lock.TryLock(ctx, owner)
		}
	} else {
		if readMode {
			err = lock.RLock(ctx, owner)
		} else {
			err = lock.Lock(ctx, owner)
		}
	}
	if err != nil {
		s.writeLockError(w, err)
		return
	}

	status, err := lock.Inspect(ctx)
	if err != nil {
		// 已经拿到锁却读不到状态，释放掉避免泄漏。
		s.releaseQuietly(lock, owner, readMode)
		s.writeLockError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, acquireResult{
		LockID: id, OwnerID: owner,
		FencingToken: uint64(status.Information.Version),
		LeaseExpire:  status.Information.LeaseExpireTime,
		Remaining:    status.RemainingLease.String(),
	})
}

func (s *Server) renew(w http.ResponseWriter, r *http.Request, tenantID, id string) {
	var req struct {
		OwnerID string `json:"owner_id"`
		TTL     string `json:"ttl,omitempty"`
	}
	if !decodeBody(w, r, s.maxBody, &req) {
		return
	}
	if req.OwnerID == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "owner_id required")
		return
	}
	ttl, ok := parseTTL(req.TTL, s.defaultLease())
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid ttl")
		return
	}
	lock, err := s.lockFor(tenantID, id, ttl)
	if err != nil {
		s.writeLockError(w, err)
		return
	}
	ctx, cancel := timeoutCtx(r.Context(), 10*time.Second)
	defer cancel()
	if err := lock.Renew(ctx, req.OwnerID); err != nil {
		s.writeLockError(w, err)
		return
	}
	status, err := lock.Inspect(ctx)
	if err != nil {
		s.writeLockError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, acquireResult{
		LockID: id, OwnerID: req.OwnerID,
		FencingToken: uint64(status.Information.Version),
		LeaseExpire:  status.Information.LeaseExpireTime,
		Remaining:    status.RemainingLease.String(),
	})
}

func (s *Server) release(w http.ResponseWriter, r *http.Request, tenantID, id string) {
	owner := ""
	if r.Method == http.MethodDelete {
		owner = r.URL.Query().Get("owner_id")
	} else {
		var req struct {
			OwnerID string `json:"owner_id"`
		}
		if !decodeBody(w, r, s.maxBody, &req) {
			return
		}
		owner = req.OwnerID
	}
	if owner == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "owner_id required")
		return
	}
	lock, err := s.lockFor(tenantID, id, 0)
	if err != nil {
		s.writeLockError(w, err)
		return
	}
	ctx, cancel := timeoutCtx(r.Context(), 10*time.Second)
	defer cancel()

	// 读锁持有者必须用 RUnLock 释放，否则会报错。
	status, err := lock.Inspect(ctx)
	if err == nil && status.Information.ReadLock && status.Information.ReadDepth(owner) > 0 {
		err = lock.RUnLock(ctx, owner)
	} else {
		err = lock.UnLock(ctx, owner)
	}
	if err != nil {
		s.writeLockError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"lock_id": id, "owner_id": owner, "status": "released"})
}

func (s *Server) inspect(w http.ResponseWriter, r *http.Request, tenantID, id string) {
	lock, err := s.lockFor(tenantID, id, 0)
	if err != nil {
		s.writeLockError(w, err)
		return
	}
	ctx, cancel := timeoutCtx(r.Context(), 5*time.Second)
	defer cancel()
	status, err := lock.Inspect(ctx)
	if err != nil {
		s.writeLockError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lockView{
		LockID:         id,
		OwnerID:        status.Information.OwnerId,
		Held:           status.Held,
		ReadLock:       status.Information.ReadLock,
		LockCount:      status.Information.LockCount,
		Version:        uint64(status.Information.Version),
		LeaseExpire:    status.Information.LeaseExpireTime,
		RemainingLease: status.RemainingLease.String(),
		WriteIntentOwn: status.WriteIntentOwner,
		Waiters:        len(status.Waiters),
	})
}

func (s *Server) waiters(w http.ResponseWriter, r *http.Request, tenantID, id string) {
	lock, err := s.lockFor(tenantID, id, 0)
	if err != nil {
		s.writeLockError(w, err)
		return
	}
	ctx, cancel := timeoutCtx(r.Context(), 5*time.Second)
	defer cancel()
	status, err := lock.Inspect(ctx)
	if err != nil {
		s.writeLockError(w, err)
		return
	}
	// 只返回等待者身份与模式，不回传对方的锁内容。
	type waiter struct {
		OwnerID string `json:"owner_id"`
		Mode    string `json:"mode"`
		Since   string `json:"since"`
	}
	out := make([]waiter, 0, len(status.Waiters))
	for _, wtr := range status.Waiters {
		out = append(out, waiter{OwnerID: wtr.OwnerId, Mode: wtr.Mode})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleList 列出本租户的锁。前缀过滤是隔离的关键——不能列出全部。
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "use GET")
		return
	}
	tenantID := tenantFrom(r.Context())
	prefix := tenantPrefix(tenantID)
	ctx, cancel := timeoutCtx(r.Context(), 10*time.Second)
	defer cancel()

	items, err := storage_lock.ListLocks(ctx, s.store, prefix)
	if err != nil {
		s.writeLockError(w, err)
		return
	}
	views := []lockView{}
	for items.Next() {
		info := items.Value()
		if info == nil || !strings.HasPrefix(info.LockId, prefix) {
			continue
		}
		held := info.LockCount > 0 && s.nowFn().Before(info.LeaseExpireTime)
		v := lockView{
			LockID:         lastSegment(info.LockId, tenantID),
			OwnerID:        info.OwnerId,
			Held:           held,
			ReadLock:       info.ReadLock,
			LockCount:      info.LockCount,
			Version:        uint64(info.Version),
			LeaseExpire:    info.LeaseExpireTime,
			WriteIntentOwn: info.WriteIntentOwner,
			Waiters:        len(info.Waiters),
		}
		if held {
			v.RemainingLease = info.LeaseExpireTime.Sub(s.nowFn()).String()
		}
		if v.LockID == "" {
			// 前缀刚好匹配但没有剩余部分（例如别的租户 id 恰好以本前缀开头），跳过。
			continue
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, views)
}

// ---- 辅助 ----

func (s *Server) defaultLease() time.Duration {
	if s.options.LeaseExpireAfter > 0 {
		return s.options.LeaseExpireAfter
	}
	return 30 * time.Second
}

func (s *Server) releaseQuietly(lock *storage_lock.StorageLock, owner string, readMode bool) {
	ctx, cancel := timeoutCtx(contextBackground(), 5*time.Second)
	defer cancel()
	if readMode {
		_ = lock.RUnLock(ctx, owner)
		return
	}
	_ = lock.UnLock(ctx, owner)
}

func (s *Server) writeLockError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}
	switch {
	case errors.Is(err, storage_lock.ErrLockBusy), errors.Is(err, storage_lock.ErrVersionMiss):
		writeErr(w, http.StatusConflict, "lock_busy", "lock is held by another owner")
	case errors.Is(err, storage_lock.ErrLockNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "lock does not exist")
	case errors.Is(err, storage_lock.ErrLockNotBelongYou), errors.Is(err, storage_lock.ErrFairLockNotOwner), errors.Is(err, storage_lock.ErrLocalOrderedLockNotOwner):
		writeErr(w, http.StatusForbidden, "not_owner", "you do not hold this lock")
	case errors.Is(err, storage_lock.ErrNotYourTurn):
		writeErr(w, http.StatusConflict, "not_your_turn", "your turn in the fair queue has not arrived")
	case errors.Is(err, storage_lock.ErrLeaseExpired):
		writeErr(w, http.StatusConflict, "lease_expired", "the lease has expired; acquire again")
	case errors.Is(err, storage_lock.ErrOwnerIdEmpty):
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		s.logger.Error("lock operation failed", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal", "storage operation failed")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"success": false, "code": code, "message": msg})
}
