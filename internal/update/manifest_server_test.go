package update

import (
	"strings"
	"testing"
)

// server 段是可选的（老清单没有它），但一旦出现就必须自洽 —— 残缺的 sha256 会在
// 「校验失败」那条路径上被截取前 12 位打日志，字段太短就是切片越界 panic。
func TestValidateServerSection(t *testing.T) {
	base := func() Manifest {
		return Manifest{
			Version: "0.2.9",
			Client:  Asset{URL: "https://example.invalid/app.exe", SHA256: strings.Repeat("a", 64)},
		}
	}

	// 没有 server 段：合法，老清单就长这样
	noServer := base()
	if err := noServer.Validate(); err != nil {
		t.Errorf("没有 server 段时应该通过，实际 %v", err)
	}

	// 合法 server 段
	m := base()
	m.Server = &Asset{URL: "https://example.invalid/antapp-linkd", SHA256: strings.Repeat("b", 64)}
	if err := m.Validate(); err != nil {
		t.Errorf("合法 server 段应该通过，实际 %v", err)
	}

	cases := []struct {
		name   string
		server Asset
		want   string
	}{
		{"缺 url", Asset{SHA256: strings.Repeat("b", 64)}, "server 段缺少 url"},
		{"sha256 为空", Asset{URL: "https://example.invalid/d"}, "server.sha256"},
		{"sha256 太短", Asset{URL: "https://example.invalid/d", SHA256: "abc"}, "server.sha256"},
		{"sha256 非十六进制", Asset{URL: "https://example.invalid/d", SHA256: strings.Repeat("z", 64)}, "不是合法十六进制"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := base()
			m.Server = &c.server
			err := m.Validate()
			if err == nil {
				t.Fatal("应该报错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息 %q 应该提到 %q", err.Error(), c.want)
			}
		})
	}
}

// 摘要字段残缺时 shortHash 不能越界，且结果要是原值的前缀。
func TestShortHashHandlesShortInput(t *testing.T) {
	for _, in := range []string{"", "abc", strings.Repeat("a", 12), strings.Repeat("a", 64)} {
		got := shortHash(in)
		if len(got) > 15 { // 12 个 ASCII 字符 + "…"（UTF-8 三字节）
			t.Errorf("shortHash(%d 位) = %q，太长了", len(in), got)
		}
		if !strings.HasPrefix(in, strings.TrimSuffix(got, "…")) {
			t.Errorf("shortHash(%q) = %q，不是前缀", in, got)
		}
	}
}
