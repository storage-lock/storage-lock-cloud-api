package main

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	memory_storage "github.com/storage-lock/go-memory-storage"
	go_storage "github.com/storage-lock/go-storage"
	storage_lock "github.com/storage-lock/go-storage-lock"
)

// 全局存储实例
var (
	lockStorage   go_storage.Storage
	storageOnce   sync.Once
)

// 初始化存储
func initStorage() error {
	var err error
	storageOnce.Do(func() {
		// 使用内存存储作为后端（开发测试用）
		// TODO: 替换为真实的 Firebase 项目
		lockStorage = memory_storage.NewMemoryStorage()
	})
	return err
}

// 请求结构体
type LockRequest struct {
	LockID  string `json:"lock_id" binding:"required"`
	TTL     int    `json:"ttl" binding:"required"`
	OwnerID string `json:"owner_id"`
}

type RenewRequest struct {
	LockID  string `json:"lock_id" binding:"required"`
	OwnerID string `json:"owner_id" binding:"required"`
	TTL     int    `json:"ttl" binding:"required"`
}

type UnlockRequest struct {
	LockID  string `json:"lock_id" binding:"required"`
	OwnerID string `json:"owner_id" binding:"required"`
}

// 响应结构体
type LockResponse struct {
	Success      bool   `json:"success"`
	Status       string `json:"status"`
	LockID       string `json:"lock_id"`
	OwnerID      string `json:"owner_id"`
	FencingToken uint64 `json:"fencing_token"`
	RemainingTTL int    `json:"remaining_ttl,omitempty"`
	Message      string `json:"message,omitempty"`
}

// 获取锁
func handleLock(c *gin.Context) {
	var req LockRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, LockResponse{
			Success: false,
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	// 从请求中获取租户 ID（基于 IP）
	// tenantID := c.ClientIP()

	// 创建锁
	lock, err := storage_lock.NewStorageLockWithOptions(lockStorage, &storage_lock.StorageLockOptions{
		LockId:           req.LockID,
		LeaseExpireAfter: time.Duration(req.TTL) * time.Second,
		DisableWatchDog:  true,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, LockResponse{
			Success: false,
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	// 尝试获取锁
	ctx := c.Request.Context()
	err = lock.TryLock(ctx, req.OwnerID)
	if err != nil {
		c.JSON(http.StatusOK, LockResponse{
			Success: false,
			Status:  "busy",
			LockID:  req.LockID,
			OwnerID: req.OwnerID,
			Message: "lock is busy",
		})
		return
	}

	// 获取 fencing token
	token, err := lock.GetFencingToken(ctx, req.OwnerID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, LockResponse{
			Success: false,
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, LockResponse{
		Success:      true,
		Status:       "held",
		LockID:       req.LockID,
		OwnerID:      req.OwnerID,
		FencingToken: uint64(token),
		RemainingTTL: req.TTL,
	})
}

// 续租
func handleRenew(c *gin.Context) {
	var req RenewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, LockResponse{
			Success: false,
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	// 创建锁
	lock, err := storage_lock.NewStorageLockWithOptions(lockStorage, &storage_lock.StorageLockOptions{
		LockId:           req.LockID,
		LeaseExpireAfter: time.Duration(req.TTL) * time.Second,
		DisableWatchDog:  true,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, LockResponse{
			Success: false,
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	// 续租
	ctx := c.Request.Context()
	err = lock.Renew(ctx, req.OwnerID)
	if err != nil {
		c.JSON(http.StatusOK, LockResponse{
			Success: false,
			Status:  "lost",
			LockID:  req.LockID,
			OwnerID: req.OwnerID,
			Message: err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, LockResponse{
		Success:      true,
		Status:       "renewed",
		LockID:       req.LockID,
		OwnerID:      req.OwnerID,
		RemainingTTL: req.TTL,
	})
}

// 释放锁
func handleUnlock(c *gin.Context) {
	var req UnlockRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, LockResponse{
			Success: false,
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	// 创建锁
	lock, err := storage_lock.NewStorageLockWithOptions(lockStorage, &storage_lock.StorageLockOptions{
		LockId:           req.LockID,
		LeaseExpireAfter: 30 * time.Second,
		DisableWatchDog:  true,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, LockResponse{
			Success: false,
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	// 释放锁
	ctx := c.Request.Context()
	err = lock.UnLock(ctx, req.OwnerID)
	if err != nil {
		c.JSON(http.StatusOK, LockResponse{
			Success: false,
			Status:  "unlock_failed",
			LockID:  req.LockID,
			OwnerID: req.OwnerID,
			Message: err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, LockResponse{
		Success: true,
		Status:  "released",
		LockID:  req.LockID,
		OwnerID: req.OwnerID,
	})
}

// 查询锁状态
func handleStatus(c *gin.Context) {
	lockID := c.Param("lock_id")
	if lockID == "" {
		c.JSON(http.StatusBadRequest, LockResponse{
			Success: false,
			Status:  "error",
			Message: "lock_id is required",
		})
		return
	}

	// 创建锁
	lock, err := storage_lock.NewStorageLockWithOptions(lockStorage, &storage_lock.StorageLockOptions{
		LockId:           lockID,
		LeaseExpireAfter: 30 * time.Second,
		DisableWatchDog:  true,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, LockResponse{
			Success: false,
			Status:  "error",
			Message: err.Error(),
		})
		return
	}

	// 查询锁状态
	ctx := c.Request.Context()
	owner, err := lock.GetOwner(ctx)
	if err != nil {
		c.JSON(http.StatusOK, LockResponse{
			Success: false,
			Status:  "free",
			LockID:  lockID,
		})
		return
	}

	// 获取 fencing token
	token, err := lock.GetFencingToken(ctx, owner)
	if err != nil {
		c.JSON(http.StatusOK, LockResponse{
			Success: false,
			Status:  "free",
			LockID:  lockID,
		})
		return
	}

	c.JSON(http.StatusOK, LockResponse{
		Success:      true,
		Status:       "held",
		LockID:       lockID,
		OwnerID:      owner,
		FencingToken: uint64(token),
	})
}

func main() {
	// 初始化存储
	if err := initStorage(); err != nil {
		log.Fatalf("Failed to initialize storage: %v", err)
	}

	// 创建 Gin 路由
	r := gin.Default()

	// 限流中间件
	r.Use(rateLimitMiddleware())

	// API 路由
	api := r.Group("/api/v1")
	{
		api.POST("/lock", handleLock)
		api.POST("/renew", handleRenew)
		api.POST("/unlock", handleUnlock)
		api.GET("/status/:lock_id", handleStatus)
	}

	// 健康检查
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 启动服务
	log.Println("Starting storage-lock-cloud-api on :8080")
	if err := r.Run(":8080"); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}