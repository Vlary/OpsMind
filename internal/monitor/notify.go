// notify.go 实现五期 M1 的通知投递（契约 5.15/2.10 约定 2/11 节 #5）：
// webhook（HMAC-SHA256 签名头）/飞书（自建应用 webhook + 签名）/钉钉
// （加签 URL）/email（SMTP）。缓冲队列 QueueSize=256——满则丢弃并置
// failed（通知尽力而为，与审计背压语义相反）；worker 2；重试 3 次
// 退避 1s/5s/15s；HTTP 10s/SMTP 15s。同一 firing 只投一次
// （last_notified_at 非空不重投——由评估器保证入队唯一 + 投递幂等检查）。
package monitor

import (
	"aiops/internal/logx"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"aiops/internal/cryptox"
	"aiops/internal/store"
)

// NotificationConfig 是渠道配置明文 JSON（cryptox 加密落库，契约 5.15
// 写死的四 kind schema）。校验失败返回 ErrInvalid（HTTP 层映射 400
// VALIDATION_FAILED / NOTIFICATION_CONFIG_INVALID）。
type NotificationConfig struct {
	URL      string   `json:"url,omitempty"`       // webhook/feishu/dingtalk
	Secret   string   `json:"secret,omitempty"`    // webhook HMAC / 飞书 / 钉钉签名密钥
	SMTPHost string   `json:"smtp_host,omitempty"` // email
	SMTPPort int      `json:"smtp_port,omitempty"`
	Username string   `json:"username,omitempty"`
	Password string   `json:"password,omitempty"`
	From     string   `json:"from,omitempty"`
	To       []string `json:"to,omitempty"`
	UseTLS   bool     `json:"use_tls,omitempty"`
}

// maxConfigJSONBytes 是渠道配置明文 JSON 的大小硬上限（解析器输入限制）。
const maxConfigJSONBytes = 16384

// maxRecipients 是 email 收件人上限。
const maxRecipients = 50

// ErrConfigInvalid 表示渠道配置 schema 校验失败。
var ErrConfigInvalid = errors.New("monitor: invalid notification config")

// ParseNotificationConfig 解析并按 kind 校验配置。
func ParseNotificationConfig(kind store.NotificationKind, raw string) (NotificationConfig, error) {
	if len(raw) == 0 || len(raw) > maxConfigJSONBytes {
		return NotificationConfig{}, ErrConfigInvalid
	}
	var cfg NotificationConfig
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&cfg); err != nil {
		return NotificationConfig{}, ErrConfigInvalid
	}
	switch kind {
	case store.NotificationWebhook, store.NotificationFeishu, store.NotificationDingtalk:
		u, err := url.Parse(cfg.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return NotificationConfig{}, ErrConfigInvalid
		}
	case store.NotificationEmail:
		if cfg.SMTPHost == "" || cfg.From == "" || len(cfg.To) == 0 || len(cfg.To) > maxRecipients {
			return NotificationConfig{}, ErrConfigInvalid
		}
		if cfg.SMTPPort < 0 || cfg.SMTPPort > 65535 {
			return NotificationConfig{}, ErrConfigInvalid
		}
		if cfg.SMTPPort == 0 {
			cfg.SMTPPort = 465
		}
		for _, to := range cfg.To {
			if to == "" || !strings.Contains(to, "@") {
				return NotificationConfig{}, ErrConfigInvalid
			}
		}
	default:
		return NotificationConfig{}, ErrConfigInvalid
	}
	return cfg, nil
}

// ConfigHint 生成非敏感掩码提示（不含任何凭据明文）。
func ConfigHint(kind store.NotificationKind, cfg NotificationConfig) string {
	switch kind {
	case store.NotificationEmail:
		return fmt.Sprintf("%s:%d → %d 收件人", cfg.SMTPHost, cfg.SMTPPort, len(cfg.To))
	default:
		u, err := url.Parse(cfg.URL)
		if err != nil || u.Host == "" {
			return ""
		}
		hint := u.Host + "/***"
		if cfg.Secret != "" {
			hint += fmt.Sprintf("(signed,%d)", len(cfg.Secret))
		}
		return hint
	}
}

// notifyTask 是一次投递任务。
type notifyTask struct {
	Event      store.AlertEvent
	RuleName   string
	ServerName string
	ChannelID  string
}

// notifier 是投递器：有界队列 + 2 worker + 重试退避。
type notifier struct {
	cfg    NotifyConfig
	st     store.Store
	cipher *cryptox.Cipher
	log    *slog.Logger
	clock  func() time.Time
	audit  auditSink

	queue      chan notifyTask
	httpClient *http.Client

	wg       sync.WaitGroup
	stopOnce sync.Once
	stopCh   chan struct{}
	cancel   context.CancelFunc

	// delivered 去重表（event id → 已投递/已失败），有界：超过上限清空
	// 重建（同一 firing 只投一次的进程内第二道防线；持久语义由
	// last_notified_at 承担）。
	mu        sync.Mutex
	delivered map[int64]struct{}
}

const deliveredMax = 4096

func newNotifier(cfg NotifyConfig, st store.Store, cipher *cryptox.Cipher,
	log *slog.Logger, clock func() time.Time, sink auditSink) *notifier {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 256
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.Retries <= 0 {
		cfg.Retries = 3
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = 10 * time.Second
	}
	if cfg.SMTPTimeout <= 0 {
		cfg.SMTPTimeout = 15 * time.Second
	}
	return &notifier{
		cfg: cfg, st: st, cipher: cipher, log: log, clock: clock, audit: sink,
		queue:      make(chan notifyTask, cfg.QueueSize),
		httpClient: &http.Client{Timeout: cfg.HTTPTimeout},
		stopCh:     make(chan struct{}),
		delivered:  make(map[int64]struct{}, 64),
	}
}

// start 启动 worker 池（有界）。
func (n *notifier) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	n.cancel = cancel
	for i := 0; i < n.cfg.Workers; i++ {
		n.wg.Add(1)
		go n.worker(ctx)
	}
}

// enqueue 入队；队列满则丢弃并直接置 failed（契约 11 节 #5：通知允许丢，
// 阻塞评估不可接受）。
func (n *notifier) enqueue(t notifyTask) {
	n.mu.Lock()
	if _, dup := n.delivered[t.Event.ID]; dup {
		n.mu.Unlock()
		return // 同一 firing 只投一次
	}
	n.delivered[t.Event.ID] = struct{}{}
	if len(n.delivered) > deliveredMax {
		n.delivered = make(map[int64]struct{}, 64)
	}
	n.mu.Unlock()

	select {
	case n.queue <- t:
	default:
		_ = n.st.AlertEvents().SetNotifyStatus(context.Background(), t.Event.ID, store.NotifyFailed, n.clock())
		n.log.Warn("monitor_notify_queue_full_dropped", "event_id", t.Event.ID)
	}
}

// notifyBackoffs 是重试退避序列（契约：1s/5s/15s）。
var notifyBackoffs = []time.Duration{time.Second, 5 * time.Second, 15 * time.Second}

func (n *notifier) worker(ctx context.Context) {
	defer n.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.stopCh:
			return
		case t := <-n.queue:
			n.deliver(ctx, t)
		}
	}
}

// deliver 单任务投递：取渠道 → 解密配置 → 按 kind 发送 → 状态回写。
func (n *notifier) deliver(ctx context.Context, t notifyTask) {
	ch, err := n.st.NotificationChannels().Get(ctx, t.ChannelID)
	if err != nil {
		_ = n.st.AlertEvents().SetNotifyStatus(ctx, t.Event.ID, store.NotifyFailed, n.clock())
		return
	}
	cfg, err := n.decodeConfig(ch)
	if err != nil {
		_ = n.st.AlertEvents().SetNotifyStatus(ctx, t.Event.ID, store.NotifyFailed, n.clock())
		n.audit.Enqueue(ctx, alertNotifiedAudit(t.Event.ID, ch, false, "CONFIG_DECRYPT_FAILED"))
		return
	}

	var lastErr error
	for attempt := 0; attempt <= n.cfg.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				_ = n.st.AlertEvents().SetNotifyStatus(context.Background(), t.Event.ID, store.NotifyFailed, n.clock())
				return
			case <-time.After(notifyBackoffs[min(attempt-1, len(notifyBackoffs)-1)]):
			}
		}
		err := n.send(ctx, ch.Kind, cfg, t)
		if err == nil {
			_ = n.st.AlertEvents().SetNotifyStatus(ctx, t.Event.ID, store.NotifySent, n.clock())
			n.audit.Enqueue(ctx, alertNotifiedAudit(t.Event.ID, ch, true, ""))
			return
		}
		lastErr = err
	}
	_ = n.st.AlertEvents().SetNotifyStatus(ctx, t.Event.ID, store.NotifyFailed, n.clock())
	// *url.Error 会携带完整 webhook URL（内嵌 access_token 等同凭据），
	// 整串落日志即泄漏——保留 host 掩码 path（五期评审发现）。
	n.log.Warn("monitor_notify_failed_after_retries", "event_id", t.Event.ID,
		"kind", string(ch.Kind), "error", logx.RedactURLInError(lastErr.Error()))
	n.audit.Enqueue(ctx, alertNotifiedAudit(t.Event.ID, ch, false, "NOTIFY_FAILED"))
}

func (n *notifier) decodeConfig(ch store.NotificationChannel) (NotificationConfig, error) {
	if n.cipher == nil || ch.ConfigEncrypted == "" {
		return NotificationConfig{}, errors.New("monitor: cipher unavailable or empty config")
	}
	plain, err := n.cipher.Decrypt(ch.ConfigEncrypted)
	if err != nil {
		return NotificationConfig{}, err
	}
	return ParseNotificationConfig(ch.Kind, plain)
}

// send 按渠道类型发送一次（不重试）。
func (n *notifier) send(ctx context.Context, kind store.NotificationKind,
	cfg NotificationConfig, t notifyTask) error {
	switch kind {
	case store.NotificationWebhook:
		return n.sendWebhook(ctx, cfg, t)
	case store.NotificationFeishu:
		return n.sendFeishu(ctx, cfg, t)
	case store.NotificationDingtalk:
		return n.sendDingtalk(ctx, cfg, t)
	case store.NotificationEmail:
		return n.sendEmail(ctx, cfg, t)
	default:
		return fmt.Errorf("monitor: unknown channel kind %q", string(kind))
	}
}

// webhookBody 是 webhook 通用 payload（契约 5.15 写死）。
type webhookBody struct {
	Event    string  `json:"event"`
	Rule     string  `json:"rule"`
	Server   string  `json:"server"`
	Metric   string  `json:"metric"`
	Value    float64 `json:"value"`
	Severity string  `json:"severity"`
	FiredAt  string  `json:"fired_at"`
	Message  string  `json:"message"`
}

func (n *notifier) sendWebhook(ctx context.Context, cfg NotificationConfig, t notifyTask) error {
	body, err := json.Marshal(webhookBody{
		Event: "alert", Rule: t.RuleName, Server: t.ServerName,
		Metric: string(t.Event.Metric), Value: t.Event.Value,
		Severity: string(t.Event.Severity), FiredAt: t.Event.FiredAt.UTC().Format(time.RFC3339),
		Message: t.Event.Message,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Secret != "" {
		mac := hmac.New(sha256.New, []byte(cfg.Secret))
		mac.Write(body)
		req.Header.Set("X-OpsMind-Signature", hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := n.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = drainBody(resp); _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook status %d", resp.StatusCode)
	}
	return nil
}

// RenderWebhookBody 渲染 webhook payload（模板化测试入口）。
func RenderWebhookBody(t notifyTask) ([]byte, error) {
	return json.Marshal(webhookBody{
		Event: "alert", Rule: t.RuleName, Server: t.ServerName,
		Metric: string(t.Event.Metric), Value: t.Event.Value,
		Severity: string(t.Event.Severity), FiredAt: t.Event.FiredAt.UTC().Format(time.RFC3339),
		Message: t.Event.Message,
	})
}

// RenderFeishuText 渲染飞书 text 卡正文：[OpsMind][severity] rule server message。
func RenderFeishuText(t notifyTask) string {
	return fmt.Sprintf("[OpsMind][%s] %s %s %s", string(t.Event.Severity), t.RuleName, t.ServerName, t.Event.Message)
}

func (n *notifier) sendFeishu(ctx context.Context, cfg NotificationConfig, t notifyTask) error {
	payload := map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": RenderFeishuText(t)},
	}
	ts := n.clock().Unix()
	if cfg.Secret != "" {
		// 飞书自建应用签名：sign = base64(hmac_sha256(key=timestamp+"\n"+secret, ""))。
		mac := hmac.New(sha256.New, []byte(fmt.Sprintf("%d", ts)+"\n"+cfg.Secret))
		payload["timestamp"] = strconv.FormatInt(ts, 10)
		payload["sign"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))
	}
	return n.postJSON(ctx, cfg.URL, payload)
}

// RenderDingtalkText 渲染钉钉 markdown 正文。
func RenderDingtalkText(t notifyTask) string {
	return fmt.Sprintf("### [OpsMind][%s] %s\n\n- 服务器：**%s**\n- 指标：%s = %.1f\n- %s",
		string(t.Event.Severity), t.RuleName, t.ServerName,
		string(t.Event.Metric), t.Event.Value, t.Event.Message)
}

func (n *notifier) sendDingtalk(ctx context.Context, cfg NotificationConfig, t notifyTask) error {
	payload := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"title": fmt.Sprintf("[OpsMind][%s] %s", string(t.Event.Severity), t.RuleName),
			"text":  RenderDingtalkText(t),
		},
	}
	target := cfg.URL
	if cfg.Secret != "" {
		// 钉钉加签：sign = urlencode(base64(hmac_sha256(secret, ts+"\n"+secret)))。
		ts := n.clock().Unix()
		stringToSign := fmt.Sprintf("%d\n%s", ts, cfg.Secret)
		mac := hmac.New(sha256.New, []byte(cfg.Secret))
		mac.Write([]byte(stringToSign))
		sign := url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target = fmt.Sprintf("%s%stimestamp=%d&sign=%s", target, sep, ts, sign)
	}
	return n.postJSON(ctx, target, payload)
}

func (n *notifier) postJSON(ctx context.Context, target string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = drainBody(resp) }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("notify status %d", resp.StatusCode)
	}
	return nil
}

// RenderEmailSubject 渲染邮件主题：[OpsMind][severity] rule server。
func RenderEmailSubject(t notifyTask) string {
	return fmt.Sprintf("[OpsMind][%s] %s %s", string(t.Event.Severity), t.RuleName, t.ServerName)
}

// RenderEmailBody 渲染邮件正文（单封 text）。
func RenderEmailBody(t notifyTask) string {
	return fmt.Sprintf("告警：%s\n服务器：%s\n指标：%s = %.1f\n触发时间：%s\n%s\n",
		t.RuleName, t.ServerName, string(t.Event.Metric), t.Event.Value,
		t.Event.FiredAt.UTC().Format(time.RFC3339), t.Event.Message)
}

func (n *notifier) sendEmail(ctx context.Context, cfg NotificationConfig, t notifyTask) error {
	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)
	msg := buildRFC5322(cfg.From, cfg.To, RenderEmailSubject(t), RenderEmailBody(t))

	// SMTP 会话带超时（契约：SMTPTimeout=15s）。
	dialCtx, cancel := context.WithTimeout(ctx, n.cfg.SMTPTimeout)
	defer cancel()
	conn, err := dialSMTP(dialCtx, addr, cfg.UseTLS)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	c, err := smtp.NewClient(conn, cfg.SMTPHost)
	if err != nil {
		return err
	}
	defer func() { _ = c.Quit() }()

	if cfg.Username != "" {
		if ok, _ := c.Extension("AUTH"); ok {
			host, _, _ := net.SplitHostPort(addr)
			if host == "" {
				host = cfg.SMTPHost
			}
			if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, host)); err != nil {
				return err
			}
		}
	}
	if err := c.Mail(cfg.From); err != nil {
		return err
	}
	for _, to := range cfg.To {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return nil
}

// dialSMTP 拨号 SMTP：use_tls=true 走隐式 TLS（465），否则明文（587 由
// 服务器侧 STARTTLS 升级，此处不强制）。
func dialSMTP(ctx context.Context, addr string, useTLS bool) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if !useTLS {
		return conn, nil
	}
	host, _, _ := net.SplitHostPort(addr)
	tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

// buildRFC5322 组装单封 text 邮件（头+正文，行尾 CRLF）。
func buildRFC5322(from string, to []string, subject, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return b.String()
}

// stop 同步收尾：关队列、等 worker（≤5s 预算由调用方控制 ctx）。
func (n *notifier) stop(timeout time.Duration) {
	n.stopOnce.Do(func() {
		close(n.stopCh)
		if n.cancel != nil {
			n.cancel()
		}
	})
	done := make(chan struct{})
	go func() {
		n.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		n.log.Warn("monitor_notify_stop_timeout")
	}
}

func drainBody(resp *http.Response) error {
	if resp.Body == nil {
		return nil
	}
	buf := make([]byte, 4096)
	total := 0
	for {
		n, err := resp.Body.Read(buf)
		total += n
		if total > 1<<20 { // 读上限 1MB（终止条件）
			return nil
		}
		if err != nil {
			return nil
		}
	}
}
