package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"aiops/internal/apierr"
	"aiops/internal/dbproxy"
	"aiops/internal/store"
	"aiops/internal/termx"
)

type databaseCreateRequest struct {
	Name         string  `json:"name"`
	Engine       string  `json:"engine"`
	Host         string  `json:"host"`
	Port         *int    `json:"port"`
	InstanceName string  `json:"instance_name"`
	DatabaseName string  `json:"database_name"`
	Username     string  `json:"username"`
	Password     *string `json:"password"`
	RedisDB      *int    `json:"redis_db"`
	AuthDB       string  `json:"auth_db"` // N2：仅 mongodb；空串 = 驱动默认 admin
	SQLitePath   string  `json:"sqlite_path"`
	SSLMode      string  `json:"ssl_mode"`
}

type databaseUpdateRequest struct {
	Name         *string `json:"name"`
	Host         *string `json:"host"`
	Port         *int    `json:"port"`
	InstanceName *string `json:"instance_name"`
	DatabaseName *string `json:"database_name"`
	Username     *string `json:"username"`
	Password     *string `json:"password"`
	RedisDB      *int    `json:"redis_db"`
	AuthDB       *string `json:"auth_db"` // N2：仅 mongodb；空串 = 驱动默认 admin
	SQLitePath   *string `json:"sqlite_path"`
	SSLMode      *string `json:"ssl_mode"`
}

type sqlQueryRequest struct {
	SQL string `json:"sql"`
}

type databaseTestResponse struct {
	OK            bool   `json:"ok"`
	LatencyMS     int64  `json:"latency_ms"`
	ServerVersion string `json:"server_version"`
}

func (d *Deps) handleListDatabases(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	items, err := d.Store.Databases().List(c.Request.Context())
	if err != nil {
		d.writeStoreError(c, err, apierr.DatabaseNotFound)
		return
	}
	out := make([]store.Database, 0, len(items))
	for _, db := range items {
		out = append(out, normalizeDatabase(db))
	}
	c.JSON(http.StatusOK, gin.H{"items": out})
}

func (d *Deps) handleCreateDatabase(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	var req databaseCreateRequest
	if !d.decodeJSON(c, &req) {
		return
	}

	name, ok := trimName(req.Name, 100)
	if !ok {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid name")
		return
	}
	engine := store.DatabaseEngine(req.Engine)
	params := store.CreateDatabaseParams{Name: name, Engine: engine}

	switch engine {
	case store.EngineSQLite:
		p, ok := validSQLitePath(req.SQLitePath)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid sqlite_path")
			return
		}
		if strings.TrimSpace(req.Host) != "" || req.Port != nil && *req.Port != 0 ||
			strings.TrimSpace(req.DatabaseName) != "" || strings.TrimSpace(req.Username) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sqlite does not accept host/port/database_name/username")
			return
		}
		if req.Password != nil && *req.Password != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sqlite does not accept a password")
			return
		}
		if strings.TrimSpace(req.InstanceName) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "instance_name is only valid for mssql")
			return
		}
		if req.RedisDB != nil {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "redis_db is only valid for redis")
			return
		}
		params.SQLitePath = p
		params.SSLMode = store.SSLModeDisable

	case store.EnginePostgres, store.EngineMySQL:
		host, ok := validHost(req.Host)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid host")
			return
		}
		dbName, ok := trimName(req.DatabaseName, 255)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid database_name")
			return
		}
		username, ok := trimName(req.Username, 100)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid username")
			return
		}
		if strings.TrimSpace(req.SQLitePath) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sqlite_path is only valid for sqlite")
			return
		}
		if strings.TrimSpace(req.InstanceName) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "instance_name is only valid for mssql")
			return
		}
		if req.RedisDB != nil {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "redis_db is only valid for redis")
			return
		}
		port := 5432
		if engine == store.EngineMySQL {
			port = 3306
		}
		if req.Port != nil {
			port = *req.Port
		}
		if !validPort(port) {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid port")
			return
		}
		sslMode, ok := parseSSLMode(req.SSLMode)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid ssl_mode")
			return
		}
		params.Host = host
		params.Port = port
		params.DatabaseName = dbName
		params.Username = username
		params.SSLMode = sslMode

		if req.Password != nil && *req.Password != "" {
			enc, err := d.encryptSecret(*req.Password)
			if err != nil {
				d.logError(c, "encrypt database password failed", err)
				writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "failed to encrypt credentials")
				return
			}
			params.Credentials.PasswordEncrypted = enc
		}

	case store.EngineMSSQL:
		// W2（契约 2.6/4.4）：host/database_name/username/password 必填；
		// instance_name 与 port 互斥——instance 非空时 port 必须为 0
		//（经 SQL Server Browser 解析），否则端口直连（默认 1433）。
		host, ok := validHost(req.Host)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid host")
			return
		}
		dbName, ok := trimName(req.DatabaseName, 255)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid database_name")
			return
		}
		username, ok := trimName(req.Username, 100)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid username")
			return
		}
		if req.Password == nil || *req.Password == "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "mssql requires a password")
			return
		}
		if strings.TrimSpace(req.SQLitePath) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sqlite_path is only valid for sqlite")
			return
		}
		if req.RedisDB != nil {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "redis_db is only valid for redis")
			return
		}
		instance, ok := validInstanceName(req.InstanceName)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid instance_name")
			return
		}
		port := 1433
		if instance != "" {
			// 命名实例：port 只能缺省或显式 0。
			if req.Port != nil && *req.Port != 0 {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "instance_name and port are mutually exclusive")
				return
			}
			port = 0
		} else if req.Port != nil {
			port = *req.Port
		}
		if port != 0 && !validPort(port) {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid port")
			return
		}
		sslMode, ok := parseSSLMode(req.SSLMode)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid ssl_mode")
			return
		}
		enc, err := d.encryptSecret(*req.Password)
		if err != nil {
			d.logError(c, "encrypt database password failed", err)
			writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "failed to encrypt credentials")
			return
		}
		params.Host = host
		params.Port = port
		params.InstanceName = instance
		params.DatabaseName = dbName
		params.Username = username
		params.Credentials.PasswordEncrypted = enc
		params.SSLMode = sslMode

	case store.EngineRedis:
		// W2（契约 2.6/4.4）：host/port 必填；username/password 可选
		//（requirepass/ACL）；redis_db 0..1023 默认 0。
		host, ok := validHost(req.Host)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid host")
			return
		}
		if strings.TrimSpace(req.DatabaseName) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "redis does not accept database_name")
			return
		}
		if strings.TrimSpace(req.SQLitePath) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sqlite_path is only valid for sqlite")
			return
		}
		if strings.TrimSpace(req.InstanceName) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "instance_name is only valid for mssql")
			return
		}
		port := 6379
		if req.Port != nil {
			port = *req.Port
		}
		if !validPort(port) {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid port")
			return
		}
		redisDB := 0
		if req.RedisDB != nil {
			if *req.RedisDB < 0 || *req.RedisDB > 1023 {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "redis_db must be between 0 and 1023")
				return
			}
			redisDB = *req.RedisDB
		}
		username := strings.TrimSpace(req.Username)
		if utf8.RuneCountInString(username) > 100 {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid username")
			return
		}
		sslMode, ok := parseSSLMode(req.SSLMode)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid ssl_mode")
			return
		}
		params.Host = host
		params.Port = port
		params.Username = username
		params.RedisDB = redisDB
		params.SSLMode = sslMode
		if req.Password != nil && *req.Password != "" {
			enc, err := d.encryptSecret(*req.Password)
			if err != nil {
				d.logError(c, "encrypt database password failed", err)
				writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "failed to encrypt credentials")
				return
			}
			params.Credentials.PasswordEncrypted = enc
		}

	case store.EngineMongoDB:
		// N2（契约 2.6/4.4）：host/port（创建未提供时填 27017）/
		// database_name 必填；username 与 password 成对——要么都空
		//（无认证部署），要么都非空；auth_db 0..100 字符缺省空串
		//（空 = 驱动默认 admin，即 AuthSource）；不提供 uri 直连形态，
		// 连接参数全部走结构化列。
		host, ok := validHost(req.Host)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid host")
			return
		}
		dbName, ok := trimName(req.DatabaseName, 255)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid database_name")
			return
		}
		if strings.TrimSpace(req.SQLitePath) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sqlite_path is only valid for sqlite")
			return
		}
		if strings.TrimSpace(req.InstanceName) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "instance_name is only valid for mssql")
			return
		}
		if req.RedisDB != nil {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "redis_db is only valid for redis")
			return
		}
		port := 27017
		if req.Port != nil {
			port = *req.Port
		}
		if !validPort(port) {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid port")
			return
		}
		username := strings.TrimSpace(req.Username)
		if utf8.RuneCountInString(username) > 100 {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid username")
			return
		}
		hasPassword := req.Password != nil && *req.Password != ""
		if (username != "") != hasPassword {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"mongodb username and password must both be set or both empty")
			return
		}
		authDB := strings.TrimSpace(req.AuthDB)
		if utf8.RuneCountInString(authDB) > 100 {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "auth_db must be at most 100 characters")
			return
		}
		sslMode, ok := parseSSLMode(req.SSLMode)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid ssl_mode")
			return
		}
		params.Host = host
		params.Port = port
		params.DatabaseName = dbName
		params.Username = username
		params.AuthDB = authDB
		params.SSLMode = sslMode
		if hasPassword {
			enc, err := d.encryptSecret(*req.Password)
			if err != nil {
				d.logError(c, "encrypt database password failed", err)
				writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "failed to encrypt credentials")
				return
			}
			params.Credentials.PasswordEncrypted = enc
		}

	default:
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
			"engine must be postgres, mysql, sqlite, mssql, redis or mongodb")
		return
	}

	created, err := d.Store.Databases().Create(c.Request.Context(), params)
	if err != nil {
		d.writeDatabaseStoreError(c, err)
		return
	}
	if d.DB != nil {
		d.DB.Invalidate(created.ID)
	}
	d.auditEvent(c, "database.create", "database", created.ID, map[string]any{
		"name":          created.Name,
		"engine":        string(created.Engine),
		"host":          created.Host,
		"port":          created.Port,
		"instance_name": created.InstanceName,
		"database":      created.DatabaseName,
		"redis_db":      created.RedisDB,
		"auth_db":       created.AuthDB,
		"sql_mode":      string(created.SSLMode),
		"has_password":  created.PasswordEncrypted != "",
	})
	c.JSON(http.StatusCreated, normalizeDatabase(created))
}

func (d *Deps) handleGetDatabase(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	db, err := d.Store.Databases().Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		d.writeStoreError(c, err, apierr.DatabaseNotFound)
		return
	}
	c.JSON(http.StatusOK, normalizeDatabase(db))
}

func (d *Deps) handleUpdateDatabase(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	var req databaseUpdateRequest
	if !d.decodeJSON(c, &req) {
		return
	}
	id := c.Param("id")
	current, err := d.Store.Databases().Get(c.Request.Context(), id)
	if err != nil {
		d.writeStoreError(c, err, apierr.DatabaseNotFound)
		return
	}

	params := store.UpdateDatabaseParams{}
	hasField := false

	if req.Name != nil {
		name, ok := trimName(*req.Name, 100)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid name")
			return
		}
		params.Name = &name
		hasField = true
	}

	switch current.Engine {
	case store.EngineSQLite:
		if req.Host != nil && strings.TrimSpace(*req.Host) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sqlite does not accept host")
			return
		}
		if req.Port != nil && *req.Port != 0 {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sqlite does not accept port")
			return
		}
		if req.DatabaseName != nil && strings.TrimSpace(*req.DatabaseName) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sqlite does not accept database_name")
			return
		}
		if req.Username != nil && strings.TrimSpace(*req.Username) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sqlite does not accept username")
			return
		}
		if req.Password != nil && *req.Password != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sqlite does not accept a password")
			return
		}
		if req.SQLitePath != nil {
			p, ok := validSQLitePath(*req.SQLitePath)
			if !ok {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid sqlite_path")
				return
			}
			params.SQLitePath = &p
			hasField = true
		}

	case store.EnginePostgres, store.EngineMySQL:
		if req.Host != nil {
			host, ok := validHost(*req.Host)
			if !ok {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid host")
				return
			}
			params.Host = &host
			hasField = true
		}
		if req.Port != nil {
			if !validPort(*req.Port) {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid port")
				return
			}
			params.Port = req.Port
			hasField = true
		}
		if req.DatabaseName != nil {
			dbName, ok := trimName(*req.DatabaseName, 255)
			if !ok {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid database_name")
				return
			}
			params.DatabaseName = &dbName
			hasField = true
		}
		if req.Username != nil {
			username, ok := trimName(*req.Username, 100)
			if !ok {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid username")
				return
			}
			params.Username = &username
			hasField = true
		}
		if req.SQLitePath != nil && strings.TrimSpace(*req.SQLitePath) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sqlite_path is only valid for sqlite")
			return
		}
		if req.InstanceName != nil && strings.TrimSpace(*req.InstanceName) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "instance_name is only valid for mssql")
			return
		}
		if req.RedisDB != nil {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "redis_db is only valid for redis")
			return
		}
		if req.Password != nil {
			enc, err := d.encryptSecret(*req.Password)
			if err != nil {
				d.logError(c, "encrypt database password failed", err)
				writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "failed to encrypt credentials")
				return
			}
			params.Credentials = &store.DatabaseCredentials{PasswordEncrypted: enc}
			hasField = true
		}

	case store.EngineMSSQL:
		// W2：同 pg/mysql 的字段集合 + instance_name；port/instance 互斥按
		// 「请求与库存合并后的生效值」校验，防止改其一漏改其二。
		if req.SQLitePath != nil && strings.TrimSpace(*req.SQLitePath) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sqlite_path is only valid for sqlite")
			return
		}
		if req.RedisDB != nil {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "redis_db is only valid for redis")
			return
		}
		if req.Host != nil {
			host, ok := validHost(*req.Host)
			if !ok {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid host")
				return
			}
			params.Host = &host
			hasField = true
		}
		if req.Port != nil {
			port := *req.Port
			if port != 0 && !validPort(port) {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid port")
				return
			}
			params.Port = &port
			hasField = true
		}
		if req.InstanceName != nil {
			instance, ok := validInstanceName(*req.InstanceName)
			if !ok {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid instance_name")
				return
			}
			params.InstanceName = &instance
			hasField = true
		}
		if req.DatabaseName != nil {
			dbName, ok := trimName(*req.DatabaseName, 255)
			if !ok {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid database_name")
				return
			}
			params.DatabaseName = &dbName
			hasField = true
		}
		if req.Username != nil {
			username, ok := trimName(*req.Username, 100)
			if !ok {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid username")
				return
			}
			params.Username = &username
			hasField = true
		}
		if req.Password != nil {
			enc, err := d.encryptSecret(*req.Password)
			if err != nil {
				d.logError(c, "encrypt database password failed", err)
				writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "failed to encrypt credentials")
				return
			}
			params.Credentials = &store.DatabaseCredentials{PasswordEncrypted: enc}
			hasField = true
		}
		// 互斥合并校验：instance 生效值非空时 port 生效值必须为 0。
		finalInstance := current.InstanceName
		if params.InstanceName != nil {
			finalInstance = *params.InstanceName
		}
		finalPort := current.Port
		if params.Port != nil {
			finalPort = *params.Port
		}
		if strings.TrimSpace(finalInstance) != "" && finalPort != 0 {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "instance_name and port are mutually exclusive")
			return
		}

	case store.EngineRedis:
		if req.DatabaseName != nil && strings.TrimSpace(*req.DatabaseName) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "redis does not accept database_name")
			return
		}
		if req.SQLitePath != nil && strings.TrimSpace(*req.SQLitePath) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sqlite_path is only valid for sqlite")
			return
		}
		if req.InstanceName != nil && strings.TrimSpace(*req.InstanceName) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "instance_name is only valid for mssql")
			return
		}
		if req.Host != nil {
			host, ok := validHost(*req.Host)
			if !ok {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid host")
				return
			}
			params.Host = &host
			hasField = true
		}
		if req.Port != nil {
			if !validPort(*req.Port) {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid port")
				return
			}
			params.Port = req.Port
			hasField = true
		}
		if req.Username != nil {
			username := strings.TrimSpace(*req.Username)
			if utf8.RuneCountInString(username) > 100 {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid username")
				return
			}
			params.Username = &username
			hasField = true
		}
		if req.RedisDB != nil {
			if *req.RedisDB < 0 || *req.RedisDB > 1023 {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "redis_db must be between 0 and 1023")
				return
			}
			params.RedisDB = req.RedisDB
			hasField = true
		}
	case store.EngineMongoDB:
		// N2：host/port/database_name/username/password/auth_db 可更新；
		// username 与 password 的成对约束按「请求与库存合并后的生效值」
		// 校验（防止只改其一漏改其二），空串密码 = 清空。
		if req.SQLitePath != nil && strings.TrimSpace(*req.SQLitePath) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sqlite_path is only valid for sqlite")
			return
		}
		if req.InstanceName != nil && strings.TrimSpace(*req.InstanceName) != "" {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "instance_name is only valid for mssql")
			return
		}
		if req.RedisDB != nil {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "redis_db is only valid for redis")
			return
		}
		if req.Host != nil {
			host, ok := validHost(*req.Host)
			if !ok {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid host")
				return
			}
			params.Host = &host
			hasField = true
		}
		if req.Port != nil {
			if !validPort(*req.Port) {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid port")
				return
			}
			params.Port = req.Port
			hasField = true
		}
		if req.DatabaseName != nil {
			dbName, ok := trimName(*req.DatabaseName, 255)
			if !ok {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid database_name")
				return
			}
			params.DatabaseName = &dbName
			hasField = true
		}
		if req.Username != nil {
			username := strings.TrimSpace(*req.Username)
			if utf8.RuneCountInString(username) > 100 {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid username")
				return
			}
			params.Username = &username
			hasField = true
		}
		if req.AuthDB != nil {
			authDB := strings.TrimSpace(*req.AuthDB)
			if utf8.RuneCountInString(authDB) > 100 {
				writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "auth_db must be at most 100 characters")
				return
			}
			params.AuthDB = &authDB
			hasField = true
		}
		if req.Password != nil {
			enc, err := d.encryptSecret(*req.Password)
			if err != nil {
				d.logError(c, "encrypt database password failed", err)
				writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "failed to encrypt credentials")
				return
			}
			params.Credentials = &store.DatabaseCredentials{PasswordEncrypted: enc}
			hasField = true
		}
		// 成对合并校验：final username 与 final 有无密码必须同空同非空。
		finalUsername := current.Username
		if params.Username != nil {
			finalUsername = *params.Username
		}
		finalHasPassword := current.PasswordEncrypted != ""
		if req.Password != nil {
			finalHasPassword = *req.Password != ""
		}
		if (finalUsername != "") != finalHasPassword {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed,
				"mongodb username and password must both be set or both empty")
			return
		}
	}

	if req.SSLMode != nil {
		mode, ok := parseSSLMode(*req.SSLMode)
		if !ok {
			writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "invalid ssl_mode")
			return
		}
		if current.Engine != store.EngineSQLite {
			params.SSLMode = &mode
			hasField = true
		}
	}

	if !hasField {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "at least one field is required")
		return
	}

	updated, err := d.Store.Databases().Update(c.Request.Context(), id, params)
	if err != nil {
		d.writeDatabaseStoreError(c, err)
		return
	}
	if d.DB != nil {
		d.DB.Invalidate(id)
	}
	d.auditEvent(c, "database.update", "database", id, map[string]any{
		"name":             updated.Name,
		"engine":           string(updated.Engine),
		"host":             updated.Host,
		"port":             updated.Port,
		"instance_name":    updated.InstanceName,
		"database":         updated.DatabaseName,
		"redis_db":         updated.RedisDB,
		"password_changed": req.Password != nil,
	})
	c.JSON(http.StatusOK, normalizeDatabase(updated))
}

func (d *Deps) handleDeleteDatabase(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	id := c.Param("id")
	db, err := d.Store.Databases().Get(c.Request.Context(), id)
	if err != nil {
		d.writeStoreError(c, err, apierr.DatabaseNotFound)
		return
	}
	if err := d.Store.Databases().Delete(c.Request.Context(), id); err != nil {
		d.writeStoreError(c, err, apierr.DatabaseNotFound)
		return
	}
	if d.DB != nil {
		d.DB.Invalidate(id)
	}
	d.auditEvent(c, "database.delete", "database", id, map[string]any{
		"name":   db.Name,
		"engine": string(db.Engine),
	})
	c.Status(http.StatusNoContent)
}

func (d *Deps) handleTestDatabase(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	if d.DB == nil {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "database executor unavailable")
		return
	}
	id := c.Param("id")
	db, err := d.Store.Databases().Get(c.Request.Context(), id)
	if err != nil {
		d.writeStoreError(c, err, apierr.DatabaseNotFound)
		return
	}
	password, err := d.decryptSecret(db.PasswordEncrypted)
	if err != nil {
		d.logError(c, "decrypt database password failed", err)
		writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "failed to prepare credentials")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()

	res, err := d.DB.Test(ctx, db, password)
	if err != nil {
		d.auditEvent(c, "database.test", "database", id, map[string]any{"ok": false, "engine": string(db.Engine)})
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		d.logError(c, "database test failed", err)
		writeAPIError(c, status, apierr.DatabaseTestFailed,
			describeDatabaseFailure("database connection test failed", db, err))
		return
	}
	d.auditEvent(c, "database.test", "database", id, map[string]any{
		"ok":         true,
		"engine":     string(db.Engine),
		"latency_ms": res.LatencyMS,
	})
	c.JSON(http.StatusOK, databaseTestResponse{
		OK:            true,
		LatencyMS:     res.LatencyMS,
		ServerVersion: res.ServerVersion,
	})
}

func (d *Deps) handleQueryDatabase(c *gin.Context) {
	if !d.requireStore(c) {
		return
	}
	if d.DB == nil {
		writeAPIError(c, http.StatusServiceUnavailable, apierr.ServiceUnavailable, "database executor unavailable")
		return
	}
	var req sqlQueryRequest
	if !d.decodeJSON(c, &req) {
		return
	}
	if strings.TrimSpace(req.SQL) == "" {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sql is required")
		return
	}
	if len(req.SQL) > 65536 {
		writeAPIError(c, http.StatusBadRequest, apierr.ValidationFailed, "sql exceeds 65536 bytes")
		return
	}

	id := c.Param("id")
	db, err := d.Store.Databases().Get(c.Request.Context(), id)
	if err != nil {
		d.writeStoreError(c, err, apierr.DatabaseNotFound)
		return
	}
	password, err := d.decryptSecret(db.PasswordEncrypted)
	if err != nil {
		d.logError(c, "decrypt database password failed", err)
		writeAPIError(c, http.StatusInternalServerError, apierr.Internal, "failed to prepare credentials")
		return
	}

	res, err := d.DB.Query(c.Request.Context(), db, password, dbproxy.QueryRequest{SQL: req.SQL})
	if err != nil {
		var nre *dbproxy.NotReadOnlyError
		if errors.As(err, &nre) || errors.Is(err, dbproxy.ErrNotReadOnly) {
			rule := ""
			if nre != nil {
				rule = nre.Result.RuleID
			}
			d.auditEvent(c, "database.query_rejected", "database", id, map[string]any{
				"engine":  string(db.Engine),
				"rule_id": rule,
			})
			msg := "查询被只读守卫拒绝"
			if rule != "" {
				// rule_id 是稳定标识（SQL_GUARD_DML 等），透出便于定位拒绝原因。
				msg += "：" + rule
			}
			writeAPIError(c, http.StatusUnprocessableEntity, apierr.SQLNotReadOnly, msg)
			return
		}
		if errors.Is(err, dbproxy.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
			d.auditEvent(c, "database.query", "database", id, map[string]any{"ok": false, "error": apierr.SQLTimeout})
			writeAPIError(c, http.StatusGatewayTimeout, apierr.SQLTimeout, "查询超时")
			return
		}
		d.auditEvent(c, "database.query", "database", id, map[string]any{"ok": false})
		d.logError(c, "database query failed", err)
		// 对齐连接测试（round 6/sprint 1）：失败必须透出底层原因，
		// 驱动错误不含 DSN/密码，可安全返回给调用方；p2 sprint 5 起
		// 同步给出中文分类短语（见 describeDatabaseFailure）。
		writeAPIError(c, http.StatusBadGateway, apierr.SQLQueryFailed,
			describeDatabaseFailure("查询失败", db, err))
		return
	}

	d.auditEvent(c, "database.query", "database", id, map[string]any{
		"engine":      string(db.Engine),
		"row_count":   res.RowCount,
		"truncated":   res.Truncated,
		"duration_ms": res.DurationMS,
	})
	c.JSON(http.StatusOK, res)
}

func normalizeDatabase(db store.Database) store.Database {
	db.HasPassword = db.PasswordEncrypted != ""
	return db
}

// describeDatabaseFailure 组装数据库连接测试/查询失败的 message（p2
// sprint 5，对齐服务器侧 describeConnTestFailure）：中文分类短语在前、
// 底层原因（折叠冗余包装，供排障）在后。修复前直接 fmt.Sprintf 透传
// 英文错误链（实测「database connection test failed: dial tcp …:
// connect: connection refused」），与服务器侧的中文分类水准不一致。
//
// prefix 保持通道口径："database connection test failed"（前端
// describeFailure 据此剥离通道前缀）或 "查询失败"。
func describeDatabaseFailure(prefix string, db store.Database, err error) string {
	reason := flattenTransportWrap(err)
	phrase := databaseFailurePhrase(err)
	if phrase == "" || db.Engine == store.EngineSQLite || db.Host == "" {
		// 未识别形态或本地 SQLite（无 host:port 语境）：前缀 + 底层原因。
		return prefix + "：" + reason
	}
	return fmt.Sprintf("%s：无法连接 %s:%d：%s；原因：%s", prefix, db.Host, db.Port, phrase, reason)
}

// databaseFailurePhrase 按错误文本特征返回数据库侧连接失败的中文分类
// 短语；未识别返回 ""。网络层类别复用 termx.ClassifyDialFailure，在此
// 之上补充数据库驱动特有的认证与 TLS 形态（大小写不敏感；错误信息只
// 用于分类，不含凭据明文）：
//   - 认证：pg "password authentication failed"、mysql "Access denied for
//     user"、redis "WRONGPASS"/"invalid password"、mssql "Login failed"；
//   - TLS：postgres/mysql 强制 sslmode 与服务端不匹配时的握手失败。
func databaseFailurePhrase(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "wrongpass"),
		strings.Contains(msg, "invalid password"),
		strings.Contains(msg, "invalid username-password"),
		strings.Contains(msg, "access denied"),
		strings.Contains(msg, "login failed"):
		return "认证失败（用户名或密码不正确）"
	case strings.Contains(msg, "tls:"), strings.Contains(msg, "x509:"):
		return "TLS 握手失败（证书或加密参数不被服务端接受）"
	}
	switch termx.ClassifyDialFailure(err) {
	case termx.DialAuthFailed:
		return "认证失败（用户名或密码不正确）"
	case termx.DialRefused:
		return "连接被拒绝（端口未开放或被防火墙拦截）"
	case termx.DialNoRoute:
		return "网络不可达（无到目标主机的路由）"
	case termx.DialDNSFailed:
		return "域名解析失败"
	case termx.DialTimedOut:
		return "连接超时（未在时限内完成连接）"
	}
	return ""
}

// writeDatabaseStoreError 在通用映射上细化 databases 写操作的冲突文案：
// databases 表的唯一约束只有 name（migrations 00001 databases_name_key），
// 创建/更新命中 409 即重名（对齐 servers 的「服务器名称已存在」与
// providers 的 "provider name already exists"，前端 apiError 机制据此
// 拼接展示具体原因）。其余错误码维持通用映射不变。
func (d *Deps) writeDatabaseStoreError(c *gin.Context, err error) {
	if errors.Is(err, store.ErrConflict) {
		writeAPIError(c, http.StatusConflict, apierr.Conflict, "database name already exists")
		return
	}
	d.writeStoreError(c, err, apierr.DatabaseNotFound)
}

// validSQLitePath 校验绝对路径，拒绝 NUL、:memory: 与相对路径；返回 clean 后的路径。
func validSQLitePath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" || len(p) > 4096 || strings.ContainsRune(p, 0) {
		return "", false
	}
	if strings.HasPrefix(p, ":memory:") {
		return "", false
	}
	if !strings.HasPrefix(p, "/") {
		return "", false
	}
	return path.Clean(p), true
}

func parseSSLMode(s string) (store.SSLMode, bool) {
	switch store.SSLMode(strings.TrimSpace(s)) {
	case "":
		return store.SSLModeDisable, true
	case store.SSLModeDisable, store.SSLModePrefer, store.SSLModeRequire,
		store.SSLModeVerifyCA, store.SSLModeVerifyFull:
		return store.SSLMode(strings.TrimSpace(s)), true
	default:
		return "", false
	}
}

// validInstanceName 校验 mssql 命名实例名：去空白后 1..100 字符（空串合法，
// 表示端口直连）。
func validInstanceName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > 100 {
		return "", false
	}
	return s, true
}
