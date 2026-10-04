package ai

import (
	"syscall"
	"time"
	"unsafe"
)

// Go's monotonic clock on Windows advances in timer ticks, too coarse to
// time a single decision, so use the performance counter instead.
var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	qpc      = kernel32.NewProc("QueryPerformanceCounter")
	qpcFreq  = func() int64 {
		var f int64
		kernel32.NewProc("QueryPerformanceFrequency").Call(uintptr(unsafe.Pointer(&f)))
		return f
	}()
)

type tick int64

func now() tick {
	var c int64
	qpc.Call(uintptr(unsafe.Pointer(&c)))
	return tick(c)
}

func since(t tick) time.Duration {
	d := int64(now() - t)
	return time.Duration(d/qpcFreq*1e9 + d%qpcFreq*1e9/qpcFreq)
}
