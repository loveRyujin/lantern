//go:build darwin

package supervisor

import (
	"encoding/json"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

var resourceWorkSink atomic.Uint64

func resourceBusy(d time.Duration) {
	end := time.Now().Add(d)
	x := uint64(1)
	for time.Now().Before(end) {
		for i := 0; i < 4096; i++ {
			x = x*6364136223846793005 + 1442695040888963407
		}
		resourceWorkSink.Store(x)
	}
}
func resourceWorkerMain(mode Mode, work string) int {
	if !knownResourceMode(mode) {
		return 125
	}
	if fileMode(mode) {
		if work == "" {
			return 125
		}
		return runFileWorkload(mode, work)
	}
	if work != "" {
		return 125
	}
	r := workerMetadata(mode)
	var mapping []byte
	if mode == TouchMemory || mode == UntouchedMemory {
		size := 96 << 20
		if mode == UntouchedMemory {
			size = 256 << 20
		}
		var e error
		mapping, e = unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
		if e != nil {
			return 125
		}
		defer unix.Munmap(mapping)
		r.MappedBytes = uint64(len(mapping))
	}
	if json.NewEncoder(os.Stdout).Encode(r) != nil {
		return 125
	}
	switch mode {
	case Normal:
		time.Sleep(100 * time.Millisecond)
	case CPUBusy:
		resourceBusy(3 * time.Second)
	case LowCPUSleep:
		time.Sleep(4 * time.Second)
	case UntouchedMemory:
		time.Sleep(time.Second)
		runtime.KeepAlive(mapping)
	case TouchMemory:
		for offset := 0; offset < len(mapping); offset += 4096 {
			mapping[offset] = 1
			touched := offset + 4096
			if touched%(16<<20) == 0 && (touched == 16<<20 || touched == 32<<20 || touched == 64<<20 || touched == 96<<20) {
				r.TouchedBytes = uint64(touched)
				if json.NewEncoder(os.Stdout).Encode(r) != nil {
					return 125
				}
			}
		}
		time.Sleep(time.Second)
		runtime.KeepAlive(mapping)
	case MultiThreadCPU:
		var ready sync.WaitGroup
		ready.Add(2)
		start := make(chan struct{})
		finished := make(chan struct{}, 2)
		for _, d := range []time.Duration{300 * time.Millisecond, 3 * time.Second} {
			go func(d time.Duration) {
				runtime.LockOSThread()
				ready.Done()
				<-start
				resourceBusy(d)
				finished <- struct{}{} /* returning locked retires this OS thread */
			}(d)
		}
		ready.Wait()
		close(start)
		<-finished
		<-finished
	}
	return 0
}
