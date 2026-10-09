//go:build windows

package fsutil

import (
	"golang.org/x/sys/windows"
)

// diskSpace 使用 GetDiskFreeSpaceEx 获取指定路径所在卷的可用空间。
func diskSpace(path string) (free, total int64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeAvail, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &freeAvail, &totalBytes, &totalFree); err != nil {
		return 0, 0, err
	}
	return int64(freeAvail), int64(totalBytes), nil
}
