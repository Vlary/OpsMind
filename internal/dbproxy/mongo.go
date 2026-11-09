// MongoDB 执行器（N2，CONTRACT.md 5.4/7.6，驱动 go.mongodb.org/mongo-driver
// v1.17.4——go.mod 锁定，v2 不引入）。
//
// 只读保障：readPreference 恒 primary、readConcern 不设置（默认 local），
// 均不作为用户配置（契约 5.4 调研裁决：read preference 只控制「从哪读」
// 不控制「读多新」，真正的只读闸是 sqlguard 命令文档白名单 + 服务端只读
// 账号）。操作超时双保险：每请求 ctx 超时（executor.go 15s 默认/60s 上限）
// + FindOptions/AggregateOptions...SetMaxTime(同值) 服务端硬停，用户提供的
// maxTimeMS 已由守卫限定（≤60000），实现里与请求级上限取更严者。
// 命令元数据（cmd/collection/limit/...）经 sqlguard.ParseMongoCommand 取得，
// filter/sort/projection/pipeline 对同一原文做 ExtJSON 有序解码——
// 「校验的文档 = 执行的文档」，且 sort/projection 的键序（排序优先级）
// 得以保留。
package dbproxy

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"aiops/internal/sqlguard"
	"aiops/internal/store"
)

// mongoBackend 是 mongo-driver 客户端（连接池由 driver 管理）。
type mongoBackend struct {
	client      *mongo.Client
	dbName      string
	log         *slog.Logger
	dialTimeout time.Duration

	pingOnce sync.Once
	pingErr  error
}

func newMongoBackend(cfg Config, db store.Database, password string, log *slog.Logger) (*mongoBackend, error) {
	if db.Host == "" {
		return nil, fmt.Errorf("dbproxy: mongodb requires host")
	}
	if db.Port < 1 || db.Port > 65535 {
		return nil, fmt.Errorf("dbproxy: mongodb requires a valid port")
	}
	if db.DatabaseName == "" {
		return nil, fmt.Errorf("dbproxy: mongodb requires database_name")
	}
	if log == nil {
		log = slog.Default()
	}

	addr := net.JoinHostPort(db.Host, strconv.Itoa(db.Port))
	// auth_db 为空 = 驱动默认 admin（AuthSource）。
	authSource := strings.TrimSpace(db.AuthDB)
	if authSource == "" {
		authSource = "admin"
	}

	// host 只经 SetHosts 结构化传递、不走 ApplyURI URI 解析：host 经
	// validHost 校验仍可能携带 ?/& 等 URI 保留字符（黑名单未覆盖），
	// 拼接或半转义的 URI 都可能让驱动把 host 片段解析为查询选项
	// （replicaSet 等）；凭据走 options.Credential、TLS 走 SetTLSConfig，
	// 客户端无需从 URI 获取任何配置（评审：防御深度，对齐 mssql 做法）。
	clientOpts := options.Client().
		SetHosts([]string{addr}).
		SetConnectTimeout(cfg.DialTimeout).
		SetServerSelectionTimeout(5 * time.Second).
		SetMaxPoolSize(uint64(cfg.MaxConnsPerDatabase)).
		SetMaxConnIdleTime(cfg.IdleTimeout).
		SetReadPreference(readpref.Primary()) // 契约 5.4：恒 primary；readConcern 不设置（默认 local）
	if db.Username != "" && password != "" {
		clientOpts.SetAuth(options.Credential{
			Username:   db.Username,
			Password:   password,
			AuthSource: authSource,
		})
	}
	if tlsCfg := mongoTLSConfig(db.SSLMode); tlsCfg != nil {
		clientOpts.SetTLSConfig(tlsCfg)
	}

	client, err := mongo.NewClient(clientOpts)
	if err != nil {
		return nil, fmt.Errorf("dbproxy: mongo client options: %w", err)
	}
	// Connect 不做 I/O 也不报部署不可用（契约 5.4）；池首次使用前由
	// ensureConnected 强制 Ping(readpref.Primary()) 成功。
	if err := client.Connect(context.Background()); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("dbproxy: mongo connect: %w", err)
	}
	return &mongoBackend{
		client:      client,
		dbName:      db.DatabaseName,
		log:         log,
		dialTimeout: cfg.DialTimeout,
	}, nil
}

// mongoTLSConfig 按契约 5.4 映射 ssl_mode（同 redis）：
// disable/prefer→无 TLS，require→TLS 跳过证书校验，
// verify-ca/verify-full→TLS 系统信任链校验。
func mongoTLSConfig(mode store.SSLMode) *tls.Config {
	switch mode {
	case store.SSLModeRequire:
		return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // 契约语义：require 即跳过证书校验
	case store.SSLModeVerifyCA, store.SSLModeVerifyFull:
		return &tls.Config{MinVersion: tls.VersionTLS12}
	default:
		return nil
	}
}

// ensureConnected 在池首次使用前强制 Ping 成功（失败按连接测试失败处理，
// 缓存至 Invalidate 重建池）。
func (b *mongoBackend) ensureConnected(ctx context.Context) error {
	b.pingOnce.Do(func() {
		pingCtx, cancel := context.WithTimeout(context.Background(), b.dialTimeout)
		defer cancel()
		if err := b.client.Ping(pingCtx, readpref.Primary()); err != nil {
			b.pingErr = fmt.Errorf("dbproxy: mongo ping: %w", err)
		}
	})
	return b.pingErr
}

// Version 执行 hello + buildInfo（契约 5.4：Ping 已由 ensureConnected 完成，
// server_version 取 buildInfo.version；hello 的 setName/msg/hosts 作为
// 拓扑摘要记入日志）。
func (b *mongoBackend) Version(ctx context.Context) (string, error) {
	if err := b.ensureConnected(ctx); err != nil {
		return "", err
	}
	db := b.client.Database(b.dbName)

	var hello bson.M
	if err := db.RunCommand(ctx, bson.D{
		{Key: "hello", Value: 1},
		{Key: "helloOk", Value: true},
	}).Decode(&hello); err == nil {
		hosts := 0
		if arr, ok := hello["hosts"].(primitive.A); ok {
			hosts = len(arr)
		}
		b.log.Info("mongodb topology summary",
			"database", b.dbName,
			"set_name", hello["setName"],
			"msg", hello["msg"], // "isdbgrid" = 经 mongos
			"hosts", hosts,
			"primary", hello["primary"],
			"max_wire_version", hello["maxWireVersion"])
	}

	var buildInfo bson.M
	if err := db.RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&buildInfo); err != nil {
		return "", err
	}
	version, _ := buildInfo["version"].(string)
	if version == "" {
		return "", errors.New("dbproxy: mongo buildInfo missing version")
	}
	return version, nil
}

// mongoParams 是 ExtJSON 有序解码后的结构化参数（filter 兼容 find 的
// filter 与 count/distinct 的 query 键；键序保留在 primitive.D 中）。
type mongoParams struct {
	filter     any
	sort       any
	projection any
	pipeline   mongo.Pipeline
	collation  *options.Collation
	hint       any
}

func extractMongoParams(doc bson.D) mongoParams {
	var p mongoParams
	for _, e := range doc {
		switch e.Key {
		case "filter", "query":
			p.filter = e.Value
		case "sort":
			p.sort = e.Value
		case "projection":
			p.projection = e.Value
		case "pipeline":
			if arr, ok := e.Value.(primitive.A); ok {
				for _, st := range arr {
					if d, ok := st.(primitive.D); ok {
						p.pipeline = append(p.pipeline, d)
					}
				}
			}
		case "collation":
			p.collation = mongoCollation(e.Value)
		case "hint":
			p.hint = e.Value
		}
	}
	return p
}

// mongoCollation 把 collation 文档映射为 options.Collation（已知字段；
// 未知字段由服务端校验，locale 缺省 en_US 与驱动默认一致）。
func mongoCollation(v any) *options.Collation {
	d, ok := v.(primitive.D)
	if !ok {
		return nil
	}
	c := &options.Collation{}
	for _, e := range d {
		switch e.Key {
		case "locale":
			c.Locale, _ = e.Value.(string)
		case "caseLevel":
			c.CaseLevel, _ = e.Value.(bool)
		case "strength":
			if n, ok := e.Value.(int32); ok {
				c.Strength = int(n)
			}
		case "numericOrdering":
			c.NumericOrdering, _ = e.Value.(bool)
		case "alternate":
			c.Alternate, _ = e.Value.(string)
		case "maxVariable":
			c.MaxVariable, _ = e.Value.(string)
		case "backwards":
			c.Backwards, _ = e.Value.(bool)
		}
	}
	return c
}

// Query 执行守卫通过的单条 JSON 命令文档（契约 5.4：find/aggregate 游标
// 逐行取，count/estimatedDocumentCount 单行单列，distinct 一列多行）。
func (b *mongoBackend) Query(ctx context.Context, sqlText string, maxRows int, maxBytes int64) (QueryResult, error) {
	// 执行器侧守卫：元数据必须来自与 Check 一致的解析路径。
	cmd, guard, err := sqlguard.ParseMongoCommand(sqlText)
	if err != nil || !guard.Allowed {
		return QueryResult{}, &NotReadOnlyError{Result: guard}
	}
	if err := b.ensureConnected(ctx); err != nil {
		return QueryResult{}, mapContextError(ctx, err)
	}

	var doc bson.D
	if err := bson.UnmarshalExtJSON([]byte(sqlText), false, &doc); err != nil {
		return QueryResult{}, fmt.Errorf("dbproxy: mongo command decode: %w", err)
	}
	params := extractMongoParams(doc)
	// v1.17 CRUD 方法对 nil 参数显式报 "document is nil"；空过滤用空文档。
	filter := params.filter
	if filter == nil {
		filter = bson.D{}
	}
	maxTime := mongoMaxTime(ctx, cmd.MaxTimeMS)
	coll := b.client.Database(b.dbName).Collection(cmd.Collection)

	switch cmd.Command {
	case "find":
		// 服务端 Limit 取探测位：未显式给 limit 时设 maxRows+1，客户端
		// 读满 maxRows 后若仍有下一行即可判定 Truncated=true（服务端
		// 恰好截断在 maxRows 会丢失「还有更多」的信号）。用户显式 limit
		// 以用户值为准——Truncated 只反映引擎行数/字节上限，不反映
		// 用户主动的 limit。
		serverLimit := int64(maxRows) + 1
		if cmd.Limit != nil && *cmd.Limit > 0 {
			serverLimit = *cmd.Limit
		}
		opts := options.Find().
			SetMaxTime(maxTime).
			SetLimit(serverLimit)
		if params.sort != nil {
			opts = opts.SetSort(params.sort)
		}
		if cmd.Skip != nil {
			opts = opts.SetSkip(*cmd.Skip)
		}
		if params.projection != nil {
			opts = opts.SetProjection(params.projection)
		}
		if cmd.BatchSize != nil {
			opts = opts.SetBatchSize(int32(*cmd.BatchSize))
		}
		// singleBatch：v1.17 FindOptions 不暴露该字段（driver 内部按
		// limit 语义处理），且其为性能语义而非只读闸——守卫层校验类型
		// 后放行，执行器不做二次映射。
		if params.collation != nil {
			opts = opts.SetCollation(params.collation)
		}
		if params.hint != nil {
			opts = opts.SetHint(params.hint)
		}
		if cmd.Comment != "" {
			opts = opts.SetComment(cmd.Comment)
		}
		if cmd.AllowDiskUse {
			opts = opts.SetAllowDiskUse(true)
		}
		cursor, err := coll.Find(ctx, filter, opts)
		if err != nil {
			return QueryResult{}, mapMongoError(ctx, err)
		}
		result, err := scanMongoCursor(ctx, cursor, maxRows, maxBytes)
		return result, mapMongoError(ctx, err)

	case "aggregate":
		opts := options.Aggregate().
			SetMaxTime(maxTime).
			SetAllowDiskUse(cmd.AllowDiskUse)
		if cmd.BatchSize != nil {
			opts = opts.SetBatchSize(int32(*cmd.BatchSize))
		}
		if params.collation != nil {
			opts = opts.SetCollation(params.collation)
		}
		if params.hint != nil {
			opts = opts.SetHint(params.hint)
		}
		if cmd.Comment != "" {
			opts = opts.SetComment(cmd.Comment)
		}
		cursor, err := coll.Aggregate(ctx, params.pipeline, opts)
		if err != nil {
			return QueryResult{}, mapMongoError(ctx, err)
		}
		result, err := scanMongoCursor(ctx, cursor, maxRows, maxBytes)
		return result, mapMongoError(ctx, err)

	case "count":
		opts := options.Count().SetMaxTime(maxTime)
		if cmd.Limit != nil && *cmd.Limit > 0 {
			opts = opts.SetLimit(*cmd.Limit)
		}
		if cmd.Skip != nil {
			opts = opts.SetSkip(*cmd.Skip)
		}
		if params.collation != nil {
			opts = opts.SetCollation(params.collation)
		}
		if params.hint != nil {
			opts = opts.SetHint(params.hint)
		}
		if cmd.Comment != "" {
			opts = opts.SetComment(cmd.Comment)
		}
		n, err := coll.CountDocuments(ctx, filter, opts)
		if err != nil {
			return QueryResult{}, mapMongoError(ctx, err)
		}
		return mongoSingleRow("count", "long", n, maxBytes), nil

	case "estimatedDocumentCount":
		opts := options.EstimatedDocumentCount().SetMaxTime(maxTime)
		if cmd.Comment != "" {
			opts = opts.SetComment(cmd.Comment)
		}
		n, err := coll.EstimatedDocumentCount(ctx, opts)
		if err != nil {
			return QueryResult{}, mapMongoError(ctx, err)
		}
		return mongoSingleRow("count", "long", n, maxBytes), nil

	case "distinct":
		opts := options.Distinct().SetMaxTime(maxTime)
		if params.collation != nil {
			opts = opts.SetCollation(params.collation)
		}
		if cmd.Comment != "" {
			opts = opts.SetComment(cmd.Comment)
		}
		values, err := coll.Distinct(ctx, cmd.Key, filter, opts)
		if err != nil {
			return QueryResult{}, mapMongoError(ctx, err)
		}
		result := QueryResult{
			Columns: []Column{{Name: "value", Type: "text"}},
			Rows:    [][]any{},
		}
		for _, v := range values {
			if len(result.Rows) >= maxRows {
				result.Truncated = true
				break
			}
			if !appendRow(&result, []any{normalizeMongoAny(v)}, maxBytes) {
				result.Truncated = true
				break
			}
		}
		result.RowCount = len(result.Rows)
		return result, nil

	default:
		// 守卫白名单之外不可能到达；失败关闭。
		return QueryResult{}, fmt.Errorf("dbproxy: unsupported mongodb command %q", cmd.Command)
	}
}

func (b *mongoBackend) Close() {
	if b.client != nil {
		_ = b.client.Disconnect(context.Background())
	}
}

// mongoMaxTime 计算服务端 MaxTime：请求 ctx 剩余时间与用户 maxTimeMS
// 取更严者（双保险；ctx 无 deadline 时退回默认查询超时）。
func mongoMaxTime(ctx context.Context, userMS *int64) time.Duration {
	t := defaultQueryTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if remain := time.Until(deadline); remain > 0 {
			t = remain
		} else {
			t = time.Millisecond
		}
	}
	if userMS != nil {
		if u := time.Duration(*userMS) * time.Millisecond; u > 0 && u < t {
			t = u
		}
	}
	return t
}

// mapMongoError 把 mongo 驱动错误映射到统一错误：MaxTimeMSExpired（code 50）
// 与 ctx 超时映射 ErrTimeout（HTTP SQL_TIMEOUT），其余交给 mapContextError
// （SQL_QUERY_FAILED 由 handler 层映射）。
func mapMongoError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) && (cmdErr.Name == "MaxTimeMSExpired" || cmdErr.Code == 50) {
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	}
	return mapContextError(ctx, err)
}

// scanMongoCursor 游标逐行取（契约 5.4）：columns 由第一行键的顺序确定
// （BSON 文档键序），行数/累计字节超限停止读取并 Truncated=true；
// 无行返回 columns=[]、rows=[]。
func scanMongoCursor(ctx context.Context, cursor *mongo.Cursor, maxRows int, maxBytes int64) (QueryResult, error) {
	result := QueryResult{Rows: [][]any{}}
	defer cursor.Close(ctx)

	var colKeys []string
	for cursor.Next(ctx) {
		raw := cursor.Current
		elems, err := raw.Elements()
		if err != nil {
			return result, err
		}
		if colKeys == nil {
			for _, e := range elems {
				colKeys = append(colKeys, e.Key())
				result.Columns = append(result.Columns,
					Column{Name: e.Key(), Type: mongoTypeName(e.Value().Type)})
			}
			if colKeys == nil {
				colKeys = []string{} // 空文档首行：锁定空列集
			}
		}
		if len(result.Rows) >= maxRows {
			result.Truncated = true
			break
		}
		values := make(map[string]any, len(colKeys))
		for _, e := range elems {
			val, err := mongoValue(e.Value())
			if err != nil {
				return result, err
			}
			values[e.Key()] = val
		}
		row := make([]any, len(colKeys))
		for i, k := range colKeys {
			row[i] = values[k] // 首行没有的键 → nil（列对齐）
		}
		if !appendRow(&result, row, maxBytes) {
			result.Truncated = true
			break
		}
	}
	if err := cursor.Err(); err != nil {
		return result, err
	}
	result.RowCount = len(result.Rows)
	return result, nil
}

// mongoSingleRow 构造单行单列结果（count/estimatedDocumentCount）。
func mongoSingleRow(name, typ string, value int64, maxBytes int64) QueryResult {
	result := QueryResult{
		Columns: []Column{{Name: name, Type: typ}},
		Rows:    [][]any{},
	}
	appendRow(&result, []any{value}, maxBytes)
	result.RowCount = len(result.Rows)
	return result
}

// mongoTypeName 返回 BSON 类型名（列 type，驱动元数据字符串）。
func mongoTypeName(t bsontype.Type) string {
	switch t {
	case bson.TypeDouble:
		return "double"
	case bson.TypeString:
		return "string"
	case bson.TypeEmbeddedDocument:
		return "object"
	case bson.TypeArray:
		return "array"
	case bson.TypeBinary:
		return "binData"
	case bson.TypeObjectID:
		return "objectId"
	case bson.TypeBoolean:
		return "bool"
	case bson.TypeDateTime:
		return "date"
	case bson.TypeNull, bson.TypeUndefined:
		return "null"
	case bson.TypeInt32:
		return "int"
	case bson.TypeInt64:
		return "long"
	case bson.TypeDecimal128:
		return "decimal"
	case bson.TypeTimestamp:
		return "timestamp"
	case bson.TypeRegex:
		return "regex"
	default:
		return "unknown"
	}
}

// mongoValue 把 bson.RawValue 映射为 JSON 友好值（契约 5.4）：Binary
// base64、ObjectID/DateTime/Decimal128 字符串化、嵌套文档保留为 JSON
// 对象（内层键序不再有语义）、数组展开。
func mongoValue(v bson.RawValue) (any, error) {
	switch v.Type {
	case bson.TypeDouble:
		return v.Double(), nil
	case bson.TypeString:
		return v.StringValue(), nil
	case bson.TypeEmbeddedDocument:
		return mongoDocToMap(v.Document())
	case bson.TypeArray:
		arrElems, err := v.Array().Elements()
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, len(arrElems))
		for _, e := range arrElems {
			item, err := mongoValue(e.Value())
			if err != nil {
				return nil, err
			}
			out = append(out, item)
		}
		return out, nil
	case bson.TypeBinary:
		_, data := v.Binary()
		return base64.StdEncoding.EncodeToString(data), nil
	case bson.TypeObjectID:
		return v.ObjectID().Hex(), nil
	case bson.TypeBoolean:
		return v.Boolean(), nil
	case bson.TypeDateTime:
		return v.Time().UTC().Format(time.RFC3339Nano), nil
	case bson.TypeNull, bson.TypeUndefined:
		return nil, nil
	case bson.TypeInt32:
		return v.Int32(), nil
	case bson.TypeInt64:
		return v.Int64(), nil
	case bson.TypeDecimal128:
		return v.Decimal128().String(), nil
	case bson.TypeTimestamp:
		t, i := v.Timestamp()
		return fmt.Sprintf("(%d,%d)", t, i), nil
	case bson.TypeRegex:
		pattern, _ := v.Regex()
		return pattern, nil
	default:
		return v.String(), nil
	}
}

// mongoDocToMap 把 BSON 文档转为 map（嵌套文档保留为 JSON 对象）。
func mongoDocToMap(raw bson.Raw) (map[string]any, error) {
	elems, err := raw.Elements()
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(elems))
	for _, e := range elems {
		v, err := mongoValue(e.Value())
		if err != nil {
			return nil, err
		}
		out[e.Key()] = v
	}
	return out, nil
}

// normalizeMongoAny 映射 Distinct 返回的驱动值（非 RawValue 形态）。
func normalizeMongoAny(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case primitive.ObjectID:
		return x.Hex()
	case primitive.Binary:
		return base64.StdEncoding.EncodeToString(x.Data)
	case primitive.Decimal128:
		return x.String()
	case primitive.DateTime:
		return x.Time().UTC().Format(time.RFC3339Nano)
	case primitive.Timestamp:
		return fmt.Sprintf("(%d,%d)", x.T, x.I)
	case primitive.D:
		out := make(map[string]any, len(x))
		for _, e := range x {
			out[e.Key] = normalizeMongoAny(e.Value)
		}
		return out
	case primitive.A:
		out := make([]any, 0, len(x))
		for _, item := range x {
			out = append(out, normalizeMongoAny(item))
		}
		return out
	default:
		return v
	}
}
