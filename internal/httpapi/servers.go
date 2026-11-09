package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/audit"
	"aiops/internal/containerx"
	"aiops/internal/netdev"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"aiops/internal/termx"
	"aiops/internal/winrmx"
)

// maxJSONBodyBytes 是除 /api/chat 外所有 JSON 请求体的上限（CONTRACT.md 2.5）。
const maxJSONBodyBytes = 1 << 20

// ---------------------------------------------------------------------------
// 路由注册与共享辅助
// ---------------------------------------------------------------------------

// RegisterBE2Routes 注册 BE2 负责的端点（资产、审批、审计查询、风险预览）。
//
// owned by BE2：router.go 属于 SCAFFOLD，由集成阶段在受保护路由组
// （api := r.Group("/api", d.requireAuth())）上调用本函数。
func (d *Deps) RegisterBE2Routes(api *gin.RouterGroup) {
	api.GET("/servers", d.handleListServers)
	api.POST("/servers", d.handleCreateServer)
	api.GET("/servers/:id", d.handleGetServer)
	api.PATCH("/servers/:id", d.handleUpdateServer)
	api.DELETE("/servers/:id", d.handleDeleteServer)
	api.POST("/servers/:id/test", d.handleTestServer)

	api.GET("/databases", d.handleListDatabases)
	api.POST("/databases", d.handleCreateDatabase)
	api.GET("/databases/:id", d.handleGetDatabase)
	api.PATCH("/databases/:id", d.handleUpdateDatabase)
	api.DELETE("/databases/:id", d.handleDeleteDatabase)
	api.POST("/databases/:id/test", d.handleTestDatabase)
	api.POST("/databases/:id/query", d.handleQueryDatabase)

	api.GET("/approvals", d.handleListApprovals)
	api.GET("/approvals/:id", d.handleGetApproval)
	api.POST("/approvals/:id/decide", d.handleDecideApproval)

	api.GET("/audit", d.handleListAudit)
	api.POST("/ops/preview-risk", d.handlePreviewRisk)
}

// now 返回 UTC 时钟时间。
func (d *Deps) now() time.Time {
	if d.Clock != nil {
		return d.Clock().UTC()
	}
	return time.Now().UTC()
}

// decodeJSON 严格解析 JSON 请求体：Content-Type 必须为 application/json，
// 拒绝未知字段，限制 1MB，拒绝尾随数据。
func (d *Deps) decodeJSON(c *gin.Context, dst any) bool {
	if c.ContentType() != "application/json" {
		writeAPIError(c, http.StatusUnsupportedMediaType, apierr.ValidationFailed,
			"content type must be application/json")
		return false
	}
	body := http.MaxBytesReader(c.Writer, c.Request.Body, maxJSONBodyBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid request body")
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid request body")
		return false
	}
	return true
}

// writeAPIError 输出统一错误体，并保留 apierr.Status 的映射语义。
func writeAPIError(c *gin.Context, status int, code, message string) {
	e := apierr.New(code, message)
	e.RequestID = c.GetString("request_id")
	c.AbortWithStatusJSON(status, gin.H{"error": e})
}

// writeAPIErrorDetail 输出携带 details 的错误体（如契约 2.9 约定 1：
// CONTAINER_UNSUPPORTED 的 detail.runtime 现值）。
func writeAPIErrorDetail(c *gin.Context, status int, code, message string, details map[string]any) {
	e := apierr.New(code, message)
	e.RequestID = c.GetString("request_id")
	e.Details = details
	c.AbortWithStatusJSON(status, gin.H{"error": e})
}

// requireStore 在主库未就绪时返回 503。
func (d *Deps) requireStore(c *gin.Context) bool {
	if d.Store == nil {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "database unavailable")
		return false
	}
	return true
}

// writeStoreError 把 store 错误映射为统一错误码，绝不暴露 SQL 错误文本。
func (d *Deps) writeStoreError(c *gin.Context, err error, notFoundCode string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeAPIError(c, http.StatusNotFound, notFoundCode, "not found")
	case errors.Is(err, store.ErrConflict):
		writeAPIError(c, http.StatusConflict, apierr.Conflict, "conflict")
	case errors.Is(err, store.ErrInvalid):
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid argument")
	case errors.Is(err, store.ErrTxUnsupported):
		d.logError(c, "nested transaction", err)
		writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "internal server error")
	default:
		d.logError(c, "store operation failed", err)
		writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "internal server error")
	}
}

// writeServerStoreError 在通用映射上细化 servers 写操作的冲突文案：
// servers 表的唯一约束只有 name（migrations 00001 servers_name_key），
// 创建/更新命中 409 即重名（对齐 providers 的 "provider name already exists"）。
// p3 sprint 1：message 由英文 "server name already exists" 场景化为中文
// 「服务器名称已存在」——三期网络设备与服务器同走本分支，通用 409 文案
// +英文原因的弹窗未具体化（对齐数据库侧重名 409 具体化的产品标准），
// 前端 CONFLICT 映射直接透出括号内的中文原因。
func (d *Deps) writeServerStoreError(c *gin.Context, err error) {
	if errors.Is(err, store.ErrConflict) {
		writeAPIError(c, http.StatusConflict, apierr.Conflict, "服务器名称已存在")
		return
	}
	d.writeStoreError(c, err, apierr.ServerNotFound)
}

func (d *Deps) logError(c *gin.Context, msg string, err error) {
	if d.Log == nil {
		return
	}
	d.Log.ErrorContext(c.Request.Context(), "http_handler_error",
		"message", msg,
		"error", err.Error(),
		"request_id", c.GetString("request_id"),
		"path", c.Request.URL.Path,
	)
}

// auditEvent 记录用户发起的写操作审计；detail 只允许非敏感字段。
func (d *Deps) auditEvent(c *gin.Context, action, resourceType, resourceID string, detail map[string]any) {
	if d.Audit == nil {
		return
	}
	e := audit.Event{
		ActorType:    "user",
		ActorID:      c.GetString("user_id"),
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		RequestID:    c.GetString("request_id"),
		IP:           c.ClientIP(),
		Detail:       detail,
	}
	if err := d.Audit.Enqueue(c.Request.Context(), e); err != nil {
		d.logError(c, "audit enqueue failed", err)
	}
}

// encryptSecret 加密凭据；空串保持空串。
func (d *Deps) encryptSecret(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	if d.Cipher == nil {
		return "", errors.New("cipher unavailable")
	}
	return d.Cipher.Encrypt(plain)
}

// decryptSecret 解密凭据；仅用于构造连接，绝不回传或写日志。
func (d *Deps) decryptSecret(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	if d.Cipher == nil {
		return "", errors.New("cipher unavailable")
	}
	return d.Cipher.Decrypt(ciphertext)
}

// parseLimit 解析 limit（默认 50，范围 1..200）。
func parseLimit(c *gin.Context, def int) (int, bool) {
	raw := strings.TrimSpace(c.Query("limit"))
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 200 {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "limit must be between 1 and 200")
		return 0, false
	}
	return n, true
}

// trimName 校验 1..max 个字符的名称字段。
func trimName(s string, max int) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > max {
		return "", false
	}
	return s, true
}

// validHost 校验 host：1..255，不含空白、"/" 与 "://"。
func validHost(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 255 || strings.ContainsAny(s, " \t\r\n/") || strings.Contains(s, "://") {
		return "", false
	}
	return s, true
}

func validPort(p int) bool { return p >= 1 && p <= 65535 }

// normalizeTags 逐项 TrimSpace、丢弃空串、保序去重，最多 16 个、每个 1..32 字符。
func normalizeTags(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if utf8.RuneCountInString(t) > 32 {
			return nil, errors.New("tag exceeds 32 characters")
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) > 16 {
			return nil, errors.New("too many tags")
		}
	}
	return out, nil
}

func normalizeServer(s store.Server) store.Server {
	s.HasPassword = s.PasswordEncrypted != ""
	s.HasPrivateKey = s.PrivateKeyEncrypted != ""
	s.HasEnableSecret = s.EnableSecretEncrypted != ""
	if s.Tags == nil {
		s.Tags = []string{}
	}
	if s.DeviceKind == "" {
		s.DeviceKind = store.DeviceKindServer
	}
	if s.DeviceKind == store.DeviceKindServer {
		s.NetdevBrand = ""
		s.AccessProto = store.AccessProtoSSH
	}
	return s
}

// ---------------------------------------------------------------------------
// 网络设备（N1，契约 2.6/2.8）
// ---------------------------------------------------------------------------

// validNetdevBrand 校验 netdev_brand 取值。
func validNetdevBrand(b string) (store.NetdevBrand, bool) {
	switch store.NetdevBrand(b) {
	case store.NetdevBrandCisco, store.NetdevBrandHuawei, store.NetdevBrandH3C,
		store.NetdevBrandJunos, store.NetdevBrandGeneric:
		return store.NetdevBrand(b), true
	}
	return "", false
}

// validAccessProto 校验 access_proto 取值。
func validAccessProto(p string) (store.AccessProto, bool) {
	switch store.AccessProto(p) {
	case store.AccessProtoSSH, store.AccessProtoTelnet:
		return store.AccessProto(p), true
	}
	return "", false
}

// netDevTarget 解密凭据并构造网络设备连接目标（契约 5.2B Target：
// 登录密码 + enable/super 密码；错误信息不含凭据）。
func (d *Deps) netDevTarget(srv store.Server) (netdev.Target, error) {
	pw, err := d.decryptSecret(srv.PasswordEncrypted)
	if err != nil {
		return netdev.Target{}, fmt.Errorf("decrypt password: %w", err)
	}
	enable, err := d.decryptSecret(srv.EnableSecretEncrypted)
	if err != nil {
		return netdev.Target{}, fmt.Errorf("decrypt enable secret: %w", err)
	}
	port := srv.Port
	if port == 0 {
		port = 22
	}
	return netdev.Target{
		ServerID:     srv.ID,
		Brand:        netdev.BrandOf(srv.NetdevBrand),
		AccessProto:  srv.AccessProto,
		Host:         srv.Host,
		Port:         port,
		Username:     srv.Username,
		Password:     pw,
		EnableSecret: enable,
		KnownHostKey: srv.HostKeyPublic,
	}, nil
}

// sshTarget 解密凭据并构造 SSH 连接目标；错误信息不含凭据。
func (d *Deps) sshTarget(srv store.Server) (sshx.Target, error) {
	target := sshx.Target{
		ServerID:     srv.ID,
		Host:         srv.Host,
		Port:         srv.Port,
		Username:     srv.Username,
		AuthMethod:   srv.AuthMethod,
		KnownHostKey: srv.HostKeyPublic,
	}
	switch srv.AuthMethod {
	case store.AuthMethodPassword:
		pw, err := d.decryptSecret(srv.PasswordEncrypted)
		if err != nil {
			return target, fmt.Errorf("decrypt password: %w", err)
		}
		target.Password = pw
	case store.AuthMethodPrivateKey:
		key, err := d.decryptSecret(srv.PrivateKeyEncrypted)
		if err != nil {
			return target, fmt.Errorf("decrypt private key: %w", err)
		}
		pass, err := d.decryptSecret(srv.PassphraseEncrypted)
		if err != nil {
			return target, fmt.Errorf("decrypt passphrase: %w", err)
		}
		target.PrivateKey = key
		target.Passphrase = pass
	default:
		return target, fmt.Errorf("unsupported auth method %q", srv.AuthMethod)
	}
	return target, nil
}

// validServerAuth 校验 os 与 auth_method 的合法组合（契约 2.6/2.7）：
// os=linux 仅 password/private_key；os=windows 另允许 password_ntlm/password_basic。
func validServerAuth(os store.ServerOS, auth store.AuthMethod) bool {
	switch os {
	case store.ServerOSLinux:
		return auth == store.AuthMethodPassword || auth == store.AuthMethodPrivateKey
	case store.ServerOSWindows:
		switch auth {
		case store.AuthMethodPassword, store.AuthMethodPrivateKey,
			store.AuthMethodPasswordNTLM, store.AuthMethodPasswordBasic:
			return true
		}
	}
	return false
}

// winRMTarget 解密凭据并构造 WinRM 连接目标（契约 5.2A：password_basic 且
// winrm_port=5986 时 UseHTTPS=true）。
func (d *Deps) winRMTarget(srv store.Server) (winrmx.Target, error) {
	target := winrmx.Target{
		ServerID: srv.ID,
		Host:     srv.Host,
		Port:     srv.WinRMPort,
		Username: srv.Username,
		AuthType: srv.AuthMethod,
		UseHTTPS: srv.AuthMethod == store.AuthMethodPasswordBasic && srv.WinRMPort == 5986,
	}
	if target.Port == 0 {
		target.Port = 5985
	}
	pw, err := d.decryptSecret(srv.PasswordEncrypted)
	if err != nil {
		return target, fmt.Errorf("decrypt password: %w", err)
	}
	target.Password = pw
	return target, nil
}

// ---------------------------------------------------------------------------
// Servers
// ---------------------------------------------------------------------------

type serverCreateRequest struct {
	Name            string   `json:"name"`
	OS              string   `json:"os"`
	DeviceKind      string   `json:"device_kind"`  // N1：缺省 server
	NetdevBrand     string   `json:"netdev_brand"` // N1：仅 network_device
	AccessProto     string   `json:"access_proto"` // N1：缺省 ssh
	Host            string   `json:"host"`
	Port            *int     `json:"port"`
	WinRMPort       *int     `json:"winrm_port"`
	Username        string   `json:"username"`
	AuthMethod      string   `json:"auth_method"`
	Password        *string  `json:"password"`
	PrivateKey      *string  `json:"private_key"`
	Passphrase      *string  `json:"passphrase"`
	EnableSecret    *string  `json:"enable_secret"`            // N1：仅 network_device；空=未配置
	ConfirmTelnet   *bool    `json:"confirm_telnet_cleartext"` // N1：telnet 明文确认
	Tags            []string `json:"tags"`
	AutoExecEnabled *bool    `json:"auto_exec_enabled"`
}

type serverUpdateRequest struct {
	Name            *string   `json:"name"`
	OS              *string   `json:"os"`
	DeviceKind      *string   `json:"device_kind"`
	NetdevBrand     *string   `json:"netdev_brand"`
	AccessProto     *string   `json:"access_proto"`
	Host            *string   `json:"host"`
	Port            *int      `json:"port"`
	WinRMPort       *int      `json:"winrm_port"`
	Username        *string   `json:"username"`
	AuthMethod      *string   `json:"auth_method"`
	Password        *string   `json:"password"`
	PrivateKey      *string   `json:"private_key"`
	Passphrase      *string   `json:"passphrase"`
	EnableSecret    *string   `json:"enable_secret"`
	ConfirmTelnet   *bool     `json:"confirm_telnet_cleartext"`
	Tags            *[]string `json:"tags"`
	AutoExecEnabled *bool     `json:"auto_exec_enabled"`
}

type serverTestResponse struct {
	OK            bool   `json:"ok"`
	LatencyMS     int64  `json:"latency_ms"`
	Fingerprint   string `json:"fingerprint"`
	FirstSeen     bool   `json:"first_seen"`
	ServerVersion string `json:"server_version"`
	// Telnet 接入的网络设备：明文风险告警（契约 2.6「UI 必须显示明文
	// 风险提示」）；其余通道空串（omitempty 不下发）。
	CleartextWarning string `json:"cleartext_warning,omitempty"`
}

func (d *Deps) handleListServers(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	items, err := d.Store.Servers().List(c.Request.Context())
	if err != nil {
		d.writeStoreError(c, err, apierr.ServerNotFound)
		return
	}
	out := make([]store.Server, 0, len(items))
	for _, s := range items {
		out = append(out, normalizeServer(s))
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

func (d *Deps) handleCreateServer(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	var req serverCreateRequest
	if !d.decodeJSON(c, &req) {
		return
	}

	name, ok := trimName(req.Name, 100)
	if !ok {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid name")
		return
	}
	host, ok := validHost(req.Host)
	if !ok {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid host")
		return
	}
	username, ok := trimName(req.Username, 100)
	if !ok {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid username")
		return
	}
	port := 22
	if req.Port != nil {
		port = *req.Port
	}
	if !validPort(port) {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid port")
		return
	}
	serverOS := store.ServerOS(req.OS)
	if req.OS == "" {
		serverOS = store.ServerOSLinux
	}
	// 三期（N1）：device_kind 缺省 server；network_device 时 os 恒 network
	//（双向绑定：os=network 仅允许出现在 network_device，契约 2.6）。
	deviceKind := store.DeviceKind(req.DeviceKind)
	if req.DeviceKind == "" {
		deviceKind = store.DeviceKindServer
	}
	switch deviceKind {
	case store.DeviceKindServer:
		if serverOS != store.ServerOSLinux && serverOS != store.ServerOSWindows {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "os must be linux or windows")
			return
		}
	case store.DeviceKindNetworkDevice:
		if req.OS != "" && serverOS != store.ServerOSNetwork {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"os must be network for network devices")
			return
		}
		serverOS = store.ServerOSNetwork
	default:
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
			"device_kind must be server or network_device")
		return
	}
	var brand store.NetdevBrand
	accessProto := store.AccessProtoSSH
	if deviceKind == store.DeviceKindNetworkDevice {
		b, ok := validNetdevBrand(req.NetdevBrand)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"netdev_brand is required for network devices (cisco|huawei|h3c|junos|generic)")
			return
		}
		brand = b
		if req.AccessProto != "" {
			p, ok := validAccessProto(req.AccessProto)
			if !ok {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
					"access_proto must be ssh or telnet")
				return
			}
			accessProto = p
		}
		// telnet 明文：必须显式确认（契约 2.6：缺失或 false 返回 400 不落库）。
		if accessProto == store.AccessProtoTelnet && (req.ConfirmTelnet == nil || !*req.ConfirmTelnet) {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"confirm_telnet_cleartext=true is required for telnet access")
			return
		}
		if req.EnableSecret != nil && len(*req.EnableSecret) > 4096 {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "enable_secret is too large")
			return
		}
	} else {
		if req.NetdevBrand != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"netdev_brand is only allowed for network devices")
			return
		}
		if req.AccessProto != "" && req.AccessProto != string(store.AccessProtoSSH) {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"access_proto is only allowed for network devices")
			return
		}
		if req.EnableSecret != nil {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"enable_secret is only allowed for network devices")
			return
		}
		if req.ConfirmTelnet != nil {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"confirm_telnet_cleartext is only allowed for network devices")
			return
		}
	}
	winrmPort := 5985
	if req.WinRMPort != nil {
		winrmPort = *req.WinRMPort
	}
	if !validPort(winrmPort) {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid winrm_port")
		return
	}
	tags, err := normalizeTags(req.Tags)
	if err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, err.Error())
		return
	}
	authMethod := store.AuthMethod(req.AuthMethod)
	if deviceKind == store.DeviceKindNetworkDevice {
		// 契约 2.6：网络设备 SSH 密钥认证 v1 不支持，失败关闭——仅 password。
		if authMethod != store.AuthMethodPassword {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"auth_method must be password for network devices")
			return
		}
		if req.PrivateKey != nil {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"private_key is not allowed for network devices")
			return
		}
	} else if !validServerAuth(serverOS, authMethod) {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
			"auth_method is not allowed for this os")
		return
	}
	creds, err := d.buildCreateCredentials(authMethod, req.Password, req.PrivateKey, req.Passphrase)
	if err != nil {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, err.Error())
		return
	}
	var enableEnc string
	if deviceKind == store.DeviceKindNetworkDevice && req.EnableSecret != nil && *req.EnableSecret != "" {
		enc, err := d.encryptSecret(*req.EnableSecret)
		if err != nil {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "failed to encrypt credentials")
			return
		}
		enableEnc = enc
	}

	created, err := d.Store.Servers().Create(c.Request.Context(), store.CreateServerParams{
		Name:                  name,
		OS:                    serverOS,
		DeviceKind:            deviceKind,
		NetdevBrand:           brand,
		AccessProto:           accessProto,
		Host:                  host,
		Port:                  port,
		WinRMPort:             winrmPort,
		Username:              username,
		Credentials:           creds,
		EnableSecretEncrypted: enableEnc,
		Tags:                  tags,
		AutoExecEnabled:       req.AutoExecEnabled != nil && *req.AutoExecEnabled,
	})
	if err != nil {
		d.writeServerStoreError(c, err)
		return
	}
	if d.SSH != nil {
		d.SSH.Invalidate(created.ID)
	}
	if d.WinRM != nil {
		d.WinRM.Invalidate(created.ID)
	}
	if d.NetCLI != nil {
		d.NetCLI.Invalidate(created.ID)
	}
	createDetail := map[string]any{
		"name":              created.Name,
		"device_kind":       string(created.DeviceKind),
		"os":                string(created.OS),
		"netdev_brand":      string(created.NetdevBrand),
		"access_proto":      string(created.AccessProto),
		"host":              created.Host,
		"port":              created.Port,
		"winrm_port":        created.WinRMPort,
		"username":          created.Username,
		"auth_method":       string(created.AuthMethod),
		"has_enable_secret": created.HasEnableSecret,
		"tags":              created.Tags,
		"auto_exec_enabled": created.AutoExecEnabled,
	}
	// 契约 9.7：telnet 明文确认体现在 server.create 的
	// detail.confirm_telnet_cleartext=true（布尔标记，不含凭据）。
	if created.AccessProto == store.AccessProtoTelnet {
		createDetail["confirm_telnet_cleartext"] = true
	}
	d.auditEvent(c, "server.create", "server", created.ID, createDetail)
	c.JSON(http.StatusCreated, normalizeServer(created))
}

func (d *Deps) handleGetServer(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	srv, err := d.Store.Servers().Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		d.writeStoreError(c, err, apierr.ServerNotFound)
		return
	}
	c.JSON(http.StatusOK, normalizeServer(srv))
}

func (d *Deps) handleUpdateServer(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	var req serverUpdateRequest
	if !d.decodeJSON(c, &req) {
		return
	}
	id := c.Param("id")
	current, err := d.Store.Servers().Get(c.Request.Context(), id)
	if err != nil {
		d.writeStoreError(c, err, apierr.ServerNotFound)
		return
	}

	params := store.UpdateServerParams{}
	hasField := false

	if req.Name != nil {
		name, ok := trimName(*req.Name, 100)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid name")
			return
		}
		params.Name = &name
		hasField = true
	}
	if req.Host != nil {
		host, ok := validHost(*req.Host)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid host")
			return
		}
		params.Host = &host
		hasField = true
	}
	if req.OS != nil {
		osVal := store.ServerOS(*req.OS)
		switch osVal {
		case store.ServerOSLinux, store.ServerOSWindows, store.ServerOSNetwork:
			params.OS = &osVal
			hasField = true
		default:
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"os must be linux, windows or network")
			return
		}
	}
	if req.DeviceKind != nil {
		kindVal := store.DeviceKind(*req.DeviceKind)
		if kindVal != store.DeviceKindServer && kindVal != store.DeviceKindNetworkDevice {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"device_kind must be server or network_device")
			return
		}
		params.DeviceKind = &kindVal
		hasField = true
	}
	if req.NetdevBrand != nil {
		// 空串表示清除（openapi：anyOf NetdevBrand | const ''；store 的
		// Update 支持置空）——设备形态切回 server 时前端发空串，不得按
		// 枚举外拒绝（三期评审：切换流程被 400 卡死）。非空才校验枚举。
		bVal := store.NetdevBrand(*req.NetdevBrand)
		if *req.NetdevBrand != "" {
			if _, ok := validNetdevBrand(*req.NetdevBrand); !ok {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
					"netdev_brand must be cisco, huawei, h3c, junos or generic")
				return
			}
		}
		params.NetdevBrand = &bVal
		hasField = true
	}
	if req.AccessProto != nil {
		pVal := store.AccessProto(*req.AccessProto)
		if _, ok := validAccessProto(*req.AccessProto); !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"access_proto must be ssh or telnet")
			return
		}
		params.AccessProto = &pVal
		hasField = true
	}
	if req.EnableSecret != nil {
		if len(*req.EnableSecret) > 4096 {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "enable_secret is too large")
			return
		}
		// 提供即替换、空串清空（契约 2.6，语义同 password）；加密在下方
		// final 校验通过后进行。
		hasField = true
	}
	if req.Port != nil {
		if !validPort(*req.Port) {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid port")
			return
		}
		params.Port = req.Port
		hasField = true
	}
	if req.WinRMPort != nil {
		if !validPort(*req.WinRMPort) {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid winrm_port")
			return
		}
		params.WinRMPort = req.WinRMPort
		hasField = true
	}
	if req.Username != nil {
		username, ok := trimName(*req.Username, 100)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid username")
			return
		}
		params.Username = &username
		hasField = true
	}
	if req.Tags != nil {
		tags, err := normalizeTags(*req.Tags)
		if err != nil {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, err.Error())
			return
		}
		params.Tags = &tags
		hasField = true
	}
	if req.AutoExecEnabled != nil {
		params.AutoExecEnabled = req.AutoExecEnabled
		hasField = true
	}

	credentialProvided := req.Password != nil || req.PrivateKey != nil || req.Passphrase != nil
	if credentialProvided {
		if req.AuthMethod == nil {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "auth_method is required when credentials are provided")
			return
		}
		hasField = true
	} else if req.AuthMethod != nil {
		// 只给 auth_method 而不给凭据不符合 OpenAPI 约束。
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "credentials are required when auth_method is provided")
		return
	}

	if !hasField {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "at least one field is required")
		return
	}

	if credentialProvided && req.AuthMethod != nil {
		authMethod := store.AuthMethod(*req.AuthMethod)
		creds, err := d.buildUpdateCredentials(authMethod, req.Password, req.PrivateKey, req.Passphrase)
		if err != nil {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, err.Error())
			return
		}
		params.Credentials = &creds
	}

	// 契约 2.6：修改 os 后若现存认证方式不合法（如 linux 换成 winrm 认证）
	// 返回 400——用更新后的最终 (os, auth_method) 组合判定。
	finalOS := current.OS
	if params.OS != nil {
		finalOS = *params.OS
	}
	finalAuth := current.AuthMethod
	if params.Credentials != nil {
		finalAuth = params.Credentials.AuthMethod
	}

	// 三期（N1）：终态组合校验（契约 2.6 的绑定关系）。
	finalKind := current.DeviceKind
	if params.DeviceKind != nil {
		finalKind = *params.DeviceKind
	}
	finalBrand := current.NetdevBrand
	if params.NetdevBrand != nil {
		finalBrand = *params.NetdevBrand
	}
	finalProto := current.AccessProto
	if current.AccessProto == "" {
		finalProto = store.AccessProtoSSH
	}
	if params.AccessProto != nil {
		finalProto = *params.AccessProto
	}
	switch finalKind {
	case store.DeviceKindNetworkDevice:
		if finalOS != store.ServerOSNetwork {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"os must be network for network devices")
			return
		}
		if _, ok := validNetdevBrand(string(finalBrand)); !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"netdev_brand is required for network devices")
			return
		}
		// 网络设备仅密码认证（契约 2.6 失败关闭）。
		if finalAuth != store.AuthMethodPassword {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"auth_method must be password for network devices")
			return
		}
		// telnet 终态必须显式确认（openapi 近似表达：跨请求终态组合由
		// 服务端按契约 2.6 强制）。
		if finalProto == store.AccessProtoTelnet && (req.ConfirmTelnet == nil || !*req.ConfirmTelnet) {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"confirm_telnet_cleartext=true is required for telnet access")
			return
		}
		// enable_secret：提供即替换、空串清空。
		if req.EnableSecret != nil {
			if *req.EnableSecret == "" {
				empty := ""
				params.EnableSecretEncrypted = &empty
			} else {
				enc, err := d.encryptSecret(*req.EnableSecret)
				if err != nil {
					writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
						"failed to encrypt credentials")
					return
				}
				params.EnableSecretEncrypted = &enc
			}
		}
	case store.DeviceKindServer:
		if finalOS != store.ServerOSLinux && finalOS != store.ServerOSWindows {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"os must be linux or windows for servers")
			return
		}
		if finalBrand != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"netdev_brand is only allowed for network devices")
			return
		}
		if req.EnableSecret != nil {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"enable_secret is only allowed for network devices")
			return
		}
		if !validServerAuth(finalOS, finalAuth) {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"auth_method is not allowed for this os")
			return
		}
	default:
		if !validServerAuth(finalOS, finalAuth) {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"auth_method is not allowed for this os")
			return
		}
	}

	updated, err := d.Store.Servers().Update(c.Request.Context(), id, params)
	if err != nil {
		d.writeServerStoreError(c, err)
		return
	}
	if d.SSH != nil {
		d.SSH.Invalidate(id)
	}
	if d.WinRM != nil {
		d.WinRM.Invalidate(id)
	}
	// 契约 2.6：修改 device_kind/netdev_brand/access_proto/host/port/username
	// 后失效网络设备连接（对 server 类设备为幂等空操作）。
	if d.NetCLI != nil {
		d.NetCLI.Invalidate(id)
	}
	updateDetail := map[string]any{
		"name":              updated.Name,
		"device_kind":       string(updated.DeviceKind),
		"os":                string(updated.OS),
		"netdev_brand":      string(updated.NetdevBrand),
		"access_proto":      string(updated.AccessProto),
		"host":              updated.Host,
		"port":              updated.Port,
		"winrm_port":        updated.WinRMPort,
		"username":          updated.Username,
		"auth_method":       string(updated.AuthMethod),
		"has_enable_secret": updated.HasEnableSecret,
		"tags":              updated.Tags,
		"auto_exec_enabled": updated.AutoExecEnabled,
		"credentials_reset": credentialProvided,
	}
	// 契约 9.7：telnet 明文确认体现在 server.update 的
	// detail.confirm_telnet_cleartext=true（布尔标记，不含凭据）。
	if updated.AccessProto == store.AccessProtoTelnet {
		updateDetail["confirm_telnet_cleartext"] = true
	}
	d.auditEvent(c, "server.update", "server", id, updateDetail)
	c.JSON(http.StatusOK, normalizeServer(updated))
}

func (d *Deps) handleDeleteServer(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	id := c.Param("id")
	srv, err := d.Store.Servers().Get(c.Request.Context(), id)
	if err != nil {
		d.writeStoreError(c, err, apierr.ServerNotFound)
		return
	}
	// 契约 2.6：删除前先结束该服务器的活动终端/RDP 会话
	// （Term.CloseByServer / RDP.CloseByServer，ended_reason=error），
	// 即时切断已建立的访问；否则级联删除会话行后，内存 live 会话的
	// PTY 继续以原凭据存活至空闲超时，且收尾 Close 因行不存在落空、
	// session.end 审计丢失（应急隔离语义失效）。
	if d.Term != nil {
		d.Term.CloseByServer(c.Request.Context(), id, "error")
	}
	if d.RDP != nil {
		d.RDP.CloseByServer(c.Request.Context(), id, "error")
	}
	if err := d.Store.Servers().Delete(c.Request.Context(), id); err != nil {
		d.writeStoreError(c, err, apierr.ServerNotFound)
		return
	}
	if d.SSH != nil {
		d.SSH.Invalidate(id)
	}
	if d.NetCLI != nil {
		d.NetCLI.Invalidate(id)
	}
	d.auditEvent(c, "server.delete", "server", id, map[string]any{
		"name":        srv.Name,
		"device_kind": string(srv.DeviceKind),
		"host":        srv.Host,
	})
	c.Status(http.StatusNoContent)
}

// probeAndWriteContainerRuntime 在连接测试成功后顺带探测容器运行时并写回
// （四期 C1，契约 2.9 约定 1 的两处写回时机之一）；探测失败按 none 写回。
// network_device 不探测。探测绝不拖垮 test 响应（连接本身已成功）：
// 凭据/依赖不全的部署（如最小测试装配）静默跳过。
func (d *Deps) probeAndWriteContainerRuntime(c *gin.Context, srv store.Server) {
	if srv.DeviceKind == store.DeviceKindNetworkDevice {
		return
	}
	defer func() { _ = recover() }() // 顺带探测尽力而为
	exec, err := d.containerExecFunc(srv)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), containerDetectTimeout)
	defer cancel()
	rt, err := containerx.Detect(ctx, exec, string(srv.OS))
	if err != nil {
		// 连接测试本身已成功的场景下探测通道错误不落库（保持 '' 待下次
		// 探测），仅探测确认后写回（同容器接口口径，评审发现钉死问题）。
		return
	}
	if _, err := d.Store.Servers().UpdateContainerRuntime(c.Request.Context(), srv.ID, store.ContainerRuntime(rt), d.now()); err != nil {
		d.logError(c, "update container runtime failed", err)
	}
}

func (d *Deps) handleTestServer(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	id := c.Param("id")
	srv, err := d.Store.Servers().Get(c.Request.Context(), id)
	if err != nil {
		d.writeStoreError(c, err, apierr.ServerNotFound)
		return
	}

	// 三期（N1）：network_device 走 netcli 探针（品牌命令，契约 2.8.4）；
	// SSH 通道底层经 sshx 拨号（TOFU 语义不变，FirstSeen 写回），Telnet
	// 通道无主机密钥语义。
	if srv.DeviceKind == store.DeviceKindNetworkDevice {
		if d.NetCLI == nil {
			writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "netdev executor unavailable")
			return
		}
		target, err := d.netDevTarget(srv)
		if err != nil {
			d.logError(c, "build netdev target failed", err)
			writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "failed to prepare credentials")
			return
		}
		ctx, cancel := contextWithTimeout(c, 45*time.Second)
		defer cancel()
		res, err := d.NetCLI.Test(ctx, target)
		if err == nil {
			if res.FirstSeen {
				if _, err := d.Store.Servers().UpdateHostKey(ctx, id, res.Fingerprint, res.HostKey, d.now()); err != nil {
					d.logError(c, "update host key failed", err)
				}
				d.auditEvent(c, "server.host_key_trusted", "server", id, map[string]any{
					"fingerprint": res.Fingerprint,
				})
			}
			d.auditEvent(c, "server.test", "server", id, map[string]any{
				"ok": true, "latency_ms": res.LatencyMS, "first_seen": res.FirstSeen,
				"channel": string(srv.AccessProto), "netdev_brand": string(srv.NetdevBrand),
			})
			c.JSON(http.StatusOK, serverTestResponse{
				OK:               true,
				LatencyMS:        res.LatencyMS,
				Fingerprint:      res.Fingerprint,
				FirstSeen:        res.FirstSeen,
				ServerVersion:    res.ServerVersion,
				CleartextWarning: res.CleartextWarning,
			})
			return
		}
		// 错误映射（契约 9.6）：登录失败 NETCLI_LOGIN_FAILED、expect 失败
		// NETCLI_EXEC_FAILED（502）；超时 SSH_TIMEOUT（504）；其余 502
		// SERVER_TEST_FAILED。
		code := apierr.ServerTestFailed
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, netdev.ErrLoginFailed):
			code = apierr.NetCLILoginFailed
		case errors.Is(err, netdev.ErrExecFailed):
			code = apierr.NetCLIExecFailed
		case errors.Is(err, context.DeadlineExceeded):
			code = apierr.SSHTimeout
			status = http.StatusGatewayTimeout
		}
		d.auditEvent(c, "server.test", "server", id, map[string]any{
			"ok": false, "error": code, "channel": string(srv.AccessProto),
		})
		d.logError(c, "netdev test failed", err)
		writeAPIError(c, status, code, describeNetdevTestFailure(
			"network device connection test failed", target.Host, target.Port, err))
		return
	}

	// 契约 2.7：连接测试按通道路由——WinRM 认证（仅 os=windows 合法）走
	// winrmx 探针（Get-WmiObject Win32_ComputerSystem，无主机密钥语义）；
	// 其余走 sshx 握手。
	if srv.AuthMethod == store.AuthMethodPasswordNTLM || srv.AuthMethod == store.AuthMethodPasswordBasic {
		if srv.OS != store.ServerOSWindows {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"winrm auth requires os=windows")
			return
		}
		if d.WinRM == nil {
			writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "winrm executor unavailable")
			return
		}
		target, err := d.winRMTarget(srv)
		if err != nil {
			d.logError(c, "build winrm target failed", err)
			writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "failed to prepare credentials")
			return
		}
		ctx, cancel := contextWithTimeout(c, 30*time.Second)
		defer cancel()
		res, err := d.WinRM.Test(ctx, target)
		if err != nil {
			code := apierr.ServerTestFailed
			status := http.StatusBadGateway
			if errors.Is(err, context.DeadlineExceeded) {
				code = apierr.SSHTimeout
				status = http.StatusGatewayTimeout
			}
			d.auditEvent(c, "server.test", "server", id, map[string]any{"ok": false, "error": code, "channel": "winrm"})
			d.logError(c, "winrm test failed", err)
			writeAPIError(c, status, code, describeConnTestFailure("winrm connection test failed", target.Host, target.Port, err))
			return
		}
		d.auditEvent(c, "server.test", "server", id, map[string]any{
			"ok": true, "latency_ms": res.LatencyMS, "first_seen": false, "channel": "winrm",
		})
		d.probeAndWriteContainerRuntime(c, srv)
		c.JSON(http.StatusOK, serverTestResponse{
			OK:            true,
			LatencyMS:     res.LatencyMS,
			Fingerprint:   "",
			FirstSeen:     false,
			ServerVersion: res.ServerVersion,
		})
		return
	}

	if d.SSH == nil {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "ssh executor unavailable")
		return
	}
	target, err := d.sshTarget(srv)
	if err != nil {
		d.logError(c, "build ssh target failed", err)
		writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "failed to prepare credentials")
		return
	}

	ctx, cancel := contextWithTimeout(c, 30*time.Second)
	defer cancel()

	res, err := d.SSH.Test(ctx, target)
	if err != nil {
		var hkErr *sshx.HostKeyError
		if errors.As(err, &hkErr) {
			d.auditEvent(c, "server.host_key_changed", "server", id, map[string]any{
				"expected": hkErr.Expected,
				"actual":   hkErr.Actual,
			})
			writeAPIError(c, http.StatusConflict, apierr.SSHHostKeyChanged, "host key changed")
			return
		}
		code := apierr.ServerTestFailed
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			code = apierr.SSHTimeout
			status = http.StatusGatewayTimeout
		}
		d.auditEvent(c, "server.test", "server", id, map[string]any{"ok": false, "error": code, "channel": "ssh"})
		d.logError(c, "ssh test failed", err)
		writeAPIError(c, status, code, describeConnTestFailure("ssh connection test failed", target.Host, target.Port, err))
		return
	}

	if res.FirstSeen {
		if _, err := d.Store.Servers().UpdateHostKey(ctx, id, res.Fingerprint, res.HostKey, d.now()); err != nil {
			d.logError(c, "update host key failed", err)
		}
		d.auditEvent(c, "server.host_key_trusted", "server", id, map[string]any{
			"fingerprint": res.Fingerprint,
		})
	}
	d.auditEvent(c, "server.test", "server", id, map[string]any{
		"ok":         true,
		"latency_ms": res.LatencyMS,
		"first_seen": res.FirstSeen,
		"channel":    "ssh",
	})
	d.probeAndWriteContainerRuntime(c, srv)

	c.JSON(http.StatusOK, serverTestResponse{
		OK:            true,
		LatencyMS:     res.LatencyMS,
		Fingerprint:   res.Fingerprint,
		FirstSeen:     res.FirstSeen,
		ServerVersion: res.ServerVersion,
	})
}

// describeNetdevTestFailure 组装 netdev 连接测试失败的 message（p3
// sprint 5）：登录/执行期哨兵（netdev.ErrLoginFailed/ErrExecFailed）在
// expect.go 以 %v 包装底层错误（不入 Unwrap 链），ClassifyDialFailure 对
// 「channel closed / 凭据被拒」等登录期形态返回 DialUnknown——
// describeConnTestFailure 对这些路径只能拼整条英文链（走查 sprint 5：
// detail 以 "netdev: device login failed: channel closed: EOF" 整条
// 开头，中文标题之外仅 netdev 登录路径无中文分类）。这里按哨兵与底层
// 形态给中文分类短语，并剥掉哨兵前缀只留底层原因（与 SSH/WinRM 的
// 「无法连接 host:port：分类；原因：…」同构）；非哨兵失败（拨号/握手
// 期）仍走 describeConnTestFailure 的网络层分类。
func describeNetdevTestFailure(prefix, host string, port int, err error) string {
	phrase, reason, ok := netdevStagePhrase(err)
	if !ok {
		return describeConnTestFailure(prefix, host, port, err)
	}
	if reason == "" {
		return fmt.Sprintf("%s: 无法登录 %s:%d：%s", prefix, host, port, phrase)
	}
	return fmt.Sprintf("%s: 无法登录 %s:%d：%s；原因：%s", prefix, host, port, phrase, reason)
}

// netdevStagePhrase 按哨兵与底层文本特征返回 netdev 登录/执行期失败的
// 中文分类短语与剥掉哨兵前缀后的底层原因（错误信息只用于分类与展示，
// 不含凭据明文）；非哨兵失败 ok=false（拨号/握手期走网络层分类）。
// 底层细分依据 expect.go 实际产出的三种形态：通道断开（channel
// closed）、总预算超时（expect timeout）、凭据被拒（登录提示重现）。
func netdevStagePhrase(err error) (phrase, reason string, ok bool) {
	msg := err.Error()
	login := netdev.ErrLoginFailed.Error()
	exec := netdev.ErrExecFailed.Error()
	switch {
	case strings.HasPrefix(msg, login):
		reason = strings.TrimLeft(strings.TrimPrefix(msg, login), ": ")
		phrase = "登录阶段失败（设备提示符未出现）"
		switch {
		case strings.Contains(msg, "credentials rejected"):
			phrase = "设备拒绝登录凭据（登录提示重新出现）"
		case strings.Contains(msg, "channel closed"):
			phrase = "设备侧在登录阶段断开通道"
		case strings.Contains(msg, "expect timeout"):
			phrase = "登录阶段超时（未在时限内等到设备提示符）"
		}
		return phrase, reason, true
	case strings.HasPrefix(msg, exec):
		reason = strings.TrimLeft(strings.TrimPrefix(msg, exec), ": ")
		phrase = "命令执行阶段失败（设备提示符未再出现）"
		switch {
		case strings.Contains(msg, "channel closed"):
			phrase = "设备侧在命令输出阶段断开通道"
		case strings.Contains(msg, "expect timeout"):
			phrase = "命令输出阶段超时（未在时限内等到设备提示符）"
		}
		return phrase, reason, true
	}
	return "", "", false
}

// describeConnTestFailure 组装连接测试失败的 message（契约 2.7）：
// 通道前缀（前端 describeFailure 据此选择「SSH / WinRM 连接测试失败」
// 通道化标题，必须保留）+ 中文分类短语（无法连接 host:port：连接被
// 拒绝 / 认证失败 / 超时……，分类特征复用 termx.ClassifyDialFailure，
// p2 sprint 3：修复前直拼英文错误链，关键原因埋在 270 字符末尾）+
// 底层原因（折叠库的冗余包装，供排障）。request_id 由 writeAPIError
// 错误体固定携带。
func describeConnTestFailure(prefix, host string, port int, err error) string {
	reason := flattenTransportWrap(err)
	kind := termx.ClassifyDialFailure(err)
	if kind == termx.DialUnknown {
		return prefix + ": " + reason
	}
	return fmt.Sprintf("%s: 无法连接 %s:%d：%s；原因：%s", prefix, host, port, connFailurePhrase(kind), reason)
}

// connFailurePhrase 是服务器连接测试场景的中文分类短语（与 termx 的
// PTY 措辞独立：这里前缀已有「无法连接 host:port」语境）。
func connFailurePhrase(kind termx.DialFailureKind) string {
	switch kind {
	case termx.DialAuthFailed:
		return "认证失败（用户名或密码不正确）"
	case termx.DialHostKeyRejected:
		return "主机密钥校验失败（服务器指纹与已记录的不一致）"
	case termx.DialRefused:
		return "连接被拒绝（端口未开放或被防火墙拦截）"
	case termx.DialNoRoute:
		return "网络不可达（无到目标主机的路由）"
	case termx.DialDNSFailed:
		return "域名解析失败"
	case termx.DialTimedOut:
		return "连接超时（未在时限内完成握手）"
	default:
		return "连接失败"
	}
}

// flattenTransportWrap 沿 errors.Unwrap 链折叠无信息量的错误包装层，
// 返回清洗后的文本（仅用于展示 message；日志仍记原始错误）：
//   - masterzen/winrm 的 HTTP 层对未知状态包装占位前缀 "unknown error "；
//   - 其 NTLM 传输对同一失败两度包装同一短语（ntlm_sealing_transport.go
//     两处 "ntlm: negotiate message encryption: "），实测错误链同一短语
//     连续重复两次（p2 sprint 3 走查）；
//   - p3 sprint 6：内部包名哨兵前缀（"netdev: telnet dial: " /
//     "telnetx: dial host:port: " / "dbproxy: mongo ping: " 等，见
//     sentinelWrapPrefixRe）同属无信息量包装，剥掉后只留最内层原因；
//     mongo driver 的 topology dump 截断到 server selection 级原因
//     （见 stripTopologyDump）。
//
// 只删冗余包装文本，底层原因（URL、dial 错误等）原样保留；非
// 「前缀 + 内层」形态的包装层保守取该层全文本。
// sentinelWrapPrefixRe 匹配内部包名哨兵的包装前缀（p3 sprint 6）：这些
// 前缀只标记错误的产生位置（netdev 的 "telnet dial: " / "ssh pty
// session: "、telnetx 的 "dial host:port: "、dbproxy 的 "mongo ping: "
// 等，见 internal/netdev/channel.go、internal/netdev/telnetx/telnetx.go、
// internal/dbproxy），不含底层原因信息；透传到用户弹窗只会在中文分类
// 短语之后拖一条长英文噪音（走查 round 6：telnet 测试弹窗 reason 为
// "netdev: telnet dial: telnetx: dial 127.0.0.1:23: dial tcp
// 127.0.0.1:23: connect: connection refused"）。中间段限制为小写字母/
// 数字/点/冒号/空格/连字符（位置短语形态），避免误伤含大写或引号的
// 库层自然语言前缀。剥离只影响展示 message，后端日志仍记原始错误链。
var sentinelWrapPrefixRe = regexp.MustCompile(`^(netdev|telnetx|dbproxy): [a-z0-9 .:-]+: $`)

// mongoTopologyDumpMark 是 mongo driver ServerSelectionError 文本尾部的
// topology dump 标记（mongo-driver v1.17.4 x/mongo/driver/topology/
// errors.go: "server selection error: %s, current topology: { … }"）：
// 拓扑快照是后端排障信息，实测拖出数百字符的英文结构体 dump；弹窗
// reason 截断到 server selection 级原因，完整 dump 仍记后端日志。
const mongoTopologyDumpMark = "current topology: {"

// stripTopologyDump 截断 reason 中 mongo driver 的 topology dump
// （两种形态均覆盖：带 wrapped 原因的 "…, current topology: { … }" 与
// 无 wrapped 的 "server selection error: current topology: { … }"）。
func stripTopologyDump(text string) string {
	i := strings.Index(text, mongoTopologyDumpMark)
	if i < 0 {
		return text
	}
	return strings.TrimRight(text[:i], " :,")
}

func flattenTransportWrap(err error) string {
	if err == nil {
		return ""
	}
	var chain []error
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		chain = append(chain, cur)
	}
	text := chain[len(chain)-1].Error()
	for i := len(chain) - 2; i >= 0; i-- {
		inner, cur := chain[i+1].Error(), chain[i].Error()
		if !strings.HasSuffix(cur, inner) {
			// 非前缀式包装（如 %v 重述）：以该层全文本为准确续向外层。
			text = cur
			continue
		}
		prefix := cur[:len(cur)-len(inner)]
		switch {
		case prefix == "":
			// 同文本多层包装，无增量。
		case prefix == "unknown error ":
			// 库的占位前缀，无信息量。
		case sentinelWrapPrefixRe.MatchString(prefix):
			// 内部包名哨兵前缀（p3 sprint 6）：位置标记无信息量，
			// 剥掉后弹窗只保留最内层原因（如 dial tcp 级）。
		case strings.HasPrefix(inner, prefix):
			// 重复包装（如两层 negotiate 短语）。
		default:
			text = prefix + text
		}
	}
	return stripTopologyDump(text)
}

// buildCreateCredentials 校验并加密创建时的凭据。
func (d *Deps) buildCreateCredentials(authMethod store.AuthMethod, password, privateKey, passphrase *string) (store.ServerCredentials, error) {
	if len(*orEmpty(password)) > 4096 || len(*orEmpty(privateKey)) > 65536 || len(*orEmpty(passphrase)) > 4096 {
		return store.ServerCredentials{}, errors.New("credential is too large")
	}
	creds := store.ServerCredentials{AuthMethod: authMethod}
	switch authMethod {
	case store.AuthMethodPassword:
		if privateKey != nil {
			return creds, errors.New("private_key is not allowed with password auth")
		}
		if password == nil || *password == "" {
			return creds, errors.New("password is required for password auth")
		}
		enc, err := d.encryptSecret(*password)
		if err != nil {
			return creds, errors.New("failed to encrypt credentials")
		}
		creds.PasswordEncrypted = enc
		if passphrase != nil && *passphrase != "" {
			p, err := d.encryptSecret(*passphrase)
			if err != nil {
				return creds, errors.New("failed to encrypt credentials")
			}
			creds.PassphraseEncrypted = p
		}
		return creds, nil

	case store.AuthMethodPrivateKey:
		if password != nil {
			return creds, errors.New("password is not allowed with private_key auth")
		}
		if privateKey == nil || *privateKey == "" {
			return creds, errors.New("private_key is required for private_key auth")
		}
		enc, err := d.encryptSecret(*privateKey)
		if err != nil {
			return creds, errors.New("failed to encrypt credentials")
		}
		creds.PrivateKeyEncrypted = enc
		if passphrase != nil && *passphrase != "" {
			p, err := d.encryptSecret(*passphrase)
			if err != nil {
				return creds, errors.New("failed to encrypt credentials")
			}
			creds.PassphraseEncrypted = p
		}
		return creds, nil

	case store.AuthMethodPasswordNTLM, store.AuthMethodPasswordBasic:
		// WinRM 认证（契约 4.3）：仅密码，密码加密存 password_enc；
		// os×auth 组合校验由 handler 的 validServerAuth 负责。
		if privateKey != nil || passphrase != nil {
			return creds, errors.New("only password is allowed with winrm auth")
		}
		if password == nil || *password == "" {
			return creds, errors.New("password is required for winrm auth")
		}
		enc, err := d.encryptSecret(*password)
		if err != nil {
			return creds, errors.New("failed to encrypt credentials")
		}
		creds.PasswordEncrypted = enc
		return creds, nil

	default:
		return creds, errors.New("auth_method must be password, private_key, password_ntlm or password_basic")
	}
}

// buildUpdateCredentials 校验并加密更新时的凭据（整体替换）。
func (d *Deps) buildUpdateCredentials(authMethod store.AuthMethod, password, privateKey, passphrase *string) (store.ServerCredentials, error) {
	return d.buildCreateCredentials(authMethod, password, privateKey, passphrase)
}

func orEmpty(s *string) *string {
	if s == nil {
		empty := ""
		return &empty
	}
	return s
}

// contextWithTimeout 在请求 context 上叠加超时。
func contextWithTimeout(c *gin.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), d)
}
