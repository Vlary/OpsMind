// W3：终端会话（terminal_sessions）的领域类型、参数与仓储接口。
//
// 契约 3.1/3.2/3.3/3.4 定稿内容。并行开发期独立成文件以避免与 BE1/W1
// 同时编辑 models.go/params.go/interfaces.go 冲突；集成时可归位。
package store

import (
	"context"
	"time"
)

// InteractiveSessionStatus 是交互会话状态（终端/RDP 共用）。
type InteractiveSessionStatus string

const (
	SessionActive InteractiveSessionStatus = "active"
	SessionClosed InteractiveSessionStatus = "closed"
)

// TerminalSession 是堡垒机式终端会话（SSH PTY + asciinema v2 录制）。
type TerminalSession struct {
	ID             string                   `json:"id"`
	ServerID       string                   `json:"server_id"`
	ServerName     string                   `json:"server_name"` // JOIN servers.name；删除后空串
	ServerOS       ServerOS                 `json:"os"`
	Username       string                   `json:"username"` // 发起者；v1 恒 admin
	Status         InteractiveSessionStatus `json:"status"`
	Cols           int                      `json:"cols"`
	Rows           int                      `json:"rows"`
	EndedReason    string                   `json:"ended_reason"` // ''|client_closed|idle_timeout|max_duration|server_shutdown|error
	RecordingFile  string                   `json:"-"`            // DATA_DIR/recordings/<id>.cast；空=未落盘
	HasRecording   bool                     `json:"has_recording"`
	BytesWritten   int64                    `json:"bytes_written"`
	StartedAt      time.Time                `json:"started_at"`
	LastActivityAt time.Time                `json:"last_activity_at"`
	EndedAt        *time.Time               `json:"ended_at"`
	CreatedAt      time.Time                `json:"created_at"`
	UpdatedAt      time.Time                `json:"updated_at"`
}

// CreateTerminalSessionParams 创建终端会话。
type CreateTerminalSessionParams struct {
	ServerID  string
	UserID    string
	Cols      int // 10..500，缺省 120
	Rows      int // 4..200，缺省 40
	StartedAt time.Time
}

// CloseInteractiveSessionParams 关闭交互会话（终端/RDP 共用）。
type CloseInteractiveSessionParams struct {
	ID           string
	Reason       string // client_closed|idle_timeout|max_duration|server_shutdown|error
	EndedAt      time.Time
	BytesWritten int64
}

// InteractiveSessionFilter 交互会话列表过滤。
type InteractiveSessionFilter struct {
	Status   InteractiveSessionStatus // ""=不过滤
	ServerID string                   // ""=不过滤
	Limit    int                      // 1..200
}

// TerminalSessionStore 是终端会话仓储。写方法不得改变已 closed 会话
// （幂等返回现状或 ErrConflict）。
type TerminalSessionStore interface {
	Create(ctx context.Context, p CreateTerminalSessionParams) (TerminalSession, error)
	Get(ctx context.Context, id string) (TerminalSession, error)
	List(ctx context.Context, f InteractiveSessionFilter) ([]TerminalSession, error)
	TouchActivity(ctx context.Context, id string, at time.Time, bytesWritten int64) error
	Close(ctx context.Context, p CloseInteractiveSessionParams) (TerminalSession, error)
	CountActive(ctx context.Context) (int64, error)
	CountActiveByServer(ctx context.Context, serverID string) (int64, error)
}
