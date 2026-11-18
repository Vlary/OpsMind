// anchor.go 实现审计链的外部 anchor 导出（契约 12.3 第 19 条 / 12.4 第 6 条）：
// 「外部 anchor 由独立 OS 用户/只读 DB 角色定期写文件」。
//
// 导出的是链头锚点（head id + head hash + 行数），不是全量审计——全量导出
// 会随审计增长无界膨胀，而链头哈希已足以在离线侧证明「此后未被整体重写」。
// 文件 0600、只追加（每次一行 JSON），由 cron/systemd timer 以独立账号调用
// `aiops audit anchor --out <file>`。
package offline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"aiops/internal/store"
)

// AnchorRecord 是一行链头锚点。
type AnchorRecord struct {
	ExportedAt time.Time `json:"exported_at"`
	HeadID     int64     `json:"head_id"`
	HeadHash   string    `json:"head_hash"`
	Rows       int64     `json:"rows"`
	Legacy     int64     `json:"legacy_rows"`
}

// maxAnchorScan 是一次导出最多扫描的行数（防 OOM 铁律：写死上限）。取 50 万
// 行已覆盖数年中小规模部署；超出时保留已扫描部分的链头并在输出里注明截断。
const maxAnchorScan = 500_000

// anchorPageSize 是逐页推进的页大小（硬上限）。
const anchorPageSize = 1000

// ExportAnchor 扫描审计链并返回链头锚点。
//
// 逐页 ListAuditAsc 推进（单页 1000、总量 50 万上限），不在内存中保留全量
// 行；同时统计 legacy 行数（00014 前存量行只校验链接）。
func ExportAnchor(ctx context.Context, st store.Store) (AnchorRecord, error) {
	vs := store.VerifyStoreOf(st)
	if vs == nil {
		return AnchorRecord{}, fmt.Errorf("anchor: store does not support ascending audit scan")
	}
	rec := AnchorRecord{ExportedAt: time.Now().UTC()}
	var afterID int64
	for scanned := 0; scanned < maxAnchorScan; {
		page, err := vs.ListAuditAsc(ctx, afterID, anchorPageSize)
		if err != nil {
			return AnchorRecord{}, err
		}
		if len(page) == 0 {
			break
		}
		for _, row := range page {
			rec.HeadID = row.ID
			rec.HeadHash = row.Hash
			rec.Rows++
			if row.LegacyHash {
				rec.Legacy++
			}
			afterID = row.ID
		}
		scanned += len(page)
		if len(page) < anchorPageSize {
			break
		}
	}
	return rec, nil
}

// WriteAnchor 把锚点追加到 out（每次一行 JSON）。目录必须已存在；文件以
// 0600 打开、O_APPEND 写入，不跟随符号链接（O_NOFOLLOW）。
func WriteAnchor(out string, rec AnchorRecord) error {
	if out == "" {
		return fmt.Errorf("anchor: output path is required")
	}
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	// 不跟随符号链接：先 Lstat，存在且是链接/非普通文件即拒绝（跨平台可用的
	// 等价 O_NOFOLLOW；Linux 上 O_NOFOLLOW 不在 os 包常量的通用集合里）。
	clean := filepath.Clean(out)
	if info, statErr := os.Lstat(clean); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("anchor: output path must be a regular file")
		}
	}
	f, err := os.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(payload, '\n')); err != nil {
		return err
	}
	return f.Sync()
}
