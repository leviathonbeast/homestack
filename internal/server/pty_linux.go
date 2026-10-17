package server

import (
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// A pseudo-terminal from the standard library alone: /dev/ptmx hands out a
// pair, the "master" end stays here and the other end becomes a program's
// terminal. `virsh console` won't run without one.

func openPTY() (master, tty *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var unlock int32
	if err := ioctlFile(m, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		m.Close()
		return nil, nil, err
	}
	var n uint32
	if err := ioctlFile(m, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		m.Close()
		return nil, nil, err
	}
	t, err := os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(n), 10), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, t, nil
}

// setPTYSize tells the program how big the browser's terminal is.
func setPTYSize(f *os.File, cols, rows int) error {
	if cols <= 0 || rows <= 0 || cols > 1000 || rows > 1000 {
		return nil
	}
	ws := struct{ Row, Col, X, Y uint16 }{Row: uint16(rows), Col: uint16(cols)}
	return ioctlFile(f, syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
}

// ioctlFile runs an ioctl without f.Fd(), which would switch the file to
// blocking mode and stop Close from waking a pending Read.
func ioctlFile(f *os.File, req, arg uintptr) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}
