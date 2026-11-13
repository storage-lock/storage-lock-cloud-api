package main

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// 简单的内存限流器
type rateLimiter struct {
	mu      sync.Mutex
	clients map[string]*clientInfo
}

type clientInfo struct {
	requests []time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		clients: make(map[string]*clientInfo),
	}
}

func (rl *rateLimiter) isAllowed(clientIP string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	windowStart := now.Add(-1 * time.Minute)

	info, exists := rl.clients[clientIP]
	if !exists {
		info = &clientInfo{
			requests: make([]time.Time, 0),
		}
		rl.clients[clientIP] = info
	}

	// 清理过期的请求记录
	validRequests := make([]time.Time, 0)
	for _, reqTime := range info.requests {
		if reqTime.After(windowStart) {
			validRequests = append(validRequests, reqTime)
		}
	}
	info.requests = validRequests

	// 每分钟最多 100 次请求
	if len(info.requests) >= 100 {
		return false
	}

	// 记录本次请求
	info.requests = append(info.requests, now)
	return true
}

// 限流中间件
func rateLimitMiddleware() gin.HandlerFunc {
	limiter := newRateLimiter()
	return func(c *gin.Context) {
		clientIP := c.ClientIP()
		if !limiter.isAllowed(clientIP) {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"success": false,
				"status":  "rate_limited",
				"message": "too many requests, please try again later",
			})
			c.Abort()
			return
		}
		c.Next()
	}
}