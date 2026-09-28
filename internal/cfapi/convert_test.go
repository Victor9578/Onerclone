package cfapi

// convert_test.go —— 只读属性文件的占位符转换（v0.3.2 真机收尾失败根因）：
// FILE_ATTRIBUTE_READONLY 会让 GENERIC_WRITE 打开被拒（Access is denied），
// ConvertToPlaceholder 必须临时清属性、转换后恢复，且不得把打开错误抛给上层。

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func attrs(t *testing.T, p string) uint32 {
	t.Helper()
	a, err := syscall.GetFileAttributes(utf16ptr(p))
	if err != nil {
		t.Fatalf("GetFileAttributes: %v", err)
	}
	return a
}

func TestConvertToPlaceholderReadonly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ro.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Windows 下无写权限位 = FILE_ATTRIBUTE_READONLY
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	const attrReadOnly uint32 = 0x1
	if attrs(t, p)&attrReadOnly == 0 {
		t.Skip("本机 chmod 未落 ReadOnly 属性，跳过")
	}

	err := ConvertToPlaceholder(p, ConvertFlagMarkInSync)
	// 文件不在同步根下 → 预期是 HRESULT 失败或成功；唯独不许是打开阶段失败
	if err != nil {
		if strings.Contains(err.Error(), "open for convert") {
			t.Fatalf("只读文件 GENERIC_WRITE 打开被拒（应被内部清属性兜住）: %v", err)
		}
		if strings.Contains(err.Error(), "clear readonly") {
			t.Fatalf("清只读属性失败: %v", err)
		}
	}
	// 无论转换成败，用户的只读属性必须恢复
	if attrs(t, p)&attrReadOnly == 0 {
		t.Fatal("只读属性未恢复")
	}
}
