// parse.go 是 docker/podman CLI JSON 输出到契约结构体的唯一映射实现
// （契约 5.2C 字段映射表，依据四期调研实测写死）。
//
// 防 OOM：入口 checkParseSize 限 256KB；JSONL 行数与数组条目数限
// maxItems；所有 []string/[]T 输出不为 nil（空为 []）。
package containerx

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// dockerCreatedAtLayout 是 docker ps/images --format json 的 CreatedAt 布局
// （如 "2026-10-26 23:36:05 +0800 CST"）。
const dockerCreatedAtLayout = "2006-01-02 15:04:05 -0700 MST"

// ---------------------------------------------------------------------------
// docker list（JSONL 逐行对象）
// ---------------------------------------------------------------------------

type dockerListRow struct {
	ID        string `json:"ID"`
	Names     string `json:"Names"`
	Image     string `json:"Image"`
	State     string `json:"State"`
	Status    string `json:"Status"`
	Ports     string `json:"Ports"`
	CreatedAt string `json:"CreatedAt"`
}

func parseDockerList(output string) ([]Container, error) {
	if len(output) > maxParseBytes {
		return nil, fmt.Errorf("containerx: parse input exceeds %d bytes", maxParseBytes)
	}
	// 通道层把 stdout/stderr 合并进 Combined：docker CLI 在 cgroup v1 等
	// 主机会向 stderr 写 WARNING 行，混入后逐行 JSON 解析会整体失败
	//（评审实测）。先剥离非 JSON 行（告警/空行），只解析 { 或 [ 开头的行。
	jsonLines := []string{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") {
			jsonLines = append(jsonLines, line)
		} else if strings.HasSuffix(line, "]") && !strings.HasPrefix(line, "[") {
			// 多行 JSON 数组的闭合行（如单独的 "]"）。
			jsonLines = append(jsonLines, line)
		}
	}
	// docker CLI <25.0 的 `ps --format json` 输出单个 JSON 数组而非逐行
	// JSONL（moby/moby#46906，25.0 起改逐行）：首个 JSON 行以 [ 开头即
	// 按数组整体解析（数组本身跨多行，不能逐行拆）——评审发现 20.10/
	// 24.x 存量主机整块 CONTAINER_CLI_FAILED。
	if len(jsonLines) > 0 && strings.HasPrefix(jsonLines[0], "[") {
		var rows []dockerListRow
		if err := json.Unmarshal([]byte(strings.Join(jsonLines, "")), &rows); err != nil {
			return nil, fmt.Errorf("containerx: invalid docker ps JSON array: %w", err)
		}
		if len(rows) > maxItems {
			return nil, fmt.Errorf("containerx: container list exceeds %d items", maxItems)
		}
		items := []Container{}
		for _, row := range rows {
			items = append(items, dockerRowToContainer(row))
		}
		return items, nil
	}
	items := []Container{}
	for _, line := range jsonLines {
		if len(items) >= maxItems {
			return nil, fmt.Errorf("containerx: container list exceeds %d items", maxItems)
		}
		var row dockerListRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			// 非预期输出失败关闭，不猜测字段。
			return nil, fmt.Errorf("containerx: invalid docker ps JSONL row: %w", err)
		}
		items = append(items, dockerRowToContainer(row))
	}
	return items, nil
}

// dockerRowToContainer 映射 dockerListRow 为对外 Container。
func dockerRowToContainer(row dockerListRow) Container {
	return Container{
		ID:      row.ID,
		Name:    row.Names,
		Image:   row.Image,
		State:   row.State,
		Status:  row.Status,
		Ports:   splitPorts(row.Ports),
		Created: formatDockerCreatedAt(row.CreatedAt),
	}
}

// splitPorts 把 docker 的 Ports 字符串按空白拆分为数组（清理项尾逗号——
// docker 的 Ports 串形如 "0.0.0.0:5432->5432/tcp, [::]:5432->5432/tcp"）。
func splitPorts(s string) []string {
	out := []string{}
	for _, f := range strings.Fields(s) {
		f = strings.TrimSuffix(f, ",")
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// formatDockerCreatedAt 解析 docker CreatedAt；成功转 RFC3339（UTC），
// 失败保留原样串（契约 5.2C）。
func formatDockerCreatedAt(s string) string {
	if s == "" {
		return ""
	}
	t, err := time.Parse(dockerCreatedAtLayout, s)
	if err != nil {
		return s
	}
	return t.UTC().Format(time.RFC3339)
}

// ---------------------------------------------------------------------------
// podman list（缩进 JSON 数组）
// ---------------------------------------------------------------------------

type podmanListRow struct {
	ID      string          `json:"Id"` // 注意大小写
	Names   []string        `json:"Names"`
	Image   string          `json:"Image"`
	State   string          `json:"State"`
	Status  string          `json:"Status"`
	Ports   json.RawMessage `json:"Ports"`     // 字段形态未实测：缺失/类型不符时输出 []，不报错
	Created json.RawMessage `json:"CreatedAt"` // 形态未定（相对时间串/epoch 数）——契约：列表 created 恒空串
	IsInfra bool            `json:"IsInfra"`
}

func parsePodmanList(output string) ([]Container, error) {
	if strings.TrimSpace(output) == "null" {
		// podman 异常输出字面 null：json.Unmarshal 到 slice「成功」但
		// 语义是缺数据——按解析失败处理，不得静默当空列表（评审发现）。
		return nil, fmt.Errorf("containerx: podman ps returned null")
	}
	var rows []podmanListRow
	if err := json.Unmarshal([]byte(output), &rows); err != nil {
		// 单对象形态防御（podman 部分版本对单容器输出对象而非数组）。
		var single podmanListRow
		if err2 := json.Unmarshal([]byte(output), &single); err2 != nil || len(output) == 0 || !strings.HasPrefix(strings.TrimSpace(output), "{") {
			return nil, fmt.Errorf("containerx: invalid podman ps JSON: %w", err)
		}
		rows = []podmanListRow{single}
	}
	items := []Container{}
	for _, row := range rows {
		if len(items) >= maxItems {
			return nil, fmt.Errorf("containerx: container list exceeds %d items", maxItems)
		}
		if row.IsInfra {
			continue // podman infra 容器（契约 5.2C：丢弃）
		}
		name := ""
		if len(row.Names) > 0 {
			name = row.Names[0]
		}
		items = append(items, Container{
			ID:      row.ID,
			Name:    name,
			Image:   row.Image,
			State:   row.State,
			Status:  row.Status,
			Ports:   podmanPorts(row.Ports),
			Created: "", // podman 列表 CreatedAt 为相对时间（"2 seconds ago"）不可解析，恒空串
		})
	}
	return items, nil
}

// podmanPorts 对未实测的 Ports 字段失败关闭为 []（不报错）。
func podmanPorts(raw json.RawMessage) []string {
	out := []string{}
	if len(raw) == 0 {
		return out
	}
	// 仅接受字符串数组形态；任何其他形态（对象/嵌套）一律 []。
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		return out
	}
	for _, s := range arr {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// inspect（docker/podman 结构兼容路径）
// ---------------------------------------------------------------------------

type inspectHealth struct {
	Status string `json:"Status"`
}

type inspectState struct {
	Status     string         `json:"Status"`
	ExitCode   int            `json:"ExitCode"`
	Pid        int            `json:"Pid"`
	StartedAt  string         `json:"StartedAt"`
	FinishedAt string         `json:"FinishedAt"`
	Health     *inspectHealth `json:"Health"`
}

type inspectMount struct {
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          *bool  `json:"RW"`
}

type inspectRow struct {
	Name    string `json:"Name"`
	Created string `json:"Created"`
	Config  struct {
		Image string `json:"Image"`
	} `json:"Config"`
	State  inspectState   `json:"State"`
	Mounts []inspectMount `json:"Mounts"`
}

func parseInspect(output string) (ContainerDetail, error) {
	trimmed := strings.TrimSpace(output)
	var rows []inspectRow
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		return ContainerDetail{}, fmt.Errorf("containerx: invalid inspect JSON: %w", err)
	}
	if len(rows) == 0 {
		return ContainerDetail{}, ErrContainerNotFound
	}
	row := rows[0]
	detail := ContainerDetail{
		Container: Container{
			ID:      "",
			Name:    strings.TrimPrefix(row.Name, "/"),
			Image:   row.Config.Image,
			State:   row.State.Status,
			Status:  row.State.Status, // inspect 无独立 Status
			Ports:   []string{},
			Created: row.Created, // RFC3339 原样
		},
		ExitCode:     row.State.ExitCode,
		PID:          row.State.Pid,
		StartedAt:    zeroTimeToEmpty(row.State.StartedAt),
		FinishedAt:   zeroTimeToEmpty(row.State.FinishedAt),
		HealthStatus: "",
		Mounts:       []Mount{},
	}
	if row.State.Health != nil {
		detail.HealthStatus = row.State.Health.Status
	}
	for _, m := range row.Mounts {
		if len(detail.Mounts) >= maxItems {
			break
		}
		rw := m.RW != nil && *m.RW
		detail.Mounts = append(detail.Mounts, Mount{Source: m.Source, Destination: m.Destination, RW: rw})
	}
	return detail, nil
}

// zeroTimeToEmpty 把 docker/podman 的零值时间（"0001-01-01T00:00:00Z" 及
// 变体、空串）转为空串；其余 RFC3339 原样保留。
func zeroTimeToEmpty(s string) string {
	if s == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, s)
	if err == nil && t.Year() <= 1 {
		return ""
	}
	return s
}

// ---------------------------------------------------------------------------
// images
// ---------------------------------------------------------------------------

type dockerImageRow struct {
	Repository string `json:"Repository"`
	Tag        string `json:"Tag"`
	Size       string `json:"Size"`
	CreatedAt  string `json:"CreatedAt"`
}

func parseDockerImages(output string) ([]Image, error) {
	items := []Image{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(items) >= maxItems {
			return nil, fmt.Errorf("containerx: image list exceeds %d items", maxItems)
		}
		var row dockerImageRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("containerx: invalid docker image JSONL row: %w", err)
		}
		items = append(items, Image{
			Repo:      row.Repository,
			Tag:       row.Tag,
			SizeBytes: parseHumanSize(row.Size),
			Created:   formatDockerCreatedAt(row.CreatedAt),
		})
	}
	return items, nil
}

// parseHumanSize 按 1000 进制解析 docker CLI 的 human size（"67.5MB"、
// "1.2GB"；docker CLI 为 SI 单位）；解析失败 0（契约 5.2C）。
func parseHumanSize(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	unit := ""
	numPart := s
	for i := len(s) - 1; i >= 0; i-- {
		c := s[i]
		if c >= '0' && c <= '9' || c == '.' {
			numPart = s[:i+1]
			unit = strings.TrimSpace(s[i+1:])
			break
		}
		if i == 0 {
			return 0
		}
	}
	if numPart == "" || unit == "" {
		return 0
	}
	v, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0
	}
	mult := float64(1)
	switch strings.ToUpper(unit) {
	case "B":
		mult = 1
	case "KB", "K":
		mult = 1e3
	case "MB", "M":
		mult = 1e6
	case "GB", "G":
		mult = 1e9
	case "TB", "T":
		mult = 1e12
	default:
		return 0
	}
	return int64(v * mult)
}

type podmanImageRow struct {
	RepoTags []string `json:"RepoTags"`
	Size     int64    `json:"Size"` // 整数字节，直接采用
	Created  int64    `json:"Created"`
}

func parsePodmanImages(output string) ([]Image, error) {
	var rows []podmanImageRow
	if err := json.Unmarshal([]byte(output), &rows); err != nil {
		return nil, fmt.Errorf("containerx: invalid podman image JSON: %w", err)
	}
	items := []Image{}
	for _, row := range rows {
		if len(items) >= maxItems {
			return nil, fmt.Errorf("containerx: image list exceeds %d items", maxItems)
		}
		if len(row.RepoTags) == 0 {
			// RepoTags 为 null/空数组：输出一行 repo=<none>,tag=<none>。
			items = append(items, Image{Repo: "<none>", Tag: "<none>", SizeBytes: row.Size, Created: epochToRFC3339(row.Created)})
			continue
		}
		for _, tag := range row.RepoTags {
			if len(items) >= maxItems {
				break
			}
			repo, t := splitRepoTag(tag)
			items = append(items, Image{Repo: repo, Tag: t, SizeBytes: row.Size, Created: epochToRFC3339(row.Created)})
		}
	}
	return items, nil
}

// splitRepoTag 按最后一个 ":" 拆 repo:tag（无冒号则 tag="<none>"）。
func splitRepoTag(s string) (string, string) {
	idx := strings.LastIndex(s, ":")
	if idx < 0 {
		return s, "<none>"
	}
	return s[:idx], s[idx+1:]
}

func epochToRFC3339(sec int64) string {
	if sec == 0 {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}
