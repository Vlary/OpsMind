package dbproxy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/go-mssqldb/msdsn"

	"aiops/internal/sqlguard"
	"aiops/internal/store"
)

func TestMSSQLEncrypt(t *testing.T) {
	cases := map[store.SSLMode]string{
		store.SSLModeDisable:    "false",
		store.SSLModePrefer:     "false", // 契约：prefer 降级为不加密
		store.SSLModeRequire:    "true",  // + TrustServerCertificate
		store.SSLModeVerifyCA:   "true",  // 系统信任链
		store.SSLModeVerifyFull: "true",
		"":                      "false",
	}
	for mode, want := range cases {
		if got := mssqlEncrypt(mode); got != want {
			t.Errorf("mssqlEncrypt(%q) = %q, want %q", mode, got, want)
		}
	}
}

// TestMSSQLDSN 用 msdsn.Parse 回读验证 DSN 构造（契约 5.4）。
func TestMSSQLDSN(t *testing.T) {
	cfg := Config{DialTimeout: 10 * time.Second}

	t.Run("port direct", func(t *testing.T) {
		db := store.Database{Engine: store.EngineMSSQL, Host: "db1.internal", Port: 1433,
			DatabaseName: "master", Username: "sa", SSLMode: store.SSLModeDisable}
		dsn, err := mssqlDSN(cfg, db, "pw")
		if err != nil {
			t.Fatal(err)
		}
		c, err := msdsn.Parse(dsn)
		if err != nil {
			t.Fatalf("msdsn.Parse(%q): %v", dsn, err)
		}
		if c.Host != "db1.internal" || c.Port != 1433 || c.Instance != "" {
			t.Fatalf("host/port/instance = %q/%d/%q, want db1.internal/1433/''", c.Host, c.Port, c.Instance)
		}
		if c.Database != "master" || c.User != "sa" || c.Password != "pw" {
			t.Fatalf("database/user/password = %q/%q/%q", c.Database, c.User, c.Password)
		}
		if c.Encryption != msdsn.EncryptionOff {
			t.Fatalf("encryption = %d, want EncryptionOff", c.Encryption)
		}
	})

	t.Run("named instance keeps port zero", func(t *testing.T) {
		db := store.Database{Engine: store.EngineMSSQL, Host: "db1.internal", Port: 0,
			InstanceName: "SQLEXPRESS", DatabaseName: "master", Username: "sa",
			SSLMode: store.SSLModeDisable}
		dsn, err := mssqlDSN(cfg, db, "pw")
		if err != nil {
			t.Fatal(err)
		}
		c, err := msdsn.Parse(dsn)
		if err != nil {
			t.Fatalf("msdsn.Parse(%q): %v", dsn, err)
		}
		// 命名实例走 SQL Server Browser（UDP 1434）解析，TCP port 保持 0。
		if c.Instance != "SQLEXPRESS" || c.Host != "db1.internal" {
			t.Fatalf("instance/host = %q/%q, want SQLEXPRESS/db1.internal", c.Instance, c.Host)
		}
		if c.Port != 0 {
			t.Fatalf("port = %d, want 0 (browser resolution)", c.Port)
		}
	})

	t.Run("default port 1433", func(t *testing.T) {
		db := store.Database{Engine: store.EngineMSSQL, Host: "h", Port: 0,
			DatabaseName: "d", Username: "u", SSLMode: store.SSLModeDisable}
		dsn, err := mssqlDSN(cfg, db, "pw")
		if err != nil {
			t.Fatal(err)
		}
		c, err := msdsn.Parse(dsn)
		if err != nil {
			t.Fatalf("msdsn.Parse(%q): %v", dsn, err)
		}
		if c.Port != 1433 {
			t.Fatalf("port = %d, want fallback 1433", c.Port)
		}
	})

	t.Run("password with special characters survives url escaping", func(t *testing.T) {
		pw := "p@ss;word}\"'x%20+&=?#"
		db := store.Database{Engine: store.EngineMSSQL, Host: "h", Port: 1433,
			DatabaseName: "d", Username: "u", SSLMode: store.SSLModeDisable}
		dsn, err := mssqlDSN(cfg, db, pw)
		if err != nil {
			t.Fatal(err)
		}
		c, err := msdsn.Parse(dsn)
		if err != nil {
			t.Fatalf("msdsn.Parse: %v", err)
		}
		if c.Password != pw {
			t.Fatalf("password roundtrip = %q, want %q", c.Password, pw)
		}
		if strings.Contains(dsn, pw) {
			t.Fatalf("dsn must percent-escape the raw password, got %q", dsn)
		}
	})

	t.Run("require implies trust server certificate", func(t *testing.T) {
		db := store.Database{Engine: store.EngineMSSQL, Host: "h", Port: 1433,
			DatabaseName: "d", Username: "u", SSLMode: store.SSLModeRequire}
		dsn, err := mssqlDSN(cfg, db, "pw")
		if err != nil {
			t.Fatal(err)
		}
		c, err := msdsn.Parse(dsn)
		if err != nil {
			t.Fatalf("msdsn.Parse: %v", err)
		}
		if c.Encryption != msdsn.EncryptionRequired {
			t.Fatalf("encryption = %d, want EncryptionRequired", c.Encryption)
		}
		if !c.TrustServerCertificate {
			t.Fatal("require mode must set TrustServerCertificate")
		}
	})

	t.Run("verify-full keeps system trust chain", func(t *testing.T) {
		db := store.Database{Engine: store.EngineMSSQL, Host: "h", Port: 1433,
			DatabaseName: "d", Username: "u", SSLMode: store.SSLModeVerifyFull}
		dsn, err := mssqlDSN(cfg, db, "pw")
		if err != nil {
			t.Fatal(err)
		}
		c, err := msdsn.Parse(dsn)
		if err != nil {
			t.Fatalf("msdsn.Parse: %v", err)
		}
		if c.Encryption != msdsn.EncryptionRequired {
			t.Fatalf("encryption = %d, want EncryptionRequired", c.Encryption)
		}
		if c.TrustServerCertificate {
			t.Fatal("verify-full must verify against the system trust chain")
		}
	})

	t.Run("dial timeout as connection timeout", func(t *testing.T) {
		db := store.Database{Engine: store.EngineMSSQL, Host: "h", Port: 1433,
			DatabaseName: "d", Username: "u", SSLMode: store.SSLModeDisable}
		dsn, err := mssqlDSN(Config{DialTimeout: 7 * time.Second}, db, "pw")
		if err != nil {
			t.Fatal(err)
		}
		c, err := msdsn.Parse(dsn)
		if err != nil {
			t.Fatalf("msdsn.Parse: %v", err)
		}
		// 契约 5.4：超时用 ctx + connection timeout（msdsn.ConnTimeout）。
		if c.ConnTimeout != 7*time.Second {
			t.Fatalf("connection timeout = %v, want 7s", c.ConnTimeout)
		}
	})
}

func TestNewMSSQLBackendValidation(t *testing.T) {
	cfg := Config{MaxConnsPerDatabase: 4, IdleTimeout: time.Minute, DialTimeout: time.Second}
	valid := func(mutate func(*store.Database)) store.Database {
		db := store.Database{Engine: store.EngineMSSQL, Host: "h", Port: 1433,
			DatabaseName: "d", Username: "u", SSLMode: store.SSLModeDisable}
		if mutate != nil {
			mutate(&db)
		}
		return db
	}
	cases := []struct {
		name string
		db   store.Database
		pw   string
	}{
		{"missing host", valid(func(d *store.Database) { d.Host = "" }), "pw"},
		{"missing database_name", valid(func(d *store.Database) { d.DatabaseName = "" }), "pw"},
		{"missing username", valid(func(d *store.Database) { d.Username = "" }), "pw"},
		{"missing password", valid(nil), ""},
		{"instance and port both set", valid(func(d *store.Database) { d.InstanceName = "SQLEXPRESS" }), "pw"},
	}
	for _, tc := range cases {
		b, err := newMSSQLBackend(cfg, tc.db, tc.pw)
		if err == nil {
			b.Close()
			t.Fatalf("newMSSQLBackend(%s): want error, got nil", tc.name)
		}
		if !strings.HasPrefix(err.Error(), "dbproxy: mssql") {
			t.Fatalf("newMSSQLBackend(%s) error = %v", tc.name, err)
		}
	}

	b, err := newMSSQLBackend(cfg, valid(nil), "pw")
	if err != nil {
		t.Fatalf("newMSSQLBackend(valid): %v", err)
	}
	defer b.Close()
	if b.db.Stats().MaxOpenConnections != 4 {
		t.Fatalf("pool max open conns = %d, want 4", b.db.Stats().MaxOpenConnections)
	}
}

// TestExecutorPoolMSSQLRouting 验证 executor 路由到 mssql 驱动：
// 单条 SELECT 通过守卫后到达驱动（连接失败而非 unsupported engine），
// 写语句被第三道闸（executor 内 sqlguard.Check）拦截。
func TestExecutorPoolMSSQLRouting(t *testing.T) {
	p := New(Config{DialTimeout: 300 * time.Millisecond}, nil)
	defer p.Close()
	db := store.Database{ID: "m1", Engine: store.EngineMSSQL, Host: "127.0.0.1", Port: 1,
		DatabaseName: "d", Username: "u", SSLMode: store.SSLModeDisable}

	_, err := p.Query(context.Background(), db, "pw", QueryRequest{SQL: "SELECT 1", Timeout: 3 * time.Second})
	if err == nil {
		t.Fatal("dial to 127.0.0.1:1 must fail")
	}
	if strings.Contains(err.Error(), "unsupported engine") {
		t.Fatalf("query routed to fallback: %v", err)
	}

	_, err = p.Query(context.Background(), db, "pw", QueryRequest{SQL: "DROP TABLE t", Timeout: 3 * time.Second})
	var nre *NotReadOnlyError
	if !errors.As(err, &nre) {
		t.Fatalf("DROP TABLE must be rejected by the in-executor guard, got %v", err)
	}
	if nre.Result.RuleID != sqlguard.RuleDDL {
		t.Fatalf("rule = %q, want SQL_GUARD_DDL", nre.Result.RuleID)
	}
}
