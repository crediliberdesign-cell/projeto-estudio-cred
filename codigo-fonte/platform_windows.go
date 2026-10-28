//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

func replaceFile(src, dst string) error {
	a, e := syscall.UTF16PtrFromString(src)
	if e != nil {
		return e
	}
	b, e := syscall.UTF16PtrFromString(dst)
	if e != nil {
		return e
	}
	p := syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")
	r, _, er := p.Call(uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(b)), uintptr(1|8))
	if r == 0 {
		return er
	}
	return nil
}
