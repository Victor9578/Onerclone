package main

// namemap_test.go —— 方案 A：云端名↔本地名映射的单测。

import (
	"testing"
)

// fakeMeta 是 state.Store 的最小替身。
type fakeMeta struct{ m map[string]string }

func newFakeMeta() *fakeMeta { return &fakeMeta{m: map[string]string{}} }

func (f *fakeMeta) GetMeta(k string) (string, bool, error) {
	v, ok := f.m[k]
	return v, ok, nil
}

func (f *fakeMeta) SetMeta(k, v string) error { f.m[k] = v; return nil }

func TestSanitizeSeg(t *testing.T) {
	cases := map[string]string{
		"plain.txt":     "plain.txt",           // 干净名必须原样（identity）
		"来自:分享":       "来自%3A分享",           // ASCII 冒号 → %3A（rclone 可访问）
		"a<b>c":         "a%3Cb%3Ec",           // 尖括号
		`a"b|c?d*e`:     "a%22b%7Cc%3Fd%2Ae",   // 其余非法字符
		"CON":           "%5FCON",              // 保留设备名
		"lpt9.dat":      "%5Flpt9.dat",         // 不分大小写、不看扩展名
		"name.":         "name%2E",             // 尾点
		"name ":         "name%20",             // 尾空格
		"mid.dle":       "mid.dle",             // 中间的点不动
		"来自：分享":        "来自%EF%BC%9A分享",     // 全角冒号会被 rclone NFKC 折成 ASCII，必转义
		"ＡＢＣ.txt":       "%EF%BC%A1%EF%BC%A2%EF%BC%A3.txt", // 全角字母同理
	}
	for in, want := range cases {
		if got := sanitizeSeg(in); got != want {
			t.Errorf("sanitizeSeg(%q) = %q, want %q", in, got, want)
		}
	}
	// 全角字符不能原样保留：rclone 在 Windows 上会 NFKC 折形，
	// 磁盘名（Go 看到）与 rclone 报的名会不同 → copyfile 找不到文件（踩坑 #16）
	if got := sanitizeSeg("来自：分享"); got == "来自：分享" {
		t.Error("全角冒号不应原样保留（rclone 会折成 ASCII 冒号）")
	}
	// 转义必须单射：形近的三个输入不得产出相同本地名
	seen := map[string]string{}
	for _, in := range []string{"a:b", "a%3Ab", "a：b"} {
		out := sanitizeSeg(in)
		if prev, ok := seen[out]; ok {
			t.Errorf("转义不单射: %q 与 %q 都映射到 %q", prev, in, out)
		}
		seen[out] = in
	}
}

func TestLocalPathAndBack(t *testing.T) {
	m := newNameMap(nil)

	got := m.localPath("来自:分享/子目录/正常文件.txt")
	want := "来自%3A分享/子目录/正常文件.txt"
	if got != want {
		t.Fatalf("localPath = %q, want %q", got, want)
	}
	// 反向还原：engine 的键必须回到云端原名
	if back := m.cloudPath(got); back != "来自:分享/子目录/正常文件.txt" {
		t.Fatalf("cloudPath = %q, want 云端原名", back)
	}
	// 干净的本地新文件：不登记、原样
	if back := m.cloudPath("新文件.txt"); back != "新文件.txt" {
		t.Fatalf("干净名不应被改写: %q", back)
	}
	// 表里只该有 1 条改名记录（"来自:分享"），干净段不登记
	if len(m.toLoc) != 1 {
		t.Fatalf("映射表应只存改名项, got %d 条: %v", len(m.toLoc), m.toLoc)
	}
}

func TestEscapeInjectiveAcrossLookalikes(t *testing.T) {
	// 三个形近输入：ASCII 冒号 / 字面 %3A / 全角冒号 —— 本地名必须互不相同
	// 且都能反查回各自云端原名（否则会串名、错删错传）
	m := newNameMap(nil)
	a := m.localSeg("a:b")
	b := m.localSeg("a%3Ab")
	c := m.localSeg("a：b")
	if a == b || b == c || a == c {
		t.Fatalf("本地名撞车: a:b=%q a%%3Ab=%q a：b=%q", a, b, c)
	}
	for _, tc := range []struct{ cloud, local string }{
		{"a:b", a}, {"a%3Ab", b}, {"a：b", c},
	} {
		if got := m.cloudPath(tc.local); got != tc.cloud {
			t.Errorf("反查 %q = %q, want %q", tc.local, got, tc.cloud)
		}
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	st := newFakeMeta()

	m1 := newNameMap(st)
	p1 := m1.localPath("来自:分享/深层/x.txt")
	if p1 != "来自%3A分享/深层/x.txt" {
		t.Fatalf("首次映射 = %q", p1)
	}

	// 模拟重启：同 store 新建实例，必须认得上次的映射（否则本地会多出一套名字）
	m2 := newNameMap(st)
	if got := m2.cloudPath(p1); got != "来自:分享/深层/x.txt" {
		t.Fatalf("重启后反查 = %q, want 云端原名", got)
	}
	if got := m2.localPath("来自:分享/深层/x.txt"); got != p1 {
		t.Fatalf("重启后正向映射 = %q, want %q", got, p1)
	}
}

func TestCorruptedMetaRecovers(t *testing.T) {
	st := newFakeMeta()
	st.m[nameMapMetaKey] = "{不是 json"
	m := newNameMap(st)
	if got := m.localPath("a:b"); got != "a%3Ab" {
		t.Fatalf("损坏后应按空表重建, got %q", got)
	}
}

func TestNilMapIsPassthrough(t *testing.T) {
	var m *nameMap
	if got := m.localPath("x:y"); got != "x:y" {
		t.Errorf("nil 映射表应直通, got %q", got)
	}
	if got := m.cloudPath("x:y"); got != "x:y" {
		t.Errorf("nil 映射表应直通, got %q", got)
	}
	m.observeCloud("anything") // 不应 panic
}
