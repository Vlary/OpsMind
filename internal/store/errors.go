package store

import "errors"

var (
	// ErrNotFound 表示资源不存在（pgx.ErrNoRows 的统一映射）。
	ErrNotFound = errors.New("store: not found")
	// ErrConflict 表示唯一约束或状态冲突。
	ErrConflict = errors.New("store: conflict")
	// ErrInvalid 表示调用参数不合法。
	ErrInvalid = errors.New("store: invalid")
	// ErrTxUnsupported 表示嵌套事务。
	ErrTxUnsupported = errors.New("store: nested transaction unsupported")
)
