package sqlguard

import (
	"strings"
	"testing"

	"aiops/internal/risk"
)

// G25（mongodb 守卫，CONTRACT.md 5.3/9.2）：六层逐条——合法
// find/aggregate/count/distinct/estimatedDocumentCount 通过（含管道白名单
// 阶段、$facet/$unionWith 递归）；顶层写命令/多键/未知请求键/未知管道
// 阶段/任意深度 $where/$function/$accumulator/$out/$merge/maxTimeMS>60000/
// limit>1000/非 JSON/数组/尾随内容/重复键拒绝；RuleID 逐一断言。
// 纯逻辑不标注 build tag，必须默认执行（契约 9.2）。

func TestCheckMongoAllowed(t *testing.T) {
	cases := []string{
		// 五个命令的最小形态。
		`{"find":"orders"}`,
		`{"aggregate":"orders","pipeline":[{"$match":{}},{"$group":{"_id":"$city","n":{"$sum":1}}}]}`,
		`{"count":"orders","query":{}}`,
		`{"distinct":"users","key":"role"}`,
		`{"estimatedDocumentCount":"users"}`,
		// find 全参数（契约 5.3 第 3 条白名单逐键）。
		`{"find":"orders","filter":{"status":"paid"},"projection":{"_id":0},"sort":{"created_at":-1},` +
			`"limit":10,"skip":5,"batchSize":50,"singleBatch":true,` +
			`"collation":{"locale":"zh"},"hint":"status_1","comment":"monthly","maxTimeMS":5000,"allowDiskUse":true}`,
		// 复杂 filter：$or/$and/$gt/$in/$elemMatch 组合（均合法查询操作符）。
		`{"find":"c","filter":{"$or":[{"a":{"$gt":1}},{"b":{"$in":[1,2,3]}},{"c":{"$elemMatch":{"d":true}}}]}}`,
		// $expr 合法形态（本身不禁止；内嵌 $function 才拒，见拒绝表）。
		`{"find":"c","filter":{"$expr":{"$eq":["$a","$b"]}}}`,
		// limit 0（合法：不限制，由执行器收敛到请求级行数上限）。
		`{"find":"c","limit":0}`,
		// maxTimeMS 边界值 1 与 60000。
		`{"find":"c","maxTimeMS":1}`,
		`{"find":"c","maxTimeMS":60000}`,
		// limit 边界值 1000。
		`{"find":"c","limit":1000}`,
		// 管道白名单阶段抽样：$unwind/$replaceRoot/$sample/$bucketAuto/
		// $sortByCount/$densify/$fill/$setWindowFields/$indexStats。
		`{"aggregate":"c","pipeline":[{"$unwind":"$items"},{"$replaceRoot":{"newRoot":"$items"}},` +
			`{"$sample":{"size":5}},{"$sortByCount":"$city"}]}`,
		`{"aggregate":"c","pipeline":[{"$bucketAuto":{"groupBy":"$price","buckets":4}},{"$setWindowFields":{` +
			`"partitionBy":"$city","sortBy":{"ts":1},"output":{"rank":{"$rank":{}}}}}]}`,
		// $facet 子管道递归（各子管道独立走阶段白名单）。
		`{"aggregate":"c","pipeline":[{"$facet":{"byCity":[{"$group":{"_id":"$city","n":{"$sum":1}}}],` +
			`"top":[{"$sort":{"n":-1}},{"$limit":3}]}}]}`,
		// $unionWith 两种形态：集合名字符串 / {coll,pipeline} 递归。
		`{"aggregate":"c","pipeline":[{"$unionWith":"archived"}]}`,
		`{"aggregate":"c","pipeline":[{"$unionWith":{"coll":"archived","pipeline":[{"$match":{"y":2026}}]}}]}`,
		// $lookup / $graphLookup 的 from 集合名校验通过形态。
		`{"aggregate":"c","pipeline":[{"$lookup":{"from":"cities","localField":"city","foreignField":"_id","as":"city"}}]}`,
		`{"aggregate":"c","pipeline":[{"$graphLookup":{"from":"employees","startWith":"$mgr","connectFromField":"mgr","connectToField":"name","as":"chain"}}]}`,
		// $lookup 的 pipeline 子管道（合法阶段递归通过）。
		`{"aggregate":"c","pipeline":[{"$lookup":{"from":"c2","pipeline":[{"$match":{"a":1}},{"$limit":3}],"as":"sub"}}]}`,
		// count 全参数。
		`{"count":"c","query":{"a":1},"limit":100,"skip":10,"collation":{"locale":"en"},` +
			`"hint":"a_1","comment":"q","maxTimeMS":3000}`,
		// distinct 全参数。
		`{"distinct":"users","key":"role","query":{"active":true},"collation":{"locale":"en"},` +
			`"comment":"q","maxTimeMS":2000}`,
		// estimatedDocumentCount 全参数（仅 comment/maxTimeMS）。
		`{"estimatedDocumentCount":"users","comment":"q","maxTimeMS":1000}`,
	}
	for _, sql := range cases {
		res, err := Check(EngineMongoDB, sql)
		if err != nil {
			t.Fatalf("Check(mongodb, %s) returned error %v", sql, err)
		}
		if !res.Allowed {
			t.Errorf("Check(mongodb, %s) = reject %s: %s", sql, res.RuleID, res.Reason)
			continue
		}
		if res.RuleID != RuleMongoReadonly || res.Kind != KindSelect || res.RiskLevel != risk.LevelSafe {
			t.Errorf("Check(mongodb, %s) = %+v; want MONGO_READONLY/select/safe", sql, res)
		}
	}
}

func TestCheckMongoRejected(t *testing.T) {
	cases := []struct {
		sql  string
		rule string
	}{
		// ---- 第 1 层 MONGO_NOT_JSON ----
		{``, RuleMongoNotJSON},                                                  // 空输入
		{`   `, RuleMongoNotJSON},                                               // 仅空白
		{`db.orders.find({})`, RuleMongoNotJSON},                                // JS shell 语法不是 JSON
		{`[{"find":"c"}]`, RuleMongoNotJSON},                                    // 顶层数组
		{`"find"`, RuleMongoNotJSON},                                            // 顶层标量
		{`{"find":"c"} {"count":"c"}`, RuleMongoNotJSON},                        // 尾随内容
		{`{"find":"c"};`, RuleMongoNotJSON},                                     // 尾随分号
		{`{"find":"c","find":"d"}`, RuleMongoNotJSON},                           // 重复键
		{`{"find":"c","filter":{"a":1},"filter":{"b":2}}`, RuleMongoNotJSON},    // 深层重复键
		{`{"find":"c` + "\x00" + `"}`, RuleMongoNotJSON},                        // NUL
		{`{"find":` + strings.Repeat(`"x",`, 30000) + `"x"}`, RuleMongoNotJSON}, // 超过 65536 字节
		{`{"find":"c"`, RuleMongoNotJSON},                                       // 未闭合
		{`{"find":"c"} }`, RuleMongoNotJSON},                                    // 尾随对象闭合符
		{`{"find":'c'}`, RuleMongoNotJSON},                                      // 单引号不是 JSON
		{`{"find":c}`, RuleMongoNotJSON},                                        // 裸标识符不是 JSON
		{`{"find":"c",}`, RuleMongoNotJSON},                                     // JSON 不允许尾随逗号

		// ---- 第 2 层 MONGO_CMD_NOT_WHITELISTED ----
		{`{"insert":"c","documents":[{"a":1}]}`, RuleMongoCmdNotWhitelisted},
		{`{"update":"c","updates":[{"q":{},"u":{"$set":{"a":1}}}]}`, RuleMongoCmdNotWhitelisted},
		{`{"delete":"c","deletes":[{"q":{}}]}`, RuleMongoCmdNotWhitelisted},
		{`{"findAndModify":"c","query":{},"update":{"$set":{"a":1}}}`, RuleMongoCmdNotWhitelisted},
		{`{"findAndModify":"c","remove":true}`, RuleMongoCmdNotWhitelisted},
		{`{"drop":"c"}`, RuleMongoCmdNotWhitelisted},
		{`{"dropDatabase":1}`, RuleMongoCmdNotWhitelisted},
		{`{"dropIndexes":"c"}`, RuleMongoCmdNotWhitelisted},
		{`{"create":"c"}`, RuleMongoCmdNotWhitelisted},
		{`{"createIndexes":"c","indexes":[]}`, RuleMongoCmdNotWhitelisted},
		{`{"renameCollection":"a.b","to":"c.d"}`, RuleMongoCmdNotWhitelisted},
		{`{"eval":"db.dropDatabase()"}`, RuleMongoCmdNotWhitelisted},
		{`{"mapReduce":"c","map":"f","reduce":"g","out":"res"}`, RuleMongoCmdNotWhitelisted},
		{`{"runCommand":{"find":"c"}}`, RuleMongoCmdNotWhitelisted},
		{`{"explain":{"find":"c"}}`, RuleMongoCmdNotWhitelisted},
		{`{"listCollections":1}`, RuleMongoCmdNotWhitelisted},
		{`{"killCursors":"c","cursors":[]}`, RuleMongoCmdNotWhitelisted},
		{`{"getMore":{}, "collection":"c"}`, RuleMongoCmdNotWhitelisted},
		{`{"getLastError":1}`, RuleMongoCmdNotWhitelisted},
		{`{"hello":1}`, RuleMongoCmdNotWhitelisted},
		{`{"buildInfo":1}`, RuleMongoCmdNotWhitelisted},
		{`{"serverStatus":1}`, RuleMongoCmdNotWhitelisted},
		{`{"currentUser":{}}`, RuleMongoCmdNotWhitelisted}, //伪装形态
		{`{"dbStats":1}`, RuleMongoCmdNotWhitelisted},
		// 多命令键（单文档多命令）。
		{`{"find":"a","count":"b"}`, RuleMongoCmdNotWhitelisted},
		{`{"aggregate":"a","distinct":"b","key":"k"}`, RuleMongoCmdNotWhitelisted},
		// 集合名非法。
		{`{"find":""}`, RuleMongoCmdNotWhitelisted},
		{`{"find":123}`, RuleMongoCmdNotWhitelisted},
		{`{"find":null}`, RuleMongoCmdNotWhitelisted},
		{`{"find":{"$gt":""}}`, RuleMongoCmdNotWhitelisted},
		{`{"find":"$cmd"}`, RuleMongoCmdNotWhitelisted},
		{`{"find":"db.coll"}`, RuleMongoCmdNotWhitelisted}, // 含 '.'：命名空间注入
		{`{"find":"system.users"}`, RuleMongoCmdNotWhitelisted},
		{`{"find":"a\u0000b"}`, RuleMongoCmdNotWhitelisted}, // 集合名含 \u0000 转义的 NUL

		// ---- 第 3 层 MONGO_PARAM_NOT_WHITELISTED ----
		{`{"find":"c","unknown":1}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","writeConcern":{"w":0}}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","let":{"v":1}}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","pipeline":[]}`, RuleMongoParamNotWhitelisted}, // pipeline 只属于 aggregate
		{`{"find":"c","returnKey":true}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","showRecordId":true}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","tailable":true}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","noCursorTimeout":true}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","awaitData":true}`, RuleMongoParamNotWhitelisted},
		{`{"aggregate":"c","pipeline":[],"limit":10}`, RuleMongoParamNotWhitelisted}, // aggregate 白名单无 limit（5.3 第 3 条）
		{`{"aggregate":"c","pipeline":null}`, RuleMongoParamNotWhitelisted},
		{`{"aggregate":"c","maxTimeMS":"5000"}`, RuleMongoParamNotWhitelisted},
		{`{"count":"c","query":[]}`, RuleMongoParamNotWhitelisted},    // query 必须是对象
		{`{"count":"c","filter":{}}`, RuleMongoParamNotWhitelisted},   // count 只认 query
		{`{"distinct":"c","key":"$a"}`, RuleMongoParamNotWhitelisted}, // key 不允许 $ 前缀（表达式注入）
		{`{"distinct":"c","key":"a.b"}`, RuleMongoParamNotWhitelisted},
		{`{"distinct":"c","sort":{}}`, RuleMongoParamNotWhitelisted}, // distinct 无 sort 参数
		{`{"estimatedDocumentCount":"c","query":{}}`, RuleMongoParamNotWhitelisted},
		{`{"estimatedDocumentCount":"c","maxTimeMS":60001}`, RuleMongoParamNotWhitelisted},
		// maxTimeMS 范围与整数形。
		{`{"find":"c","maxTimeMS":60001}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","maxTimeMS":0}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","maxTimeMS":-1}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","maxTimeMS":1.5}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","maxTimeMS":1e2}`, RuleMongoParamNotWhitelisted}, // 非整数形（宁可误拦）
		{`{"find":"c","maxTimeMS":true}`, RuleMongoParamNotWhitelisted},
		// limit 范围与整数形。
		{`{"find":"c","limit":1001}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","limit":-1}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","limit":10.5}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","limit":"10"}`, RuleMongoParamNotWhitelisted},
		{`{"count":"c","limit":1001}`, RuleMongoParamNotWhitelisted},
		// 其他标量参数类型。
		{`{"find":"c","skip":-1}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","skip":1.5}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","batchSize":0}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","batchSize":"50"}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","singleBatch":"yes"}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","comment":123}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","filter":[]}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","sort":[["a",1]]}`, RuleMongoParamNotWhitelisted}, // sort 必须是对象，不接受数组形态
		{`{"find":"c","projection":"a"}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","collation":"en"}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","allowDiskUse":"true"}`, RuleMongoParamNotWhitelisted},
		{`{"find":"c","hint":123}`, RuleMongoParamNotWhitelisted},
		// filter 大小上限：序列化后超过 32768 字节。
		{`{"find":"c","filter":{"pad":"` + strings.Repeat("x", 33000) + `"}}`, RuleMongoParamNotWhitelisted},
		// 嵌套深度上限：30 层。
		{deepFilter(40), RuleMongoNotJSON},

		// ---- 第 4 层 MONGO_STAGE_NOT_WHITELISTED ----
		{`{"aggregate":"c","pipeline":[{"$out":"steal"}]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{"$merge":{"into":"steal"}}]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{"$changeStream":{}}]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{"$listSessions":{}}]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{"$documents":[{}]}]}`, RuleMongoStageNotWhitelisted},
		// $lookup 子管道递归同校验：$out 混入子管道按阶段白名单拒绝；
		// 未知键（extra）失败关闭。
		{`{"aggregate":"c","pipeline":[{"$lookup":{"from":"c2","pipeline":[{"$out":"x"}],"as":"y"}}]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{"$lookup":{"from":"c2","extra":1,"as":"y"}}]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{}]}`, RuleMongoStageNotWhitelisted},            // 空阶段对象
		{`{"aggregate":"c","pipeline":[["$match",{}]]}`, RuleMongoStageNotWhitelisted}, // 阶段必须是对象
		{`{"aggregate":"c","pipeline":{"$match":{}}}`, RuleMongoParamNotWhitelisted},   // pipeline 必须是数组
		{`{"aggregate":"c","pipeline":[5]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{"$match":{},"$limit":2}]}`, RuleMongoStageNotWhitelisted}, // 阶段多键
		{`{"aggregate":"c","pipeline":[{"match":{}}]}`, RuleMongoStageNotWhitelisted},             // 阶段名缺 $
		// $facet / $unionWith 递归白名单。
		{`{"aggregate":"c","pipeline":[{"$facet":{"a":[{"$out":"x"}]}}]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{"$facet":{"a":[["x"]]}}]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{"$facet":{"a":{"$match":{}}}}]}`, RuleMongoStageNotWhitelisted}, // 子管道必须是数组
		{`{"aggregate":"c","pipeline":[{"$unionWith":{"coll":"a","pipeline":[{"$graphFake":{}}]}}]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{"$unionWith":{"pipeline":[],"extra":1}}]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{"$unionWith":{"coll":"$cmd"}}]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{"$unionWith":{"coll":"a.b"}}]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{"$unionWith":{"coll":123}}]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{"$unionWith":123}]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{"$lookup":{"from":"$cmd","as":"x"}}]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{"$lookup":{"from":123}}]}`, RuleMongoStageNotWhitelisted},
		{`{"aggregate":"c","pipeline":[{"$lookup":"c2"}]}`, RuleMongoStageNotWhitelisted}, // $lookup 值必须是对象
		{`{"aggregate":"c","pipeline":[{"$graphLookup":{"from":"a.b"}}]}`, RuleMongoStageNotWhitelisted},
		// 管道阶段数上限。
		{`{"aggregate":"c","pipeline":[` + strings.Repeat(`{"$match":{}},`, 150) + `{"$match":{}}]}`, RuleMongoParamNotWhitelisted},

		// ---- 第 5 层 MONGO_OPERATOR_FORBIDDEN ----
		// $where 在 filter 顶层、深层嵌套、字符串值内以操作符键形态出现。
		{`{"find":"c","filter":{"$where":"this.a>1"}}`, RuleMongoOperatorForbidden},
		{`{"find":"c","filter":{"a":{"$where":"1"}}}`, RuleMongoOperatorForbidden},
		{`{"find":"c","filter":{"$or":[{"b":{"$where":"1"}}]}}`, RuleMongoOperatorForbidden},
		{`{"find":"c","filter":{"a":{"$in":[{"$where":"1"}]}}}`, RuleMongoOperatorForbidden},
		// $expr 内嵌 $function / $where（契约 5.3 第 5 条点名形态）。
		{`{"find":"c","filter":{"$expr":{"$function":{"body":"function(){return 1}","args":[],"lang":"js"}}}}`, RuleMongoOperatorForbidden},
		{`{"find":"c","filter":{"$expr":{"$eq":["$a",{"$where":"1"}]}}}`, RuleMongoOperatorForbidden},
		// $accumulator 在 $group 内（合法阶段、禁止操作符）。
		{`{"aggregate":"c","pipeline":[{"$group":{"_id":null,"x":{"$accumulator":{"init":"function(){return 0}",` +
			`"accumulate":"function(s,v){return s+v}","accumulateArgs":["$v"],"merge":"function(s1,s2){return s1+s2}",` +
			`"finalize":"function(s){return s}","lang":"js"}}}}]}`, RuleMongoOperatorForbidden},
		// $out/$merge 嵌套在非顶层管道位置（$unionWith/$facet 子管道内由第 4 层
		// 先拒；这里构造 sort/projection/count query 内的嵌套绕过尝试）。
		{`{"find":"c","sort":{"$where":1}}`, RuleMongoOperatorForbidden},
		{`{"find":"c","projection":{"a":{"$function":{}}}}`, RuleMongoOperatorForbidden},
		{`{"count":"c","query":{"$merge":{}}}`, RuleMongoOperatorForbidden},
		{`{"distinct":"c","key":"k","query":{"$out":"x"}}`, RuleMongoOperatorForbidden},
		// 管道阶段体内深层嵌套。
		{`{"aggregate":"c","pipeline":[{"$match":{"a":{"$function":{"body":"f","args":[],"lang":"js"}}}}]}`, RuleMongoOperatorForbidden},
		{`{"aggregate":"c","pipeline":[{"$addFields":{"x":{"$where":"1"}}}]}`, RuleMongoOperatorForbidden},
		{`{"aggregate":"c","pipeline":[{"$lookup":{"from":"ok","let":{"v":1},` +
			`"pipeline":[{"$match":{"$expr":{"$function":{"body":"f","args":[],"lang":"js"}}}}],"as":"x"}}]}`, RuleMongoOperatorForbidden},
		// $mergeCursors（契约禁止清单）。
		{`{"aggregate":"c","pipeline":[{"$match":{"a":{"$mergeCursors":{}}}}]}`, RuleMongoOperatorForbidden},
	}
	for _, tc := range cases {
		res, err := Check(EngineMongoDB, tc.sql)
		if err != nil {
			t.Fatalf("Check(mongodb, %s) returned error %v", tc.sql, err)
		}
		if res.Allowed {
			t.Errorf("Check(mongodb, %s) = allowed; want reject %s", tc.sql, tc.rule)
			continue
		}
		if res.RuleID != tc.rule {
			t.Errorf("Check(mongodb, %s) rule = %s (%s); want %s", tc.sql, res.RuleID, res.Reason, tc.rule)
		}
		if res.RiskLevel != risk.LevelNeedsApproval || res.Kind != KindUnknown {
			t.Errorf("Check(mongodb, %s) = %+v; want needs_approval/unknown", tc.sql, res)
		}
	}
}

// deepFilter 构造 n 层嵌套的 filter 文档。
func deepFilter(n int) string {
	return `{"find":"c","filter":` + strings.Repeat(`{"a":`, n) + `1` + strings.Repeat(`}`, n) + `}`
}

// TestMongoGuardBypasses 覆盖任务点名的绕过手法：JS 执行与服务端写
// 以任何嵌套形态进入命令文档都必须被拦截。
func TestMongoGuardBypasses(t *testing.T) {
	bypasses := []string{
		// 写命令藏在 filter 值里（服务端会忽略未知键，但守卫失败关闭：
		// $out/$merge 键名任何深度出现即拒）。
		`{"find":"c","filter":{"a":{"$out":"x"}}}`,
		`{"find":"c","filter":{"$and":[{"a":1},{"b":{"$merge":{"into":"x"}}}]}}`,
		// update 形态的操作符嵌在 filter：$where 携带 JS。
		`{"find":"c","filter":{"$where":"while(1){}"}}`,
		`{"find":"c","filter":{"a":{"$gt":{"$code":"malicious"}},"$where":"this.a"}}`,
		// aggregate 各阶段体内藏 $function/$accumulator。
		`{"aggregate":"c","pipeline":[{"$bucketAuto":{"groupBy":{"$function":{"body":"f","args":[],"lang":"js"}},"buckets":2}}]}`,
		`{"aggregate":"c","pipeline":[{"$sortByCount":{"$accumulator":{}}}]}`,
		// $unionWith 递归管道内藏 $where。
		`{"aggregate":"c","pipeline":[{"$unionWith":{"coll":"x","pipeline":[{"$match":{"$where":"1"}}]}}]}`,
		// $facet 子管道内藏 $function。
		`{"aggregate":"c","pipeline":[{"$facet":{"a":[{"$group":{"_id":null,"x":{"$function":{"body":"f","args":[],"lang":"js"}}}}]}}]}`,
		// maxTimeMS 超限的 DoS 形态。
		`{"aggregate":"c","pipeline":[{"$match":{}}],"maxTimeMS":2147483647}`,
		// limit 超大试图拉全库。
		`{"find":"c","limit":999999}`,
		// filter 里嵌巨型文档（资源耗尽形态）。
		`{"find":"c","filter":{"$or":[` + strings.Repeat(`{"a":1},`, 9000) + `{"b":2}]}}`,
	}
	for _, sql := range bypasses {
		res, err := Check(EngineMongoDB, sql)
		if err != nil {
			t.Fatalf("Check(mongodb, %s) returned error %v", sql, err)
		}
		if res.Allowed {
			t.Errorf("bypass %s was ALLOWED (%+v)", sql, res)
		}
	}
}

// TestMongoCheckEntryBypassesLexSQL 验证 mongodb 分流发生在 lexSQL 之前：
// JSON 的 \" 转义在 SQL 词法眼里是未闭合字符串，若误走词法会错误拒绝。
func TestMongoCheckEntryBypassesLexSQL(t *testing.T) {
	sql := `{"find":"c","filter":{"msg":"a\"b"}}`
	res, err := Check(EngineMongoDB, sql)
	if err != nil {
		t.Fatalf("Check returned error: %v", err)
	}
	if !res.Allowed || res.RuleID != RuleMongoReadonly {
		t.Fatalf("escaped-quote JSON must pass the guard, got %+v", res)
	}
	// 同样的输入在 mysql 词法下按 SQL 处理（未闭合引号），证明两套语义隔离。
	lexRes, _ := Check(EngineMySQL, sql)
	if lexRes.Allowed {
		t.Fatalf("mysql engine must not allow JSON command documents")
	}
}

// TestParseMongoCommandFields 验证执行器复用的结构化抽取（契约 5.4：
// Query 解析命令文档后分发——执行器与守卫共用同一份解析）。
func TestParseMongoCommandFields(t *testing.T) {
	cmd, res, err := ParseMongoCommand(
		`{"find":"orders","filter":{"a":1},"limit":10,"skip":3,"batchSize":50,` +
			`"singleBatch":true,"comment":"q","maxTimeMS":5000,"allowDiskUse":true}`)
	if err != nil || !res.Allowed {
		t.Fatalf("ParseMongoCommand allowed path failed: %+v %v", res, err)
	}
	if cmd.Command != "find" || cmd.Collection != "orders" {
		t.Errorf("command/collection = %s/%s", cmd.Command, cmd.Collection)
	}
	if cmd.Limit == nil || *cmd.Limit != 10 || cmd.Skip == nil || *cmd.Skip != 3 ||
		cmd.BatchSize == nil || *cmd.BatchSize != 50 {
		t.Errorf("numeric params = %+v", cmd)
	}
	if !cmd.SingleBatch || !cmd.AllowDiskUse || cmd.Comment != "q" ||
		cmd.MaxTimeMS == nil || *cmd.MaxTimeMS != 5000 {
		t.Errorf("flag params = %+v", cmd)
	}

	// 未提供的参数必须保持 nil/零值（执行器据此区分「不限制」与「0」）。
	cmd2, _, err := ParseMongoCommand(`{"find":"c"}`)
	if err != nil {
		t.Fatalf("ParseMongoCommand minimal failed: %v", err)
	}
	if cmd2.Limit != nil || cmd2.Skip != nil || cmd2.MaxTimeMS != nil || cmd2.BatchSize != nil {
		t.Errorf("absent params must stay nil, got %+v", cmd2)
	}

	// 拒绝路径返回带 RuleID 的 Result 与错误。
	_, res3, err3 := ParseMongoCommand(`{"drop":"c"}`)
	if err3 == nil || res3.Allowed || res3.RuleID != RuleMongoCmdNotWhitelisted {
		t.Errorf("rejection path = %+v %v", res3, err3)
	}
}
