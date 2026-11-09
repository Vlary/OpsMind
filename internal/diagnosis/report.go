// report.go 定义诊断报告 schema（契约 5.16：M2 唯一定义处，OpenAPI 同形）
// 与其校验/截断/固化规则（契约 11 节 #8）：evidence ≤20 条、excerpt ≤4KB/条、
// suggestions ≤10 条、整体序列化 ≤64KB（超限截 evidence 并保留 suggestions）；
// forbidden 的建议不得给出——引擎过滤。
package diagnosis

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"aiops/internal/risk"
)

// 契约 11 节 #8 写死的报告体积上限（常量，不进用户配置）。
const (
	MaxEvidenceCount  = 20
	MaxExcerptBytes   = 4096
	MaxSuggestionCnt  = 10
	ReportMaxBytes    = 65536
	maxSummaryRunes   = 2048
	maxRootCauseRunes = 8192
	maxNoteRunes      = 1024
)

// DiagnosisReport 是 report jsonb 的 schema。
type DiagnosisReport struct {
	Summary     string              `json:"summary"`     // 一段结论（≤2KB）
	Severity    string              `json:"severity"`    // info|warning|critical（诊断判定）
	RootCause   string              `json:"root_cause"`  // 原因分析（≤8KB）
	Evidence    []DiagnosisEvidence `json:"evidence"`    // 证据列表
	Suggestions []FixSuggestion     `json:"suggestions"` // 建议修复命令列表
	Confidence  string              `json:"confidence"`  // low|medium|high
}

// DiagnosisEvidence 是单条证据。
type DiagnosisEvidence struct {
	Source        string `json:"source"`            // command|metric|alert|log
	Command       string `json:"command,omitempty"` // source=command 时
	OutputExcerpt string `json:"output_excerpt"`    // ≤4KB
	Note          string `json:"note"`
}

// FixSuggestion 是单条修复建议（fix 端点要求 command 逐字节相等）。
type FixSuggestion struct {
	Command   string `json:"command"` // 修复命令
	Reason    string `json:"reason"`
	RiskLevel string `json:"risk_level"` // 由 risk.Classify 预估（safe|needs_approval）
}

// ErrReportInvalid 表示模型输出不可解析为合法报告。
var ErrReportInvalid = errors.New("diagnosis: invalid report payload")

// ParseReport 严格解析模型输出为报告：输入超 ReportMaxBytes、非法 JSON、
// 未知字段、枚举越界均拒绝（未知字段拒绝——契约 5.16）。
func ParseReport(raw []byte) (DiagnosisReport, error) {
	if len(raw) > ReportMaxBytes {
		return DiagnosisReport{}, fmt.Errorf("%w: payload exceeds %d bytes", ErrReportInvalid, ReportMaxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r DiagnosisReport
	if err := dec.Decode(&r); err != nil {
		return DiagnosisReport{}, fmt.Errorf("%w: %v", ErrReportInvalid, err)
	}
	if err := dec.Decode(&struct{}{}); err == nil {
		return DiagnosisReport{}, fmt.Errorf("%w: trailing data", ErrReportInvalid)
	}
	switch r.Severity {
	case "info", "warning", "critical":
	default:
		return DiagnosisReport{}, fmt.Errorf("%w: severity must be info|warning|critical", ErrReportInvalid)
	}
	switch r.Confidence {
	case "low", "medium", "high":
	default:
		return DiagnosisReport{}, fmt.Errorf("%w: confidence must be low|medium|high", ErrReportInvalid)
	}
	if strings.TrimSpace(r.Summary) == "" {
		return DiagnosisReport{}, fmt.Errorf("%w: summary is required", ErrReportInvalid)
	}
	for _, ev := range r.Evidence {
		switch ev.Source {
		case "command", "metric", "alert", "log":
		default:
			return DiagnosisReport{}, fmt.Errorf("%w: evidence source must be command|metric|alert|log", ErrReportInvalid)
		}
	}
	return r, nil
}

// Normalize 就地截断越界字段并过滤 forbidden 建议（risk_level 由
// risk.Classify 预估重算——模型自报不可信）。
func (r *DiagnosisReport) Normalize(os risk.OS) {
	r.Summary = truncateRunes(r.Summary, maxSummaryRunes)
	r.RootCause = truncateRunes(r.RootCause, maxRootCauseRunes)
	if len(r.Evidence) > MaxEvidenceCount {
		r.Evidence = r.Evidence[:MaxEvidenceCount]
	}
	for i := range r.Evidence {
		r.Evidence[i].OutputExcerpt = truncateBytes(r.Evidence[i].OutputExcerpt, MaxExcerptBytes)
		r.Evidence[i].Note = truncateRunes(r.Evidence[i].Note, maxNoteRunes)
		if r.Evidence[i].Source != "command" {
			r.Evidence[i].Command = ""
		}
	}
	kept := make([]FixSuggestion, 0, len(r.Suggestions))
	for _, s := range r.Suggestions {
		if strings.TrimSpace(s.Command) == "" {
			continue
		}
		s.Command = truncateBytes(s.Command, 16384) // 与全局命令上限一致（agent.MaxCommandBytes）
		s.Reason = truncateRunes(s.Reason, maxNoteRunes)
		// forbidden 的建议不得给出（契约 5.16 FixSuggestion.RiskLevel 注释）；
		// risk_level 由引擎重算（模型自报不可信）。
		switch level := risk.Classify(s.Command, os).Level; level {
		case risk.LevelForbidden:
			continue
		case risk.LevelSafe:
			s.RiskLevel = string(level)
		default:
			s.RiskLevel = string(risk.LevelNeedsApproval)
		}
		kept = append(kept, s)
		if len(kept) >= MaxSuggestionCnt {
			break
		}
	}
	r.Suggestions = kept
}

// Serialize 固化为 report jsonb：整体不超过 ReportMaxBytes，超限逐步丢弃
// evidence（保留 suggestions——修复建议优先于证据正文）。
func (r DiagnosisReport) Serialize() (json.RawMessage, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	for len(raw) > ReportMaxBytes && len(r.Evidence) > 0 {
		r.Evidence = r.Evidence[:len(r.Evidence)/2]
		if len(r.Evidence) == 1 {
			r.Evidence = r.Evidence[:0]
		}
		raw, err = json.Marshal(r)
		if err != nil {
			return nil, err
		}
	}
	if len(raw) > ReportMaxBytes {
		// suggestions 也超限（极端）：清空证据后仍超——截断 root_cause 兜底。
		r.Evidence = nil
		r.RootCause = truncateBytes(r.RootCause, maxRootCauseRunes)
		raw, err = json.Marshal(r)
		if err != nil {
			return nil, err
		}
	}
	if len(raw) > ReportMaxBytes {
		return nil, fmt.Errorf("%w: serialized report exceeds %d bytes", ErrReportInvalid, ReportMaxBytes)
	}
	return json.RawMessage(raw), nil
}

// SuggestionCommands 返回建议命令列表（fix 端点逐字节比对用）。
func (r DiagnosisReport) SuggestionCommands() []string {
	out := make([]string, 0, len(r.Suggestions))
	for _, s := range r.Suggestions {
		out = append(out, s.Command)
	}
	return out
}

// truncateRunes 按 rune 截断（不切多字节字符）。
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max])
}

// truncateBytes 按字节截断并向后退到 UTF-8 边界。
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
