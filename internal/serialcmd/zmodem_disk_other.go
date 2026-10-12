//go:build !windows

package serialcmd

import (
	"errors"
	"math"
	"syscall"
)

// zFreeSpace answers ZFREECNT. Outside Windows it reports no limit.
func zFreeSpace(dir string) uint32 {
	return math.MaxUint32
}

func isDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}
