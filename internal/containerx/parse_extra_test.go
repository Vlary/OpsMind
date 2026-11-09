package containerx

import "testing"

// docker CLI <25.0 输出单个 JSON 数组（moby/moby#46906），必须整块解析
// 而非逐行（评审发现 20.10/24.x 存量主机整块失败）。
func TestParseDockerListJSONArray(t *testing.T) {
	items, err := parseDockerList("[\n {\"ID\":\"a\",\"Names\":\"pg\",\"Image\":\"pg:17\",\"State\":\"running\",\"Status\":\"Up\",\"Ports\":\"\",\"CreatedAt\":\"2026-10-26 23:36:05 +0800 CST\"}\n]\n")
	if err != nil {
		t.Fatalf("array form: %v", err)
	}
	if len(items) != 1 || items[0].ID != "a" || items[0].Name != "pg" {
		t.Fatalf("items = %+v", items)
	}
	// JSONL（25.0+）保持兼容。
	items, err = parseDockerList("{\"ID\":\"b\"}\n{\"ID\":\"c\"}\n")
	if err != nil || len(items) != 2 {
		t.Fatalf("jsonl form: %v %+v", err, items)
	}
}

// 通道层合并 stdout/stderr：cgroup v1 主机的 WARNING 行不得使解析整体
// 失败（评审发现）。
func TestParseDockerListSkipsStderrLines(t *testing.T) {
	items, err := parseDockerList("WARNING: Your kernel does not support cgroup blkio weight\n{\"ID\":\"a\"}\n")
	if err != nil {
		t.Fatalf("stderr line: %v", err)
	}
	if len(items) != 1 || items[0].ID != "a" {
		t.Fatalf("items = %+v", items)
	}
}

// podman 字面 null 是异常输出：按失败处理，不静默当空列表（评审发现）。
func TestParsePodmanListNullFails(t *testing.T) {
	if _, err := parsePodmanList("null\n"); err == nil {
		t.Fatal("literal null must be an error, not empty list")
	}
}
