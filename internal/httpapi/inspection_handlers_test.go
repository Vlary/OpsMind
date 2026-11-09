// inspection_handlers_test.go 覆盖五期 M2 巡检报告 REST 的契约形状：
// GET /api/inspection/reports/:id 与 POST /api/inspection/run 必须返回嵌套
// InspectionReportDetail {report, items, previous_scores}（openapi.yaml
// required [report, items, previous_scores]；前端 ReportDrawer/RunNowModal
// 读 detail.report.*）——此前响应体匿名嵌入 store.InspectionReport 导致
// JSON 平铺，两个功能全坏（五期走查发现）。同时提供 memStore 的
// InspectionJobs/InspectionReports 内存 fake（替换 fakes_test.go 的占位
// stub，M3 batchState 模式）。
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"aiops/internal/inspection"
	"aiops/internal/store"
)

// ---------------------------------------------------------------------------
// memStore 的 InspectionJob/InspectionReport 内存 fake
// ---------------------------------------------------------------------------

// inspectionFakeState 是巡检数据面（job + 报告 + 明细；有界：测试写入，
// resetInspectionState 每用例重建）。
type inspectionFakeState struct {
	mu      sync.Mutex
	jobs    map[string]store.InspectionJob
	jobSeq  int
	reports []store.InspectionReport // CreatedAt DESC 由 List 排序返回
	repSeq  int
	items   map[string][]store.InspectionReportItem
	itemSeq int64
}

var inspectionState = newInspectionFakeState()

func newInspectionFakeState() *inspectionFakeState {
	return &inspectionFakeState{
		jobs:  map[string]store.InspectionJob{},
		items: map[string][]store.InspectionReportItem{},
	}
}

func resetInspectionState() { inspectionState = newInspectionFakeState() }

// ---- InspectionJobStore（独立 wrapper：两个接口的 Create 同名，不能共型）----

type inspectionJobsFake struct{ st *inspectionFakeState }

func (f *inspectionJobsFake) List(context.Context) ([]store.InspectionJob, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	out := make([]store.InspectionJob, 0, len(f.st.jobs))
	for _, j := range f.st.jobs {
		out = append(out, j)
	}
	return out, nil
}

func (f *inspectionJobsFake) Get(_ context.Context, id string) (store.InspectionJob, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	if j, ok := f.st.jobs[id]; ok {
		return j, nil
	}
	return store.InspectionJob{}, store.ErrNotFound
}

func (f *inspectionJobsFake) Create(_ context.Context, p store.CreateInspectionJobParams) (store.InspectionJob, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	f.st.jobSeq++
	id := fmt.Sprintf("ijob-%d", f.st.jobSeq)
	j := store.InspectionJob{
		ID: id, Name: p.Name, Cron: p.Cron, TargetTags: p.TargetTags,
		Template: p.Template, NotifyEnabled: p.NotifyEnabled,
		NotifyChannelID: p.NotifyChannelID, Enabled: p.Enabled,
		NextRunAt: p.NextRunAt, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	f.st.jobs[id] = j
	return j, nil
}

func (f *inspectionJobsFake) Update(_ context.Context, id string, p store.UpdateInspectionJobParams) (store.InspectionJob, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	j, ok := f.st.jobs[id]
	if !ok {
		return store.InspectionJob{}, store.ErrNotFound
	}
	if p.Name != nil {
		j.Name = *p.Name
	}
	if p.Cron != nil {
		j.Cron = *p.Cron
	}
	if p.TargetTags != nil {
		j.TargetTags = *p.TargetTags
	}
	if p.Template != nil {
		j.Template = *p.Template
	}
	if p.NotifyEnabled != nil {
		j.NotifyEnabled = *p.NotifyEnabled
	}
	if p.Enabled != nil {
		j.Enabled = *p.Enabled
	}
	j.UpdatedAt = time.Now().UTC()
	f.st.jobs[id] = j
	return j, nil
}

func (f *inspectionJobsFake) Delete(_ context.Context, id string) error {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	if _, ok := f.st.jobs[id]; !ok {
		return store.ErrNotFound
	}
	delete(f.st.jobs, id)
	return nil
}

func (f *inspectionJobsFake) MarkRun(_ context.Context, id string, lastRunAt, nextRunAt time.Time) error {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	j, ok := f.st.jobs[id]
	if !ok {
		return store.ErrNotFound
	}
	j.LastRunAt = &lastRunAt
	j.NextRunAt = &nextRunAt
	f.st.jobs[id] = j
	return nil
}

func (f *inspectionJobsFake) ListDue(context.Context, time.Time) ([]store.InspectionJob, error) {
	return []store.InspectionJob{}, nil // cron 循环不启用（handler 测试不依赖）
}

// ---- InspectionReportStore ----

type inspectionReportsFake struct{ st *inspectionFakeState }

func (f *inspectionReportsFake) Create(_ context.Context, p store.CreateInspectionReportParams) (store.InspectionReport, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	f.st.repSeq++
	id := fmt.Sprintf("rep-%d", f.st.repSeq)
	r := store.InspectionReport{
		ID: id, JobID: p.JobID, Trigger: p.Trigger, ServerCount: p.ServerCount,
		AvgScore: p.AvgScore, AnomalyCount: p.AnomalyCount,
		StartedAt: p.StartedAt, FinishedAt: p.FinishedAt, CreatedAt: time.Now().UTC(),
	}
	f.st.reports = append(f.st.reports, r)
	return r, nil
}

func (f *inspectionReportsFake) Get(_ context.Context, id string) (store.InspectionReport, []store.InspectionReportItem, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	for _, r := range f.st.reports {
		if r.ID == id {
			items := f.st.items[id]
			out := make([]store.InspectionReportItem, len(items))
			copy(out, items)
			return r, out, nil
		}
	}
	return store.InspectionReport{}, nil, store.ErrNotFound
}

func (f *inspectionReportsFake) List(_ context.Context, flt store.InspectionReportFilter) ([]store.InspectionReport, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	// CreatedAt DESC（Create 追加序），同 job_id 过滤。
	var ordered []store.InspectionReport
	for i := len(f.st.reports) - 1; i >= 0; i-- {
		r := f.st.reports[i]
		if flt.JobID != "" && (r.JobID == nil || *r.JobID != flt.JobID) {
			continue
		}
		ordered = append(ordered, r)
	}
	limit := flt.Limit
	if limit <= 0 {
		limit = 50
	}
	if len(ordered) > limit {
		ordered = ordered[:limit]
	}
	return ordered, nil
}

func (f *inspectionReportsFake) CreateItem(_ context.Context, p store.CreateInspectionItemParams) (store.InspectionReportItem, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	f.st.itemSeq++
	it := store.InspectionReportItem{
		ID: f.st.itemSeq, ReportID: p.ReportID, ServerID: p.ServerID, Score: p.Score,
		AnomalyCount: p.AnomalyCount, Anomalies: p.Anomalies, RawOutput: p.RawOutput,
		Truncated: p.Truncated, DurationMS: p.DurationMS, ErrorCode: p.ErrorCode,
		CreatedAt: time.Now().UTC(),
	}
	f.st.items[p.ReportID] = append(f.st.items[p.ReportID], it)
	return it, nil
}

// PreviousScores 取同 jobID、StartedAt 严格早于 before 的最新一份报告的
// per-server 评分（与生产 pgstore 同语义）。
func (f *inspectionReportsFake) PreviousScores(_ context.Context, jobID string, before time.Time) (map[string]int, error) {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	var best *store.InspectionReport
	for i := range f.st.reports {
		r := &f.st.reports[i]
		if r.JobID == nil || *r.JobID != jobID || !r.StartedAt.Before(before) {
			continue
		}
		if best == nil || r.StartedAt.After(best.StartedAt) {
			best = r
		}
	}
	if best == nil {
		return map[string]int{}, nil
	}
	out := map[string]int{}
	for _, it := range f.st.items[best.ID] {
		out[it.ServerID] = it.Score
	}
	return out, nil
}

// memStore 的巡检装配（替换 fakes_test.go 的 M2 占位 stub）。
func (m *memStore) InspectionJobs() store.InspectionJobStore {
	return &inspectionJobsFake{st: inspectionState}
}

func (m *memStore) InspectionReports() store.InspectionReportStore {
	return &inspectionReportsFake{st: inspectionState}
}

// ---------------------------------------------------------------------------
// 测试环境
// ---------------------------------------------------------------------------

func newInspectionTestEnv(t *testing.T) (*gin.Engine, *Deps) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	resetInspectionState()
	st := newMemStore()
	cipher := testCipherForMonitor(t)
	// fakeSSHExec 默认 ExitCode=0/Combined="ok"：模板命令全部成功。
	mgr := inspection.NewManager(inspection.DefaultManagerConfig(), st, &fakeSSHExec{}, nil,
		cipher, nil, func() time.Time { return time.Now().UTC() }, nil)
	d := &Deps{Store: st, Cipher: cipher, Inspect: mgr, Clock: func() time.Time { return time.Now().UTC() }}
	r := gin.New()
	d.RegisterInspectionRoutes(r.Group("/api"))
	return r, d
}

// seedInspectionServer 种一台 password 认证的 Linux 服务器（返回 id）。
func seedInspectionServer(t *testing.T, d *Deps, name string) string {
	t.Helper()
	enc, err := d.Cipher.Encrypt("pw-" + name)
	if err != nil {
		t.Fatal(err)
	}
	srvID := uuid.NewString()
	ms := d.Store.(*memStore)
	ms.mu.Lock()
	ms.servers[srvID] = store.Server{
		ID: srvID, Name: name, DeviceKind: store.DeviceKindServer, OS: store.ServerOSLinux,
		Host: "127.0.0.1", Port: 22, Username: "root",
		AuthMethod: store.AuthMethodPassword, PasswordEncrypted: enc,
	}
	ms.mu.Unlock()
	return srvID
}

// ---------------------------------------------------------------------------
// 契约形状测试（本轮打磨 #1 的回归防线）
// ---------------------------------------------------------------------------

// detailKeys 断言响应体顶层键集恰为 {report, items, previous_scores}——
// 平铺回归（顶层出现 avg_score 等 InspectionReport 字段）即刻失败。
func detailKeys(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("response not a JSON object: %v\n%s", err, body)
	}
	if len(top) != 3 {
		t.Fatalf("top-level keys = %v, want exactly [items previous_scores report] (nested InspectionReportDetail)", keysOf(top))
	}
	for _, key := range []string{"report", "items", "previous_scores"} {
		if _, ok := top[key]; !ok {
			t.Fatalf("top-level key %q missing; got %v", key, keysOf(top))
		}
	}
	if _, flat := top["avg_score"]; flat {
		t.Fatal("flat report field leaked to top level (regression of nested contract)")
	}
	return top
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestRunInspectionNowNestedContract：即时巡检同步返回嵌套 detail——
// report 头 + 逐台 items + previous_scores=null（即时巡检无可比报告）。
func TestRunInspectionNowNestedContract(t *testing.T) {
	r, d := newInspectionTestEnv(t)
	srvID := seedInspectionServer(t, d, "web-1")

	w := doJSON(t, r, http.MethodPost, "/api/inspection/run", map[string]any{
		"server_ids": []string{srvID}, "template": "basic",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	top := detailKeys(t, w.Body.Bytes())

	var report store.InspectionReport
	if err := json.Unmarshal(top["report"], &report); err != nil {
		t.Fatalf("report member invalid: %v", err)
	}
	if report.ID == "" || report.ServerCount != 1 || report.JobID != nil {
		t.Fatalf("report = %+v", report)
	}
	var items []store.InspectionReportItem
	if err := json.Unmarshal(top["items"], &items); err != nil || len(items) != 1 {
		t.Fatalf("items = %+v err=%v (Run 后从 store 读回逐台明细)", items, err)
	}
	if items[0].ServerID != srvID {
		t.Fatalf("item server = %s, want %s", items[0].ServerID, srvID)
	}
	if string(top["previous_scores"]) != "null" {
		t.Fatalf("previous_scores = %s, want null (即时巡检无趋势对比)", top["previous_scores"])
	}
}

// TestGetInspectionReportNestedContract：报告详情嵌套 detail；job 报告带
// 同 job 上一份的 previous_scores；即时报告（job_id=null）为 null。
func TestGetInspectionReportNestedContract(t *testing.T) {
	r, d := newInspectionTestEnv(t)
	ctx := context.Background()

	job, err := d.Store.InspectionJobs().Create(ctx, store.CreateInspectionJobParams{
		Name: "nightly", Cron: "0 3 * * *", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-2 * time.Hour)
	old, err := d.Store.InspectionReports().Create(ctx, store.CreateInspectionReportParams{
		JobID: &job.ID, Trigger: store.InspectionTriggerCron, ServerCount: 1,
		AvgScore: 60, StartedAt: base, FinishedAt: &base,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Store.InspectionReports().CreateItem(ctx, store.CreateInspectionItemParams{
		ReportID: old.ID, ServerID: "srv-old", Score: 60,
	}); err != nil {
		t.Fatal(err)
	}
	newStart := base.Add(1 * time.Hour)
	cur, err := d.Store.InspectionReports().Create(ctx, store.CreateInspectionReportParams{
		JobID: &job.ID, Trigger: store.InspectionTriggerCron, ServerCount: 1,
		AvgScore: 90, StartedAt: newStart, FinishedAt: &newStart,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Store.InspectionReports().CreateItem(ctx, store.CreateInspectionItemParams{
		ReportID: cur.ID, ServerID: "srv-old", Score: 90,
	}); err != nil {
		t.Fatal(err)
	}

	w := doJSON(t, r, http.MethodGet, "/api/inspection/reports/"+cur.ID, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", w.Code, w.Body.String())
	}
	top := detailKeys(t, w.Body.Bytes())
	var report store.InspectionReport
	if err := json.Unmarshal(top["report"], &report); err != nil || report.ID != cur.ID {
		t.Fatalf("report = %+v err=%v", report, err)
	}
	var prev map[string]int
	if err := json.Unmarshal(top["previous_scores"], &prev); err != nil {
		t.Fatalf("previous_scores invalid: %v (%s)", err, top["previous_scores"])
	}
	if prev["srv-old"] != 60 {
		t.Fatalf("previous_scores = %v, want srv-old:60 (同 job 上一份)", prev)
	}

	// 即时巡检报告（job_id=null）：previous_scores 必须为 null 而非 {}。
	now := time.Now().UTC()
	adhoc, err := d.Store.InspectionReports().Create(ctx, store.CreateInspectionReportParams{
		Trigger: store.InspectionTriggerManual, ServerCount: 0,
		AvgScore: 0, StartedAt: now, FinishedAt: &now,
	})
	if err != nil {
		t.Fatal(err)
	}
	w = doJSON(t, r, http.MethodGet, "/api/inspection/reports/"+adhoc.ID, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("adhoc code = %d", w.Code)
	}
	top = detailKeys(t, w.Body.Bytes())
	if string(top["previous_scores"]) != "null" {
		t.Fatalf("adhoc previous_scores = %s, want null", top["previous_scores"])
	}
	var items []store.InspectionReportItem
	if err := json.Unmarshal(top["items"], &items); err != nil || items == nil {
		t.Fatalf("adhoc items = %v err=%v, want [] not null", items, err)
	}
}

// TestRunInspectionNowLimits：目标 1..200 输入上限（0 → 400；201 → 400）；
// 200 台在限时内同步返回且 items 恰 200 行（响应组装有界，契约 maxItems）。
func TestRunInspectionNowLimits(t *testing.T) {
	r, d := newInspectionTestEnv(t)

	if w := doJSON(t, r, http.MethodPost, "/api/inspection/run", map[string]any{
		"server_ids": []string{},
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("empty server_ids code = %d, want 400", w.Code)
	}
	over := make([]string, 201)
	for i := range over {
		over[i] = uuid.NewString()
	}
	if w := doJSON(t, r, http.MethodPost, "/api/inspection/run", map[string]any{
		"server_ids": over,
	}); w.Code != http.StatusBadRequest {
		t.Fatalf("201 server_ids code = %d, want 400", w.Code)
	}

	ids := make([]string, 200)
	for i := range ids {
		ids[i] = seedInspectionServer(t, d, fmt.Sprintf("bulk-%d", i))
	}
	w := doJSON(t, r, http.MethodPost, "/api/inspection/run", map[string]any{
		"server_ids": ids,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("200 servers code = %d body=%s", w.Code, w.Body.String())
	}
	top := detailKeys(t, w.Body.Bytes())
	var report store.InspectionReport
	if err := json.Unmarshal(top["report"], &report); err != nil || report.ServerCount != 200 {
		t.Fatalf("report = %+v err=%v", report, err)
	}
	var items []store.InspectionReportItem
	if err := json.Unmarshal(top["items"], &items); err != nil || len(items) != 200 {
		t.Fatalf("items = %d (err=%v), want exactly 200 (契约 maxItems)", len(items), err)
	}
}

// TestGetInspectionReportNotFound：404 错误码映射。
func TestGetInspectionReportNotFound(t *testing.T) {
	r, _ := newInspectionTestEnv(t)
	w := doJSON(t, r, http.MethodGet, "/api/inspection/reports/"+uuid.NewString(), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error.Code == "" {
		t.Fatalf("error.code missing: %s", w.Body.String())
	}
}

// 编译期断言（防 fake 漂离接口）。
var (
	_ store.InspectionJobStore    = (*inspectionJobsFake)(nil)
	_ store.InspectionReportStore = (*inspectionReportsFake)(nil)
)
