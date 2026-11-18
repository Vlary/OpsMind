package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"aiops/internal/store"
)

// TestDiagnosisListScopesToOwnerOrAdmin 第 11 轮走查 medium：会话列表必须
// 与 GET /diagnosis/sessions/:id 同源——非 admin 只看自己创建的会话（
// created_by IS NULL 的 system 行仅 admin），否则列表里会出现他人摘要、
// 点「查看报告」才 404。过滤必须在 SQL 层，第三方较新会话不能占满 limit。
func TestDiagnosisListScopesToOwnerOrAdmin(t *testing.T) {
	env := newTestEnv(t)
	serverID := seedContainerServerForAPI(t, env, store.ContainerRuntimeDocker, store.DeviceKindServer)
	operator := addTestUser(t, env, "operator", testPassword, store.RoleOperator, true)
	operatorCookie := loginAs(t, env, "operator", testPassword)

	ctx := context.Background()
	adminID := "user-1"
	create := func(createdBy *string, summary string) store.DiagnosisSession {
		t.Helper()
		sess, err := env.st.DiagnosisSessions().Create(ctx, store.CreateDiagnosisParams{
			ServerID: serverID, Trigger: store.DiagnosisTriggerManual,
			ExpiresAt: time.Now().UTC().Add(30 * time.Minute),
			CreatedBy: createdBy,
		})
		if err != nil {
			t.Fatalf("create diagnosis: %v", err)
		}
		// 报告直接写入内存 fake，模拟引擎产出（列表会带出完整 report）。
		report := json.RawMessage(`{"summary":"` + summary + `","severity":"info","root_cause":"r","evidence":[],"suggestions":[],"confidence":"high"}`)
		if _, err := env.st.DiagnosisSessions().UpdateStatus(ctx, store.UpdateDiagnosisStatusParams{
			ID: sess.ID, From: store.DiagnosisPending, To: store.DiagnosisReport,
			Report: report, Timestamp: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("report diagnosis: %v", err)
		}
		return sess
	}
	adminSession := create(&adminID, "ADMIN-ONLY-DIAG-REPORT-11")
	operatorSession := create(&operator.ID, "OPERATOR-DIAG-REPORT-11")
	systemSession := create(nil, "SYSTEM-ALERT-DIAG-REPORT-11")

	ids := func(cookie string) (map[string]bool, string) {
		t.Helper()
		rec := env.doAs(cookie, http.MethodGet, "/api/diagnosis/sessions?limit=50", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Items []struct {
				ID     string          `json:"id"`
				Report json.RawMessage `json:"report"`
			} `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, item := range body.Items {
			out[item.ID] = true
		}
		return out, rec.Body.String()
	}

	got, body := ids(operatorCookie)
	if len(got) != 1 || !got[operatorSession.ID] {
		t.Fatalf("operator list = %s, want only own session", body)
	}
	if strings.Contains(body, "ADMIN-ONLY-DIAG-REPORT-11") || strings.Contains(body, "SYSTEM-ALERT-DIAG-REPORT-11") {
		t.Fatalf("operator list leaked other report: %s", body)
	}

	// 同一行：GET 详情按 owner-or-admin 处理，他人/系统会话 404。
	for _, id := range []string{adminSession.ID, systemSession.ID} {
		rec := env.doAs(operatorCookie, http.MethodGet, "/api/diagnosis/sessions/"+id, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("operator get %s = %d %s", id, rec.Code, rec.Body.String())
		}
	}
	if rec := env.doAs(operatorCookie, http.MethodGet, "/api/diagnosis/sessions/"+operatorSession.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("operator get own = %d %s", rec.Code, rec.Body.String())
	}

	// admin 看全量（含 system），也能读他人详情。
	got, body = ids(env.cookie)
	for _, want := range []string{adminSession.ID, operatorSession.ID, systemSession.ID} {
		if !got[want] {
			t.Fatalf("admin list missing %s: %s", want, body)
		}
	}
	if rec := env.do(http.MethodGet, "/api/diagnosis/sessions/"+operatorSession.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("admin get operator session = %d", rec.Code)
	}
}

// TestDiagnosisListOwnerFilterBeforeLimit 验证归属过滤发生在 LIMIT 之前：
// 他人较新的会话不能把非 admin 自己的会话挤出 limit。
func TestDiagnosisListOwnerFilterBeforeLimit(t *testing.T) {
	env := newTestEnv(t)
	serverID := seedContainerServerForAPI(t, env, store.ContainerRuntimeDocker, store.DeviceKindServer)
	operator := addTestUser(t, env, "operator", testPassword, store.RoleOperator, true)
	operatorCookie := loginAs(t, env, "operator", testPassword)
	ctx := context.Background()

	adminID := "user-1"
	own, err := env.st.DiagnosisSessions().Create(ctx, store.CreateDiagnosisParams{
		ServerID: serverID, Trigger: store.DiagnosisTriggerManual,
		ExpiresAt: time.Now().UTC().Add(30 * time.Minute), CreatedBy: &operator.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := env.st.DiagnosisSessions().Create(ctx, store.CreateDiagnosisParams{
			ServerID: serverID, Trigger: store.DiagnosisTriggerManual,
			ExpiresAt: time.Now().UTC().Add(30 * time.Minute), CreatedBy: &adminID,
		}); err != nil {
			t.Fatal(err)
		}
	}

	rec := env.doAs(operatorCookie, http.MethodGet, "/api/diagnosis/sessions?limit=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.Items[0].ID != own.ID {
		t.Fatalf("limit=1 items = %+v, want own %s", body.Items, own.ID)
	}
}
