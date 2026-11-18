package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	remote_client "github.com/storage-lock/go-remote-storage/client"
	"github.com/storage-lock/go-storage"
	memory_storage "github.com/storage-lock/go-memory-storage"
	sqlite3_storage "github.com/storage-lock/go-sqlite3-storage"
	storage_lock "github.com/storage-lock/go-storage-lock"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", envOr("STORAGE_LOCK_ADDR", ":8080"), "监听地址")
	dataPath := flag.String("data", envOr("STORAGE_LOCK_DATA", "locks.db"), "SQLite 数据文件路径；empty 表示仅内存（不持久化）；配了 --remote 时忽略")
	remoteURL := flag.String("remote", envOr("STORAGE_LOCK_REMOTE", ""), "远程存储服务地址（go-remote-storage）；配置后优先于本地 SQLite")
	remoteToken := flag.String("remote-token", envOr("STORAGE_LOCK_REMOTE_TOKEN", ""), "远程存储服务的访问令牌")
	tlsCert := flag.String("tls-cert", os.Getenv("STORAGE_LOCK_TLS_CERT"), "TLS 证书路径；为空则以明文 HTTP 启动")
	tlsKey := flag.String("tls-key", os.Getenv("STORAGE_LOCK_TLS_KEY"), "TLS 私钥路径")
	tenantKeySeed := flag.String("tenant-key", os.Getenv("STORAGE_LOCK_TENANT_KEY"), "租户 HMAC 主密钥（base64，至少 32 字节）。为空则每次启动随机生成，重启后旧租户全部失效")
	trustProxy := flag.String("trusted-proxies", envOr("STORAGE_LOCK_TRUSTED_PROXIES", ""), "受信任反代 CIDR，逗号分隔；为空则只认直连 IP")
	endpoints := flag.String("endpoints", envOr("STORAGE_LOCK_ENDPOINTS", ""), "对外公布的节点清单，逗号分隔，每项 url|region|description；为空则只公布自己")
	publicURL := flag.String("public-url", envOr("STORAGE_LOCK_PUBLIC_URL", ""), "本节点的对外地址（用于 /api/v1/endpoints）")
	perIP := flag.Float64("per-ip-per-min", 120, "每 IP 每分钟请求上限")
	perTenant := flag.Float64("per-tenant-per-min", 600, "每租户每分钟请求上限")
	defaultTTL := flag.Duration("default-ttl", 30*time.Second, "默认租约时长")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	store, closer, err := openStorage(context.Background(), *dataPath, *remoteURL, *remoteToken)
	if err != nil {
		return err
	}
	defer closer()

	key, err := newTenantKey(*tenantKeySeed)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*tenantKeySeed) == "" {
		logger.Warn("未设置 STORAGE_LOCK_TENANT_KEY，已随机生成临时密钥；重启后所有既有租户都将失效。" +
			"生产部署请固定该值（base64，至少 32 字节）")
	}

	trusted, err := parseCIDRs(*trustProxy)
	if err != nil {
		return err
	}

	limits := newLimiters(*perIP, *perTenant)
	stopSweeper := make(chan struct{})
	defer close(stopSweeper)
	go limits.perIP.runSweeper(stopSweeper, 5*time.Minute)
	go limits.perUser.runSweeper(stopSweeper, 5*time.Minute)

	options := storage_lock.StorageLockOptions{
		LeaseExpireAfter: *defaultTTL,
		DisableWatchDog:  true,
	}
	if options.LeaseExpireAfter <= 3*time.Second {
		return fmt.Errorf("--default-ttl 必须大于 3s（核心库要求租期大于刷新间隔的安全余量）")
	}

	var specs []string
	if strings.TrimSpace(*endpoints) != "" {
		specs = strings.Split(*endpoints, ",")
	}
	if strings.TrimSpace(*publicURL) != "" && len(specs) == 0 {
		specs = []string{*publicURL}
	}
	var reg *endpointRegistry
	if len(specs) > 0 {
		reg, err = newEndpointRegistry(specs, *publicURL, logger)
		if err != nil {
			return err
		}
	}

	srv := newServer(store, key, limits, options, trusted, logger)
	srv.endpoints = reg
	srv.healthFn = func() error {
		_, err := store.GetTime(context.Background())
		return err
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// 长轮询 / wait=true 的请求可能超过 WriteTimeout，这里设成上限的 2 倍。
		WriteTimeout:   60 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 16 << 10,
		ErrorLog:       slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", *addr, "tls", *tlsCert != "", "data", *dataPath)
		var err error
		if *tlsCert != "" && *tlsKey != "" {
			err = httpServer.ListenAndServeTLS(*tlsCert, *tlsKey)
		} else {
			err = httpServer.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}

// openStorage 打开持久化后端。
//
// 优先级：--remote（远程存储服务）> --data（本地 SQLite）> 内存。
// 远程模式让同一台 VPS 上的多个进程、以及集群里其他机器共用同一份锁存储。
func openStorage(ctx context.Context, path, remoteURL, remoteToken string) (storage.Storage, func(), error) {
	if strings.TrimSpace(remoteURL) != "" {
		rs, err := remote_client.New(ctx, remote_client.Config{
			BaseURL: remoteURL,
			Secret:  remoteToken,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("connect remote storage %q: %w", remoteURL, err)
		}
		return rs, func() { _ = rs.Close(context.Background()) }, nil
	}
	if strings.TrimSpace(path) == "" {
		store := memory_storage.NewMemoryStorage()
		return store, func() {}, nil
	}
	opts := sqlite3_storage.NewSqlite3StorageOptions()
	opts.TableName = "cloud_locks"
	cm, err := sqlite3_storage.NewSqlite3ConnectionManager(sqlitePath(path))
	if err != nil {
		return nil, nil, err
	}
	opts.ConnectionManager = cm
	store, err := sqlite3_storage.NewSqlite3Storage(ctx, opts)
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	return store, func() { _ = store.Close(context.Background()) }, nil
}

// sqlitePath 在 DSN 上追加并发相关的参数。
//
//   - _journal_mode=WAL：允许读写并发，是 SQLite 多进程可用的前提；
//   - _busy_timeout=5000：写锁竞争时等待而不是立刻报 "database is locked"；
//   - _synchronous=FULL：崩溃后已提交的事务不丢，锁记录比性能重要；
//   - _txlock=immediate：BEGIN 立即取写锁，避免"读升级写"死锁。
//
// 参数分隔符必须是 "&"：写成 "(" 会让 mattn/go-sqlite3 静默忽略整串参数，
// 后果是高并发下随机报 "database is locked"。这个坑踩过一次。
func sqlitePath(path string) string {
	// _wal_autocheckpoint：每 1000 页回收 WAL，否则会随写入无限膨胀
	// （实测 20000 次写入后 WAL 达 166MB 而主库仅 4.7MB）。
	const params = "_journal_mode=WAL&_busy_timeout=5000&_synchronous=FULL&_txlock=immediate" +
		"&_wal_autocheckpoint=1000&_auto_vacuum=incremental"
	if strings.Contains(path, "?") {
		// 已有查询串：用 & 追加，而不是原样返回（否则并发参数全部丢失）。
		return path + "&" + params
	}
	return path + "?" + params
}

func parseCIDRs(raw string) ([]*net.IPNet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// 默认信任本机回环（nginx 与网关同机转发时必须命中这一条）。
		raw = "127.0.0.0/8,::1/128"
	}
	var out []*net.IPNet
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !strings.Contains(item, "/") {
			ip := net.ParseIP(item)
			if ip == nil {
				return nil, fmt.Errorf("invalid trusted proxy %q", item)
			}
			if ip.To4() != nil {
				item += "/32"
			} else {
				item += "/128"
			}
		}
		_, cidr, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q: %w", item, err)
		}
		out = append(out, cidr)
	}
	return out, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
