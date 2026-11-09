// phase5_m2_integration_test.go 是五期 M2 的 store 集成测试（真实
// PostgreSQL，契约 9.2 G11/G33-G35 的持久层部分）：诊断会话状态机 CAS
// 与 ExpireStale；巡检任务 CRUD/MarkRun/ListDue；报告 + 明细落库与
// PreviousScores 趋势对比。
package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"aiops/internal/store"
)

func cleanPhase5M2Tables(t *testing.T) {
	t.Helper()
	_, err := testDB.pool.Exec(context.Background(), `
		TRUNCATE servers, diagnosis_sessions, inspection_jobs,
		         inspection_reports, inspection_report_items
		RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate phase5 m2 tables: %v", err)
	}
}

func TestDiagnosisSessionStateMachine(t *testing.T) {
	cleanPhase5M2Tables(t)
	ctx := context.Background()
	srv := mustServerRow(t)

	sess, err := testDB.DiagnosisSessions().Create(ctx, store.CreateDiagnosisParams{
		ServerID: srv.ID, Trigger: store.DiagnosisTriggerManual,
		ExpiresAt: time.Now().UTC().Add(30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sess.Status != store.DiagnosisPending || sess.ServerName != srv.Name {
		t.Fatalf("created = %+v", sess)
	}

	// CAS：错误的前置状态被拒。
	if _, err := testDB.DiagnosisSessions().UpdateStatus(ctx, store.UpdateDiagnosisStatusParams{
		ID: sess.ID, From: store.DiagnosisDiagnosing, To: store.DiagnosisReport,
		Timestamp: time.Now().UTC(),
	}); err == nil {
		t.Fatalf("CAS with wrong From should conflict")
	}

	now := time.Now().UTC()
	diag, err := testDB.DiagnosisSessions().UpdateStatus(ctx, store.UpdateDiagnosisStatusParams{
		ID: sess.ID, From: store.DiagnosisPending, To: store.DiagnosisDiagnosing, Timestamp: now,
	})
	if err != nil || diag.StartedAt == nil {
		t.Fatalf("pending->diagnosing: (%+v, %v)", diag, err)
	}

	report := []byte(`{"summary":"s","severity":"info","root_cause":"r","evidence":[],"suggestions":[],"confidence":"high"}`)
	rep, err := testDB.DiagnosisSessions().UpdateStatus(ctx, store.UpdateDiagnosisStatusParams{
		ID: sess.ID, From: store.DiagnosisDiagnosing, To: store.DiagnosisReport,
		Report: report, Timestamp: now,
	})
	if err != nil {
		t.Fatalf("diagnosing->report: %v", err)
	}
	// jsonb 不保序：语义比较（反序列化逐字段）而非字节比较。
	var wantReport, gotReport map[string]any
	if err := json.Unmarshal(report, &wantReport); err != nil {
		t.Fatalf("want report unmarshal: %v", err)
	}
	if err := json.Unmarshal(rep.Report, &gotReport); err != nil {
		t.Fatalf("got report unmarshal: %v", err)
	}
	if gotReport["summary"] != wantReport["summary"] || len(gotReport) != len(wantReport) {
		t.Fatalf("report roundtrip = %s", rep.Report)
	}

	await, err := testDB.DiagnosisSessions().UpdateStatus(ctx, store.UpdateDiagnosisStatusParams{
		ID: sess.ID, From: store.DiagnosisReport, To: store.DiagnosisAwaitingFix, Timestamp: now,
	})
	if err != nil {
		t.Fatalf("report->awaiting_fix: %v", err)
	}
	// 进入 awaiting_fix 时 expires_at 重置为 timestamp+30m（契约 4.21）。
	if !await.ExpiresAt.After(now.Add(29 * time.Minute)) {
		t.Fatalf("expires_at not refreshed on awaiting_fix: %s", await.ExpiresAt)
	}

	// ExpireStale：未过期 0 行；拨过期后 1 行。
	if n, err := testDB.DiagnosisSessions().ExpireStale(ctx, time.Now().UTC()); err != nil || n != 0 {
		t.Fatalf("ExpireStale fresh = (%d, %v)", n, err)
	}
	if n, err := testDB.DiagnosisSessions().ExpireStale(ctx, time.Now().UTC().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("ExpireStale stale = (%d, %v)", n, err)
	}
	got, _ := testDB.DiagnosisSessions().Get(ctx, sess.ID)
	if got.Status != store.DiagnosisExpired {
		t.Fatalf("status = %s, want expired", got.Status)
	}
}

func TestInspectionJobAndReports(t *testing.T) {
	cleanPhase5M2Tables(t)
	ctx := context.Background()
	srv := mustServerRow(t)

	next := time.Now().UTC().Add(5 * time.Minute)
	job, err := testDB.InspectionJobs().Create(ctx, store.CreateInspectionJobParams{
		Name: "nightly", Cron: "0 8 * * *", TargetTags: []string{"prod"},
		Template: store.InspectionBasic, Enabled: true, NextRunAt: &next,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if len(job.TargetTags) != 1 || job.TargetTags[0] != "prod" {
		t.Fatalf("target_tags = %v", job.TargetTags)
	}

	// ListDue：未到点 0 行；到点后命中。
	if due, err := testDB.InspectionJobs().ListDue(ctx, time.Now().UTC()); err != nil || len(due) != 0 {
		t.Fatalf("ListDue early = (%d, %v)", len(due), err)
	}
	if due, err := testDB.InspectionJobs().ListDue(ctx, next.Add(time.Minute)); err != nil || len(due) != 1 {
		t.Fatalf("ListDue due = (%d, %v)", len(due), err)
	}

	// MarkRun 后不再 due。
	future := time.Now().UTC().Add(time.Hour)
	if err := testDB.InspectionJobs().MarkRun(ctx, job.ID, time.Now().UTC(), future); err != nil {
		t.Fatalf("MarkRun: %v", err)
	}
	if due, _ := testDB.InspectionJobs().ListDue(ctx, time.Now().UTC()); len(due) != 0 {
		t.Fatalf("ListDue after MarkRun = %d, want 0", len(due))
	}

	// 报告 + 明细 + PreviousScores。
	started := time.Now().UTC().Add(-time.Minute)
	finished := time.Now().UTC()
	report, err := testDB.InspectionReports().Create(ctx, store.CreateInspectionReportParams{
		JobID: &job.ID, Trigger: store.InspectionTriggerCron,
		ServerCount: 1, AvgScore: 72, AnomalyCount: 3,
		StartedAt: started, FinishedAt: &finished,
	})
	if err != nil {
		t.Fatalf("create report: %v", err)
	}
	if report.JobName != "nightly" {
		t.Fatalf("job_name = %q, want nightly（JOIN 装配）", report.JobName)
	}
	anomalies := []byte(`[{"item":"disk","detail":"/ 91%","severity":"critical"}]`)
	if _, err := testDB.InspectionReports().CreateItem(ctx, store.CreateInspectionItemParams{
		ReportID: report.ID, ServerID: srv.ID, Score: 85, AnomalyCount: 1,
		Anomalies: anomalies, RawOutput: "raw", Command: "uptime\ndf -h", DurationMS: 123,
	}); err != nil {
		t.Fatalf("create item: %v", err)
	}
	got, items, err := testDB.InspectionReports().Get(ctx, report.ID)
	if err != nil || got.AvgScore != 72 || len(items) != 1 {
		t.Fatalf("Get report = (%+v, %d items, %v)", got, len(items), err)
	}
	if items[0].ServerName != srv.Name {
		t.Fatalf("item server_name = %q", items[0].ServerName)
	}
	// 六期打磨 sprint 1（迁移 00011）：命令清单往返（契约 8.7 ⑧）。
	if items[0].Command != "uptime\ndf -h" {
		t.Fatalf("item command = %q, want 模板展开后的命令清单", items[0].Command)
	}
	var gotAnomaly []map[string]any
	if err := json.Unmarshal(items[0].Anomalies, &gotAnomaly); err != nil || len(gotAnomaly) != 1 ||
		gotAnomaly[0]["item"] != "disk" || gotAnomaly[0]["severity"] != "critical" {
		t.Fatalf("item anomalies = %s", items[0].Anomalies)
	}

	// 同 job 更早一份报告 → PreviousScores 取其 per-server 评分。
	earlier := started.Add(-time.Hour)
	older, err := testDB.InspectionReports().Create(ctx, store.CreateInspectionReportParams{
		JobID: &job.ID, Trigger: store.InspectionTriggerCron,
		ServerCount: 1, AvgScore: 40, StartedAt: earlier,
	})
	if err != nil {
		t.Fatalf("create older report: %v", err)
	}
	if _, err := testDB.InspectionReports().CreateItem(ctx, store.CreateInspectionItemParams{
		ReportID: older.ID, ServerID: srv.ID, Score: 40, Anomalies: []byte("[]"),
	}); err != nil {
		t.Fatalf("create older item: %v", err)
	}
	prev, err := testDB.InspectionReports().PreviousScores(ctx, job.ID, started)
	if err != nil || prev[srv.ID] != 40 {
		t.Fatalf("PreviousScores = (%v, %v), want map[%s:40]", prev, err, srv.ID)
	}

	// 列表过滤。
	list, err := testDB.InspectionReports().List(ctx, store.InspectionReportFilter{JobID: job.ID, Limit: 10})
	if err != nil || len(list) != 2 {
		t.Fatalf("List by job = (%d, %v), want 2", len(list), err)
	}
	all, err := testDB.InspectionReports().List(ctx, store.InspectionReportFilter{Limit: 10})
	if err != nil || len(all) != 2 {
		t.Fatalf("List all = (%d, %v)", len(all), err)
	}

	// 任务删除级联报告。
	if err := testDB.InspectionJobs().Delete(ctx, job.ID); err != nil {
		t.Fatalf("delete job: %v", err)
	}
	if _, _, err := testDB.InspectionReports().Get(ctx, report.ID); err == nil {
		t.Fatalf("report should cascade on job delete")
	}
}
