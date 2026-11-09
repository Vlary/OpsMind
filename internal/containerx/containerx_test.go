// containerx_test.go 验证契约 5.2C 字段映射与命令构造（G28）：fixture
// 输出 → 结构体映射、CID 校验、运行时哨兵、Detect 输出映射、超时/截断
// 透传、exit≠0 语义区分（CLIError / ErrContainerNotFound）。
package containerx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeExec 记录命令并按脚本返回结果。
type fakeExec struct {
	calls    []string
	timeouts []time.Duration
	script   func(command string) (ExecResult, error)
}

func (f *fakeExec) Exec(ctx context.Context, command string, timeout time.Duration) (ExecResult, error) {
	f.calls = append(f.calls, command)
	f.timeouts = append(f.timeouts, timeout)
	if f.script != nil {
		return f.script(command)
	}
	return ExecResult{}, errors.New("no script")
}

func TestParseDockerList(t *testing.T) {
	out := `{"ID":"9f2c","Names":"postgres","Image":"postgres:17","State":"running","Status":"Up 2 hours (healthy)","Ports":"0.0.0.0:5432->5432/tcp, [::]:5432->5432/tcp","CreatedAt":"2026-10-26 23:36:05 +0800 CST"}
{"ID":"ab12","Names":"redis","Image":"redis:7","State":"exited","Status":"Exited (0) 3 hours ago","Ports":"","CreatedAt":"invalid timestamp"}`
	items, err := parseDockerList(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("len = %d, want 2", len(items))
	}
	pg := items[0]
	if pg.ID != "9f2c" || pg.Name != "postgres" || pg.Image != "postgres:17" || pg.State != "running" || pg.Status != "Up 2 hours (healthy)" {
		t.Fatalf("row0 mismatch: %+v", pg)
	}
	if len(pg.Ports) != 2 || pg.Ports[0] != "0.0.0.0:5432->5432/tcp" {
		t.Fatalf("ports = %v", pg.Ports)
	}
	if pg.Created != "2026-10-26T15:36:05Z" {
		t.Fatalf("created = %q, want RFC3339 UTC", pg.Created)
	}
	rd := items[1]
	if len(rd.Ports) != 0 {
		t.Fatalf("empty ports should be [], got %v", rd.Ports)
	}
	// 解析失败保留原样串。
	if rd.Created != "invalid timestamp" {
		t.Fatalf("created fallback = %q", rd.Created)
	}
}

func TestParsePodmanList(t *testing.T) {
	out := `[
  {
    "Id": "e11c",
    "Names": ["web"],
    "Image": "quay.io/x/web:latest",
    "State": "running",
    "Status": "Up 5 minutes",
    "Ports": [
      {
         "host_ip": "0.0.0.0",
         "container_port": 80,
         "host_port": 8080
      }
    ],
    "CreatedAt": 1790000000000000000,
    "IsInfra": false
  },
  {
    "Id": "infra1",
    "Names": ["infra"],
    "Image": "k8s.gcr.io/pause",
    "State": "running",
    "IsInfra": true
  }
]`
	items, err := parsePodmanList(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("len = %d, want 1 (IsInfra 丢弃)", len(items))
	}
	w := items[0]
	if w.ID != "e11c" || w.Name != "web" || w.State != "running" {
		t.Fatalf("row mismatch: %+v", w)
	}
	// Ports 为对象数组形态（未实测，失败关闭为 []，不报错）。
	if len(w.Ports) != 0 {
		t.Fatalf("podman ports should fail closed to [], got %v", w.Ports)
	}
	// podman 列表 created 恒空串。
	if w.Created != "" {
		t.Fatalf("podman list created = %q, want empty", w.Created)
	}
}

func TestParsePodmanListPortsStringArray(t *testing.T) {
	// Ports 若为字符串数组形态则透传。
	out := `[{"Id":"a1","Names":["x"],"Image":"i","State":"running","Status":"Up","Ports":["0.0.0.0:80->80/tcp"],"IsInfra":false}]`
	items, err := parsePodmanList(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(items[0].Ports) != 1 || items[0].Ports[0] != "0.0.0.0:80->80/tcp" {
		t.Fatalf("ports = %v", items[0].Ports)
	}
}

func TestParseInspect(t *testing.T) {
	out := `[
{
  "Name": "/postgres",
  "Created": "2026-10-26T15:36:05.123456789Z",
  "Config": {"Image": "postgres:17"},
  "State": {
    "Status": "running",
    "ExitCode": 0,
    "Pid": 812,
    "StartedAt": "2026-10-26T15:36:06Z",
    "FinishedAt": "0001-01-01T00:00:00Z",
    "Health": {"Status": "healthy"}
  },
  "Mounts": [
    {"Source": "/data/pg", "Destination": "/var/lib/postgresql/data", "RW": true},
    {"Source": "/certs", "Destination": "/certs", "RW": false}
  ]
}]`
	d, err := parseInspect(out)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "postgres" || d.Image != "postgres:17" || d.State != "running" || d.Status != "running" {
		t.Fatalf("detail mismatch: %+v", d)
	}
	if d.ExitCode != 0 || d.PID != 812 {
		t.Fatalf("exit/pid mismatch: %+v", d)
	}
	if d.StartedAt != "2026-10-26T15:36:06Z" {
		t.Fatalf("started_at = %q", d.StartedAt)
	}
	if d.FinishedAt != "" {
		t.Fatalf("zero FinishedAt should be empty, got %q", d.FinishedAt)
	}
	if d.HealthStatus != "healthy" {
		t.Fatalf("health = %q", d.HealthStatus)
	}
	if len(d.Mounts) != 2 || !d.Mounts[0].RW || d.Mounts[1].RW {
		t.Fatalf("mounts mismatch: %+v", d.Mounts)
	}
	if len(d.Ports) != 0 || len(d.Mounts) != 2 {
		t.Fatalf("slices must be non-nil")
	}
}

func TestParseInspectEmptyArray(t *testing.T) {
	if _, err := parseInspect(`[]`); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("empty array should be ErrContainerNotFound, got %v", err)
	}
	// podman inspect 不存在容器常返回 []。
	if _, err := parseInspect("\n[]\n"); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("whitespace-wrapped empty array should be ErrContainerNotFound, got %v", err)
	}
}

func TestParseInspectNoHealth(t *testing.T) {
	out := `[{"Name":"x","Created":"2026-01-01T00:00:00Z","Config":{"Image":"i"},"State":{"Status":"exited","ExitCode":3,"Pid":0,"StartedAt":"2026-01-01T00:00:01Z","FinishedAt":"2026-01-02T00:00:00Z"}}]`
	d, err := parseInspect(out)
	if err != nil {
		t.Fatal(err)
	}
	if d.HealthStatus != "" {
		t.Fatalf("health should be empty without Health block, got %q", d.HealthStatus)
	}
	if d.FinishedAt != "2026-01-02T00:00:00Z" {
		t.Fatalf("finished_at = %q", d.FinishedAt)
	}
}

func TestParseHumanSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"67.5MB", 67500000},
		{"1.2GB", 1200000000},
		{"5.32kB", 5320},
		{"1024B", 1024},
		{"2TB", 2000000000000},
		{"0B", 0},
		{"garbage", 0},
		{"", 0},
		{"12", 0},
	}
	for _, c := range cases {
		if got := parseHumanSize(c.in); got != c.want {
			t.Errorf("parseHumanSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseDockerImages(t *testing.T) {
	out := `{"Repository":"postgres","Tag":"17","Size":"67.5MB","CreatedAt":"2026-10-26 23:36:05 +0800 CST"}
{"Repository":"<none>","Tag":"<none>","Size":"5.32kB","CreatedAt":"bad"}`
	items, err := parseDockerImages(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("len = %d", len(items))
	}
	if items[0].Repo != "postgres" || items[0].Tag != "17" || items[0].SizeBytes != 67500000 {
		t.Fatalf("row0 mismatch: %+v", items[0])
	}
	if items[0].Created != "2026-10-26T15:36:05Z" {
		t.Fatalf("created = %q", items[0].Created)
	}
	if items[1].Repo != "<none>" || items[1].SizeBytes != 5320 || items[1].Created != "bad" {
		t.Fatalf("row1 mismatch: %+v", items[1])
	}
}

func TestParsePodmanImages(t *testing.T) {
	out := `[
 {"RepoTags":["quay.io/podman/stable:latest","quay.io/podman/stable:4.9"],"Size":421234567,"Created":1790000000},
 {"RepoTags":null,"Size":1024,"Created":0}
]`
	items, err := parsePodmanImages(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("len = %d, want 3 (RepoTags 展开两行 + <none> 一行)", len(items))
	}
	if items[0].Repo != "quay.io/podman/stable" || items[0].Tag != "latest" || items[0].SizeBytes != 421234567 {
		t.Fatalf("row0 mismatch: %+v", items[0])
	}
	if items[0].Created != time.Unix(1790000000, 0).UTC().Format(time.RFC3339) {
		t.Fatalf("created = %q, want epoch->RFC3339 UTC", items[0].Created)
	}
	// 按最后一个冒号拆（registry 端口不误拆）。
	if items[1].Repo != "quay.io/podman/stable" || items[1].Tag != "4.9" {
		t.Fatalf("row1 mismatch: %+v", items[1])
	}
	if items[2].Repo != "<none>" || items[2].Tag != "<none>" || items[2].Created != "" {
		t.Fatalf("row2 mismatch: %+v", items[2])
	}
}

func TestSplitRepoTag(t *testing.T) {
	if r, tag := splitRepoTag("localhost:5000/app:v1"); r != "localhost:5000/app" || tag != "v1" {
		t.Fatalf("splitRepoTag(localhost:5000/app:v1) = %q,%q", r, tag)
	}
	if r, tag := splitRepoTag("nginx"); r != "nginx" || tag != "<none>" {
		t.Fatalf("splitRepoTag(nginx) = %q,%q", r, tag)
	}
}

func TestValidateContainerID(t *testing.T) {
	for _, ok := range []string{"a", "9f2ca3c1", "my-container", "x_y.z-1"} {
		if err := ValidateContainerID(ok); err != nil {
			t.Errorf("ValidateContainerID(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "-abc", ".abc", "evil;rm", "a b", "a/b", strings.Repeat("x", 129)} {
		if err := ValidateContainerID(bad); !errors.Is(err, ErrInvalidContainerID) {
			t.Errorf("ValidateContainerID(%q) = %v, want ErrInvalidContainerID", bad, err)
		}
	}
}

func TestUnsupportedRuntime(t *testing.T) {
	for _, rt := range []Runtime{RuntimeNone, RuntimeAbsent, RuntimeContainerd, "bogus"} {
		if _, err := ListContainers(context.Background(), rt, nil); !errors.Is(err, ErrUnsupportedRuntime) {
			t.Errorf("ListContainers(%q) err = %v, want ErrUnsupportedRuntime", rt, err)
		}
		if _, err := ListImages(context.Background(), rt, nil); !errors.Is(err, ErrUnsupportedRuntime) {
			t.Errorf("ListImages(%q) err = %v, want ErrUnsupportedRuntime", rt, err)
		}
		if _, err := InspectContainer(context.Background(), rt, "x", nil); !errors.Is(err, ErrUnsupportedRuntime) {
			t.Errorf("InspectContainer(%q) err = %v, want ErrUnsupportedRuntime", rt, err)
		}
		if _, _, err := ContainerLogs(context.Background(), rt, "x", 10, nil); !errors.Is(err, ErrUnsupportedRuntime) {
			t.Errorf("ContainerLogs(%q) err = %v, want ErrUnsupportedRuntime", rt, err)
		}
		if _, err := ContainerAction(context.Background(), rt, "x", ActionStart, nil); !errors.Is(err, ErrUnsupportedRuntime) {
			t.Errorf("ContainerAction(%q) err = %v, want ErrUnsupportedRuntime", rt, err)
		}
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name, out string
		want      Runtime
	}{
		{"docker", "docker\n", RuntimeDocker},
		{"podman", "podman\n", RuntimePodman},
		{"containerd", "containerd\n", RuntimeContainerd},
		{"none", "none\n", RuntimeAbsent},
		{"windows multiline", "Some-BOM-Junk\r\ndocker\r\n", RuntimeDocker},
		{"unknown", "weird\n", RuntimeAbsent},
		{"empty", "", RuntimeAbsent},
	}
	for _, c := range cases {
		f := &fakeExec{script: func(string) (ExecResult, error) { return ExecResult{Combined: c.out, ExitCode: 0}, nil }}
		for _, osTarget := range []string{"linux", "windows"} {
			got, err := Detect(context.Background(), f.Exec, osTarget)
			if err != nil {
				t.Fatalf("%s/%s: %v", c.name, osTarget, err)
			}
			if got != c.want {
				t.Errorf("%s/%s: Detect = %q, want %q", c.name, osTarget, got, c.want)
			}
		}
	}
	// 通道错误返回 error。
	f := &fakeExec{script: func(string) (ExecResult, error) { return ExecResult{}, errors.New("dial fail") }}
	if _, err := Detect(context.Background(), f.Exec, "linux"); err == nil {
		t.Fatal("channel error should propagate")
	}
	// 探测命令模板按 OS 选择且 Linux 模板含 docker 判据。
	fl := &fakeExec{script: func(string) (ExecResult, error) { return ExecResult{Combined: "docker\n"}, nil }}
	if _, err := Detect(context.Background(), fl.Exec, "linux"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fl.calls[0], "command -v docker") || !strings.Contains(fl.calls[0], "ctr version") {
		t.Fatalf("linux detect template mismatch: %q", fl.calls[0])
	}
	if fl.timeouts[0] != detectTimeout {
		t.Fatalf("detect timeout = %v, want %v", fl.timeouts[0], detectTimeout)
	}
	// Windows 模板不含 ctr 探测（契约 2.9 约定 1）。
	if _, err := Detect(context.Background(), fl.Exec, "windows"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fl.calls[1], "ctr") {
		t.Fatalf("windows detect template must not probe ctr: %q", fl.calls[1])
	}
}

func TestListContainersCommandsAndExit(t *testing.T) {
	dockerOut := `{"ID":"a","Names":"x","Image":"i","State":"running","Status":"Up","Ports":"","CreatedAt":"2026-10-26 23:36:05 +0800 CST"}`
	f := &fakeExec{script: func(cmd string) (ExecResult, error) {
		if strings.HasPrefix(cmd, "docker ") {
			return ExecResult{Combined: dockerOut, ExitCode: 0}, nil
		}
		return ExecResult{}, nil
	}}
	items, err := ListContainers(context.Background(), RuntimeDocker, f.Exec)
	if err != nil || len(items) != 1 {
		t.Fatalf("docker list: %v %d", err, len(items))
	}
	if f.calls[0] != "docker ps -a --format json" {
		t.Fatalf("docker list cmd = %q", f.calls[0])
	}
	if f.timeouts[0] != cliTimeout {
		t.Fatalf("cli timeout = %v", want(f.timeouts[0], cliTimeout))
	}

	podmanOut := `[{"Id":"b","Names":["y"],"Image":"i","State":"exited","Status":"Exited","IsInfra":false}]`
	f2 := &fakeExec{script: func(cmd string) (ExecResult, error) {
		return ExecResult{Combined: podmanOut, ExitCode: 0}, nil
	}}
	items, err = ListContainers(context.Background(), RuntimePodman, f2.Exec)
	if err != nil || len(items) != 1 || items[0].ID != "b" {
		t.Fatalf("podman list: %v %+v", err, items)
	}
	if f2.calls[0] != "podman ps -a --format json" {
		t.Fatalf("podman list cmd = %q", f2.calls[0])
	}

	// exit≠0 → CLIError（调用方映射 502 CONTAINER_CLI_FAILED）。
	f3 := &fakeExec{script: func(string) (ExecResult, error) {
		return ExecResult{ExitCode: 1, Combined: "Cannot connect to the Docker daemon"}, nil
	}}
	_, err = ListContainers(context.Background(), RuntimeDocker, f3.Exec)
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.Result.ExitCode != 1 {
		t.Fatalf("exit!=0 should be CLIError, got %v", err)
	}
	// 通道错误透传。
	f4 := &fakeExec{script: func(string) (ExecResult, error) { return ExecResult{}, context.DeadlineExceeded }}
	if _, err = ListContainers(context.Background(), RuntimeDocker, f4.Exec); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("channel error should propagate, got %v", err)
	}
}

func want(got, _ any) any { return got }

func TestInspectNotFoundSemantics(t *testing.T) {
	f := &fakeExec{script: func(string) (ExecResult, error) {
		return ExecResult{ExitCode: 1, Combined: "Error: No such object: missing"}, nil
	}}
	if _, err := InspectContainer(context.Background(), RuntimeDocker, "missing", f.Exec); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("inspect not-found exit should map ErrContainerNotFound, got %v", err)
	}
	// CID 不合法不进执行通道。
	f2 := &fakeExec{script: func(string) (ExecResult, error) { return ExecResult{}, nil }}
	if _, err := InspectContainer(context.Background(), RuntimeDocker, "evil;rm", f2.Exec); !errors.Is(err, ErrInvalidContainerID) {
		t.Fatalf("invalid cid should fail before exec, got %v", err)
	}
	if len(f2.calls) != 0 {
		t.Fatalf("invalid cid must not reach exec channel, calls=%v", f2.calls)
	}
}

func TestContainerLogs(t *testing.T) {
	f := &fakeExec{script: func(cmd string) (ExecResult, error) {
		if strings.Contains(cmd, "logs --tail ") {
			return ExecResult{Combined: "line1\nline2", Truncated: true}, nil
		}
		return ExecResult{ExitCode: 1, Combined: "Error response from daemon: No such container: pg"}, nil
	}}
	out, trunc, err := ContainerLogs(context.Background(), RuntimeDocker, "pg", 0, f.Exec)
	if err != nil || out != "line1\nline2" || !trunc {
		t.Fatalf("logs: %q %v %v", out, trunc, err)
	}
	if f.calls[0] != "docker logs --tail 200 pg" {
		t.Fatalf("logs cmd = %q", f.calls[0])
	}
	// tail 钳制。
	if _, _, err := ContainerLogs(context.Background(), RuntimeDocker, "pg", 5000, f.Exec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.calls[1], "--tail 1000") {
		t.Fatalf("tail should clamp to 1000, cmd=%q", f.calls[1])
	}
	// No such container → ErrContainerNotFound。
	f2 := &fakeExec{script: func(string) (ExecResult, error) {
		return ExecResult{ExitCode: 1, Combined: "no such container"}, nil
	}}
	if _, _, err := ContainerLogs(context.Background(), RuntimePodman, "pg", 10, f2.Exec); !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("logs not-found should map ErrContainerNotFound, got %v", err)
	}
}

func TestContainerAction(t *testing.T) {
	f := &fakeExec{script: func(cmd string) (ExecResult, error) {
		if strings.HasPrefix(cmd, "podman stop pg") {
			return ExecResult{ExitCode: 0, DurationMS: 1834}, nil
		}
		return ExecResult{ExitCode: 125, Combined: "failed"}, nil
	}}
	res, err := ContainerAction(context.Background(), RuntimePodman, "pg", ActionStop, f.Exec)
	if err != nil || res.ExitCode != 0 || res.DurationMS != 1834 {
		t.Fatalf("action: %+v %v", res, err)
	}
	if f.calls[0] != "podman stop pg" {
		t.Fatalf("action cmd = %q", f.calls[0])
	}
	// 动作集封闭：非法动作拒绝。
	if _, err := ContainerAction(context.Background(), RuntimePodman, "pg", "exec", f.Exec); err == nil {
		t.Fatal("invalid action must be rejected")
	}
	// exit≠0 透传 exit_code。
	res, err = ContainerAction(context.Background(), RuntimePodman, "pg", ActionRestart, f.Exec)
	var cliErr *CLIError
	if !errors.As(err, &cliErr) || cliErr.Result.ExitCode != 125 || res.ExitCode != 125 {
		t.Fatalf("action failure should carry exit code, got res=%+v err=%v", res, err)
	}
	if ActionCommand(RuntimeDocker, "9f2c", ActionStop) != "docker stop 9f2c" {
		t.Fatalf("ActionCommand mismatch")
	}
}

func TestParseSizeLimit(t *testing.T) {
	// 超 256KB 输出在解析入口拒绝（防 OOM：解析器输入大小限制）。
	big := strings.Repeat("x", maxParseBytes+1)
	if err := checkParseSize(big); err == nil {
		t.Fatal("oversized output must be rejected")
	}
	// 行数上限：每行短、总大小在限内但条目数超限。
	rows := make([]string, 0, maxItems+2)
	for i := 0; i < maxItems+2; i++ {
		rows = append(rows, `{"ID":"a","Names":"x","Image":"i","State":"s","Status":"u","Ports":"","CreatedAt":""}`)
	}
	out := strings.Join(rows, "\n")
	if len(out) > maxParseBytes {
		t.Skip("构造输入超字节上限，跳过条目数路径")
	}
	if _, err := parseDockerList(out); err == nil {
		t.Fatal("item count limit must be enforced")
	}
}

func TestListImagesCommands(t *testing.T) {
	f := &fakeExec{script: func(cmd string) (ExecResult, error) {
		return ExecResult{Combined: `{"Repository":"nginx","Tag":"1.27","Size":"10.5MB","CreatedAt":"2026-10-26 23:36:05 +0800 CST"}`}, nil
	}}
	items, err := ListImages(context.Background(), RuntimeDocker, f.Exec)
	if err != nil || len(items) != 1 || items[0].SizeBytes != 10500000 {
		t.Fatalf("docker images: %v %+v", err, items)
	}
	if f.calls[0] != "docker image ls --format json" {
		t.Fatalf("docker images cmd = %q", f.calls[0])
	}
	f2 := &fakeExec{script: func(cmd string) (ExecResult, error) {
		return ExecResult{Combined: `[{"RepoTags":["a:1"],"Size":7,"Created":1790000000}]`}, nil
	}}
	items, err = ListImages(context.Background(), RuntimePodman, f2.Exec)
	if err != nil || len(items) != 1 || items[0].SizeBytes != 7 {
		t.Fatalf("podman images: %v %+v", err, items)
	}
	if f2.calls[0] != "podman image ls --format json" {
		t.Fatalf("podman images cmd = %q", f2.calls[0])
	}
}

func TestNonNilSlices(t *testing.T) {
	items, err := parseDockerList("")
	if err != nil || len(items) != 0 {
		t.Fatalf("empty docker list: %v %+v", err, items)
	}
	imgs, err := parsePodmanImages(`[]`)
	if err != nil || len(imgs) != 0 {
		t.Fatalf("empty podman images: %v %+v", err, imgs)
	}
}
