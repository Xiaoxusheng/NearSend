package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"普通中文件名", "报告.pdf", "报告.pdf", false},
		{"含空格", "my report final.docx", "my report final.docx", false},
		{"反斜杠注入", `..\..\windows\system32\evil.exe`, "evil.exe", false},
		{"正斜杠注入", "../../../etc/passwd", "passwd", false},
		{"非法字符", `a<b>c:d"e|f?g*h.txt`, "a_b_c_d_e_f_g_h.txt", false},
		{"控制字符", "abc\x00\x01def", "abc__def", false},
		{"仅有点", "...", "", true},
		{"仅空白", "   ", "", true},
		{"空字符串", "", "", true},
		{"保留设备名", "CON.txt", "_CON.txt", false},
		{"保留设备名小写", "nul", "_nul", false},
		{"首尾点与空格", "  .hidden.  ", "hidden", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := SanitizeName(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望错误，实际得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("非预期错误: %v", err)
			}
			if got != c.want {
				t.Fatalf("SanitizeName(%q) = %q, 期望 %q", c.in, got, c.want)
			}
			// 净化结果绝不能包含路径分隔符，否则可能逃逸目录。
			if strings.ContainsAny(got, `/\`) {
				t.Fatalf("净化结果仍含分隔符: %q", got)
			}
		})
	}
}

func TestSanitizeNameLongName(t *testing.T) {
	long := strings.Repeat("中", 500) + ".pdf"
	got, err := SanitizeName(long)
	if err != nil {
		t.Fatalf("非预期错误: %v", err)
	}
	if len(got) > maxNameRunes*3 {
		t.Fatalf("截断后仍过长: %d 字节", len(got))
	}
	if !strings.HasSuffix(got, ".pdf") {
		t.Fatalf("截断应保留扩展名，实际 %q", got[len(got)-12:])
	}
}

func TestSanitizeRelPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"docs/报告.pdf", "docs/报告.pdf"},
		{`docs\sub\a.txt`, "docs/sub/a.txt"},
		{"../../etc/passwd", "etc/passwd"},
		{"/absolute/path/file.txt", "absolute/path/file.txt"},
		{"a/./b/../c.txt", "a/b/c.txt"},
		{"..//../x", "x"},
	}
	for _, c := range cases {
		got, err := SanitizeRelPath(c.in)
		if err != nil {
			t.Fatalf("SanitizeRelPath(%q) 报错: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("SanitizeRelPath(%q) = %q, 期望 %q", c.in, got, c.want)
		}
		// 关键安全断言：净化后的相对路径不得包含任何上级引用。
		if strings.Contains(got, "..") {
			t.Fatalf("结果仍含上级引用: %q", got)
		}
	}
}

func TestSafeJoinRejectsEscape(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 正常情况
	got, err := SafeJoin(root, "sub", "a.txt")
	if err != nil {
		t.Fatalf("合法路径被拒绝: %v", err)
	}
	if !WithinRoot(root, got) {
		t.Fatalf("结果不在根目录内: %s", got)
	}

	// 各类逃逸尝试
	escapes := []string{
		"..", "../..", "../../../etc/passwd",
		filepath.Join("..", "outside.txt"),
	}
	for _, e := range escapes {
		if _, err := SafeJoin(root, e); err == nil {
			t.Fatalf("逃逸路径 %q 未被拒绝", e)
		}
	}

	// 绝对路径注入：filepath.Join 会把 root 吃掉，SafeJoin 必须检测出来。
	if p, err := SafeJoin(root, "/etc/passwd"); err == nil {
		if !WithinRoot(root, p) {
			t.Fatalf("绝对路径逃逸成功: %s", p)
		}
	}
}

func TestSafeJoinRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 创建符号链接通常需要管理员权限，跳过符号链接逃逸测试")
	}
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	if _, err := SafeJoin(root, "link", "evil.txt"); err == nil {
		t.Fatal("通过符号链接逃逸未被拒绝")
	}
}

func TestUniqueName(t *testing.T) {
	dir := t.TempDir()
	// 目标不存在时应返回原名。
	name, existed, err := UniqueName(dir, "报告.pdf", "rename", true)
	if err != nil || existed || name != "报告.pdf" {
		t.Fatalf("期望原名且无冲突, 得到 %q existed=%v err=%v", name, existed, err)
	}

	// 创建后按 rename 策略应生成 "报告 (1).pdf"。
	if err := os.WriteFile(filepath.Join(dir, "报告.pdf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, existed, err = UniqueName(dir, "报告.pdf", "rename", true)
	if err != nil {
		t.Fatal(err)
	}
	if !existed || name != "报告 (1).pdf" {
		t.Fatalf("期望 报告 (1).pdf, 得到 %q existed=%v", name, existed)
	}

	// 再创建一次，应继续递增。
	if err := os.WriteFile(filepath.Join(dir, "报告 (1).pdf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	name, _, _ = UniqueName(dir, "报告.pdf", "rename", true)
	if name != "报告 (2).pdf" {
		t.Fatalf("期望 报告 (2).pdf, 得到 %q", name)
	}

	// skip 策略：返回 exists=true 且保持原名。
	name, existed, _ = UniqueName(dir, "报告.pdf", "skip", false)
	if !existed || name != "报告.pdf" {
		t.Fatalf("skip 策略异常: %q %v", name, existed)
	}

	// overwrite 策略：返回原名，允许调用方覆盖。
	name, existed, _ = UniqueName(dir, "报告.pdf", "overwrite", false)
	if !existed || name != "报告.pdf" {
		t.Fatalf("overwrite 策略异常: %q %v", name, existed)
	}

	// neverOverwrite 兜底：即使策略是 overwrite 也必须改名，避免静默覆盖。
	name, existed, _ = UniqueName(dir, "报告.pdf", "overwrite", true)
	if !existed || name == "报告.pdf" {
		t.Fatalf("neverOverwrite 未生效: %q", name)
	}
}

func TestAtomicCommit(t *testing.T) {
	dir := t.TempDir()
	temp := filepath.Join(dir, "a.part")
	final := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(temp, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AtomicCommit(temp, final, false); err != nil {
		t.Fatalf("首次提交失败: %v", err)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatal("临时文件应已被移走")
	}
	b, _ := os.ReadFile(final)
	if string(b) != "hello" {
		t.Fatalf("内容不符: %q", b)
	}

	// 目标已存在且不允许替换：必须失败，绝不静默覆盖。
	if err := os.WriteFile(temp, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AtomicCommit(temp, final, false); err == nil {
		t.Fatal("不允许替换时却成功了")
	}
	b, _ = os.ReadFile(final)
	if string(b) != "hello" {
		t.Fatalf("原文件被意外修改: %q", b)
	}

	// 允许替换时应写入新内容。
	if err := AtomicCommit(temp, final, true); err != nil {
		t.Fatalf("允许替换时提交失败: %v", err)
	}
	b, _ = os.ReadFile(final)
	if string(b) != "new" {
		t.Fatalf("覆盖后内容不符: %q", b)
	}

	// 目标为目录时必须拒绝。
	sub := filepath.Join(dir, "adir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temp, []byte("z"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AtomicCommit(temp, sub, true); err == nil {
		t.Fatal("目标是目录却提交成功")
	}
}

func TestHashFileMatchesKnownVector(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	// "abc" 的 SHA-256 是公开测试向量。
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	got, err := HashFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("SHA-256 不符: %s", got)
	}
	if HashBytes([]byte("abc")) != want {
		t.Fatal("HashBytes 结果与 HashFile 不一致")
	}
}

func TestDiskSpace(t *testing.T) {
	free, total, err := DiskSpace(t.TempDir())
	if err != nil {
		t.Skipf("当前平台不支持磁盘探测: %v", err)
	}
	if free <= 0 || total <= 0 || free > total {
		t.Fatalf("磁盘数值不合理 free=%d total=%d", free, total)
	}
	ok, _, _ := FreeSpaceOK(t.TempDir(), 1024)
	if !ok {
		t.Fatal("1KB 应始终可用")
	}
}
