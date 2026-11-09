// MongoDB JSON 命令文档守卫（N2，CONTRACT.md 5.3）。
//
// 输入是与 MongoDB database command 同形的单条 JSON 对象（如
// {"find":"coll","filter":{...},"limit":10}），不是 SQL——不走 lexSQL
// 词法（JSON 的引号语义与 SQL 不同），在 Check 入口处分流到 checkMongo。
// 六层逐条校验（契约 5.3 写死），任何一层不通过即 allowed=false，全部
// 拒绝映射 SQL_NOT_READONLY（422，不创建审批）：
//  1. encoding/json 严格解析为单一 JSON 对象（拒绝数组/标量/尾随内容/
//     重复键；空输入、超 65536 字节、含 NUL 拒绝）→ MONGO_NOT_JSON；
//  2. 顶层恰好一个命令键（find/aggregate/count/distinct/
//     estimatedDocumentCount，值为合法集合名）→ MONGO_CMD_NOT_WHITELISTED；
//  3. 请求参数白名单 + 类型/范围（maxTimeMS≤60000、limit≤1000、
//     filter/projection/sort 深度与大小上限）→ MONGO_PARAM_NOT_WHITELISTED；
//  4. 管道阶段白名单（$facet 子管道与 $unionWith.pipeline 递归同校验）
//     → MONGO_STAGE_NOT_WHITELISTED；
//  5. 全文档递归扫描禁止操作符（$where/$function/$accumulator/$out/
//     $merge/$mergeCursors，任意嵌套深度；$expr 内嵌同样被覆盖）
//     → MONGO_OPERATOR_FORBIDDEN；
//  6. 通过则 RuleID=MONGO_READONLY、Kind=KindSelect、RiskLevel=safe。
package sqlguard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// MongoDB 规则 ID（CONTRACT.md 5.3，审计与 G25 断言引用）。
const (
	RuleMongoReadonly            = "MONGO_READONLY"
	RuleMongoNotJSON             = "MONGO_NOT_JSON"
	RuleMongoCmdNotWhitelisted   = "MONGO_CMD_NOT_WHITELISTED"
	RuleMongoParamNotWhitelisted = "MONGO_PARAM_NOT_WHITELISTED"
	RuleMongoStageNotWhitelisted = "MONGO_STAGE_NOT_WHITELISTED"
	RuleMongoOperatorForbidden   = "MONGO_OPERATOR_FORBIDDEN"
)

// mongo 文档结构上限（失败关闭：宁可误拦巨型嵌套文档，不放行任何
// 无法在有限步内证明安全的输入）。
const (
	mongoMaxFilterBytes  = 32768 // filter/query/projection/sort 单个子文档的字节上限
	mongoMaxPipelineLen  = 100   // 管道阶段数上限（含递归子管道）
	mongoMaxDepth        = 30    // 嵌套深度上限（顶层对象为第 1 层）
	mongoMaxTimeMS       = 60000 // 与 5.4 的 60s 上限对齐（契约 5.3 第 6 条）
	mongoMaxLimit        = 1000  // 行数上限同其他引擎（契约 5.3 第 6 条）
	mongoMaxCollNameSize = 255
)

// mongoForbiddenOps 是任意嵌套深度都禁止的操作符（契约 5.3 第 5 条）：
// $where/$function/$accumulator 是服务端 JS 执行，$out/$merge/$mergeCursors
// 是服务端写。$expr 内嵌以上任一同样命中（全文档扫描天然覆盖）。
var mongoForbiddenOps = map[string]bool{
	"$where":        true,
	"$function":     true,
	"$accumulator":  true,
	"$out":          true,
	"$merge":        true,
	"$mergeCursors": true,
}

// mongoCommandParams 是各命令允许的请求参数键（契约 5.3 第 3 条逐字）。
var mongoCommandParams = map[string]map[string]bool{
	"find": {
		"filter": true, "projection": true, "sort": true, "limit": true,
		"skip": true, "batchSize": true, "singleBatch": true, "collation": true,
		"hint": true, "comment": true, "maxTimeMS": true, "allowDiskUse": true,
	},
	"aggregate": {
		"pipeline": true, "allowDiskUse": true, "collation": true, "hint": true,
		"comment": true, "maxTimeMS": true, "batchSize": true,
	},
	"count": {
		"query": true, "limit": true, "skip": true, "collation": true,
		"hint": true, "comment": true, "maxTimeMS": true,
	},
	"distinct": {
		"key": true, "query": true, "collation": true, "comment": true,
		"maxTimeMS": true,
	},
	"estimatedDocumentCount": {
		"comment": true, "maxTimeMS": true,
	},
}

// mongoStages 是 aggregate 管道阶段白名单（契约 5.3 第 4 条逐字）。
var mongoStages = map[string]bool{
	"$match": true, "$project": true, "$unset": true, "$addFields": true,
	"$set": true, "$group": true, "$sort": true, "$count": true,
	"$limit": true, "$skip": true, "$unwind": true, "$lookup": true,
	"$graphLookup": true, "$facet": true, "$bucket": true, "$bucketAuto": true,
	"$sortByCount": true, "$replaceRoot": true, "$replaceWith": true,
	"$sample": true, "$unionWith": true, "$collStats": true, "$indexStats": true,
	"$densify": true, "$fill": true, "$setWindowFields": true,
}

// checkMongo 对单条 JSON 命令文档做六层只读校验（Check 的 mongodb 分支）。
func checkMongo(sql string) Result {
	_, res, _ := parseMongo(sql)
	return res
}

// MongoCommand 是守卫通过后的规范化命令标量参数，供 dbproxy 执行器
// 复用（契约 5.4：Query 解析命令文档后分发）。filter/projection/sort/
// pipeline 等结构化参数不由这里承载——执行器对同一原文做 ExtJSON
// 有序解码，保证「校验的文档 = 执行的文档」（redis SplitRedisCommand
// 的同一裁决：两处各自解析会引入"校验所见 ≠ 执行所发"的绕过面）。
type MongoCommand struct {
	Command      string // find|aggregate|count|distinct|estimatedDocumentCount
	Collection   string
	Limit        *int64 // 未提供 = nil（find/count：nil 或 0 表示不限制，由执行器收敛到请求级行数上限）
	Skip         *int64
	BatchSize    *int64
	SingleBatch  bool
	Comment      string
	MaxTimeMS    *int64 // 未提供 = nil；守卫已限定 1..60000
	AllowDiskUse bool
	Key          string // 仅 distinct
}

// ParseMongoCommand 跑与 Check(EngineMongoDB, sql) 完全一致的六层校验，
// 通过后返回结构化命令；拒绝时返回拒绝 Result 与错误。执行器必须以
// 本函数（而非自行解析）取得命令元数据，确保与守卫判定一致。
func ParseMongoCommand(sql string) (MongoCommand, Result, error) {
	return parseMongo(sql)
}

func parseMongo(sql string) (MongoCommand, Result, error) {
	var cmdOut MongoCommand
	normalized := strings.TrimSpace(sql)

	rejectCmd := func(rule, reason string) (MongoCommand, Result, error) {
		return cmdOut, reject(KindUnknown, rule, reason, normalized), errors.New(reason)
	}

	// 第 1 层：严格 JSON 对象。
	if normalized == "" {
		return rejectCmd(RuleMongoNotJSON, "command document is empty")
	}
	if len(sql) > MaxSQLBytes {
		return rejectCmd(RuleMongoNotJSON, "command document exceeds 65536 bytes")
	}
	if strings.IndexByte(sql, 0) >= 0 {
		return rejectCmd(RuleMongoNotJSON, "command document contains NUL byte")
	}
	doc, err := decodeStrictJSONObject(sql)
	if err != nil {
		return rejectCmd(RuleMongoNotJSON, err.Error())
	}

	// 第 2 层：顶层恰好一个命令键，值为合法集合名。
	var cmd, collection string
	cmdCount := 0
	for k, v := range doc {
		if _, isCmd := mongoCommandParams[k]; !isCmd {
			continue
		}
		cmdCount++
		s, ok := v.(string)
		if !ok {
			return rejectCmd(RuleMongoCmdNotWhitelisted,
				"command "+k+" requires a collection name string")
		}
		cmd, collection = k, s
	}
	if cmdCount == 0 {
		return rejectCmd(RuleMongoCmdNotWhitelisted, "top-level command is not whitelisted")
	}
	if cmdCount > 1 {
		return rejectCmd(RuleMongoCmdNotWhitelisted,
			"exactly one command key is allowed per document")
	}
	if err := validMongoCollection(collection); err != nil {
		return rejectCmd(RuleMongoCmdNotWhitelisted, err.Error())
	}

	// 第 3 层：请求参数白名单 + 类型/范围 + 深度/大小上限。
	allowedParams := mongoCommandParams[cmd]
	for k, v := range doc {
		if k == cmd {
			continue
		}
		if !allowedParams[k] {
			return rejectCmd(RuleMongoParamNotWhitelisted,
				"parameter "+k+" is not whitelisted for "+cmd)
		}
		if res, bad := checkMongoParam(cmd, k, v, normalized); bad {
			return cmdOut, res, errors.New(res.Reason)
		}
	}

	// 第 4 层：管道阶段白名单（含 $facet / $unionWith / $lookup 递归）。
	// aggregate 的 pipeline 是必须参数：缺失或 null（非数组）一律拒绝。
	if cmd == "aggregate" {
		pipeline, present := doc["pipeline"]
		stages, isArr := pipeline.([]any)
		if !present || !isArr {
			return rejectCmd(RuleMongoParamNotWhitelisted,
				"pipeline must be an array of stage objects")
		}
		if err := validateMongoPipeline(stages); err != nil {
			return rejectCmd(errRule(err), err.Error())
		}
	}

	// 第 5 层：全文档递归扫描禁止操作符（含 filter/query/projection/
	// pipeline 及一切嵌套对象；$expr 内嵌 $where/$function/$accumulator
	// 天然命中）。
	if err := scanMongoForbidden(doc, 1); err != nil {
		return rejectCmd(RuleMongoOperatorForbidden, err.Error())
	}

	// 全部通过：抽取标量参数供执行器分发（结构化参数由执行器对同一
	// 原文做 ExtJSON 有序解码取得，见 MongoCommand 文档注释）。
	cmdOut = MongoCommand{Command: cmd, Collection: collection}
	for k, v := range doc {
		switch k {
		case "limit":
			if n, ok := mongoJSONInt(v); ok {
				i := n
				cmdOut.Limit = &i
			}
		case "skip":
			if n, ok := mongoJSONInt(v); ok {
				i := n
				cmdOut.Skip = &i
			}
		case "batchSize":
			if n, ok := mongoJSONInt(v); ok {
				i := n
				cmdOut.BatchSize = &i
			}
		case "singleBatch":
			cmdOut.SingleBatch, _ = v.(bool)
		case "allowDiskUse":
			cmdOut.AllowDiskUse, _ = v.(bool)
		case "comment":
			cmdOut.Comment, _ = v.(string)
		case "maxTimeMS":
			if n, ok := mongoJSONInt(v); ok {
				i := n
				cmdOut.MaxTimeMS = &i
			}
		case "key":
			cmdOut.Key, _ = v.(string)
		}
	}

	return cmdOut, allowed(KindSelect, RuleMongoReadonly,
		"read-only "+cmd+" command", normalized), nil
}

// errRule 从 validateMongoPipeline 的错误推断规则 ID：阶段名问题归
// MONGO_STAGE_NOT_WHITELISTED，结构资源问题归 MONGO_PARAM_NOT_WHITELISTED。
type ruleError struct {
	rule string
	msg  string
}

func (e *ruleError) Error() string { return e.msg }

func errRule(err error) string {
	var re *ruleError
	if errors.As(err, &re) {
		return re.rule
	}
	return RuleMongoParamNotWhitelisted
}

// decodeStrictJSONObject 用 json.Decoder 逐 token 解析单个 JSON 对象：
// 拒绝顶层数组/标量、尾随内容与重复键（encoding/json 的 Unmarshal 默认
// 静默接受后写的重复键，必须显式检测）。数字保留 json.Number，供
// 整数形校验（1e2 / 1.5 一律不认可为整数——宁可误拦）。
func decodeStrictJSONObject(sql string) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(sql))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("top-level value must be a JSON object")
	}
	obj, err := decodeJSONObject(dec, 1)
	if err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing content after JSON document")
	}
	return obj, nil
}

func decodeJSONObject(dec *json.Decoder, depth int) (map[string]any, error) {
	if depth > mongoMaxDepth {
		return nil, fmt.Errorf("document nesting exceeds %d levels", mongoMaxDepth)
	}
	obj := make(map[string]any)
	for dec.More() {
		ktok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		key, ok := ktok.(string)
		if !ok {
			return nil, errors.New("object key must be a string")
		}
		if _, dup := obj[key]; dup {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		val, err := decodeJSONValue(dec, depth+1)
		if err != nil {
			return nil, err
		}
		obj[key] = val
	}
	if _, err := dec.Token(); err != nil { // 消费 '}'
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return obj, nil
}

func decodeJSONArray(dec *json.Decoder, depth int) ([]any, error) {
	if depth > mongoMaxDepth {
		return nil, fmt.Errorf("document nesting exceeds %d levels", mongoMaxDepth)
	}
	arr := make([]any, 0, 4)
	for dec.More() {
		val, err := decodeJSONValue(dec, depth+1)
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)
	}
	if _, err := dec.Token(); err != nil { // 消费 ']'
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	return arr, nil
}

func decodeJSONValue(dec *json.Decoder, depth int) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return decodeJSONObject(dec, depth)
		case '[':
			return decodeJSONArray(dec, depth)
		default:
			return nil, errors.New("unexpected closing delimiter")
		}
	default:
		return tok, nil
	}
}

// validMongoCollection 校验集合名（保守失败关闭）：非空、≤255 字节、
// 不含 NUL/$/.、不以 system. 开头。
func validMongoCollection(name string) error {
	if name == "" {
		return errors.New("collection name is empty")
	}
	if len(name) > mongoMaxCollNameSize {
		return fmt.Errorf("collection name exceeds %d bytes", mongoMaxCollNameSize)
	}
	if strings.IndexByte(name, 0) >= 0 {
		return errors.New("collection name contains NUL byte")
	}
	if strings.Contains(name, "$") {
		return errors.New("collection name must not contain '$'")
	}
	if strings.Contains(name, ".") {
		return errors.New("collection name must not contain '.'")
	}
	if strings.HasPrefix(name, "system.") {
		return errors.New("system collections are not allowed")
	}
	return nil
}

// checkMongoParam 校验单个请求参数的类型/范围与子文档大小上限。
// 返回的 Result 仅在拒绝时有意义（bad=true）。
func checkMongoParam(cmd, key string, v any, normalized string) (Result, bool) {
	rejectParam := func(reason string) (Result, bool) {
		return reject(KindUnknown, RuleMongoParamNotWhitelisted, reason, normalized), true
	}

	switch key {
	case "filter", "query", "collation":
		if _, ok := v.(map[string]any); !ok {
			return rejectParam(key + " must be an object")
		}
	case "projection", "sort":
		if _, ok := v.(map[string]any); !ok {
			return rejectParam(key + " must be an object")
		}
	case "limit":
		n, ok := mongoJSONInt(v)
		if !ok || n < 0 || n > mongoMaxLimit {
			return rejectParam(fmt.Sprintf("limit must be an integer between 0 and %d", mongoMaxLimit))
		}
	case "skip":
		n, ok := mongoJSONInt(v)
		if !ok || n < 0 {
			return rejectParam("skip must be a non-negative integer")
		}
	case "batchSize":
		n, ok := mongoJSONInt(v)
		if !ok || n <= 0 {
			return rejectParam("batchSize must be a positive integer")
		}
	case "singleBatch", "allowDiskUse":
		if _, ok := v.(bool); !ok {
			return rejectParam(key + " must be a boolean")
		}
	case "maxTimeMS":
		n, ok := mongoJSONInt(v)
		if !ok || n <= 0 || n > mongoMaxTimeMS {
			return rejectParam(fmt.Sprintf("maxTimeMS must be an integer between 1 and %d", mongoMaxTimeMS))
		}
	case "comment":
		if _, ok := v.(string); !ok {
			return rejectParam("comment must be a string")
		}
	case "hint":
		switch v.(type) {
		case string, map[string]any:
		default:
			return rejectParam("hint must be a string or an object")
		}
	case "key":
		s, ok := v.(string)
		if !ok || s == "" || strings.IndexByte(s, 0) >= 0 ||
			strings.HasPrefix(s, "$") || strings.Contains(s, ".") {
			return rejectParam("key must be a non-empty field name")
		}
	}

	// 子文档大小上限：filter/query/projection/sort 单独序列化后不得超过
	// mongoMaxFilterBytes（总输入 64KB 之外再挡一层巨型单参数）。
	switch key {
	case "filter", "query", "projection", "sort":
		b, err := json.Marshal(v)
		if err != nil || len(b) > mongoMaxFilterBytes {
			return rejectParam(key + " exceeds the size limit")
		}
	}
	return Result{}, false
}

// mongoJSONInt 只接受 JSON 整数（json.Number 且可被 ParseInt 解析）。
// 1.5、1e2、"10" 都不是整数形——宁可误拦。
func mongoJSONInt(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil {
		return 0, false
	}
	return i, true
}

// mongoLookupKeys / mongoGraphLookupKeys 是 $lookup / $graphLookup 阶段体
// 的键白名单（from/pipeline 额外做类型与递归校验，见 validateMongoStageBody）。
var mongoLookupKeys = map[string]bool{
	"from": true, "localField": true, "foreignField": true,
	"let": true, "pipeline": true, "as": true, "unwrap": true,
}

var mongoGraphLookupKeys = map[string]bool{
	"from": true, "startWith": true, "connectFromField": true,
	"connectToField": true, "maxDepth": true, "depthField": true,
	"restrictSearchWithMatch": true, "as": true,
}

// validateMongoPipeline 递归校验管道（契约 5.3 第 4 条）：每个阶段对象
// 恰好一个键且命中白名单；$facet 的每个子管道与 $unionWith.pipeline
// 递归同校验；$lookup/$graphLookup 的 from、$unionWith 的 coll 若提供
// 必须是合法集合名。
func validateMongoPipeline(stages []any) error {
	if len(stages) > mongoMaxPipelineLen {
		return &ruleError{RuleMongoParamNotWhitelisted,
			fmt.Sprintf("pipeline exceeds %d stages", mongoMaxPipelineLen)}
	}
	for _, st := range stages {
		stage, ok := st.(map[string]any)
		if !ok || len(stage) != 1 {
			return &ruleError{RuleMongoStageNotWhitelisted,
				"each pipeline stage must be an object with exactly one key"}
		}
		for name, val := range stage {
			if !mongoStages[name] {
				return &ruleError{RuleMongoStageNotWhitelisted,
					"pipeline stage " + name + " is not whitelisted"}
			}
			if err := validateMongoStageBody(name, val); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateMongoStageBody 校验需要递归/引用集合名的阶段体。
func validateMongoStageBody(name string, val any) error {
	switch name {
	case "$facet":
		// 值是 {<field>: [子管道]}，逐字段递归同校验。
		facets, ok := val.(map[string]any)
		if !ok {
			return &ruleError{RuleMongoStageNotWhitelisted, "$facet must be an object of sub-pipelines"}
		}
		for _, sub := range facets {
			stages, ok := sub.([]any)
			if !ok {
				return &ruleError{RuleMongoStageNotWhitelisted,
					"$facet sub-pipelines must be arrays"}
			}
			if err := validateMongoPipeline(stages); err != nil {
				return err
			}
		}
	case "$unionWith":
		// 值是集合名字符串，或 {coll, pipeline} 对象；pipeline 递归同校验。
		switch v := val.(type) {
		case string:
			if err := validMongoCollection(v); err != nil {
				return &ruleError{RuleMongoStageNotWhitelisted, "$unionWith: " + err.Error()}
			}
		case map[string]any:
			for k, sub := range v {
				switch k {
				case "coll":
					s, ok := sub.(string)
					if !ok {
						return &ruleError{RuleMongoStageNotWhitelisted,
							"$unionWith.coll must be a collection name string"}
					}
					if err := validMongoCollection(s); err != nil {
						return &ruleError{RuleMongoStageNotWhitelisted, "$unionWith: " + err.Error()}
					}
				case "pipeline":
					stages, ok := sub.([]any)
					if !ok {
						return &ruleError{RuleMongoStageNotWhitelisted,
							"$unionWith.pipeline must be an array"}
					}
					if err := validateMongoPipeline(stages); err != nil {
						return err
					}
				default:
					return &ruleError{RuleMongoStageNotWhitelisted,
						"$unionWith parameter " + k + " is not whitelisted"}
				}
			}
		default:
			return &ruleError{RuleMongoStageNotWhitelisted,
				"$unionWith must be a collection name or an object"}
		}
	case "$lookup", "$graphLookup":
		// 键白名单 + from 集合名校验 + pipeline 递归同校验（$lookup 的
		// 子管道与 $unionWith.pipeline 同理：服务端会执行其中的阶段，
		// 必须走同一份阶段白名单，未知键失败关闭）。
		body, ok := val.(map[string]any)
		if !ok {
			return &ruleError{RuleMongoStageNotWhitelisted, name + " must be an object"}
		}
		allowedKeys := mongoLookupKeys
		if name == "$graphLookup" {
			allowedKeys = mongoGraphLookupKeys
		}
		for k, sub := range body {
			if !allowedKeys[k] {
				return &ruleError{RuleMongoStageNotWhitelisted,
					name + " parameter " + k + " is not whitelisted"}
			}
			switch k {
			case "from":
				s, ok := sub.(string)
				if !ok {
					return &ruleError{RuleMongoStageNotWhitelisted, name + ".from must be a string"}
				}
				if err := validMongoCollection(s); err != nil {
					return &ruleError{RuleMongoStageNotWhitelisted, name + ": " + err.Error()}
				}
			case "pipeline":
				stages, ok := sub.([]any)
				if !ok {
					return &ruleError{RuleMongoStageNotWhitelisted,
						name + ".pipeline must be an array"}
				}
				if err := validateMongoPipeline(stages); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// scanMongoForbidden 递归扫描任意深度的对象/数组键，命中禁止操作符即
// 返回错误。null/数字/字符串/布尔不是容器，天然无害。
func scanMongoForbidden(v any, depth int) error {
	if depth > mongoMaxDepth {
		return fmt.Errorf("document nesting exceeds %d levels", mongoMaxDepth)
	}
	switch x := v.(type) {
	case map[string]any:
		for k, sub := range x {
			if mongoForbiddenOps[k] {
				return fmt.Errorf("operator %s is not allowed at any depth", k)
			}
			if err := scanMongoForbidden(sub, depth+1); err != nil {
				return err
			}
		}
	case []any:
		for _, sub := range x {
			if err := scanMongoForbidden(sub, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}
