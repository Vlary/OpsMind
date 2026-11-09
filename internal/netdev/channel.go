// channel.go 是 CLI 通道抽象（契约 5.2B）：SSH 通道由 sshx.Session（PTY）
// 提供，Telnet 通道由 internal/netdev/telnetx 提供；外部只见字节流。
package netdev

import (
	"context"
	"fmt"
	"time"

	"aiops/internal/netdev/telnetx"
	"aiops/internal/sshx"
	"aiops/internal/store"
)

// Channel 是 CLI 通道抽象（契约 5.2B）。
type Channel interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
}

// openChannel 打开一次性 CLI 通道：telnet 裸拨；ssh 经 sshx.Session 请求
// PTY（主机密钥校验/TOFU 完全复用 sshx，5.2）。返回的通道由调用方 Close。
func (p *Pool) openChannel(ctx context.Context, t Target) (Channel, error) {
	if t.AccessProto == store.AccessProtoTelnet {
		conn, err := telnetx.Dial(ctx, t.Host, t.Port, telnetx.Config{DialTimeout: p.cfg.DialTimeout})
		if err != nil {
			return nil, fmt.Errorf("netdev: telnet dial: %w", err)
		}
		return conn, nil
	}

	// SSH 通道：网络设备仅密码认证（契约 2.6：密钥认证 v1 不支持，失败关闭）。
	opener, ok := p.ssh.(sshx.PTYOpener)
	if !ok {
		return nil, fmt.Errorf("netdev: ssh executor has no PTY support")
	}
	sess, err := opener.Session(ctx, sshTarget(t), sshx.SessionRequest{
		Cols: defaultPTYCols, Rows: defaultPTYRows, Term: "xterm-256color",
	})
	if err != nil {
		return nil, fmt.Errorf("netdev: ssh pty session: %w", err)
	}
	return sess, nil
}

// sshTarget 把 netdev.Target 映射为 sshx.Target（契约 5.2B：映射在包内）。
func sshTarget(t Target) sshx.Target {
	return sshx.Target{
		ServerID:     t.ServerID,
		Host:         t.Host,
		Port:         t.Port,
		Username:     t.Username,
		AuthMethod:   store.AuthMethodPassword,
		Password:     t.Password,
		KnownHostKey: t.KnownHostKey,
	}
}

// expectSession 的 PTY 窗口（网络设备输出按行协议，窗口尺寸不影响语义）。
const (
	defaultPTYCols = 120
	defaultPTYRows = 40
	// closeGrace 是收尾（exit 行）与放弃 enable 二级提示等待的宽限。
	closeGrace = 2 * time.Second
)
