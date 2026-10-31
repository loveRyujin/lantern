//go:build darwin

package supervisor

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// allocatedRegularBytes is the physical data-fork size. On APFS, st_blocks can
// describe a fully allocated span even when the caller only wrote one byte at
// a high offset; ATTR_FILE_DATAALLOCSIZE is the physical size after a punch.
func allocatedRegularBytes(path string, _ unix.Stat_t) (uint64, error) {
	p, err := unix.BytePtrFromString(path)
	if err != nil {
		return 0, err
	}
	al := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Fileattr: unix.ATTR_FILE_DATAALLOCSIZE}
	// length is 4 bytes. The following off_t is aligned to 4, not 8.
	var buf [16]byte
	_, _, errno := unix.Syscall6(unix.SYS_GETATTRLIST, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&al)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unix.FSOPT_NOFOLLOW), 0)
	runtime.KeepAlive(p)
	runtime.KeepAlive(&al)
	if errno != 0 {
		return 0, errno
	}
	n := binary.LittleEndian.Uint32(buf[:4])
	if n < 12 {
		return 0, fmt.Errorf("short data allocation attribute: %d", n)
	}
	alloc := int64(binary.LittleEndian.Uint64(buf[4:12]))
	if alloc < 0 {
		return 0, fmt.Errorf("negative data allocation")
	}
	return uint64(alloc), nil
}
