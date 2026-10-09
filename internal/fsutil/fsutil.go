// Package fsutil 提供安全文件操作：文件名净化、路径逃逸防护、
// 重名策略、临时文件原子提交、磁盘空间检测。
//
// 所有落盘路径都必须经过 SafeJoin，任何情况下不允许拼接出受控根目录之外的路径。
package fsutil

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrPathEscape 表示目标路径逃逸出受控根目录。
var ErrPathEscape = errors.New("path escapes controlled root")

// ErrEmptyName 表示文件名为空或全为非法字符。
var ErrEmptyName = errors.New("empty or fully sanitized file name")

// windowsReserved 是 Windows 保留设备名，作为文件名（无论扩展名）时不可用。
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// 跨平台非法字符：Windows 禁止 \ / : * ? " < > |，控制字符在所有平台都危险。
var illegalChars = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`)

// maxNameRunes 单个文件名（含扩展名）的字符数上限。
// 200 是保守值：Linux 限制 255 字节，中文 UTF-8 占 3 字节，200 字符 ≈ 600 字节，
// 因此对纯中文名会进一步按字节截断，见 clampBytes。
const maxNameRunes = 200

// maxPathBytes 完整路径字节上限，为 Windows MAX_PATH 留出余量。
const maxPathBytes = 240

// SanitizeName 把任意输入净化成一个安全的、单层的文件名（不含目录分隔符）。
// 保留中文与空格，替换非法字符，去除首尾空白与点，处理保留名。
func SanitizeName(raw string) (string, error) {
	if raw == "" {
		return "", ErrEmptyName
	}
	// 统一分隔符并只取最后一段，防止 `a/b` 或 `..\..\x` 之类的注入。
	raw = strings.ReplaceAll(raw, "\\", "/")
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		raw = raw[i+1:]
	}
	raw = strings.TrimSpace(raw)
	// 去掉控制字符与非法字符。
	raw = illegalChars.ReplaceAllString(raw, "_")
	// 去掉首尾的点与空格：`..` 和 `... ` 在 Windows 上会出问题。
	raw = strings.Trim(raw, " .")
	if raw == "" || raw == "." || raw == ".." {
		return "", ErrEmptyName
	}
	if utf8.RuneCountInString(raw) > maxNameRunes {
		raw = clampBytes(raw, maxNameRunes*3)
	}
	// Windows 保留名：CON.pdf 同样非法，因此在主名上判断。
	base := raw
	if i := strings.Index(raw, "."); i > 0 {
		base = raw[:i]
	}
	if windowsReserved[strings.ToUpper(base)] {
		raw = "_" + raw
	}
	if raw == "" {
		return "", ErrEmptyName
	}
	return raw, nil
}

// clampBytes 按字节数截断字符串，保持 UTF-8 边界完整，并尽量保留扩展名。
func clampBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	ext := filepath.Ext(s)
	if len(ext) > 20 {
		ext = ""
	}
	stem := strings.TrimSuffix(s, ext)
	for len(stem)+len(ext) > limit && len(stem) > 0 {
		_, size := utf8.DecodeLastRuneInString(stem)
		stem = stem[:len(stem)-size]
	}
	if stem == "" {
		stem = "file"
	}
	return stem + ext
}

// SanitizeRelPath 净化相对路径并保留目录结构。
// 每一段都经过 SanitizeName，因此不会出现 .. 或绝对路径。
func SanitizeRelPath(raw string) (string, error) {
	raw = strings.ReplaceAll(raw, "\\", "/")
	raw = strings.TrimPrefix(raw, "/")
	parts := strings.Split(raw, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || p == "." || p == ".." {
			// 丢弃空段与上级引用，阻止目录逃逸。
			continue
		}
		name, err := SanitizeName(p)
		if err != nil {
			continue
		}
		out = append(out, name)
	}
	if len(out) == 0 {
		return "", ErrEmptyName
	}
	joined := strings.Join(out, "/")
	if len(joined) > maxPathBytes {
		// 层级过深时只保留最后若干段，保证路径可用。
		for len(out) > 1 && len(strings.Join(out, "/")) > maxPathBytes {
			out = out[1:]
		}
		joined = strings.Join(out, "/")
		joined = clampBytes(joined, maxPathBytes)
	}
	return joined, nil
}

// SafeJoin 把若干相对片段拼到 root 之下，并强制校验结果仍在 root 内。
//
// 校验分两步：先清理成绝对路径比较前缀，再对最终路径做 EvalSymlinks
// （目标可能尚不存在，因此逐级向上找到最近的已存在祖先再解析），
// 阻止通过符号链接逃逸受控目录。
func SafeJoin(root string, parts ...string) (string, error) {
	if root == "" {
		return "", errors.New("empty root")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		cleaned = append(cleaned, filepath.FromSlash(p))
	}
	target := filepath.Join(append([]string{absRoot}, cleaned...)...)
	target, err = filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if !WithinRoot(absRoot, target) {
		return "", ErrPathEscape
	}
	// 符号链接检查：解析最近存在的祖先目录。
	resolvedRoot, err := resolveExisting(absRoot)
	if err == nil {
		if resolved, rerr := resolveExistingAncestor(target); rerr == nil {
			if !WithinRoot(resolvedRoot, resolved) {
				return "", ErrPathEscape
			}
		}
	}
	return target, nil
}

// WithinRoot 判断 target 是否等于 root 或位于 root 之下（大小写按平台处理）。
func WithinRoot(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if runtime.GOOS == "windows" {
		root = strings.ToLower(root)
		target = strings.ToLower(target)
	}
	if target == root {
		return true
	}
	sep := string(os.PathSeparator)
	return strings.HasPrefix(target, root+sep)
}

func resolveExisting(p string) (string, error) {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r, nil
	}
	return p, nil
}

// resolveExistingAncestor 向上寻找最近的已存在祖先做符号链接解析，再拼回剩余片段。
func resolveExistingAncestor(p string) (string, error) {
	rest := []string{}
	cur := filepath.Clean(p)
	for {
		if fi, err := os.Lstat(cur); err == nil {
			_ = fi
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				resolved = cur
			}
			for i := len(rest) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, rest[i])
			}
			return resolved, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// UniqueName 按策略为 destDir 下的 name 生成唯一文件名。
//
// policy 为 rename 时返回 "报告 (1).pdf" 形式；skip 时返回 exists=true；
// overwrite 时直接返回原名（调用方随后覆盖）。neverOverwrite 为 true 时
// 即使 policy 是 overwrite 也强制改名——用于「绝不静默覆盖」的兜底路径。
func UniqueName(destDir, name string, policy string, neverOverwrite bool) (finalName string, exists bool, err error) {
	name, err = SanitizeName(name)
	if err != nil {
		return "", false, err
	}
	full, err := SafeJoin(destDir, name)
	if err != nil {
		return "", false, err
	}
	st, statErr := os.Lstat(full)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return name, false, nil
		}
		return "", false, statErr
	}
	exists = true
	if policy == "overwrite" && !neverOverwrite {
		if st.IsDir() {
			return "", true, fmt.Errorf("target is a directory: %s", name)
		}
		return name, true, nil
	}
	if policy == "skip" {
		return name, true, nil
	}
	// rename：生成 "原名 (n).扩展名"
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i <= 9999; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		p, jerr := SafeJoin(destDir, candidate)
		if jerr != nil {
			return "", false, jerr
		}
		if _, serr := os.Lstat(p); os.IsNotExist(serr) {
			return candidate, true, nil
		}
	}
	return "", true, errors.New("cannot generate unique file name")
}

// EnsureDir 创建目录（含父级），权限 0o755。
func EnsureDir(dir string) error {
	if dir == "" {
		return errors.New("empty dir")
	}
	return os.MkdirAll(dir, 0o755)
}

// AtomicCommit 把临时文件原子地提交为最终路径。
//
// 同目录下 os.Rename 在 POSIX 上是原子的；Windows 上若目标已存在需先删除
// （仅当 allowReplace 为 true，且已确认目标不是目录）。跨卷时退化为复制 + 删除。
func AtomicCommit(tempPath, finalPath string, allowReplace bool) error {
	if _, err := os.Lstat(tempPath); err != nil {
		return fmt.Errorf("temp file missing: %w", err)
	}
	if st, err := os.Lstat(finalPath); err == nil {
		if st.IsDir() {
			return fmt.Errorf("target is a directory: %s", finalPath)
		}
		if !allowReplace {
			return os.ErrExist
		}
		if runtime.GOOS == "windows" {
			if err := os.Remove(finalPath); err != nil {
				return err
			}
		}
	}
	if err := os.Rename(tempPath, finalPath); err == nil {
		return nil
	}
	// 跨设备回退路径。
	if err := copyFile(tempPath, finalPath); err != nil {
		return err
	}
	return os.Remove(tempPath)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// HashFile 流式计算文件 SHA-256，返回十六进制字符串。不把整个文件读进内存。
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashReader 计算 reader 的 SHA-256，用于分块校验。
func HashReader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HashBytes 计算字节切片的 SHA-256。
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// DiskSpace 返回指定路径所在文件系统的可用字节数与总字节数。
// 使用平台实现；不支持的平台返回 unknown 错误。
func DiskSpace(path string) (free, total int64, err error) {
	return diskSpace(path)
}

// ClassifyWriteError 把底层 I/O 错误映射为稳定的错误码，便于前端给出可执行引导。
func ClassifyWriteError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, os.ErrPermission) {
		return "permission_denied"
	}
	if errors.Is(err, os.ErrExist) {
		return "conflict_exists"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "no space left"), strings.Contains(msg, "disk full"),
		strings.Contains(msg, "not enough space"), strings.Contains(msg, "disk quota"):
		return "disk_full"
	case strings.Contains(msg, "permission denied"), strings.Contains(msg, "access is denied"),
		strings.Contains(msg, "read-only file system"):
		return "permission_denied"
	case strings.Contains(msg, "file exists"), strings.Contains(msg, "already exists"):
		return "conflict_exists"
	}
	return "write_failed"
}

// FreeSpaceOK 判断在 path 所在盘是否还有至少 need 字节（保留 32MiB 余量）。
func FreeSpaceOK(path string, need int64) (bool, int64, error) {
	free, _, err := DiskSpace(path)
	if err != nil {
		// 无法探测时不阻塞业务，交由真实写入错误处理。
		return true, 0, nil
	}
	const reserve = 32 << 20
	return free-need > reserve, free, nil
}

// NowMillis 返回当前毫秒时间戳，统一时间来源。
func NowMillis() int64 { return time.Now().UnixMilli() }

// HumanBytes 生成人类可读的大小文本，供日志与导出使用。
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
