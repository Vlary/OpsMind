// policy_test.go 覆盖六期 BE-P 统一审批门限策略（契约 2.11 约定 1/2）：
// 四档 × 条件矩阵逐格断言、服务器级硬保护、forbidden 保守、门限单调性
// 与 EffectiveMode 解析顺序（会话覆盖 > 全局缺省 standard）。
package approvals

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"aiops/internal/risk"
	"aiops/internal/store"
)

// TestNeedsApprovalMatrix 逐格断言契约 2.11 约定 2 的写死矩阵
// （6 条件 × 4 档 = 24 格；standard 列=一至五期现行行为）。
func TestNeedsApprovalMatrix(t *testing.T) {
	cases := []struct {
		name string
		in   PolicyInput // Mode 由循环注入
		want map[store.ApprovalMode]bool
	}{
		{
			"safe, auto_exec=true",
			PolicyInput{AutoExecEnabled: true, Level: risk.LevelSafe},
			map[store.ApprovalMode]bool{store.ApprovalModeAll: true, store.ApprovalModeStandard: false, store.ApprovalModeHigh: false, store.ApprovalModeNone: false},
		},
		{
			"safe, auto_exec=false",
			PolicyInput{AutoExecEnabled: false, Level: risk.LevelSafe},
			map[store.ApprovalMode]bool{store.ApprovalModeAll: true, store.ApprovalModeStandard: true, store.ApprovalModeHigh: false, store.ApprovalModeNone: false},
		},
		{
			"needs_approval(warning), auto_exec=true",
			PolicyInput{AutoExecEnabled: true, Level: risk.LevelNeedsApproval, Severity: risk.SeverityWarning},
			map[store.ApprovalMode]bool{store.ApprovalModeAll: true, store.ApprovalModeStandard: true, store.ApprovalModeHigh: false, store.ApprovalModeNone: false},
		},
		{
			"needs_approval(warning), auto_exec=false",
			PolicyInput{AutoExecEnabled: false, Level: risk.LevelNeedsApproval, Severity: risk.SeverityWarning},
			map[store.ApprovalMode]bool{store.ApprovalModeAll: true, store.ApprovalModeStandard: true, store.ApprovalModeHigh: true, store.ApprovalModeNone: true},
		},
		{
			"needs_approval(high), auto_exec=true",
			PolicyInput{AutoExecEnabled: true, Level: risk.LevelNeedsApproval, Severity: risk.SeverityHigh},
			map[store.ApprovalMode]bool{store.ApprovalModeAll: true, store.ApprovalModeStandard: true, store.ApprovalModeHigh: true, store.ApprovalModeNone: false},
		},
		{
			"needs_approval(high), auto_exec=false",
			PolicyInput{AutoExecEnabled: false, Level: risk.LevelNeedsApproval, Severity: risk.SeverityHigh},
			map[store.ApprovalMode]bool{store.ApprovalModeAll: true, store.ApprovalModeStandard: true, store.ApprovalModeHigh: true, store.ApprovalModeNone: true},
		},
	}
	for _, c := range cases {
		for mode, want := range c.want {
			in := c.in
			in.Mode = mode
			if got := NeedsApproval(in); got != want {
				t.Errorf("matrix[%s][%s] = %v, want %v", c.name, mode, got, want)
			}
		}
	}
}

// TestNeedsApprovalHardProtection 服务器级硬保护（契约 2.11 约定 2）：
// auto_exec_enabled=false 时非只读命令无论门限一律审批。
func TestNeedsApprovalHardProtection(t *testing.T) {
	for _, mode := range []store.ApprovalMode{
		store.ApprovalModeAll, store.ApprovalModeStandard, store.ApprovalModeHigh, store.ApprovalModeNone,
	} {
		for _, sev := range []risk.Severity{risk.SeverityWarning, risk.SeverityHigh} {
			if !NeedsApproval(PolicyInput{Mode: mode, AutoExecEnabled: false, Level: risk.LevelNeedsApproval, Severity: sev}) {
				t.Errorf("hard protection violated: mode=%s severity=%s auto=false should require approval", mode, sev)
			}
		}
	}
}

// TestNeedsApprovalForbiddenConservative：level=forbidden 是契约违规调用，
// 实现按「审批」保守处理（契约 5.5 PolicyInput 注释）。
func TestNeedsApprovalForbiddenConservative(t *testing.T) {
	for _, mode := range []store.ApprovalMode{
		store.ApprovalModeNone, store.ApprovalModeHigh, store.ApprovalModeStandard, store.ApprovalModeAll,
	} {
		if !NeedsApproval(PolicyInput{Mode: mode, AutoExecEnabled: true, Level: risk.LevelForbidden}) {
			t.Errorf("forbidden level must be conservative (approval) under mode=%s", mode)
		}
	}
}

// TestNeedsApprovalMonotonicity 门限单调性：同一条件下审批面
// all ≥ standard ≥ high ≥ none（审批面只缩不涨——从 all 到 none 允许
// true→false，禁止 false→true）。
func TestNeedsApprovalMonotonicity(t *testing.T) {
	for _, level := range []risk.Level{risk.LevelSafe, risk.LevelNeedsApproval} {
		for _, auto := range []bool{true, false} {
			for _, sev := range []risk.Severity{risk.SeverityWarning, risk.SeverityHigh} {
				var prev bool = true // all 恒 true
				for _, mode := range []store.ApprovalMode{store.ApprovalModeAll, store.ApprovalModeStandard, store.ApprovalModeHigh, store.ApprovalModeNone} {
					cur := NeedsApproval(PolicyInput{Mode: mode, AutoExecEnabled: auto, Level: level, Severity: sev})
					if !prev && cur {
						t.Errorf("monotonicity violated: mode=%s level=%s auto=%v expanded approval surface", mode, level, auto)
					}
					prev = cur
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// EffectiveMode / GlobalMode / ResolveMode（契约 2.11 约定 1）
// ---------------------------------------------------------------------------

type fakeSettingStore struct {
	store.SettingStore
	items map[string]store.Setting
}

func (f *fakeSettingStore) Get(_ context.Context, key string) (store.Setting, error) {
	if s, ok := f.items[key]; ok {
		return s, nil
	}
	return store.Setting{}, store.ErrNotFound
}

func (f *fakeSettingStore) Set(_ context.Context, key string, value json.RawMessage, at time.Time) (store.Setting, error) {
	s := store.Setting{Key: key, Value: value, UpdatedAt: at}
	f.items[key] = s
	return s, nil
}

type modeStore struct {
	store.Store
	settings *fakeSettingStore
}

func (m *modeStore) Settings() store.SettingStore { return m.settings }

func setMode(t *testing.T, st *modeStore, mode string) {
	t.Helper()
	raw, err := json.Marshal(mode)
	if err != nil {
		t.Fatal(err)
	}
	st.settings.items["approval.mode"] = store.Setting{Key: "approval.mode", Value: raw}
}

func TestGlobalModeFallbacks(t *testing.T) {
	ctx := context.Background()
	// 键缺失 → standard。
	st := &modeStore{settings: &fakeSettingStore{items: map[string]store.Setting{}}}
	if m, err := GlobalMode(ctx, st); err != nil || m != store.ApprovalModeStandard {
		t.Fatalf("GlobalMode missing key = %v,%v want standard", m, err)
	}
	// 值非法（非 JSON 字符串 / 非法枚举）→ standard。
	for _, bad := range []string{`"fast"`, `42`, `{"a":1}`, `""`} {
		st := &modeStore{settings: &fakeSettingStore{items: map[string]store.Setting{}}}
		st.settings.items["approval.mode"] = store.Setting{Key: "approval.mode", Value: json.RawMessage(bad)}
		if m, err := GlobalMode(ctx, st); err != nil || m != store.ApprovalModeStandard {
			t.Fatalf("GlobalMode(%s) = %v,%v want standard", bad, m, err)
		}
	}
	// 合法值原样返回。
	for _, ok := range []string{"all", "high", "none"} {
		st := &modeStore{settings: &fakeSettingStore{items: map[string]store.Setting{}}}
		setMode(t, st, ok)
		got, err := GlobalMode(ctx, st)
		if err != nil || got != store.ApprovalMode(ok) {
			t.Fatalf("GlobalMode(%q) = %v,%v", ok, got, err)
		}
	}
	// nil store 兜底 standard 不报错。
	if m, err := GlobalMode(ctx, nil); err != nil || m != store.ApprovalModeStandard {
		t.Fatalf("GlobalMode(nil store) = %v,%v want standard", m, err)
	}
}

func TestEffectiveModePrecedence(t *testing.T) {
	ctx := context.Background()
	// 覆盖优先于全局。
	st := &modeStore{settings: &fakeSettingStore{items: map[string]store.Setting{}}}
	setMode(t, st, "none")
	high := store.ApprovalModeHigh
	conv := store.Conversation{ApprovalMode: &high}
	if m, err := EffectiveMode(ctx, st, conv); err != nil || m != store.ApprovalModeHigh {
		t.Fatalf("EffectiveMode override = %v,%v want high", m, err)
	}
	// 无覆盖取全局。
	st2 := &modeStore{settings: &fakeSettingStore{items: map[string]store.Setting{}}}
	setMode(t, st2, "all")
	if m, err := EffectiveMode(ctx, st2, store.Conversation{}); err != nil || m != store.ApprovalModeAll {
		t.Fatalf("EffectiveMode global = %v,%v want all", m, err)
	}
	// 无覆盖且键缺失 → standard。
	st3 := &modeStore{settings: &fakeSettingStore{items: map[string]store.Setting{}}}
	if m, err := EffectiveMode(ctx, st3, store.Conversation{}); err != nil || m != store.ApprovalModeStandard {
		t.Fatalf("EffectiveMode default = %v,%v want standard", m, err)
	}
	// 覆盖非法值 → 回落全局。
	st4 := &modeStore{settings: &fakeSettingStore{items: map[string]store.Setting{}}}
	setMode(t, st4, "high")
	bad := store.ApprovalMode("fast")
	if m, err := EffectiveMode(ctx, st4, store.Conversation{ApprovalMode: &bad}); err != nil || m != store.ApprovalModeHigh {
		t.Fatalf("EffectiveMode invalid override = %v,%v want high(global)", m, err)
	}
}

func TestResolveModeBatch(t *testing.T) {
	all := store.ApprovalModeAll
	cases := []struct {
		conv   store.Conversation
		global store.ApprovalMode
		want   store.ApprovalMode
	}{
		{store.Conversation{ApprovalMode: &all}, store.ApprovalModeNone, store.ApprovalModeAll},
		{store.Conversation{}, store.ApprovalModeNone, store.ApprovalModeNone},
		{store.Conversation{}, store.ApprovalModeHigh, store.ApprovalModeHigh},
		{store.Conversation{}, "", store.ApprovalModeStandard},
	}
	for i, c := range cases {
		if got := ResolveMode(c.conv, c.global); got != c.want {
			t.Errorf("ResolveMode[%d] = %v, want %v", i, got, c.want)
		}
	}
}
