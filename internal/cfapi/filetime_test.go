//go:build windows

package cfapi

// filetime_test.go —— 踩坑 #15 回归：FILETIME 偏移与越界日期。

import (
	"testing"
	"time"
)

func TestToFiletimeOffset(t *testing.T) {
	mt := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	want := (mt.Unix() + 11644473600) * 1e7
	if got := toFiletime(mt); got != want {
		t.Errorf("toFiletime(2026) = %d, want %d", got, want)
	}
	// 旧实现：偏移少一个 0（11644473600000000）→ 时间前移 116 年。
	// 确保坏公式的结果与正确值不同（防回归）。
	if legacy := mt.UnixNano()/100 + 11644473600000000; legacy == want {
		t.Fatal("测试前提失效：坏值与好值相等")
	}
}

func TestToFiletimeOldDateNeverNegative(t *testing.T) {
	// 真实样本：spike-remote/big.bin 的 mtime = 1694-08-19。
	// 旧实现算出负 FILETIME → CfCreatePlaceholders 报 0x80070057（无限重试）。
	mt := time.Date(1694, 8, 19, 4, 14, 39, 116997600, time.UTC)
	ft := toFiletime(mt)
	if ft <= 0 {
		t.Errorf("1694 年必须给出正 FILETIME, got %d", ft)
	}
	// 正确值应落在 1601 之后（约 93 年 = 2.95e16 个 100ns）
	if ft < 90*365*24*3600*1e7 {
		t.Errorf("1694 的 FILETIME 明显偏小: %d", ft)
	}

	// 1601 年之前没有合法表示 → 截断为 0（实测 FILETIME=0 可被接受）
	if got := toFiletime(time.Date(1500, 1, 1, 0, 0, 0, 0, time.UTC)); got != 0 {
		t.Errorf("1601 之前应截断为 0, got %d", got)
	}
}

func TestToFiletimeZeroAndFarFuture(t *testing.T) {
	if got := toFiletime(time.Time{}); got != 0 {
		t.Errorf("zero time → %d, want 0", got)
	}
	// 极远未来：秒级算术不能溢出成负数
	far := time.Date(40000, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := toFiletime(far); got <= 0 {
		t.Errorf("极远未来应饱和为正数, got %d", got)
	}
	// 常见现代时间仍精确
	mt := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	if got, want := toFiletime(mt), int64(11644473600)*1e7; got != want {
		t.Errorf("epoch → %d, want %d", got, want)
	}
}
