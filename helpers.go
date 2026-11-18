package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// 请求体大小上限之外还要防止空 body。
var errEmptyBody = errors.New("empty request body")

// decodeBody 解 JSON 并限制体积；失败时已写好响应，返回 false。
func decodeBody(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, "bad_request", errEmptyBody.Error())
			return false
		}
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid json: "+err.Error())
		return false
	}
	// 只允许单个 JSON 值，拒绝尾随内容。
	if dec.More() {
		writeErr(w, http.StatusBadRequest, "bad_request", "unexpected trailing data")
		return false
	}
	return true
}

// parseTTL 解析 "10s"/"5m" 形式的租期；空字符串回退到默认值。
// 加了下限与上限，避免 0 或过大的值把存储打穿。
func parseTTL(raw string, fallback time.Duration) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, true
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false
	}
	const minTTL = 4 * time.Second
	const maxTTL = 24 * time.Hour
	if d < minTTL || d > maxTTL {
		return 0, false
	}
	return d, true
}

// timeoutCtx 给单次存储操作加超时，避免慢后端把连接池占满。
func timeoutCtx(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}

func contextBackground() context.Context { return context.Background() }

// splitAction 把 "{id}/{action}" 拆开；没有 "/" 时 action 为空。
func splitAction(path string) (id, action string) {
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i], path[i+1:]
	}
	return path, ""
}

// 把 tenant id 放进 context，避免在签名里层层传递。
type tenantCtxKey struct{}

func withTenantID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, id)
}

func tenantFrom(ctx context.Context) string {
	if v, ok := ctx.Value(tenantCtxKey{}).(string); ok {
		return v
	}
	return ""
}
