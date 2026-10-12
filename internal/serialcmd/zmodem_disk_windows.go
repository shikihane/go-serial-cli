//go:build windows

package serialcmd

import (
	"errors"
	"math"

	"golang.org/x/sys/windows"
)

// zFreeSpace answers ZFREECNT with the free bytes under dir, capped to the
// 32-bit header field.
func zFreeSpace(dir string) uint32 {
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return math.MaxUint32
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(path, &free, &total, &totalFree); err != nil {
		return math.MaxUint32
	}
	if free > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(free)
}

func isDiskFull(err error) bool {
	return errors.Is(err, windows.ERROR_DISK_FULL) || errors.Is(err, windows.ERROR_HANDLE_DISK_FULL)
}
