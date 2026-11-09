package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"aiops/internal/store"
)

// TestWriteServerStoreError 第 4 轮打磨：servers 创建/更新的唯一约束只有
// name（migrations 00001 servers_name_key），409 必须返回具体 message
// （对齐 providers 的 "provider name already exists"），前端据此告知用户
// 冲突在名称字段；其余错误码维持通用映射不变。
// p3 sprint 1：message 场景化为中文「服务器名称已存在」（走查：通用 409
// 文案 + 英文原因未像数据库侧一样具体化）。
func TestWriteServerStoreError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantMsg    string
	}{
		{
			name:       "conflict reports duplicate name",
			err:        store.ErrConflict,
			wantStatus: http.StatusConflict,
			wantCode:   "CONFLICT",
			wantMsg:    "服务器名称已存在",
		},
		{
			name:       "not found keeps generic mapping",
			err:        store.ErrNotFound,
			wantStatus: http.StatusNotFound,
			wantCode:   "SERVER_NOT_FOUND",
			wantMsg:    "not found",
		},
		{
			name:       "invalid keeps generic mapping",
			err:        store.ErrInvalid,
			wantStatus: http.StatusBadRequest,
			wantCode:   "VALIDATION_FAILED",
			wantMsg:    "invalid argument",
		},
		{
			name:       "wrapped conflict still matches",
			err:        errors.Join(store.ErrConflict, errors.New("23505")),
			wantStatus: http.StatusConflict,
			wantCode:   "CONFLICT",
			wantMsg:    "服务器名称已存在",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &Deps{}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Set("request_id", "test-req")
			d.writeServerStoreError(c, tc.err)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v (%s)", err, rec.Body.String())
			}
			if body.Error.Code != tc.wantCode {
				t.Fatalf("code = %q, want %q", body.Error.Code, tc.wantCode)
			}
			if body.Error.Message != tc.wantMsg {
				t.Fatalf("message = %q, want %q", body.Error.Message, tc.wantMsg)
			}
		})
	}
}
