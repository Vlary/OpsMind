// runner_test.go 覆盖契约 G34（巡检评分）/G35（cron 调度）与防 OOM 守卫：
// 评分表逐条 fixture 断言、解析器（df 剔除伪文件系统/systemctl 失败单元/
// journal 错误行）、raw 64KB 截断、目标 200 上限、均值聚合（0 台=100）、
// cron 边界与非法表达式、同 job 互斥、SAFE_INSPECT 模板命令分类、
// 内存分配预算。
package inspection

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"aiops/internal/cryptox"
	"aiops/internal/risk"
	"aiops/internal/sshx"
	"aiops/internal/store"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type fakeInspStore struct {
	store.Store

	mu       sync.Mutex
	servers  map[string]store.Server
	reports  map[string]store.InspectionReport
	items    map[string][]store.InspectionReportItem
	jobs     map[string]store.InspectionJob
	events   []store.AlertEvent
	nextItem int64
}

func newFakeInspStore() *fakeInspStore {
	return &fakeInspStore{
		servers: map[string]store.Server{},
		reports: map[string]store.InspectionReport{},
		items:   map[string][]store.InspectionReportItem{},
		jobs:    map[string]store.InspectionJob{},
	}
}

func (f *fakeInspStore) Servers() store.ServerStore { return (*fakeInspServers)(f) }

type fakeInspServers fakeInspStore

func (f *fakeInspServers) List(context.Context) ([]store.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.Server, 0, len(f.servers))
	for _, s := range f.servers {
		out = append(out, s)
	}
	return out, nil
}
func (f *fakeInspServers) Get(_ context.Context, id string) (store.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.servers[id]; ok {
		return s, nil
	}
	return store.Server{}, store.ErrNotFound
}
func (f *fakeInspServers) Create(context.Context, store.CreateServerParams) (store.Server, error) {
	return store.Server{}, errors.New("not implemented")
}
func (f *fakeInspServers) Update(context.Context, string, store.UpdateServerParams) (store.Server, error) {
	return store.Server{}, errors.New("not implemented")
}
func (f *fakeInspServers) Delete(context.Context, string) error { return errors.New("not implemented") }
func (f *fakeInspServers) UpdateHostKey(context.Context, string, string, string, time.Time) (store.Server, error) {
	return store.Server{}, errors.New("not implemented")
}
func (f *fakeInspServers) UpdateContainerRuntime(context.Context, string, store.ContainerRuntime, time.Time) (store.Server, error) {
	return store.Server{}, errors.New("not implemented")
}

func (f *fakeInspStore) AlertEvents() store.AlertEventStore { return (*fakeInspEvents)(f) }

type fakeInspEvents fakeInspStore

func (f *fakeInspEvents) Create(context.Context, store.AlertRule, string, float64, string, time.Time) (store.AlertEvent, error) {
	return store.AlertEvent{}, errors.New("not implemented")
}
func (f *fakeInspEvents) List(_ context.Context, fl store.AlertEventFilter) ([]store.AlertEvent, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.AlertEvent
	for _, ev := range f.events {
		if fl.Status != "" && ev.Status != fl.Status {
			continue
		}
		if fl.ServerID != "" && ev.ServerID != fl.ServerID {
			continue
		}
		out = append(out, ev)
		if fl.Limit > 0 && len(out) >= fl.Limit {
			break
		}
	}
	return out, false, nil
}
func (f *fakeInspEvents) Get(context.Context, int64) (store.AlertEvent, error) {
	return store.AlertEvent{}, store.ErrNotFound
}
func (f *fakeInspEvents) Resolve(context.Context, int64, time.Time) (store.AlertEvent, error) {
	return store.AlertEvent{}, errors.New("not implemented")
}
func (f *fakeInspEvents) SetNotifyStatus(context.Context, int64, store.NotifyStatus, time.Time) error {
	return nil
}
func (f *fakeInspEvents) CountFiring(context.Context) (int64, error) { return 0, nil }
func (f *fakeInspEvents) CountFiringByServer(context.Context, string) (int64, error) {
	return 0, nil
}

func (f *fakeInspStore) InspectionJobs() store.InspectionJobStore { return (*fakeInspJobs)(f) }

type fakeInspJobs fakeInspStore

func (f *fakeInspJobs) List(context.Context) ([]store.InspectionJob, error) { return nil, nil }
func (f *fakeInspJobs) Get(_ context.Context, id string) (store.InspectionJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if j, ok := f.jobs[id]; ok {
		return j, nil
	}
	return store.InspectionJob{}, store.ErrNotFound
}
func (f *fakeInspJobs) Create(context.Context, store.CreateInspectionJobParams) (store.InspectionJob, error) {
	return store.InspectionJob{}, errors.New("not implemented")
}
func (f *fakeInspJobs) Update(context.Context, string, store.UpdateInspectionJobParams) (store.InspectionJob, error) {
	return store.InspectionJob{}, errors.New("not implemented")
}
func (f *fakeInspJobs) Delete(context.Context, string) error                        { return nil }
func (f *fakeInspJobs) MarkRun(context.Context, string, time.Time, time.Time) error { return nil }
func (f *fakeInspJobs) ListDue(context.Context, time.Time) ([]store.InspectionJob, error) {
	return nil, nil
}

func (f *fakeInspStore) InspectionReports() store.InspectionReportStore { return (*fakeInspReports)(f) }

type fakeInspReports fakeInspStore

func (f *fakeInspReports) Create(_ context.Context, p store.CreateInspectionReportParams) (store.InspectionReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("rep-%d", len(f.reports))
	r := store.InspectionReport{
		ID: id, JobID: p.JobID, Trigger: p.Trigger, ServerCount: p.ServerCount,
		AvgScore: p.AvgScore, AnomalyCount: p.AnomalyCount,
		StartedAt: p.StartedAt, FinishedAt: p.FinishedAt, CreatedAt: time.Now().UTC(),
	}
	f.reports[id] = r
	return r, nil
}

func (f *fakeInspReports) Get(_ context.Context, id string) (store.InspectionReport, []store.InspectionReportItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.reports[id]
	if !ok {
		return store.InspectionReport{}, nil, store.ErrNotFound
	}
	return r, append([]store.InspectionReportItem(nil), f.items[id]...), nil
}

func (f *fakeInspReports) List(context.Context, store.InspectionReportFilter) ([]store.InspectionReport, error) {
	return nil, nil
}

func (f *fakeInspReports) CreateItem(_ context.Context, p store.CreateInspectionItemParams) (store.InspectionReportItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextItem++
	it := store.InspectionReportItem{
		ID: f.nextItem, ReportID: p.ReportID, ServerID: p.ServerID, Score: p.Score,
		AnomalyCount: p.AnomalyCount, Anomalies: p.Anomalies, RawOutput: p.RawOutput,
		Command: p.Command, Truncated: p.Truncated, DurationMS: p.DurationMS, ErrorCode: p.ErrorCode,
		CreatedAt: time.Now().UTC(),
	}
	f.items[p.ReportID] = append(f.items[p.ReportID], it)
	return it, nil
}

func (f *fakeInspReports) PreviousScores(context.Context, string, time.Time) (map[string]int, error) {
	return map[string]int{}, nil
}

// fakeInspSSH 记录命令并按脚本回放。
type fakeInspSSH struct {
	mu       sync.Mutex
	commands []string
	reply    func(cmd string) (string, error)
}

func (f *fakeInspSSH) Test(context.Context, sshx.Target) (sshx.TestResult, error) {
	return sshx.TestResult{}, nil
}
func (f *fakeInspSSH) Execute(_ context.Context, _ sshx.Target, req sshx.RunRequest) (sshx.RunResult, error) {
	f.mu.Lock()
	f.commands = append(f.commands, req.Command)
	reply := f.reply
	f.mu.Unlock()
	if reply == nil {
		return sshx.RunResult{ExitCode: 0, Combined: "ok"}, nil
	}
	out, err := reply(req.Command)
	if err != nil {
		return sshx.RunResult{}, err
	}
	return sshx.RunResult{ExitCode: 0, Combined: out}, nil
}
func (f *fakeInspSSH) Invalidate(string) {}
func (f *fakeInspSSH) Close() error      { return nil }

func (f *fakeInspSSH) executed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func newTestManager(t *testing.T, fs *fakeInspStore, ssh *fakeInspSSH) *Manager {
	t.Helper()
	cipher, err := cryptox.New(make([]byte, 32))
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	return NewManager(DefaultManagerConfig(), fs, ssh, nil, cipher, nil, nil, nil)
}

func fakeServerEnc(t *testing.T, m *Manager, id string, mods ...func(*store.Server)) store.Server {
	t.Helper()
	secret, err := m.cipher.Encrypt("secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	s := store.Server{ID: id, Name: "srv-" + id, DeviceKind: store.DeviceKindServer,
		OS: store.ServerOSLinux, AuthMethod: store.AuthMethodPassword,
		Host: "127.0.0.1", Port: 22, PasswordEncrypted: secret}
	for _, mod := range mods {
		mod(&s)
	}
	return s
}

// ---------------------------------------------------------------------------
// G34：评分规则（契约 7.7 写死表逐条）
// ---------------------------------------------------------------------------

func baseOutputs() map[string]string {
	return map[string]string{
		"uptime":            " 12:00:01 up 3 days",
		"df -B1":            "Filesystem 1B-blocks Used Available Use% Mounted\ntmpfs 100 90 10 /dev/shm\n/dev/sda1 1000 400 600 40% /",
		"cat /proc/loadavg": "0.10 0.20 0.30 1/100 12345",
		"grep -E '^(MemTotal|MemAvailable):' /proc/meminfo": "MemTotal: 100000 kB\nMemAvailable: 50000 kB",
		"systemctl --failed --no-legend":                    "0 loaded units listed.",
		"getconf _NPROCESSORS_ONLN":                         "4",
	}
}

func TestScoreHealthyServer(t *testing.T) {
	res := scoreServer(scoreInput{Outputs: baseOutputs()})
	if res.Score != 100 || len(res.Anomalies) != 0 {
		t.Fatalf("healthy server: score=%d anomalies=%v, want 100/0", res.Score, res.Anomalies)
	}
}

func TestScoreFailedServicesCap(t *testing.T) {
	out := baseOutputs()
	out["systemctl --failed --no-legend"] = strings.Repeat(
		"nginx.service loaded failed failed nginx\n", 5) // 5 单元 → -30（上限）
	res := scoreServer(scoreInput{Outputs: out})
	if res.Score != 70 {
		t.Fatalf("5 failed units: score=%d, want 70（-30 上限）", res.Score)
	}
	svcAnomalies := 0
	for _, a := range res.Anomalies {
		if a.Item == "service" {
			svcAnomalies++
		}
	}
	if svcAnomalies != 5 {
		t.Fatalf("service anomalies = %d, want 5（每个一条）", svcAnomalies)
	}
}

func TestScoreDiskBands(t *testing.T) {
	for _, tc := range []struct {
		used, avail int64
		wantScore   int
		wantSev     string
	}{
		{760, 240, 95, "warning"},  // 76% → -5
		{860, 140, 85, "critical"}, // 86% → -15
	} {
		out := baseOutputs()
		out["df -B1"] = fmt.Sprintf("Filesystem 1B-blocks Used Available Use%% Mounted\n/dev/sda1 1000 %d %d 80%% /", tc.used, tc.avail)
		res := scoreServer(scoreInput{Outputs: out})
		if res.Score != tc.wantScore {
			t.Fatalf("disk %d/%d: score=%d, want %d", tc.used, tc.used+tc.avail, res.Score, tc.wantScore)
		}
		if len(res.Anomalies) == 0 || res.Anomalies[0].Severity != tc.wantSev {
			t.Fatalf("disk anomaly severity = %+v, want %s", res.Anomalies, tc.wantSev)
		}
	}
}

func TestScoreLoadBands(t *testing.T) {
	for _, tc := range []struct {
		load      float64
		wantScore int
	}{
		{5.0, 90}, // > 4 核 → -10
		{9.0, 80}, // > 8 = 2×4 → -20
	} {
		out := baseOutputs()
		out["cat /proc/loadavg"] = fmt.Sprintf("%.2f 0.2 0.3 1/100 1", tc.load)
		res := scoreServer(scoreInput{Outputs: out})
		if res.Score != tc.wantScore {
			t.Fatalf("load %.1f: score=%d, want %d", tc.load, res.Score, tc.wantScore)
		}
	}
	// 核数取不到按 4 计。
	out := baseOutputs()
	delete(out, "getconf _NPROCESSORS_ONLN")
	out["cat /proc/loadavg"] = "5.00 0.2 0.3 1/100 1"
	if res := scoreServer(scoreInput{Outputs: out}); res.Score != 90 {
		t.Fatalf("missing cores default 4: score=%d, want 90", res.Score)
	}
}

func TestScoreMemoryAndJournal(t *testing.T) {
	out := baseOutputs()
	out["grep -E '^(MemTotal|MemAvailable):' /proc/meminfo"] = "MemTotal: 100000 kB\nMemAvailable: 8000 kB" // 92%
	res := scoreServer(scoreInput{Outputs: out})
	if res.Score != 90 {
		t.Fatalf("mem 92%%: score=%d, want 90", res.Score)
	}
	out["journalctl -p err -n 50 --no-pager"] = "-- Logs begin --\nOct 27 error: disk failure imminent\nOct 27 something failed to start\nOct 27 all good"
	res = scoreServer(scoreInput{Outputs: out})
	if res.Score != 85 { // -10 mem -5 journal
		t.Fatalf("mem+journal: score=%d, want 85", res.Score)
	}
	found := false
	for _, a := range res.Anomalies {
		if a.Item == "journal" {
			found = true
		}
	}
	if !found {
		t.Fatalf("journal anomaly missing: %+v", res.Anomalies)
	}
}

func TestScoreActiveAlertsCap(t *testing.T) {
	res := scoreServer(scoreInput{Outputs: baseOutputs(), Alerts: []alertInput{
		{ID: 1, Severity: "critical", Message: "cpu 95%"},
		{ID: 2, Severity: "warning", Message: "disk 80%"},
		{ID: 3, Severity: "info", Message: "net high"},
	}})
	if res.Score != 80 { // 3 条 → -20 上限
		t.Fatalf("3 alerts: score=%d, want 80（-20 上限）", res.Score)
	}
	if len(res.Anomalies) != 3 {
		t.Fatalf("alert anomalies = %d, want 3", len(res.Anomalies))
	}
}

func TestScoreCollectFailure(t *testing.T) {
	res := scoreServer(scoreInput{ExecErr: errors.New("ssh: connect timeout")})
	if res.Score != 0 || len(res.Anomalies) != 1 || res.Anomalies[0].Item != "collect" || res.Anomalies[0].Severity != "critical" {
		t.Fatalf("collect failure: %+v", res)
	}
}

func TestParseMaxDiskSkipsPseudoFS(t *testing.T) {
	out := `Filesystem 1B-blocks Used Available Use% Mounted
tmpfs 1000000 990000 10000 99% /dev/shm
overlay 100 99 1 99% /var/lib/docker/overlay2/x
squashfs 100 99 1 99% /snap/core/1
/dev/sda1 1000 400 600 40% /`
	if pct := parseMaxDiskPct(out); pct > 40.1 {
		t.Fatalf("max disk pct = %.2f, want <= 40（伪文件系统剔除）", pct)
	}
}

// ---------------------------------------------------------------------------
// Run 集成：模板命令执行、报告聚合、raw 截断、目标上限
// ---------------------------------------------------------------------------

func TestRunInspectionAggregates(t *testing.T) {
	fs := newFakeInspStore()
	ssh := &fakeInspSSH{reply: func(cmd string) (string, error) {
		if cmd == "cat /proc/loadavg" {
			return "0.1 0.2 0.3 1/1 1", nil
		}
		return "ok\n", nil
	}}
	m := newTestManager(t, fs, ssh)
	fs.servers["a"] = fakeServerEnc(t, m, "a")
	fs.servers["b"] = fakeServerEnc(t, m, "b")

	report, err := m.Run(context.Background(), nil, store.InspectionTriggerManual, []string{"a", "b"}, store.InspectionBasic)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.ServerCount != 2 || report.AvgScore != 100 || report.AnomalyCount != 0 {
		t.Fatalf("report = %+v", report)
	}
	items := fs.items[report.ID]
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	// 模板命令全部经 SAFE_INSPECT 豁免执行（basic 6 条/台）。
	cmds := ssh.executed()
	if len(cmds) != 12 {
		t.Fatalf("executed %d commands, want 12（6 条 × 2 台）: %v", len(cmds), cmds)
	}
	for _, c := range cmds {
		if d := risk.Classify(c, risk.OSLinux); d.Level != risk.LevelSafe {
			t.Fatalf("template command %q classified %s (%s)", c, d.Level, d.RuleID)
		}
	}
	// 不存在的目标跳过（部分成功）。
	if _, err := m.Run(context.Background(), nil, store.InspectionTriggerManual, []string{"a", "no-such"}, store.InspectionBasic); err != nil {
		t.Fatalf("run with missing server: %v", err)
	}
}

func TestRunRawOutputTruncated(t *testing.T) {
	fs := newFakeInspStore()
	big := strings.Repeat("x", 30000)
	ssh := &fakeInspSSH{reply: func(cmd string) (string, error) { return big, nil }}
	m := newTestManager(t, fs, ssh)
	fs.servers["a"] = fakeServerEnc(t, m, "a")

	report, err := m.Run(context.Background(), nil, store.InspectionTriggerManual, []string{"a"}, store.InspectionBasic)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	item := fs.items[report.ID][0]
	if len(item.RawOutput) > rawOutputMax {
		t.Fatalf("raw output %d > %d", len(item.RawOutput), rawOutputMax)
	}
	if !item.Truncated {
		t.Fatalf("truncated flag not set")
	}
}

func TestResolveTargetsCap(t *testing.T) {
	fs := newFakeInspStore()
	ssh := &fakeInspSSH{}
	m := newTestManager(t, fs, ssh)
	for i := 0; i < maxTargets+1; i++ {
		id := fmt.Sprintf("s%d", i)
		fs.servers[id] = fakeServerEnc(t, m, id)
	}
	if _, err := m.resolveTargets(context.Background(), nil, nil, nil); !errors.Is(err, ErrTooManyTargets) {
		t.Fatalf("201 servers: err = %v, want ErrTooManyTargets", err)
	}
	// 显式列表超限同样拒绝。
	ids := make([]string, maxTargets+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("s%d", i)
	}
	if _, err := m.resolveTargets(context.Background(), nil, nil, ids); !errors.Is(err, ErrTooManyTargets) {
		t.Fatalf("explicit 201: err = %v, want ErrTooManyTargets", err)
	}
	// network_device 不参与。
	fs2 := newFakeInspStore()
	m2 := newTestManager(t, fs2, ssh)
	fs2.servers["net1"] = store.Server{ID: "net1", DeviceKind: store.DeviceKindNetworkDevice, OS: store.ServerOSNetwork}
	targets, err := m2.resolveTargets(context.Background(), nil, nil, nil)
	if err != nil || len(targets) != 0 {
		t.Fatalf("network only: targets=%d err=%v", len(targets), err)
	}
}

func TestAggregateReport(t *testing.T) {
	if got := aggregateReport(nil); got != 100 {
		t.Fatalf("0 台 avg = %d, want 100", got)
	}
	if got := aggregateReport([]int{90, 95}); got != 93 { // 92.5 → 四舍五入 93
		t.Fatalf("avg(90,95) = %d, want 93", got)
	}
	if got := aggregateReport([]int{0, 0}); got != 0 {
		t.Fatalf("avg(0,0) = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// G35：cron 解析与调度
// ---------------------------------------------------------------------------

func TestNextRun(t *testing.T) {
	base := time.Date(2026, 10, 27, 8, 3, 30, 0, time.UTC)
	cases := []struct {
		expr string
		want time.Time
	}{
		{"*/5 * * * *", time.Date(2026, 10, 27, 8, 5, 0, 0, time.UTC)},
		{"0 8 * * *", time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)},
		{"30 4 1 * *", time.Date(2026, 11, 1, 4, 30, 0, 0, time.UTC)},
		{"0 0 31 * *", time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)},
		{"0 0 1 1 *", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}, // 跨年
		{"59 23 31 12 *", time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC)},
		{"0 0 * * 0", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}, // 周日
		{"0 0 * * 7", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}, // 7 也是周日
		{"15,45 * * * *", time.Date(2026, 10, 27, 8, 15, 0, 0, time.UTC)},
		{"10-20/5 * * * *", time.Date(2026, 10, 27, 8, 10, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		got, err := NextRun(tc.expr, base)
		if err != nil {
			t.Fatalf("NextRun(%q): %v", tc.expr, err)
		}
		if !got.Equal(tc.want) {
			t.Fatalf("NextRun(%q) = %s, want %s", tc.expr, got, tc.want)
		}
	}
}

func TestNextRunInvalid(t *testing.T) {
	bad := []string{
		"*/5 * *",       // 4 段
		"*/5 * * * * *", // 6 段
		"60 * * * *",    // 分越界
		"* 24 * * *",    // 时越界
		"* * 32 * *",    // 日越界
		"* * * 13 *",    // 月越界
		"* * * * 9",     // 周越界
		"a * * * *",     // 字母
		"5-1 * * * *",   // 区间倒置
		"*/0 * * * *",   // 步长 0
		"0 0 30 2 *",    // 永不匹配（2 月 30 日）
	}
	for _, expr := range bad {
		if _, err := NextRun(expr, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
			t.Fatalf("NextRun(%q) should fail", expr)
		} else if !errors.Is(err, ErrCronInvalid) {
			t.Fatalf("NextRun(%q) err = %v, want ErrCronInvalid", expr, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 并发与互斥守卫（契约 11 节 #9）
// ---------------------------------------------------------------------------

func TestJobMutexAndConcurrency(t *testing.T) {
	m := newTestManager(t, newFakeInspStore(), &fakeInspSSH{})
	if cap(m.sem) != 2 {
		t.Fatalf("sem cap = %d, want 2（MaxConcurrentJobs）", cap(m.sem))
	}
	// 同 job 互斥。
	if !m.tryLockJob("job-1") {
		t.Fatalf("first lock should succeed")
	}
	if m.tryLockJob("job-1") {
		t.Fatalf("second lock of same job should fail（同 job 互斥）")
	}
	m.unlockJob("job-1")
	if !m.tryLockJob("job-1") {
		t.Fatalf("lock after unlock should succeed")
	}
	// DispatchJob 同 job 二次触发拒绝。
	fs := newFakeInspStore()
	fs.jobs["job-1"] = store.InspectionJob{ID: "job-1", Name: "j", Cron: "*/5 * * * *", Template: store.InspectionBasic}
	m2 := newTestManager(t, fs, &fakeInspSSH{})
	m2.Stop() // 不启动循环，仅测互斥
	if !m2.DispatchJob(fs.jobs["job-1"], store.InspectionTriggerManual) {
		t.Fatalf("first dispatch should be accepted")
	}
	if m2.DispatchJob(fs.jobs["job-1"], store.InspectionTriggerManual) {
		t.Fatalf("second dispatch of same job should be rejected")
	}
	// 等第一个 dispatch 的 goroutine 退出（目标 0 台，立即完成）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		m2.runningMu.Lock()
		n := len(m2.running)
		m2.runningMu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// 内存守卫（防 OOM 铁律：核心评分路径分配预算）
// ---------------------------------------------------------------------------

func TestScoreServerAllocBudget(t *testing.T) {
	out := baseOutputs()
	out["systemctl --failed --no-legend"] = "nginx.service loaded failed failed nginx\ndb.service loaded failed failed db"
	in := scoreInput{Outputs: out, Alerts: []alertInput{{ID: 1, Severity: "warning", Message: "cpu high"}}}
	allocs := testing.AllocsPerRun(20, func() {
		scoreServer(in)
	})
	if allocs > 60 {
		t.Fatalf("scoreServer allocs = %.0f, want <= 60（评分与异常构造路径分配有界）", allocs)
	}
}

func TestParserInputLimit(t *testing.T) {
	huge := strings.Repeat("MemTotal: 1 kB\n", 100000) // ~1.4MB
	if pct := parseMeminfo(clampParserInput(huge)); pct == 0 {
		t.Fatalf("clamped parse should still read first fields")
	}
	if got := len(clampParserInput(huge)); got != parserInputLimit {
		t.Fatalf("clamp = %d, want %d", got, parserInputLimit)
	}
}

// ---------------------------------------------------------------------------
// SAFE_INSPECT / WIN_SAFE_INSPECT 模板登记（契约 7.7：整串豁免放行）
// ---------------------------------------------------------------------------

func TestInspectTemplatesClassifySafe(t *testing.T) {
	for _, cmd := range risk.InspectBasicCommands {
		if d := risk.Classify(cmd, risk.OSLinux); d.Level != risk.LevelSafe {
			t.Fatalf("basic command %q classified %s (%s), want safe", cmd, d.Level, d.RuleID)
		}
	}
	for _, cmd := range risk.InspectFullExtraCommands {
		if d := risk.Classify(cmd, risk.OSLinux); d.Level != risk.LevelSafe {
			t.Fatalf("full command %q classified %s (%s), want safe", cmd, d.Level, d.RuleID)
		}
	}
	for _, cmd := range append(append([]string(nil), risk.WinInspectBasicCommands...), risk.WinInspectFullCommands...) {
		if d := risk.Classify(cmd, risk.OSWindows); d.Level != risk.LevelSafe {
			t.Fatalf("win command classified %s (%s), want safe", d.Level, d.RuleID)
		}
	}
	// 采集模板经 WIN_SAFE_COLLECT 豁免（Windows basic 第 ① 条）。
	if d := risk.Classify(risk.WinCollectCommand, risk.OSWindows); d.Level != risk.LevelSafe {
		t.Fatalf("win collect classified %s", d.Level)
	}
	// 模板被改动（加一个字符）即失去豁免——失败关闭。
	if d := risk.Classify(risk.InspectBasicCommands[0]+" ;", risk.OSLinux); d.RuleID == "SAFE_INSPECT" && d.Level == risk.LevelSafe {
		t.Fatalf("modified template should not match exemption")
	}
}

// TestRunRespectsGlobalConcurrency 即时巡检路径（Run）与 job 型共用全局
// 并发信号量（契约 5.17「cron+手动合计」上限）：MaxConcurrentJobs=2 时
// 三个并发 Run 的执行重叠数 ≤2（五期评审发现 Run 曾不受约束）。
func TestRunRespectsGlobalConcurrency(t *testing.T) {
	fs := newFakeInspStore()
	ssh := &fakeInspSSH{}
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	release := make(chan struct{})
	ssh.reply = func(string) (string, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		return "0.10 0.20 0.30 1/100 1", nil
	}
	m := newTestManager(t, fs, ssh)
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("c%d", i)
		fs.servers[id] = fakeServerEnc(t, m, id)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := m.Run(ctx, nil, store.InspectionTriggerManual,
				[]string{fmt.Sprintf("c%d", i)}, store.InspectionBasic)
			errs <- err
		}(i)
	}
	// 等三条命令都进入执行（若无限并发会同时进入）或信号量排队。
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	started := maxInFlight
	mu.Unlock()
	if started > 2 {
		t.Fatalf("max in-flight = %d, want <=2（即时巡检不受全局信号量约束）", started)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
}

// TestSSHTargetEmptyPassphraseShortCircuits 覆盖六期打磨 sprint 4 的走查
// high：私钥认证 + 未填 passphrase（openapi 4.3 该字段可选）时巡检不再
// 报 "inspection: decrypt passphrase failed"——此前 decrypt 对空串无短路，
// cryptox 返回 ErrInvalidCiphertext，整轮巡检 0 分且错误列直显英文原文。
func TestSSHTargetEmptyPassphraseShortCircuits(t *testing.T) {
	fs := newFakeInspStore()
	m := newTestManager(t, fs, &fakeInspSSH{})
	key, err := m.cipher.Encrypt("KEY-BODY")
	if err != nil {
		t.Fatalf("encrypt key: %v", err)
	}
	srv := fakeServerEnc(t, m, "srv-pk", func(s *store.Server) {
		s.AuthMethod = store.AuthMethodPrivateKey
		s.PrivateKeyEncrypted = key
		s.PassphraseEncrypted = ""
	})
	target, err := m.sshTarget(srv)
	if err != nil {
		t.Fatalf("sshTarget with empty passphrase: %v", err)
	}
	if target.PrivateKey != "KEY-BODY" || target.Passphrase != "" {
		t.Fatalf("target = %+v", target)
	}
	// 非法密文仍报错（不因空串短路放宽）。
	if _, err := decrypt(m.cipher, "bogus"); err == nil {
		t.Fatal("invalid ciphertext must still fail")
	}
}
