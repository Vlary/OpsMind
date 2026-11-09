package httpapi

import "github.com/gin-gonic/gin"

// bindJSON 绑定 JSON 请求体；失败时写出 400 VALIDATION_FAILED。
func bindJSON(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		writeError(c, "VALIDATION_FAILED", "invalid request body")
		return false
	}
	return true
}
