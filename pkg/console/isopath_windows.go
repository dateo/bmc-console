//go:build windows

package console

import (
	"os"
	"syscall"
	// bmc-vnc uses the standard library Windows attribute constant.
)

func isReparsePoint(info os.FileInfo) bool {
	if info.Mode()&os.ModeSymlink != 0 {
		return true
	}
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
