package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/risk"
	"aiops/internal/store"
)

type riskPreviewRequest struct {
	Command     string `json:"command"`
	OS          string `json:"os"`
	NetdevBrand string `json:"netdev_brand"`
	ServerID    string `json:"server_id"`
}

func (d *Deps) handlePreviewRisk(c *gin.Context) {
	var req riskPreviewRequest
	if !d.decodeJSON(c, &req) {
		return
	}
	if len(req.Command) == 0 {
		writeAPIError(c, http.StatusBadRequest, apierr.CommandInvalid, "command is required")
		return
	}
	if len(req.Command) > risk.MaxCommandBytes {
		writeAPIError(c, http.StatusBadRequest, apierr.CommandTooLarge, "command exceeds 16384 bytes")
		return
	}

	// 目标 os 决定分册（契约 7.3/7.3B/7.3C）；preview 无服务器上下文时
	// 缺省 linux；os=network 时 netdev_brand 生效（缺省 generic）。
	osArg := risk.OSLinux
	brandArg := risk.NetBrandGeneric
	switch risk.OS(req.OS) {
	case "":
	case risk.OSLinux, risk.OSWindows:
		osArg = risk.OS(req.OS)
	case risk.OSNetwork:
		osArg = risk.OSNetwork
		if req.NetdevBrand != "" {
			b, ok := risk.NetBrand(req.NetdevBrand), false
			switch risk.NetBrand(req.NetdevBrand) {
			case risk.NetBrandCisco, risk.NetBrandHuawei, risk.NetBrandH3C,
				risk.NetBrandJunos, risk.NetBrandGeneric:
				ok = true
			}
			if !ok {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
					"netdev_brand must be cisco, huawei, h3c, junos or generic")
				return
			}
			brandArg = b
		}
	default:
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
			"os must be linux, windows or network")
		return
	}

	// server_id 上下文覆盖显式 os/netdev_brand（契约 7.3C 分册路由：
	// network_device 资产按实际 brand 覆盖；server 类资产按实际 os 覆盖）。
	if req.ServerID != "" && d.Store != nil {
		if srv, err := d.Store.Servers().Get(c.Request.Context(), req.ServerID); err == nil {
			if srv.DeviceKind == store.DeviceKindNetworkDevice {
				osArg = risk.OSNetwork
				brandArg = riskNetBrand(srv.NetdevBrand)
			} else {
				osArg = risk.OS(srv.OS)
				if osArg != risk.OSWindows {
					osArg = risk.OSLinux
				}
			}
		}
	}

	var decision risk.Decision
	if osArg == risk.OSNetwork {
		decision = risk.ClassifyNet(req.Command, brandArg)
	} else {
		decision = risk.Classify(req.Command, osArg)
	}

	d.auditEvent(c, "risk.preview", "command", req.ServerID, map[string]any{
		"risk_level":   decision.Level,
		"rule_id":      decision.RuleID,
		"os":           string(osArg),
		"netdev_brand": string(brandArg),
	})

	c.JSON(http.StatusOK, gin.H{
		"risk_level":         decision.Level,
		"rule_id":            decision.RuleID,
		"reason":             decision.Reason,
		"normalized_command": decision.Normalized,
		"reasons":            decision.Reasons,
	})
}

// riskNetBrand 把 store.NetdevBrand 映射为 risk.NetBrand；未知值失败关闭
// 到 generic。
func riskNetBrand(b store.NetdevBrand) risk.NetBrand {
	switch b {
	case store.NetdevBrandCisco:
		return risk.NetBrandCisco
	case store.NetdevBrandHuawei:
		return risk.NetBrandHuawei
	case store.NetdevBrandH3C:
		return risk.NetBrandH3C
	case store.NetdevBrandJunos:
		return risk.NetBrandJunos
	default:
		return risk.NetBrandGeneric
	}
}
