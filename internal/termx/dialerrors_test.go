package termx

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// 走查 6（二期冲刺 1）：PTY 拨号失败的错误帧 message 必须是分类后的
// 中文短语 + 底层原因，不再写死英文技术串 "pty connection failed"。
// 错误形态对齐 sshx.Pool.dial 的包装（"sshx: dial addr: %w" /
// "sshx: handshake: %w"）与 x/crypto/ssh、net 的原始错误。
func TestDescribeDialError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		// wantPrefix：返回值的开头短语。
		wantPrefix string
		// wantCause：非空时返回值必须包含该底层原因（哨兵错误不拼原因）。
		wantCause string
	}{
		{
			name: "auth failure publickey",
			err: fmt.Errorf("sshx: handshake: ssh: handshake failed: %w",
				errors.New("ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain")),
			wantPrefix: "SSH 认证失败（用户名或密码/私钥不正确）：",
			wantCause:  "ssh: unable to authenticate",
		},
		{
			name:       "auth failure password rejected",
			err:        errors.New("sshx: handshake: ssh: handshake failed: ssh: unable to authenticate, attempted methods [none password], no supported methods remain"),
			wantPrefix: "SSH 认证失败（用户名或密码/私钥不正确）：",
			wantCause:  "no supported methods remain",
		},
		{
			name:       "host key mismatch",
			err:        errors.New("sshx: handshake: ssh: handshake failed: knownhosts: key mismatch"),
			wantPrefix: "SSH 主机密钥校验失败",
			wantCause:  "knownhosts: key mismatch",
		},
		{
			name:       "connection refused",
			err:        fmt.Errorf("sshx: dial 10.0.0.9:22: %w", errors.New("dial tcp 10.0.0.9:22: connect: connection refused")),
			wantPrefix: "网络不可达（连接被拒绝",
			wantCause:  "connection refused",
		},
		{
			name:       "no route",
			err:        errors.New("sshx: dial h:22: dial tcp: connect: no route to host"),
			wantPrefix: "网络不可达（无到目标主机的路由）：",
			wantCause:  "no route to host",
		},
		{
			name:       "io timeout",
			err:        errors.New("sshx: dial h:22: dial tcp 10.0.0.9:22: i/o timeout"),
			wantPrefix: "连接超时",
			wantCause:  "i/o timeout",
		},
		{
			name:       "handshake timeout",
			err:        errors.New("sshx: handshake: ssh: handshake failed: read tcp: i/o timeout"),
			wantPrefix: "连接超时",
			wantCause:  "i/o timeout",
		},
		{
			name:       "dns failure",
			err:        errors.New("sshx: dial h.example:22: dial tcp: lookup h.example: no such host"),
			wantPrefix: "域名解析失败：",
			wantCause:  "no such host",
		},
		{
			name:       "shutting down",
			err:        ErrShuttingDown,
			wantPrefix: "终端服务正在关闭，请稍后重试",
		},
		{
			name:       "no pty support",
			err:        ErrNoPTYSupport,
			wantPrefix: "终端服务不可用：SSH 执行器缺少 PTY 支持",
		},
		{
			name:       "unknown falls back with cause",
			err:        errors.New("sshx: handshake: ssh: connection dropped"),
			wantPrefix: "PTY 连接建立失败：sshx: handshake: ssh: connection dropped",
			wantCause:  "sshx: handshake: ssh: connection dropped",
		},
		{
			name:       "nil error",
			err:        nil,
			wantPrefix: "PTY 连接建立失败",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DescribeDialError(tc.err)
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Fatalf("DescribeDialError(%v)\n = %q\nwant prefix %q", tc.err, got, tc.wantPrefix)
			}
			if tc.wantCause != "" && !strings.Contains(got, tc.wantCause) {
				t.Fatalf("message %q 必须携带底层原因 %q", got, tc.wantCause)
			}
		})
	}
}
