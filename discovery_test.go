package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEndpointRegistryParsesSpecs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg, err := newEndpointRegistry([]string{
		"https://cn.example.com|cn-east|上海",
		"https://sg.example.com|ap-sg",
		"https://us.example.com",
	}, "https://cn.example.com", logger)
	require.NoError(t, err)

	list := reg.list()
	require.True(t, list.SharedStorage, "必须明确告诉客户端这些节点共享存储")
	require.Len(t, list.Endpoints, 3)
	require.Equal(t, "cn-east", list.Endpoints[0].Region)
	require.Equal(t, "上海", list.Endpoints[0].Description)
	require.Equal(t, "https://sg.example.com", list.Endpoints[1].URL)
	require.Equal(t, "https://us.example.com/health", list.Endpoints[2].HealthURL)
}

func TestEndpointRegistryServesSelf(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// 单节点部署：只给 public-url 也能被公布出来。
	reg, err := newEndpointRegistry([]string{"http://10.0.0.5:8080"}, "http://10.0.0.5:8080", logger)
	require.NoError(t, err)
	require.Len(t, reg.list().Endpoints, 1)
}

func TestInvalidEndpointSpecRejected(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := newEndpointRegistry([]string{"not-a-url"}, "", logger)
	require.Error(t, err, "非 http(s) 地址必须在启动时就被拒绝，而不是运行时报错")
}

// TestEndpointsHandler 验证 HTTP 层输出形状与鉴权要求。
func TestEndpointsHandler(t *testing.T) {
	srv := newTestServer(t)
	srv.endpoints = mustRegistry(t)
	// endpoints 不需要租户凭据：选节点发生在拿到 secret 之前。
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/endpoints", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var body endpointsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.True(t, body.SharedStorage)
	require.GreaterOrEqual(t, len(body.Endpoints), 2)
}

func TestEndpointsNotConfigured(t *testing.T) {
	srv := newTestServer(t)
	srv.endpoints = nil
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/endpoints", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestPickFastestPrefersHealthyAndClose 验证选点会跳过不可用节点。
func TestPickFastestPrefersHealthyAndClose(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer fast.Close()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(80 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer slow.Close()

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer dead.Close()

	reg, err := newEndpointRegistry([]string{
		slow.URL + "|slow",
		dead.URL + "|dead",
		fast.URL + "|fast",
	}, "", logger)
	require.NoError(t, err)

	best := reg.pickFastest(reg.list().Endpoints)
	require.Equal(t, fast.URL, best.URL, "应选健康且延迟最低的节点")
}

// TestPickFastestFallsBackWhenAllDown 验证全挂时仍有返回，且不 panic。
func TestPickFastestFallsBackWhenAllDown(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer dead.Close()

	reg, err := newEndpointRegistry([]string{dead.URL}, "", logger)
	require.NoError(t, err)
	require.Equal(t, dead.URL, reg.pickFastest(reg.list().Endpoints).URL)
}

func mustRegistry(t *testing.T) *endpointRegistry {
	t.Helper()
	reg, err := newEndpointRegistry([]string{
		"https://a.example.com|cn", "https://b.example.com|sg",
	}, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	return reg
}
