//go:build windows

package main

// Virtual screen fallback, the way RustDesk, Parsec and Sunshine handle hosts
// whose monitors are off: when a physical monitor sleeps, Windows often treats
// it as unplugged, so there's nothing to capture. A virtual display never
// sleeps, so Windows always has a screen to draw the desktop on.
//
// Uses the Parsec Virtual Display driver (MIT, signed), installed separately:
//   https://builds.parsec.app/vdd/parsec-vdd-0.41.0.0.exe  (run as admin, or with /S)
// Protocol from github.com/nomi-san/parsec-vdd core/parsec-vdd.h.

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var vddAdapterGUID = windows.GUID{Data1: 0x00b41627, Data2: 0x04c4, Data3: 0x429e, Data4: [8]byte{0xa2, 0x6e, 0x02, 0x65, 0xcf, 0x50, 0xc8, 0xfa}}

const (
	vddIoctlAdd     = 0x0022e004
	vddIoctlRemove  = 0x0022a008
	vddIoctlUpdate  = 0x0022a00c
	vddIoctlVersion = 0x0022e010
)

var vdd struct {
	mu      sync.Mutex
	h       windows.Handle
	plugged map[int]bool
	stop    chan struct{}
	warned  bool
}

// vddOpen finds and opens the driver. Returns false if it isn't installed.
func vddOpen() bool {
	if vdd.h != 0 {
		return true
	}
	paths, err := windows.CM_Get_Device_Interface_List("", &vddAdapterGUID, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
	if err != nil || len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		name, err := windows.UTF16PtrFromString(p)
		if err != nil {
			continue
		}
		h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
			windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_NO_BUFFERING|windows.FILE_FLAG_OVERLAPPED|windows.FILE_FLAG_WRITE_THROUGH, 0)
		if err == nil && h != windows.InvalidHandle {
			vdd.h = h
			vdd.plugged = map[int]bool{}
			v, _ := vddIoctl(vddIoctlVersion, nil)
			log.Printf("virtual screen driver found (Parsec VDD 0.%d)", v)
			return true
		}
		if !vdd.warned {
			vdd.warned = true
			log.Printf("virtual screen driver is installed but couldn't be opened: %v (try running the agent as administrator)", err)
		}
	}
	return false
}

// vddIoctl sends one command to the driver (32-byte input, 4-byte output, overlapped).
func vddIoctl(code uint32, data []byte) (int, error) {
	var in [32]byte
	copy(in[:], data)
	var out uint32
	ev, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return -1, err
	}
	defer windows.CloseHandle(ev)
	ov := windows.Overlapped{HEvent: ev}
	err = windows.DeviceIoControl(vdd.h, code, &in[0], uint32(len(in)), (*byte)(unsafe.Pointer(&out)), 4, nil, &ov)
	if err != nil && !errors.Is(err, windows.ERROR_IO_PENDING) {
		return -1, err
	}
	if ev, _ := windows.WaitForSingleObject(ev, 5000); ev != windows.WAIT_OBJECT_0 {
		windows.CancelIoEx(vdd.h, &ov)
		return -1, fmt.Errorf("driver didn't answer")
	}
	var n uint32
	if err := windows.GetOverlappedResult(vdd.h, &ov, &n, false); err != nil {
		return -1, err
	}
	return int(out), nil
}

// vscreenAvailable reports whether a virtual screen can be added.
func vscreenAvailable() bool {
	vdd.mu.Lock()
	defer vdd.mu.Unlock()
	return vddOpen()
}

// vscreenAdd plugs in a virtual screen and keeps it alive (the driver unplugs
// displays that aren't pinged at least every ~100ms).
func vscreenAdd() (int, error) {
	vdd.mu.Lock()
	defer vdd.mu.Unlock()
	if !vddOpen() {
		return -1, fmt.Errorf("virtual screen driver not installed")
	}
	idx, err := vddIoctl(vddIoctlAdd, nil)
	if err != nil || idx < 0 {
		return -1, fmt.Errorf("couldn't add a virtual screen: %v", err)
	}
	vddIoctl(vddIoctlUpdate, nil)
	vdd.plugged[idx] = true
	if vdd.stop == nil {
		vdd.stop = make(chan struct{})
		go vddKeepAlive(vdd.stop)
	}
	return idx, nil
}

func vddKeepAlive(stop chan struct{}) {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			vdd.mu.Lock()
			if vdd.h != 0 {
				vddIoctl(vddIoctlUpdate, nil)
			}
			vdd.mu.Unlock()
		}
	}
}

// vscreenRemove unplugs a virtual screen added with vscreenAdd.
func vscreenRemove(idx int) {
	vdd.mu.Lock()
	defer vdd.mu.Unlock()
	if vdd.h == 0 || !vdd.plugged[idx] {
		return
	}
	// the driver expects the index as a big-endian 16-bit number
	vddIoctl(vddIoctlRemove, []byte{byte(idx >> 8), byte(idx)})
	vddIoctl(vddIoctlUpdate, nil)
	delete(vdd.plugged, idx)
	if len(vdd.plugged) == 0 && vdd.stop != nil {
		close(vdd.stop)
		vdd.stop = nil
	}
}
