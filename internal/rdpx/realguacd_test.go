// W4：可选的真 guacd 集成验证（非 CI 门禁；契约 9.2 G21 用 fake guacd）。
//
// 运行：AIOPS_REAL_GUACD=127.0.0.1:4822 go test ./internal/rdpx/ -run RealGuacd -count=1 -v
// 用 docker 拉起 guacamole/guacd:1.5.0 后验证 Gateway 握手与真服务的兼容性。
package rdpx

import (
	"os"
	"testing"
)

func TestRealGuacdHandshake(t *testing.T) {
	addr := os.Getenv("AIOPS_REAL_GUACD")
	if addr == "" {
		t.Skip("AIOPS_REAL_GUACD not set; real guacd integration skipped")
	}
	f := newTestFixture(t, Config{Addr: addr}, nil)
	session := f.openDefault(t)

	ws := newFakeWSConn()
	defer ws.Close()
	if err := f.gateway.Attach(ws, session.ID, "user-1", "req-real", "127.0.0.1"); err != nil {
		t.Fatalf("attach to real guacd: %v", err)
	}

	// 真 guacd 在 connect 后返回 ready（连接 ID），应原样透传到 WS。
	// 握手（select/args/size/audio/video/image/connect）与真服务完全兼容。
	ws.clientExpect(t, "ready,")

	// 实测补充（guacamole/guacd:1.5.0）：
	//   - ready 后 guacd 不主动发 nop——契约 6.5"心跳由 guacd 自身发出"
	//     的描述与实测不符；nop/sync 心跳实际由客户端驱动，透传架构
	//     不受影响（谁发的字节都原样转发）。
	//   - RDP 目标不可达时 error 指令在 FreeRDP 连接超时后才出现
	//     （>7s），不作为断言（与网关无关）。
}
