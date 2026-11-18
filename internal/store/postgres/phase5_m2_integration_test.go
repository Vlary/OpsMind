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

// TestDiagnosisListCreatedByFilter 第 11 轮走查 medium：归属过滤必须在 SQL
// 层（WHERE d.created_by=... 先于 LIMIT）。非 admin 只拿到自己的会话；
// created_by IS NULL 的 system 行仅 admin 可见（契约 12.3 第 16 条）。
func TestDiagnosisListCreatedByFilter(t *testing.T) {
	cleanDB(t)
	ctx := context.Background()
	owner := rbacUserID(t)
	other, err := testDB.Users().Create(ctx, store.CreateUserParams{
		Username: "diag-other", PasswordHash: "$2a$12$diagother",
		Role: store.RoleOperator, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := mustServerRow(t)
	now := time.Now().UTC()

	mk := func(createdBy *string) store.DiagnosisSession {
		t.Helper()
		d, err := testDB.DiagnosisSessions().Create(ctx, store.CreateDiagnosisParams{
			ServerID: srv.ID, Trigger: store.DiagnosisTriggerManual,
			ExpiresAt: now.Add(30 * time.Minute), CreatedBy: createdBy,
		})
		if err != nil {
			t.Fatalf("create diagnosis: %v", err)
		}
		return d
	}
	// other 的会话先建（较旧），owner 的三条后建（较新）。
	otherSession := mk(&other.ID)
	var ownerSessions []string
	for i := 0; i < 3; i++ {
		ownerSessions = append(ownerSessions, mk(&owner).ID)
	}
	systemSession := mk(nil)

	own, err := testDB.DiagnosisSessions().List(ctx, "", "", owner, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 3 {
		t.Fatalf("owner list = %d rows, want 3", len(own))
	}
	for _, d := range own {
		if d.CreatedBy == nil || *d.CreatedBy != owner {
			t.Fatalf("owner list leaked created_by=%v", d.CreatedBy)
		}
		if d.ID == otherSession.ID || d.ID == systemSession.ID {
			t.Fatalf("owner list leaked session %s", d.ID)
		}
	}

	// LIMIT 1：过滤在 LIMIT 前，owner 的会话不会因 other 先建/较旧而丢失。
	first, err := testDB.DiagnosisSessions().List(ctx, "", "", owner, 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("owner limit=1 = (%d, %v)", len(first), err)
	}
	if first[0].CreatedBy == nil || *first[0].CreatedBy != owner {
		t.Fatalf("owner limit=1 leaked created_by=%v", first[0].CreatedBy)
	}

	all, err := testDB.DiagnosisSessions().List(ctx, "", "", "", 50)
	if err != nil || len(all) != 5 {
		t.Fatalf("admin list = (%d, %v), want 5", len(all), err)
	}
	foundSystem := false
	for _, d := range all {
		if d.ID == systemSession.ID {
			if d.CreatedBy != nil {
				t.Fatalf("system session created_by = %v, want nil", d.CreatedBy)
			}
			foundSystem = true
		}
	}
	if !foundSystem {
		t.Fatal("admin list missing system session")
	}
}
