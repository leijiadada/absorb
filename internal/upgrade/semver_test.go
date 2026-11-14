package upgrade

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in      string
		want    string // 期望 String()；dev 为 "dev"
		wantDev bool
		wantErr bool
	}{
		{in: "0.1.0", want: "0.1.0"},
		{in: "v1.2.3", want: "1.2.3"},
		{in: "1.2.3-beta.1", want: "1.2.3-beta.1"},
		{in: "1.2.3+build.5", want: "1.2.3"}, // 构建元数据忽略
		{in: "1.2.3.4", want: "1.2.3"},       // 四段扩展版本取前三段
		{in: "0.0.0", want: "0.0.0"},
		{in: "dev", want: "dev", wantDev: true},
		{in: "0.0.0-dev-abc123", want: "dev", wantDev: true},
		{in: "1.2.3-dirty", want: "dev", wantDev: true},
		{in: "", wantErr: true},
		{in: "1.2", wantErr: true},
		{in: "a.b.c", wantErr: true},
		{in: "1.2.3.4.5", wantErr: true},
		{in: "1.2.3-", wantErr: true},        // 有预-release 分隔符但标识为空 → 非法
		{in: "1.2.3-beta..1", wantErr: true}, // 空标识
	}
	for _, c := range cases {
		got, err := ParseVersion(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseVersion(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVersion(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got.Dev != c.wantDev {
			t.Errorf("ParseVersion(%q).Dev = %v, want %v", c.in, got.Dev, c.wantDev)
		}
		if got.String() != c.want {
			t.Errorf("ParseVersion(%q) = %q, want %q", c.in, got.String(), c.want)
		}
	}
}

func TestVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.2.0", -1},
		{"0.2.0", "0.1.0", 1},
		{"0.2.0", "0.2.0", 0},
		{"0.9.0", "0.10.0", -1}, // 字典序陷阱：数值比较必须 0.9.0 < 0.10.0
		{"0.10.0", "0.9.0", 1},
		{"1.0.0-beta.1", "1.0.0", -1}, // 预发布 < 正式版
		{"1.0.0-beta.1", "1.0.0-beta.2", -1},
		{"1.0.0-beta.2", "1.0.0-beta.10", -1}, // 数字标识数值比较
		{"1.0.0-alpha", "1.0.0-beta", -1},     // 字母标识字典序
		{"1.0.0-1", "1.0.0-alpha", -1},        // 数字标识 < 字母标识
		{"1.0.0-beta", "1.0.0-beta.1", -1},    // 前缀相等时更短更小
	}
	for _, c := range cases {
		av, err := ParseVersion(c.a)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", c.a, err)
		}
		bv, err := ParseVersion(c.b)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", c.b, err)
		}
		if got := av.Compare(bv); got != c.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestExtensionVersion(t *testing.T) {
	cases := map[string]string{
		"0.2.0":        "0.2.0.0",
		"1.2.3":        "1.2.3.0",
		"0.2.0-beta.1": "0.2.0.1",
		"0.2.0-beta":   "0.2.0.1",
		"1.2.3.4":      "1.2.3.0",
		"dev":          "0.0.0.0",
		"garbage":      "0.0.0.0",
	}
	for in, want := range cases {
		if got := ExtensionVersion(in); got != want {
			t.Errorf("ExtensionVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
