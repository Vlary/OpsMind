// 无依赖的假 guacd（guacamole 协议最小实现），供 Playwright 端到端测试
// 的 RDP 真链路使用（p2 sprint 3）。
//
// 协议行为（对齐 internal/rdpx/gateway.go 的握手序列与真实 guacd 1.5.0
// 实测，见 internal/httpapi/rdp_handlers_test.go 的 Go 版 stub）：
//   1. 收 select rdp → 回 args（参数名列表，首参协议版本）；
//   2. 收 size / audio / video / image（各一条）；
//   3. 收 connect（含网关注入的 hostname/password 等参数）→ 回
//      ready（连接 ID）——网关把它原样透传给浏览器；
//   4. 发 sync 指令：guacamole-common-js 的 Client 在 WAITING 态收到
//      sync 即转 CONNECTED（dist 源码 sync handler），页面出现「已连接」；
//   5. 之后丢弃客户端上行指令（sync 回应 / nop keep-alive）。
//
// 用法：node fake-guacd.mjs [port]   （默认 18082）

import net from 'node:net';
import { randomUUID } from 'node:crypto';

const PORT = Number(process.argv[2] ?? process.env.FAKE_GUACD_PORT ?? 18082);

const ARGS_REPLY =
  '4.args,13.VERSION_1_5_0,8.hostname,4.port,8.username,8.password,8.security,' +
  '11.ignore-cert,5.width,6.height,3.dpi,14.recording-name;';

/** 指令编码："<len>.<e0>,<len>.<e1>,...;"（长度前缀定界，无需转义）。 */
function encode(elements) {
  return `${elements.map((e) => `${e.length}.${e}`).join(',')};`;
}

/**
 * 从缓冲区解析一条完整指令；不足一条返回 null。
 * 返回 [元素数组, 消费的字节数]。
 */
function decodeInstruction(buffer) {
  let i = 0;
  const elements = [];
  for (;;) {
    const dot = buffer.indexOf(46, i); // '.'
    if (dot < 0) {
      return null;
    }
    const lenStr = buffer.toString('latin1', i, dot);
    if (!/^\d+$/.test(lenStr)) {
      throw new Error(`bad length prefix: ${JSON.stringify(lenStr)}`);
    }
    const len = Number(lenStr);
    const start = dot + 1;
    const end = start + len;
    if (buffer.length < end + 1) {
      return null; // 元素或尾分隔符未到齐
    }
    elements.push(buffer.toString('latin1', start, end));
    const sep = buffer[end];
    i = end + 1;
    if (sep === 59) {
      // ';'
      return [elements, i];
    }
    if (sep !== 44) {
      // ','
      throw new Error(`bad separator: ${String.fromCharCode(sep)}`);
    }
  }
}

function handleConnection(socket) {
  let buffer = Buffer.alloc(0);
  let connected = false;

  const readInstruction = () => {
    const decoded = decodeInstruction(buffer);
    if (decoded === null) {
      return null;
    }
    buffer = buffer.subarray(decoded[1]);
    return decoded[0];
  };

  socket.on('data', (chunk) => {
    buffer = Buffer.concat([buffer, chunk]);
    try {
      for (;;) {
        const instruction = readInstruction();
        if (instruction === null) {
          return;
        }
        const [opcode] = instruction;
        if (opcode === 'select') {
          socket.write(ARGS_REPLY);
        } else if (opcode === 'connect' && !connected) {
          connected = true;
          // ready（连接 ID）+ sync：浏览器端 guacamole Client 转 CONNECTED。
          const uuid = randomUUID();
          socket.write(encode(['ready', uuid]));
          socket.write(encode(['sync', '0']));
        }
        // size/audio/video/image 与上行 sync/nop：无需响应。
      }
    } catch (error) {
      console.error(`[fake-guacd] protocol error: ${error.message}`);
      socket.destroy();
    }
  });
  socket.on('error', () => socket.destroy());
}

const server = net.createServer(handleConnection);
server.listen(PORT, '127.0.0.1', () => {
  console.log(`[fake-guacd] listening on 127.0.0.1:${PORT}`);
});
