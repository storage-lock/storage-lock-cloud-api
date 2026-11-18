package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 多节点就近接入
//
// 一批节点（不同地区/不同运营商）各自跑同一份服务，但**共用同一个存储后端**——
// 否则同一个 lock id 在不同节点上是两把不同的锁，互斥直接失效。
//
// 因此：
//   - 节点之间不复制锁数据，靠共享存储（SQLite 只能单机，多节点必须换成
//     PostgreSQL/MySQL/Redis 等真正的网络存储）达成一致；
//   - 节点只负责接入，客户端挑延迟最低的那个；
//   - 一个统一的 /api/v1/endpoints 返回候选列表，客户端本地测延迟后选最快。
//
// 没有用 GeoDNS 或 Anycast，因为那需要域名控制权与网络侧配置；
// 客户端主动测速更简单，也允许用户手动指定节点。

// endpointInfo 是 /api/v1/endpoints 返回的单个节点。
type endpointInfo struct {
	URL         string `json:"url"`
	Region      string `json:"region,omitempty"`
	HealthURL   string `json:"health_url"`
	Description string `json:"description,omitempty"`
}

type endpointsResponse struct {
	Endpoints []endpointInfo `json:"endpoints"`
	// SharedStorage 提醒客户端：这些节点共用同一份存储，因此可以任意切换。
	SharedStorage bool   `json:"shared_storage"`
	Note          string `json:"note,omitempty"`
	GeneratedAt   string `json:"generated_at"`
}

// endpointRegistry 保存对外公布的可选节点。
//
// 节点列表可以来自配置文件（静态），也可以来自"每个节点启动时把自己注册到共享存储"。
// 这里实现静态清单，因为它不依赖任何外部服务，也更容易审计。
type endpointRegistry struct {
	mu        sync.RWMutex
	endpoints []endpointInfo
	healthFn  func(string) bool
	client    *http.Client
	logger    *slog.Logger
	refresh   time.Duration
}

func newEndpointRegistry(specs []string, self string, logger *slog.Logger) (*endpointRegistry, error) {
	r := &endpointRegistry{
		client: &http.Client{Timeout: 3 * time.Second},
		logger: logger,
		refresh: 2 * time.Minute,
	}
	for _, spec := range specs {
		// 格式：url|region|description，后两段可省略。
		parts := strings.Split(spec, "|")
		if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
			continue
		}
		url := strings.TrimRight(strings.TrimSpace(parts[0]), "/")
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return nil, &invalidSpecError{spec: spec}
		}
		info := endpointInfo{URL: url, HealthURL: url + "/health"}
		if len(parts) > 1 {
			info.Region = strings.TrimSpace(parts[1])
		}
		if len(parts) > 2 {
			info.Description = strings.TrimSpace(parts[2])
		}
		r.endpoints = append(r.endpoints, info)
	}
	// 把自己排在最前，单节点部署时客户端直接用当前地址即可。
	if self != "" {
		self = strings.TrimRight(self, "/")
		for i, e := range r.endpoints {
			if e.URL == self {
				r.endpoints[0], r.endpoints[i] = r.endpoints[i], r.endpoints[0]
				break
			}
		}
	}
	return r, nil
}

type invalidSpecError struct{ spec string }

func (e *invalidSpecError) Error() string {
	return "invalid endpoint spec (want url|region|description): " + e.spec
}

// list 返回当前清单。可选地对每个节点做一次健康探测并标记。
func (r *endpointRegistry) list() endpointsResponse {
	r.mu.RLock()
	eps := append([]endpointInfo(nil), r.endpoints...)
	r.mu.RUnlock()

	// 保持注册顺序：把自己排最前，其余按配置顺序。
	// 不按 region 字符串排序——那样会让空 region 排到最前，且打乱运维预期的顺序。

	return endpointsResponse{
		Endpoints:     eps,
		SharedStorage: true,
		Note: "这些节点共用同一份锁存储；客户端应选延迟最低的一个。" +
			"若某个节点不可用，换另一个即可，锁状态不会丢。",
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

// pickFastest 在给定节点里选延迟最低的（客户端侧也能自己实现同样的逻辑）。
// 这里保留在服务端只是为了提供一个可选的 /api/v1/endpoints/best 能力，
// 但生产上更推荐客户端本地测速——服务端看到的延迟不是客户端看到的延迟。
func (r *endpointRegistry) pickFastest(candidates []endpointInfo) endpointInfo {
	if len(candidates) == 0 {
		return endpointInfo{}
	}
	type result struct {
		ep  endpointInfo
		d   time.Duration
		ok  bool
	}
	results := make(chan result, len(candidates))
	var wg sync.WaitGroup
	for _, ep := range candidates {
		wg.Add(1)
		go func(ep endpointInfo) {
			defer wg.Done()
			start := time.Now()
			req, err := http.NewRequest(http.MethodGet, ep.HealthURL, nil)
			if err != nil {
				results <- result{ep: ep}
				return
			}
			resp, err := r.client.Do(req)
			if err != nil {
				results <- result{ep: ep}
				return
			}
			resp.Body.Close()
			results <- result{ep: ep, d: time.Since(start), ok: resp.StatusCode == http.StatusOK}
		}(ep)
	}
	wg.Wait()
	close(results)

	var best endpointInfo
	var bestD time.Duration
	found := false
	for res := range results {
		if !res.ok {
			continue
		}
		if !found || res.d < bestD {
			best, bestD, found = res.ep, res.d, true
		}
	}
	if !found {
		return candidates[0]
	}
	return best
}

// encode 便于测试断言。
func (r endpointsResponse) encode() ([]byte, error) { return json.Marshal(r) }
