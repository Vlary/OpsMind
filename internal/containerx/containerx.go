// Package containerx 是四期容器运行时适配层（契约 5.2C）。
//
// 零 internal 依赖：执行经调用方注入的 ExecFunc（agent/httpapi 把
// sshx/winrmx 适配进来，Windows 目标由调用方按 2.7 通道规则包装
// PowerShell），本包只见命令串与结果。docker 与 podman 的 --format json
// 输出互不兼容（docker=JSONL 逐行对象、podman=缩进 JSON 数组），命令
// 模板与解析按运行时分册。
//
// 失败关闭：任何非预期输出按错误处理，不猜测字段。
package containerx

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Runtime 与 store.ContainerRuntime 取值一致；本包不 import store。
type Runtime string

const (
	RuntimeNone       Runtime = "" // 未探测（仅作为入参哨兵，不作为返回值）
	RuntimeAbsent     Runtime = "none"
	RuntimeDocker     Runtime = "docker"
	RuntimePodman     Runtime = "podman"
	RuntimeContainerd Runtime = "containerd" // 仅探测展示，管理面不支持
)

// Action 是容器动作（封闭集，契约 2.9 约定 5：不提供 exec/run）。
type Action string

const (
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionRestart Action = "restart"
)

// ExecResult 是一次通道执行的结果（stdout+stderr 已由通道层合并）。
type ExecResult struct {
	ExitCode   int
	Combined   string
	Truncated  bool
	DurationMS int64
}

// ExecFunc 由调用方注入：执行一条命令，带超时；通道错误（拨号失败、
// 认证失败等）返回 error，命令本身 exit≠0 不算通道错误。
type ExecFunc func(ctx context.Context, command string, timeout time.Duration) (ExecResult, error)

// Container 是列表项（契约 5.2C）。
type Container struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Image   string   `json:"image"`
	State   string   `json:"state"`   // running|exited|paused|created|...
	Status  string   `json:"status"`  // 原样人类可读（如 "Up 2 hours (healthy)"）
	Ports   []string `json:"ports"`   // 原样发布串；无则 []
	Created string   `json:"created"` // RFC3339；无法可靠解析时为空串（podman 列表）
}

// Mount 是 inspect 摘要中的挂载项。
type Mount struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	RW          bool   `json:"rw"`
}

// ContainerDetail 是 inspect 摘要。
type ContainerDetail struct {
	Container
	ExitCode     int     `json:"exit_code"`
	PID          int     `json:"pid"`
	StartedAt    string  `json:"started_at"`    // RFC3339；未启动空串
	FinishedAt   string  `json:"finished_at"`   // RFC3339；未停止空串
	HealthStatus string  `json:"health_status"` // healthy|unhealthy|starting|...；无 Health 块空串
	Mounts       []Mount `json:"mounts"`
}

// Image 是本地镜像列表项。
type Image struct {
	Repo      string `json:"repo"`       // "<none>" 原样保留
	Tag       string `json:"tag"`        // "<none>" 原样保留
	SizeBytes int64  `json:"size_bytes"` // docker 人类可读串按 1000 进制解析；解析失败 0
	Created   string `json:"created"`    // RFC3339；无法解析时空串
}

var (
	ErrUnsupportedRuntime = errors.New("containerx: runtime has no management plane") // ''/none/containerd
	ErrContainerNotFound  = errors.New("containerx: container not found")             // inspect 空数组 / logs 指向不存在容器
	ErrInvalidContainerID = errors.New("containerx: invalid container id")            // 不匹配 ^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$
)

// CLIError 表示 CLI 命令本身失败（exit≠0 且非「容器不存在」语义）；
// 调用方映射 502 CONTAINER_CLI_FAILED（action 的 exit_code 透传进审计 detail）。
type CLIError struct {
	Result ExecResult
}

func (e *CLIError) Error() string {
	return fmt.Sprintf("containerx: cli command failed with exit code %d", e.Result.ExitCode)
}

// 超时与上限（契约 5.2C/7.2）。
const (
	detectTimeout = 15 * time.Second // 探测（调用方经 ctx 约束，这里双保险）
	cliTimeout    = 60 * time.Second // list/inspect/images/logs/action
	// maxParseBytes 是解析器输入硬上限：通道层已按 256KB 截断，这里防御
	// 绕过通道的直接调用（防 OOM 铁律：解析器必须有输入大小限制）。
	maxParseBytes = 256 << 10
	// maxItems 是列表/数组解析的条目上限（双保险：正常容器/镜像数远小于此）。
	maxItems = 10000
	// DefaultTail 是 logs 缺省行数；MaxTail 是上限（契约 7.2：tail 1..1000）。
	DefaultTail = 200
	MaxTail     = 1000
)

// containerIDRe 是容器 ID/名称白名单（契约 2.9 约定 2：CLI 命令为拼接构造，
// ID 不合法直接拒绝，不进入执行通道）。
var containerIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// ValidateContainerID 校验容器 ID/名称。
func ValidateContainerID(id string) error {
	if !containerIDRe.MatchString(id) {
		return ErrInvalidContainerID
	}
	return nil
}

// 探测命令模板（契约 5.2C，写死）。Linux 为单条登录 shell 命令原样发送；
// Windows 为 PowerShell 语句块——经 WinRM 时由 winrmx 的 PowerShell 端点
// 执行（调用方注入的 ExecFunc 直接透传），经 SSH 时由调用方包装外壳。
const detectLinux = `if command -v docker >/dev/null 2>&1 && docker version --format '{{.Server.Version}}' >/dev/null 2>&1; then echo docker; elif command -v podman >/dev/null 2>&1 && podman info >/dev/null 2>&1; then echo podman; elif command -v ctr >/dev/null 2>&1 && ctr version >/dev/null 2>&1; then echo containerd; else echo none; fi`

// 单行形态：SSH 通道对模板做 `powershell -Command <script>` 前缀拼接
// 且不加引号，多行未引用参数会被外层 shell 拆解/截断（评审发现）——
// 折叠为分号分隔的单行，语义等价（PowerShell 行尾即语句边界）。
const detectWindows = `$cr='none'; if (Get-Command docker -ErrorAction SilentlyContinue) { $null = docker version --format '{{.Server.Version}}' 2>$null; if ($LASTEXITCODE -eq 0) { $cr='docker' } }; if ($cr -eq 'none' -and (Get-Command podman -ErrorAction SilentlyContinue)) { $null = podman info 2>$null; if ($LASTEXITCODE -eq 0) { $cr='podman' } }; $cr`

// DetectCommand 返回探测命令（osTarget="linux"|"windows"；写死模板），
// 供 server_facts 等模板登记方引用同一常量。
func DetectCommand(osTarget string) string {
	if osTarget == "windows" {
		return detectWindows
	}
	return detectLinux
}

// Detect 探测运行时：输出取 trim 后最后一行映射 Runtime；探测命令整体
// 失败（通道错误）返回错误；输出无法映射时失败关闭为 none。
func Detect(ctx context.Context, exec ExecFunc, osTarget string) (Runtime, error) {
	if exec == nil {
		return RuntimeNone, errors.New("containerx: exec func is nil")
	}
	res, err := exec(ctx, DetectCommand(osTarget), detectTimeout)
	if err != nil {
		return RuntimeNone, err
	}
	if res.ExitCode != 0 {
		// 判据脚本的每个分支都 echo 一个值且整体 exit 0；非零退出码说明
		// 远端 shell 异常，失败关闭为 none（不猜测）。
		return RuntimeAbsent, nil
	}
	lines := strings.Split(strings.TrimSpace(res.Combined), "\n")
	last := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			last = s
			break
		}
	}
	switch Runtime(last) {
	case RuntimeDocker, RuntimePodman, RuntimeContainerd:
		return Runtime(last), nil
	default:
		// 空输出/未知值：不猜测。
		return RuntimeAbsent, nil
	}
}

// cli 是运行时 CLI 名。
func cli(rt Runtime) string { return string(rt) }

// requireManagement 校验运行时可管理（契约 2.9 约定 2：仅 docker/podman）。
func requireManagement(rt Runtime) error {
	if rt != RuntimeDocker && rt != RuntimePodman {
		return ErrUnsupportedRuntime
	}
	return nil
}

// runCLI 执行一条容器 CLI 命令并做通道/exit 码区分。
func runCLI(ctx context.Context, exec ExecFunc, command string) (ExecResult, error) {
	if exec == nil {
		return ExecResult{}, errors.New("containerx: exec func is nil")
	}
	res, err := exec(ctx, command, cliTimeout)
	if err != nil {
		return ExecResult{}, err
	}
	return res, nil
}

// notFoundSemantics 判定 exit≠0 的输出是否「容器/对象不存在」。
func notFoundSemantics(combined string) bool {
	s := strings.ToLower(combined)
	return strings.Contains(s, "no such container") || strings.Contains(s, "no such object")
}

// ListContainers 列出全部容器（含已停止）。docker 为 JSONL、podman 为
// JSON 数组（契约 5.2C 模板表）。
func ListContainers(ctx context.Context, rt Runtime, exec ExecFunc) ([]Container, error) {
	if err := requireManagement(rt); err != nil {
		return nil, err
	}
	res, err := runCLI(ctx, exec, cli(rt)+` ps -a --format json`)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, &CLIError{Result: res}
	}
	if err := checkParseSize(res.Combined); err != nil {
		return nil, err
	}
	if rt == RuntimeDocker {
		return parseDockerList(res.Combined)
	}
	return parsePodmanList(res.Combined)
}

// InspectContainer 查看单个容器详情摘要。
func InspectContainer(ctx context.Context, rt Runtime, containerID string, exec ExecFunc) (ContainerDetail, error) {
	if err := requireManagement(rt); err != nil {
		return ContainerDetail{}, err
	}
	if err := ValidateContainerID(containerID); err != nil {
		return ContainerDetail{}, err
	}
	res, err := runCLI(ctx, exec, cli(rt)+" inspect "+containerID)
	if err != nil {
		return ContainerDetail{}, err
	}
	if res.ExitCode != 0 {
		if notFoundSemantics(res.Combined) {
			return ContainerDetail{}, ErrContainerNotFound
		}
		return ContainerDetail{}, &CLIError{Result: res}
	}
	if err := checkParseSize(res.Combined); err != nil {
		return ContainerDetail{}, err
	}
	return parseInspect(res.Combined)
}

// ContainerAction 执行 start/stop/restart；exit_code 透传给调用方进审计。
func ContainerAction(ctx context.Context, rt Runtime, containerID string, action Action, exec ExecFunc) (ExecResult, error) {
	if err := requireManagement(rt); err != nil {
		return ExecResult{}, err
	}
	switch action {
	case ActionStart, ActionStop, ActionRestart:
	default:
		return ExecResult{}, fmt.Errorf("containerx: invalid action %q", string(action))
	}
	if err := ValidateContainerID(containerID); err != nil {
		return ExecResult{}, err
	}
	res, err := runCLI(ctx, exec, cli(rt)+" "+string(action)+" "+containerID)
	if err != nil {
		return ExecResult{}, err
	}
	if res.ExitCode != 0 {
		return res, &CLIError{Result: res}
	}
	return res, nil
}

// ContainerLogs 读取最近 N 行日志（tail 1..1000，0 视为缺省 200；调用方
// 已校验，这里兜底钳制）。输出上限 256KB 由 ExecFunc 通道层截断，
// Truncated 透传。
func ContainerLogs(ctx context.Context, rt Runtime, containerID string, tail int, exec ExecFunc) (output string, truncated bool, err error) {
	if err := requireManagement(rt); err != nil {
		return "", false, err
	}
	if err := ValidateContainerID(containerID); err != nil {
		return "", false, err
	}
	if tail <= 0 {
		tail = DefaultTail
	}
	if tail > MaxTail {
		tail = MaxTail
	}
	res, err := runCLI(ctx, exec, fmt.Sprintf("%s logs --tail %d %s", cli(rt), tail, containerID))
	if err != nil {
		return "", false, err
	}
	if res.ExitCode != 0 {
		if notFoundSemantics(res.Combined) {
			return "", false, ErrContainerNotFound
		}
		return "", false, &CLIError{Result: res}
	}
	return res.Combined, res.Truncated, nil
}

// ListImages 列出本地镜像。
func ListImages(ctx context.Context, rt Runtime, exec ExecFunc) ([]Image, error) {
	if err := requireManagement(rt); err != nil {
		return nil, err
	}
	res, err := runCLI(ctx, exec, cli(rt)+" image ls --format json")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, &CLIError{Result: res}
	}
	if err := checkParseSize(res.Combined); err != nil {
		return nil, err
	}
	if rt == RuntimeDocker {
		return parseDockerImages(res.Combined)
	}
	return parsePodmanImages(res.Combined)
}

// checkParseSize 强制解析器输入上限（防 OOM 铁律）。
func checkParseSize(s string) error {
	if len(s) > maxParseBytes {
		return fmt.Errorf("containerx: output exceeds %d bytes parse limit", maxParseBytes)
	}
	return nil
}

// ActionCommand 返回某运行时某动作的逐字节执行命令（契约 2.9 约定 3：
// 审批 command 固化为按运行时拼好的命令，decide 时逐字节执行）。
func ActionCommand(rt Runtime, containerID string, action Action) string {
	return cli(rt) + " " + string(action) + " " + containerID
}
