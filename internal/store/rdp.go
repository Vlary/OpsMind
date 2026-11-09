// W4：RDP 网关会话（rdp_sessions）的领域类型、参数与仓储接口。
//
// 契约 3.2/3.3/3.4 定稿内容。并行开发期独立成文件以避免与 BE1/W3 同时
// 编辑 models.go/params.go/interfaces.go 冲突；集成时可归位。
package store

import (
	"context"
	"time"
)

// RdpSession 是堡垒机式 RDP 网关会话（guacd 边车，凭据仅存在于 Go 侧握手）。
type RdpSession struct {
	ID             string                   `json:"id"`
	ServerID       string                   `json:"server_id"`
	ServerName     string                   `json:"server_name"`
	Width          int                      `json:"width"`
	Height         int                      `json:"height"`
	DPI            int                      `json:"dpi"`
	Status         InteractiveSessionStatus `json:"status"`
	EndedReason    string                   `json:"ended_reason"`
	RecordingFile  string                   `json:"-"` // <RdpRecordingsDir>/<id>.guac；空=未落盘
	HasRecording   bool                     `json:"has_recording"`
	BytesWritten   int64                    `json:"bytes_written"`
	StartedAt      time.Time                `json:"started_at"`
	LastActivityAt time.Time                `json:"last_activity_at"`
	EndedAt        *time.Time               `json:"ended_at"`
	CreatedAt      time.Time                `json:"created_at"`
	UpdatedAt      time.Time                `json:"updated_at"`
}

// CreateRdpSessionParams 创建 RDP 会话。
type CreateRdpSessionParams struct {
	ServerID  string
	UserID    string
	Width     int // 640..3840，缺省 1280
	Height    int // 480..2160，缺省 800
	DPI       int // 72..120，缺省 96
	StartedAt time.Time
}

// RdpSessionStore 是 RDP 会话仓储；方法集与 TerminalSessionStore 同构
// （契约 3.4）。写方法不得改变已 closed 会话（幂等返回现状或 ErrConflict）。
type RdpSessionStore interface {
	Create(ctx context.Context, p CreateRdpSessionParams) (RdpSession, error)
	Get(ctx context.Context, id string) (RdpSession, error)
	List(ctx context.Context, f InteractiveSessionFilter) ([]RdpSession, error)
	TouchActivity(ctx context.Context, id string, at time.Time, bytesWritten int64) error
	Close(ctx context.Context, p CloseInteractiveSessionParams) (RdpSession, error)
	CountActive(ctx context.Context) (int64, error)
	CountActiveByServer(ctx context.Context, serverID string) (int64, error)
}
