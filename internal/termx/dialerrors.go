// W3：PTY 拨号失败的错误分类（契约 6.4 错误帧 message）。
//
// 走查 6：Attach 拨号失败时错误帧 message 曾写死英文技术串
// "pty connection failed"，用户无法区分密码错 / 不可达 / 超时。
// DescribeDialError 按底层错误特征分类为中文短语并保留原始原因，
// 供 termx 内部（Attach 拨号失败）与 httpapi（升级后同步失败路径）
// 共用；ClassifyDialFailure 供 httpapi 服务器连接测试（SSH/WinRM，
// p2 sprint 3）复用同一分类特征；不引入对 sshx 错误类型的依赖，仅按
// 字符串特征匹配（x/crypto/ssh 与 net 的错误形态稳定）。
package termx

import (
	"errors"
	"strings"
)

// DialFailureKind 是按底层错误特征归纳的连接失败类别。
type DialFailureKind int

const (
	// DialUnknown 未识别形态（保留原始错误展示）。
	DialUnknown DialFailureKind = iota
	// DialAuthFailed 认证被拒（密码/私钥不匹配）。
	DialAuthFailed
	// DialHostKeyRejected SSH 主机密钥与已记录指纹不一致。
	DialHostKeyRejected
	// DialRefused 连接被拒绝（端口未开放/防火墙拦截）。
	DialRefused
	// DialNoRoute 无到目标主机的路由 / 网络不可达。
	DialNoRoute
	// DialDNSFailed 域名解析失败。
	DialDNSFailed
	// DialTimedOut 时限内未完成握手。
	DialTimedOut
)

// ClassifyDialFailure 按错误文本特征分类连接失败原因（大小写不敏感；
// net 错误 "i/o timeout" / ssh 错误 "unable to authenticate" 均为小写，
// 防御性统一转小写）。错误信息只用于分类，不含凭据明文。
func ClassifyDialFailure(err error) DialFailureKind {
	if err == nil {
		return DialUnknown
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unable to authenticate"),
		strings.Contains(msg, "authentication failed"),
		strings.Contains(msg, "no supported methods remain"):
		return DialAuthFailed
	case strings.Contains(msg, "key mismatch"),
		strings.Contains(msg, "knownhosts"):
		return DialHostKeyRejected
	case strings.Contains(msg, "connection refused"):
		return DialRefused
	case strings.Contains(msg, "no route to host"),
		strings.Contains(msg, "unreachable"):
		return DialNoRoute
	case strings.Contains(msg, "no such host"),
		strings.Contains(msg, "lookup "):
		return DialDNSFailed
	case strings.Contains(msg, "timeout"),
		strings.Contains(msg, "deadline exceeded"):
		return DialTimedOut
	}
	return DialUnknown
}

// DescribeDialError 返回面向用户的 PTY 连接失败描述（中文短语 + 底层原因）。
// 错误信息只包含地址、认证方法名等排障要素，不含凭据明文。
func DescribeDialError(err error) string {
	if err == nil {
		return "PTY 连接建立失败"
	}
	if errors.Is(err, ErrShuttingDown) {
		return "终端服务正在关闭，请稍后重试"
	}
	if errors.Is(err, ErrNoPTYSupport) {
		return "终端服务不可用：SSH 执行器缺少 PTY 支持"
	}
	phrase := "PTY 连接建立失败"
	switch ClassifyDialFailure(err) {
	case DialAuthFailed:
		phrase = "SSH 认证失败（用户名或密码/私钥不正确）"
	case DialHostKeyRejected:
		phrase = "SSH 主机密钥校验失败（服务器指纹与已记录的不一致）"
	case DialRefused:
		phrase = "网络不可达（连接被拒绝，目标端口未开放或被防火墙拦截）"
	case DialNoRoute:
		phrase = "网络不可达（无到目标主机的路由）"
	case DialDNSFailed:
		phrase = "域名解析失败"
	case DialTimedOut:
		phrase = "连接超时（目标未在时限内完成 SSH 握手）"
	}
	return phrase + "：" + err.Error()
}
