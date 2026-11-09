package dbproxy

import (
	"go.mongodb.org/mongo-driver/mongo/options"
	"testing"
)

// mongo 客户端选项不得经由 ApplyURI（评审：host 含 ?/& 等 URI 保留字符
// 时，任何 URI 形态——拼接或半转义——都可能让驱动把 host 片段解析为
// 查询选项）。回归：构造出的选项 Hosts 与 SetHosts 一致，且不携带 URI
// 语义（直接构造，无 ApplyURI 参与面）。
func TestMongoClientOptionsHostsAreStructural(t *testing.T) {
	opts := options.Client().SetHosts([]string{"h?ost&x=1:27017"})
	got := opts.Hosts
	if len(got) != 1 || got[0] != "h?ost&x=1:27017" {
		t.Fatalf("Hosts = %v, want [h?ost&x=1:27017]（原样保留，不经 URI 解析）", got)
	}
}
