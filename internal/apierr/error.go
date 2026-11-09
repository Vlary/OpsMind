package apierr

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Error 是统一错误体中的 error 对象。
type Error struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Details   map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// New 构造一个 Error；message 使用英文短句。
func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Wrap 在错误链中查找 *Error，否则返回 INTERNAL。
func Wrap(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return New(Internal, "internal server error")
}

// Status 返回错误码对应的 HTTP 状态码。
func Status(code string) int {
	switch code {
	case AuthRequired, AuthInvalid, ApprovalDenied:
		if code == ApprovalDenied {
			return http.StatusForbidden
		}
		return http.StatusUnauthorized
	case AuthRateLimited, TooManyStreams, TermSessionLimit, RdpSessionLimit, DiagnosisBusy:
		return http.StatusTooManyRequests
	case OriginForbidden, CommandForbidden:
		return http.StatusForbidden
	case ValidationFailed, CommandTooLarge, CommandInvalid, ToolNotFound, ToolArgsInvalid,
		TermUnsupported, TermWSProtocol, RdpUnsupported, RdpNoCredentials,
		NotificationConfigInvalid, BatchTooManyTargets, CronInvalid:
		return http.StatusBadRequest
	case NotFound, ConversationNotFound, ProviderNotFound, ServerNotFound, DatabaseNotFound,
		ApprovalNotFound, TermSessionNotFound, RecordingNotFound, RdpSessionNotFound,
		AlertRuleNotFound, AlertEventNotFound, NotificationChannelNotFound, BatchJobNotFound,
		DiagnosisSessionNotFound, InspectionJobNotFound, InspectionReportNotFound:
		return http.StatusNotFound
	case Conflict, ConversationModelLocked, StreamActive, SSHHostKeyChanged, ApprovalNotPending,
		TermSessionExists, RdpSessionExists:
		return http.StatusConflict
	case NoProviderConfigured, SQLNotReadOnly:
		return http.StatusUnprocessableEntity
	case SSHTimeout, SQLTimeout, LLMTimeout, ApprovalTimeout:
		return http.StatusGatewayTimeout
	case ServerTestFailed, DatabaseTestFailed, ProviderTestFailed, ProviderModelsFailed,
		SSHExecFailed, SQLQueryFailed, AgentMaxIterations, LLMUpstreamError, LLMOutputTruncated,
		LLMStreamError, TermConnectFailed, RecordingUnavailable, RdpConnectFailed,
		NotificationTestFailed:
		return http.StatusBadGateway
	case ServiceUnavailable, GuacdUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Write 把错误写入 HTTP 响应；request_id 从 gin context 读取。
func Write(c *gin.Context, err *Error) {
	if err.RequestID == "" {
		err.RequestID = c.GetString("request_id")
	}
	if err.Message == "" {
		err.Message = "internal server error"
	}
	status := Status(err.Code)
	c.AbortWithStatusJSON(status, gin.H{"error": err})
}
