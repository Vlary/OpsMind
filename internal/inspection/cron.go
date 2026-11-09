// cron.go 实现五段标准 cron 解析与下一触发时刻计算（契约 5.17）：
// 分 时 日 月 周（UTC；支持 * / - ,）。非法表达式返回 ErrCronInvalid
// （HTTP 400 CRON_INVALID）。NextRun 用逐分钟推进 + 迭代上限（永不匹配
// 的表达式如「2 月 30 日」有界终止——防 OOM 铁律的终止条件）。
package inspection

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// ErrCronInvalid 表示 cron 表达式非法。
var ErrCronInvalid = errors.New("inspection: invalid cron expression")

// nextRunSearchLimit 是 NextRun 的推进上限（4 年的分钟数）：超限视为
// 永不匹配（非法组合），返回 ErrCronInvalid——终止条件。
const nextRunSearchLimit = 4 * 366 * 24 * 60

// cronField 是单个字段的解析结果：bits[min..max] 布尔集合。
type cronField struct {
	bits [60]bool // 日/月字段只用到 32/13 位；统一 60 上限简化实现
}

// parseCronField 解析单字段：* 、*/n、a-b、a-b/n、a,b,c、数字。
func parseCronField(field string, min, max int) (cronField, error) {
	var f cronField
	if field == "" {
		return f, ErrCronInvalid
	}
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return f, ErrCronInvalid
		}
		step := 1
		rangeLo, rangeHi := min, max
		body := part
		if i := strings.IndexByte(part, '/'); i >= 0 {
			body = part[:i]
			s, err := strconv.Atoi(part[i+1:])
			if err != nil || s < 1 || s > max {
				return f, ErrCronInvalid
			}
			step = s
		}
		switch {
		case body == "*":
			// 缺省全范围
		case strings.ContainsRune(body, '-'):
			lo, hi, err := parseRange(body, min, max)
			if err != nil {
				return f, err
			}
			rangeLo, rangeHi = lo, hi
		default:
			v, err := strconv.Atoi(body)
			if err != nil {
				return f, ErrCronInvalid
			}
			if v < min || v > max {
				return f, ErrCronInvalid
			}
			rangeLo, rangeHi = v, v
		}
		for v := rangeLo; v <= rangeHi; v += step {
			f.bits[v] = true
		}
	}
	return f, nil
}

func parseRange(body string, min, max int) (int, int, error) {
	loStr, hiStr, ok := strings.Cut(body, "-")
	if !ok {
		return 0, 0, ErrCronInvalid
	}
	lo, err1 := strconv.Atoi(loStr)
	hi, err2 := strconv.Atoi(hiStr)
	if err1 != nil || err2 != nil {
		return 0, 0, ErrCronInvalid
	}
	if lo < min || hi > max || lo > hi {
		return 0, 0, ErrCronInvalid
	}
	return lo, hi, nil
}

// cronSchedule 是解析后的五字段调度。
type cronSchedule struct {
	minute, hour, dom, month, dow cronField
}

// parseCron 解析五段表达式（契约：五段标准 cron，UTC）。
func parseCron(expr string) (cronSchedule, error) {
	var s cronSchedule
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return s, ErrCronInvalid
	}
	var err error
	if s.minute, err = parseCronField(fields[0], 0, 59); err != nil {
		return s, err
	}
	if s.hour, err = parseCronField(fields[1], 0, 23); err != nil {
		return s, err
	}
	if s.dom, err = parseCronField(fields[2], 1, 31); err != nil {
		return s, err
	}
	if s.month, err = parseCronField(fields[3], 1, 12); err != nil {
		return s, err
	}
	// 周字段 0..7（0 与 7 都是周日——cron 传统），存储归一到 0..6。
	dow, err := parseCronField(fields[4], 0, 7)
	if err != nil {
		return s, err
	}
	if dow.bits[7] {
		dow.bits[7] = false
		dow.bits[0] = true
	}
	s.dow = dow
	return s, nil
}

// matches 判断时刻是否命中调度（周按 Go 约定：周日=0）。
func (s cronSchedule) matches(t time.Time) bool {
	if !s.minute.bits[t.Minute()] || !s.hour.bits[t.Hour()] {
		return false
	}
	if !s.month.bits[int(t.Month())] {
		return false
	}
	domHit := s.dom.bits[t.Day()]
	dowHit := s.dow.bits[int(t.Weekday())]
	// 标准 cron 语义：日与周都为 * 时取并集之任一；其一受限时取交集。
	domRestricted := !fullRange(s.dom, 1, 31)
	dowRestricted := !fullRange(s.dow, 0, 6)
	switch {
	case domRestricted && dowRestricted:
		return domHit || dowHit
	case domRestricted:
		return domHit
	case dowRestricted:
		return dowHit
	default:
		return true
	}
}

func fullRange(f cronField, min, max int) bool {
	for v := min; v <= max; v++ {
		if !f.bits[v] {
			return false
		}
	}
	return true
}

// NextRun 解析五段 cron（分 时 日 月 周，UTC；支持 * / - ,）并返回 t 之后
// 的下一触发时刻。非法表达式（含永不匹配的组合）返回 ErrCronInvalid。
func NextRun(expr string, t time.Time) (time.Time, error) {
	s, err := parseCron(expr)
	if err != nil {
		return time.Time{}, err
	}
	start := t.UTC().Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < nextRunSearchLimit; i++ {
		candidate := start.Add(time.Duration(i) * time.Minute)
		if s.matches(candidate) {
			return candidate, nil
		}
	}
	return time.Time{}, ErrCronInvalid
}
