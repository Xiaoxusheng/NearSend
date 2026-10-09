//go:build !windows

package fsutil

import "golang.org/x/sys/unix"

// diskSpace 使用 statfs 获取指定路径所在文件系统的可用空间。
func diskSpace(path string) (free, total int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := int64(st.Bsize)
	return int64(st.Bavail) * bs, int64(st.Blocks) * bs, nil
}
