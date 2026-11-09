package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// healthResponse 与 OpenAPI HealthResponse 一致。
type healthResponse struct {
	Status   string    `json:"status"`
	Version  string    `json:"version"`
	Time     time.Time `json:"time"`
	Database string    `json:"database"`
}

// handleHealth 检查进程与主库；数据库不可达返回 503。
//
// 骨架阶段 d.Store 为 nil 时视为 degraded，便于无数据库启动调试。
func (d *Deps) handleHealth(c *gin.Context) {
	now := time.Now
	if d.Clock != nil {
		now = d.Clock
	}

	database := "ok"
	status := "ok"
	code := http.StatusOK

	if d.Store == nil {
		database = "error"
		status = "degraded"
		code = http.StatusServiceUnavailable
	} else {
		ctx, cancel := context.WithTimeout(c.Request.Context(), time.Second)
		defer cancel()
		if err := d.Store.Ping(ctx); err != nil {
			database = "error"
			status = "degraded"
			code = http.StatusServiceUnavailable
		}
	}

	c.JSON(code, healthResponse{
		Status:   status,
		Version:  d.Version,
		Time:     now().UTC(),
		Database: database,
	})
}
