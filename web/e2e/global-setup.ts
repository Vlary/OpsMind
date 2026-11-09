// Playwright globalSetup：为端到端测试准备完整、隔离的环境。
//
// 步骤（每次运行全量重建，保证幂等）：
//  1. 确认 127.0.0.1:65432 PostgreSQL 可用（不可用先 docker compose up -d --wait db）；
//  2. 重建全新的 aiops_e2e 数据库（后端启动时 goose 自动迁移建表）；
//  3. go build 后端二进制到 /tmp/aiops-e2e，构建前端产物 web/dist；
//  4. 启动假 LLM（e2e/fixtures/fake-llm.mjs，端口 18086）、假 guacd
//     （e2e/fixtures/fake-guacd.mjs，端口 18082，p2 sprint 3 RDP 真链路）
//     与假 sshd（cmd/e2esshd，SSH 18083 + 控制 HTTP 18084，p2 sprint 5
//     终端真链路）；
//  5. 以 aiops_e2e 库 + 临时 DATA_DIR + 固定 ADMIN_PASSWORD + 假 guacd 地址
//     启动后端（127.0.0.1:18080）；
//  6. vite preview（独立配置 e2e/preview.config.mjs，代理 /api 到后端）提供页面（127.0.0.1:4173）；
//  7. 通过 API 创建指向假 LLM 的 provider 与一台假服务器，并把登录会话写入
//     e2e/.auth/user.json 供测试用 storageState 复用（控制登录次数，避开限流）。
//
// teardown：反向清理全部进程、临时目录与数据库。

import { spawn } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync, mkdirSync } from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const webDir = path.resolve(here, '..');
const repoRoot = path.resolve(webDir, '..');

const DB_PORT = 65432;
const BACKEND_PORT = 18080;
// 假 LLM 端口固定 18086：不得与冒烟模块的常驻 aiops-server（固定
// AIOPS_ADDR=:18081，门禁序列中先于 e2e 启动且 disown 不清理）共用
// 18081——端口预检会因占用而中止 e2e（三期门禁实测）。两者语义不同
// （假 LLM vs 真实服务器），端口隔离后互不干扰。
const FAKE_LLM_PORT = 18086;
const FAKE_GUACD_PORT = 18082;
const FAKE_SSH_PORT = 18083;
const FAKE_SSH_HTTP_PORT = 18084;
const PREVIEW_PORT = 4173;
const PREVIEW_ORIGIN = `http://localhost:${PREVIEW_PORT}`;
const BACKEND_URL = `http://127.0.0.1:${BACKEND_PORT}`;
const E2E_DATABASE = 'aiops_e2e';
const ADMIN_PASSWORD = 'e2e-admin-pass-123';
const BACKEND_BIN = '/tmp/aiops-e2e';
const FAKE_SSHD_BIN = '/tmp/aiops-e2e-fakesshd';
const AUTH_STATE_FILE = path.join(here, '.auth', 'user.json');

interface Child {
  proc: ReturnType<typeof spawn>;
  name: string;
  log: string[];
}

const children: Child[] = [];
let dataDir: string | null = null;

function run(cmd: string, args: string[], opts: { cwd: string; env?: NodeJS.ProcessEnv }) {
  return new Promise<void>((resolve, reject) => {
    const proc = spawn(cmd, args, { ...opts, stdio: ['ignore', 'pipe', 'pipe'] });
    let err = '';
    proc.stdout?.on('data', (chunk) =>
      process.stdout.write(`[setup:${path.basename(cmd)}] ${chunk}`),
    );
    proc.stderr?.on('data', (chunk) => {
      err += chunk;
      process.stderr.write(`[setup:${path.basename(cmd)}] ${chunk}`);
    });
    proc.on('error', reject);
    proc.on('exit', (code) => {
      if (code === 0) {
        resolve();
      } else {
        reject(new Error(`${cmd} ${args.join(' ')} exited with ${code}\n${err.slice(-4000)}`));
      }
    });
  });
}

function startBackground(
  name: string,
  cmd: string,
  args: string[],
  opts: { cwd: string; env?: NodeJS.ProcessEnv },
) {
  const proc = spawn(cmd, args, { ...opts, stdio: ['ignore', 'pipe', 'pipe'] });
  const child: Child = { proc, name, log: [] };
  children.push(child);
  const note = (stream: NodeJS.ReadableStream | null, tag: string) =>
    stream?.on('data', (chunk: Buffer) => {
      const text = chunk.toString();
      child.log.push(text);
      if (child.log.length > 200) {
        child.log.shift();
      }
      process.stdout.write(`[${name}:${tag}] ${text}`);
    });
  note(proc.stdout, 'out');
  note(proc.stderr, 'err');
  proc.on('exit', (code, signal) => {
    if (code !== null && code !== 0) {
      console.error(`[${name}] exited unexpectedly with code ${code}`);
    } else if (signal) {
      console.log(`[${name}] terminated by ${signal}`);
    }
  });
  return child;
}

async function stopAll() {
  for (const child of children.reverse()) {
    if (child.proc.exitCode === null && !child.proc.killed) {
      child.proc.kill('SIGTERM');
    }
  }
  await new Promise((resolve) => setTimeout(resolve, 800));
  for (const child of children) {
    if (child.proc.exitCode === null) {
      child.proc.kill('SIGKILL');
    }
  }
  children.length = 0;
}

function tcpProbe(port: number, timeoutMs = 1000): Promise<boolean> {
  return new Promise((resolve) => {
    const socket = net.connect({ host: '127.0.0.1', port, timeout: timeoutMs });
    const finish = (ok: boolean) => {
      socket.destroy();
      resolve(ok);
    };
    socket.on('connect', () => finish(true));
    socket.on('timeout', () => finish(false));
    socket.on('error', () => finish(false));
  });
}

async function waitFor(label: string, check: () => Promise<boolean>, timeoutMs: number) {
  const startedAt = Date.now();
  let lastError: unknown = null;
  while (Date.now() - startedAt < timeoutMs) {
    try {
      if (await check()) {
        return;
      }
    } catch (error) {
      lastError = error;
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  const tail = children
    .map((child) => `[${child.name}]\n${child.log.join('').slice(-2000)}`)
    .join('\n');
  throw new Error(
    `timeout waiting for ${label} after ${timeoutMs}ms; last error: ${String(lastError)}\n${tail}`,
  );
}

// waitForService 等待由本 setup 拉起的服务就绪：子进程在就绪前退出（如
// 端口被占 EADDRINUSE）时立即失败并携带日志尾巴，而不是傻等超时。
// 单纯的端口探测无法区分「自己人已就绪」与「端口被残留进程占用」。
async function waitForService(
  child: Child,
  label: string,
  check: () => Promise<boolean>,
  timeoutMs: number,
) {
  const startedAt = Date.now();
  let lastError: unknown = null;
  while (Date.now() - startedAt < timeoutMs) {
    if (child.proc.exitCode !== null) {
      throw new Error(
        `[${child.name}] exited with code ${child.proc.exitCode} before ${label} became ready; log tail:\n${child.log.join('').slice(-2000)}`,
      );
    }
    try {
      if (await check()) {
        return;
      }
    } catch (error) {
      lastError = error;
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  const tail = children.map((c) => `[${c.name}]\n${c.log.join('').slice(-1500)}`).join('\n');
  throw new Error(
    `timeout waiting for ${label} after ${timeoutMs}ms; last error: ${String(lastError)}\n${tail}`,
  );
}

// ensurePortsFree 预检 e2e 独占端口：被残留进程占用时 tcpProbe 会把占用者
// 误认成目标服务（门禁实测：另一工作流的冒烟 aiops-server 残留占 18081，
// 假 LLM EADDRINUSE 后 waitFor 仍「探通」端口，34 个聊天链路用例集体
// 失败且报错信息毫无指向）。预检让这种污染在 setup 阶段就以明确错误
// 暴露。65432（postgres）例外：docker compose 的 db 复用是合法状态。
async function ensurePortsFree() {
  for (const [name, port] of [
    ['fake-llm', FAKE_LLM_PORT],
    ['fake-guacd', FAKE_GUACD_PORT],
    ['fake-sshd', FAKE_SSH_PORT],
    ['fake-sshd-http', FAKE_SSH_HTTP_PORT],
    ['aiops-backend', BACKEND_PORT],
    ['vite-preview', PREVIEW_PORT],
  ] as const) {
    if (await tcpProbe(port)) {
      throw new Error(
        `setup aborted: 127.0.0.1:${port} (${name}) 已被其他进程占用，e2e 需要独占该端口；` +
          `请先停掉占用进程（如残留的冒烟 aiops-server / vite preview）后重跑。` +
          `此前的门禁大面积失败正是残留进程占用假 LLM 端口所致。`,
      );
    }
  }
}

async function ensurePostgres() {
  if (await tcpProbe(DB_PORT)) {
    console.log('[setup] postgres already reachable on 127.0.0.1:65432');
    return;
  }
  console.log('[setup] postgres not reachable, starting docker compose db …');
  await run('docker', ['compose', 'up', '-d', '--wait', 'db'], { cwd: repoRoot });
  await waitFor('postgres after docker compose up', () => tcpProbe(DB_PORT), 60_000);
}

async function recreateDatabase() {
  // 幂等：先踢掉残留连接再 DROP/CREATE。
  // 注意：psql 单个 -c 内的多语句会被包进事务，DROP DATABASE 不允许，
  // 因此拆成多个 -c（psql 对每个 -c 独立执行）。
  await run(
    'docker',
    [
      'compose',
      'exec',
      '-T',
      'db',
      'psql',
      '-U',
      'aiops',
      '-d',
      'aiops',
      '-v',
      'ON_ERROR_STOP=1',
      '-c',
      `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '${E2E_DATABASE}' AND pid <> pg_backend_pid()`,
      '-c',
      `DROP DATABASE IF EXISTS ${E2E_DATABASE}`,
      '-c',
      `CREATE DATABASE ${E2E_DATABASE}`,
    ],
    { cwd: repoRoot },
  );
  console.log(`[setup] database ${E2E_DATABASE} recreated`);
}

// buildWithRetry 对瞬时构建失败（text file busy / 构建缓存锁竞争等）
// 等待 1s 后重试一次；仍失败才抛出。
async function buildWithRetry(
  cmd: string,
  args: string[],
  opts: { cwd: string; env?: NodeJS.ProcessEnv },
) {
  try {
    await run(cmd, args, opts);
  } catch (error) {
    console.error(
      `[setup] ${cmd} build failed once, retrying in 1s: ${String(error).slice(0, 300)}`,
    );
    await new Promise((resolve) => setTimeout(resolve, 1000));
    await run(cmd, args, opts);
  }
}

async function buildAll() {
  // 重型命令限资源：go build 限 2 核；前端构建（tsc + vite build）限堆 2G。
  // go build 输出到固定路径 /tmp/aiops-e2e：上一轮 teardown 的 SIGKILL 与本轮
  // 写入之间偶发竞争，失败重试一次吸收。
  await Promise.all([
    buildWithRetry('go', ['build', '-o', BACKEND_BIN, './cmd/aiops'], {
      cwd: repoRoot,
      env: { ...process.env, GOMAXPROCS: '2' },
    }),
    buildWithRetry('go', ['build', '-o', FAKE_SSHD_BIN, './cmd/e2esshd'], {
      cwd: repoRoot,
      env: { ...process.env, GOMAXPROCS: '2' },
    }),
    run('pnpm', ['build'], {
      cwd: webDir,
      env: { ...process.env, NODE_OPTIONS: '--max-old-space-size=2048' },
    }),
  ]);
  console.log('[setup] backend binary, fake sshd and web dist built');
}

async function httpOk(url: string) {
  try {
    const response = await fetch(url);
    return response.ok;
  } catch {
    return false;
  }
}

// 带 Origin 的 API 请求（后端对 /api 写请求做 Origin 精确校验）。
async function api(method: string, path: string, body: unknown, cookie?: string) {
  const response = await fetch(`${BACKEND_URL}${path}`, {
    method,
    headers: {
      'Content-Type': 'application/json',
      Origin: PREVIEW_ORIGIN,
      ...(cookie ? { Cookie: cookie } : {}),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await response.text();
  if (!response.ok) {
    throw new Error(`${method} ${path} -> ${response.status}: ${text}`);
  }
  return { response, text, setCookie: response.headers.getSetCookie?.() ?? [] };
}

async function seed() {
  const login = await api('POST', '/api/auth/login', { password: ADMIN_PASSWORD });
  const session = login.setCookie
    .find((value) => value.startsWith('opsmind_session='))
    ?.split(';')[0];
  if (!session) {
    throw new Error(`login did not set opsmind_session cookie: ${login.setCookie.join(', ')}`);
  }
  // 把会话写进 storageState，供非登录类测试复用（后端登录限流 5 次/12s，
  // 各测试逐次页面登录会被限流）。
  const [, value] = session.split('=');
  mkdirSync(path.dirname(AUTH_STATE_FILE), { recursive: true });
  writeFileSync(
    AUTH_STATE_FILE,
    JSON.stringify(
      {
        cookies: [
          {
            name: 'opsmind_session',
            value,
            domain: 'localhost',
            path: '/',
            expires: -1,
            httpOnly: true,
            secure: false,
            sameSite: 'Lax',
          },
        ],
        origins: [],
      },
      null,
      2,
    ),
  );

  const provider = await api(
    'POST',
    '/api/providers',
    {
      name: 'e2e-fake-llm',
      preset: 'custom',
      kind: 'openai_compatible',
      base_url: `http://127.0.0.1:${FAKE_LLM_PORT}/v1`,
      api_key: 'e2e-fake-key',
      default_model: 'fake-model',
      models: ['fake-model'],
    },
    session,
  );
  const server = await api(
    'POST',
    '/api/servers',
    {
      name: 'e2e-fake-host',
      host: '127.0.0.1',
      port: 1,
      username: 'root',
      auth_method: 'password',
      password: 'not-a-real-password',
      tags: ['e2e'],
      auto_exec_enabled: false,
    },
    session,
  );
  const providerId = JSON.parse(provider.text || '{}').id;
  const serverId = JSON.parse(server.text || '{}').id;
  console.log(`[setup] seeded provider=${providerId} server=${serverId}`);
}

export default async function globalSetup() {
  dataDir = mkdtempSync('/tmp/aiops-e2e-data-');
  try {
    await ensurePortsFree();
    await ensurePostgres();
    await recreateDatabase();
    await buildAll();

    const fakeLlm = startBackground(
      'fake-llm',
      process.execPath,
      [path.join(here, 'fixtures', 'fake-llm.mjs'), String(FAKE_LLM_PORT)],
      {
        cwd: webDir,
      },
    );
    await waitForService(fakeLlm, 'fake-llm port', () => tcpProbe(FAKE_LLM_PORT), 15_000);

    // 假 guacd（p2 sprint 3）：RDP 真链路用例经 vite preview 的 ws:true
    // 代理 → 后端网关 → 本 stub 完成 guacamole 握手与 sync 指令下发。
    const fakeGuacd = startBackground(
      'fake-guacd',
      process.execPath,
      [path.join(here, 'fixtures', 'fake-guacd.mjs'), String(FAKE_GUACD_PORT)],
      {
        cwd: webDir,
      },
    );
    await waitForService(fakeGuacd, 'fake-guacd port', () => tcpProbe(FAKE_GUACD_PORT), 15_000);

    // 假 sshd（p2 sprint 5，cmd/e2esshd）：终端真链路用例的后端 PTY 对端。
    // SSH 端口供后端 sshx 拨号（pty-req + shell + window-change），HTTP
    // 端口供用例查询 PTY 尺寸记录（断言浏览器 → WS → 后端 → PTY 同步）。
    const fakeSshd = startBackground('fake-sshd', FAKE_SSHD_BIN, [
      `-ssh=127.0.0.1:${FAKE_SSH_PORT}`,
      `-http=127.0.0.1:${FAKE_SSH_HTTP_PORT}`,
    ]);
    await waitForService(fakeSshd, 'fake-sshd ssh port', () => tcpProbe(FAKE_SSH_PORT), 15_000);

    const backend = startBackground('aiops-backend', BACKEND_BIN, [], {
      cwd: repoRoot,
      env: {
        ...process.env,
        // 与浏览器、vite preview 同机运行：限制后端并行度，保证整套
        // 测试在低配环境可跑（e2e 负载极低，2 核足够）。
        GOMAXPROCS: '2',
        AIOPS_ADDR: `127.0.0.1:${BACKEND_PORT}`,
        AIOPS_DATABASE_URL: `postgres://aiops:aiops@127.0.0.1:${DB_PORT}/${E2E_DATABASE}?sslmode=disable`,
        AIOPS_DATA_DIR: dataDir,
        ADMIN_PASSWORD,
        // 审批 TTL 从默认 120s 收紧到 45s：「审批超时」E2E 用例无需等满 2 分钟；
        // 既有用例都在审批卡出现后数秒内决策，45s 窗口仍有约 10 倍余量。
        AIOPS_APPROVAL_TTL: '45s',
        AIOPS_ALLOWED_ORIGINS: `http://localhost:${PREVIEW_PORT},http://127.0.0.1:${PREVIEW_PORT}`,
        AIOPS_PUBLIC_ORIGIN: PREVIEW_ORIGIN,
        AIOPS_WEB_DIR: '',
        AIOPS_LOG_LEVEL: 'info',
        // RDP 网关指向假 guacd（空 = RDP 禁用；真链路用例需要它可达）。
        AIOPS_GUACD_ADDR: `127.0.0.1:${FAKE_GUACD_PORT}`,
      },
    });
    await waitForService(
      backend,
      'backend /healthz',
      () => httpOk(`${BACKEND_URL}/healthz`),
      60_000,
    );

    const preview = startBackground(
      'vite-preview',
      'pnpm',
      ['exec', 'vite', 'preview', '--config', path.join(here, 'preview.config.mjs')],
      {
        cwd: webDir,
      },
    );
    await waitForService(
      preview,
      'vite preview',
      () => httpOk(`http://127.0.0.1:${PREVIEW_PORT}/`),
      60_000,
    );

    await seed();
    console.log('[setup] e2e environment ready');
  } catch (error) {
    await stopAll();
    await cleanup();
    throw error;
  }

  return async () => {
    await stopAll();
    await cleanup();
  };
}

async function cleanup() {
  if (dataDir) {
    rmSync(dataDir, { recursive: true, force: true });
    dataDir = null;
  }
  try {
    await run(
      'docker',
      [
        'compose',
        'exec',
        '-T',
        'db',
        'psql',
        '-U',
        'aiops',
        '-d',
        'aiops',
        '-c',
        `DROP DATABASE IF EXISTS ${E2E_DATABASE}`,
      ],
      { cwd: repoRoot },
    );
    console.log(`[teardown] database ${E2E_DATABASE} dropped`);
  } catch (error) {
    console.error('[teardown] drop database failed:', error);
  }
}
