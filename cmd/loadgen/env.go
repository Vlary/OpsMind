package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	dbHostPort = "127.0.0.1:65432"
	dbAdminDSN = "postgres://aiops:aiops@127.0.0.1:65432/postgres?sslmode=disable"
	loadDBName = "aiops_load"
	loadDBDSN  = "postgres://aiops:aiops@127.0.0.1:65432/" + loadDBName + "?sslmode=disable"

	// hostMemPrecheckKB：启动前宿主机 MemAvailable 下限（3GiB）。
	// 本机 30GB 内存且需给 ZCode 与桌面环境留足余量，低于此值直接
	// 以明确错误退出，不启动数据库编排、构建或任何负载。
	hostMemPrecheckKB = 3 << 30 / 1024 // 3GiB，单位 kB
)

// testEnv 是一次负载运行的全部被测环境资源。
type testEnv struct {
	repoRoot  string
	workDir   string // /tmp/aiops-loadgen-<rand>/：服务端二进制、DATA_DIR、服务端日志
	dataDir   string
	serverBin string
	serverLog string

	serverCmd  *exec.Cmd
	serverPID  int
	serverAddr string // http://127.0.0.1:<port>

	mock *mockLLM

	adminPassword string
	providerID    string
	chatServerID  string
}

// findRepoRoot 从当前目录向上查找 module aiops 的 go.mod。
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("loadgen: go.mod not found upward from %s (run inside aiops repo)", dir)
		}
		dir = parent
	}
}

// ensureDatabaseHost 探测 65432，不可用时 docker compose up -d --wait db。
func ensureDatabaseHost(ctx context.Context, repoRoot string) error {
	if dialOK(dbHostPort) {
		return nil
	}
	fmt.Println("loadgen: 127.0.0.1:65432 unavailable, running `docker compose up -d --wait db`")
	cmd := exec.CommandContext(ctx, "docker", "compose", "--project-directory", repoRoot, "up", "-d", "--wait", "db")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose up: %w\n%s", err, string(out))
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if dialOK(dbHostPort) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("loadgen: database at %s still unreachable after docker compose up", dbHostPort)
}

func dialOK(hostPort string) bool {
	conn, err := net.DialTimeout("tcp", hostPort, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// precheckHostMemory 在任何编排动作之前检查宿主机 /proc/meminfo 的
// MemAvailable；不足 3GiB 时以明确错误退出，不产生任何负载。
func precheckHostMemory() error {
	memKB, err := readMemAvailableKB()
	if err != nil {
		return fmt.Errorf("read /proc/meminfo: %w", err)
	}
	if memKB < hostMemPrecheckKB {
		return fmt.Errorf(
			"host MemAvailable %dKiB below precheck floor %dKiB (3GiB); aborting before any load (keep headroom for ZCode and the desktop)",
			memKB, hostMemPrecheckKB)
	}
	fmt.Printf("loadgen: host MemAvailable=%.2fGiB (precheck floor 3GiB) OK\n", float64(memKB)/(1<<20))
	return nil
}

// recreateLoadDB 每次运行前 DROP+CREATE，保证全新 aiops_load 库。
func recreateLoadDB(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, dbAdminDSN)
	if err != nil {
		return fmt.Errorf("connect maintenance db: %w", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS "`+loadDBName+`" WITH (FORCE)`); err != nil {
		return fmt.Errorf("drop %s: %w", loadDBName, err)
	}
	if _, err := conn.Exec(ctx, `CREATE DATABASE "`+loadDBName+`"`); err != nil {
		return fmt.Errorf("create %s: %w", loadDBName, err)
	}
	return nil
}

// dropLoadDB 运行结束后清理测试库（尽力而为）。
func dropLoadDB() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dbAdminDSN)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	_, err = conn.Exec(ctx, `DROP DATABASE IF EXISTS "`+loadDBName+`" WITH (FORCE)`)
	return err
}

// buildServer go build 服务端二进制到 workDir。
func buildServer(ctx context.Context, repoRoot, outPath string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", outPath, "./cmd/aiops")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build ./cmd/aiops: %w\n%s", err, string(out))
	}
	return nil
}

// startServer 以 aiops_load 库启动被测服务端（AIOPS_PPROF=1）。
func startServer(workDir, dataDir, adminPassword string) (*exec.Cmd, string, error) {
	port, err := freePort()
	if err != nil {
		return nil, "", err
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)

	logFile, err := os.Create(filepath.Join(workDir, "server.log"))
	if err != nil {
		return nil, "", err
	}

	cmd := exec.Command(filepath.Join(workDir, "aiops-server"))
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(),
		"AIOPS_ADDR="+addr,
		"AIOPS_DATABASE_URL="+loadDBDSN,
		"AIOPS_DATA_DIR="+dataDir,
		"ADMIN_PASSWORD="+adminPassword,
		"AIOPS_PPROF=1",
		"GOMEMLIMIT=768MiB",
		"AIOPS_MAX_STREAMS=100",
		"AIOPS_WEB_DIR=",
		"AIOPS_LOG_LEVEL=info",
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, "", err
	}
	return cmd, "http://" + addr, nil
}

// waitHealthy 轮询 /healthz 直到 200。
func waitHealthy(ctx context.Context, baseURL string) error {
	hc := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := hc.Get(baseURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("server at %s not healthy within 30s", baseURL)
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// setupEnv 完整编排：meminfo 预检 → db → 库 → build → 服务端 → mock。
func setupEnv(ctx context.Context, mc modeConfig) (*testEnv, error) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		return nil, err
	}
	if err := precheckHostMemory(); err != nil {
		return nil, err
	}
	if err := ensureDatabaseHost(ctx, repoRoot); err != nil {
		return nil, err
	}
	if err := recreateLoadDB(ctx); err != nil {
		return nil, err
	}

	workDir, err := os.MkdirTemp("", "aiops-loadgen-*")
	if err != nil {
		return nil, err
	}
	randBytes := make([]byte, 8)
	_, _ = rand.Read(randBytes)
	env := &testEnv{
		repoRoot:      repoRoot,
		workDir:       workDir,
		dataDir:       filepath.Join(workDir, "data"),
		serverBin:     filepath.Join(workDir, "aiops-server"),
		serverLog:     filepath.Join(workDir, "server.log"),
		adminPassword: "loadgen-admin-" + hex.EncodeToString(randBytes),
	}
	if err := os.MkdirAll(env.dataDir, 0o755); err != nil {
		env.Close()
		return nil, err
	}

	buildCtx, cancelBuild := context.WithTimeout(ctx, 5*time.Minute)
	defer cancelBuild()
	if err := buildServer(buildCtx, repoRoot, env.serverBin); err != nil {
		env.Close()
		return nil, err
	}
	fmt.Println("loadgen: built server binary ->", env.serverBin)

	env.serverCmd, env.serverAddr, err = startServer(workDir, env.dataDir, env.adminPassword)
	if err != nil {
		env.Close()
		return nil, fmt.Errorf("start server: %w", err)
	}
	env.serverPID = env.serverCmd.Process.Pid

	if err := waitHealthy(ctx, env.serverAddr); err != nil {
		env.Close()
		return nil, err
	}

	env.mock, err = newMockLLM(mc.tokenDelay, mc.tokenCount)
	if err != nil {
		env.Close()
		return nil, err
	}
	fmt.Printf("loadgen: server pid=%d addr=%s mock_llm=%s\n", env.serverPID, env.serverAddr, env.mock.URL())
	return env, nil
}

// Close 逆序释放：服务端 → mock → 库 → 临时目录。
func (e *testEnv) Close() error {
	var errs []error
	if e.serverCmd != nil && e.serverCmd.Process != nil {
		_ = e.serverCmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = e.serverCmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = e.serverCmd.Process.Kill()
			<-done
		}
	}
	if e.mock != nil {
		e.mock.Close()
	}
	if err := dropLoadDB(); err != nil {
		errs = append(errs, fmt.Errorf("drop %s: %w", loadDBName, err))
	}
	if e.workDir != "" {
		if err := os.RemoveAll(e.workDir); err != nil {
			errs = append(errs, err)
		}
	}
	for _, err := range errs {
		fmt.Fprintf(os.Stderr, "LOADGEN_CLEANUP_WARNING: %v\n", err)
	}
	return nil
}
