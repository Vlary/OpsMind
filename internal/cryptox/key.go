package cryptox

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const masterKeyFile = "master.key"

// LoadMasterKey 加载或生成 32 字节主密钥。
//
// 顺序与契约一致：
//  1. envValue 非空时按标准 base64 解码（允许首尾空白）；
//  2. 否则读取 <dataDir>/master.key；
//  3. 文件不存在时以 O_EXCL 创建，内容为 base64 + 换行，权限 0600。
//
// 已存在的 keyfile 权限必须恰好为 0600；不自动 chmod。
func LoadMasterKey(dataDir, envValue string) ([]byte, error) {
	if v := strings.TrimSpace(envValue); v != "" {
		return decodeMasterKey(v)
	}

	if strings.TrimSpace(dataDir) == "" {
		dataDir = "."
	}
	path := filepath.Join(dataDir, masterKeyFile)

	key, err := readMasterKeyFile(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("cryptox: create data dir: %w", err)
	}

	key = make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("cryptox: generate master key: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key) + "\n"

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			// 并发启动时另一个进程已创建；重新读取它。
			return readMasterKeyFile(path)
		}
		return nil, fmt.Errorf("cryptox: create master key file: %w", err)
	}
	if _, err := f.WriteString(encoded); err != nil {
		_ = f.Close()
		// 写入失败时移除半成品，避免下次读到损坏密钥。
		_ = os.Remove(path)
		return nil, fmt.Errorf("cryptox: write master key file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("cryptox: close master key file: %w", err)
	}
	return key, nil
}

func readMasterKeyFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("cryptox: %s must have permissions 0600", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cryptox: read master key file: %w", err)
	}
	return decodeMasterKey(strings.TrimSpace(string(raw)))
}

func decodeMasterKey(encoded string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("cryptox: master key is not valid base64")
	}
	if len(key) != keySize {
		return nil, ErrInvalidMasterKey
	}
	return key, nil
}
