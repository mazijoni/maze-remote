//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Lists monitors in the same order ffmpeg's ddagrab sees them: the outputs of
// the default DXGI adapter (ddagrab's output_idx indexes into this list).
// Falls back to EnumDisplayMonitors if DXGI isn't usable.

var (
	dxgiDLL                = syscall.NewLazyDLL("dxgi.dll")
	procCreateDXGIFactory1 = dxgiDLL.NewProc("CreateDXGIFactory1")
	iidIDXGIFactory1       = syscall.GUID{Data1: 0x770aae78, Data2: 0xf26f, Data3: 0x4dba, Data4: [8]byte{0xa8, 0x29, 0x25, 0x3c, 0x83, 0xd1, 0xb3, 0x87}}
)

// COM vtable slots
const (
	vtRelease      = 2
	vtEnumAdapters = 7 // IDXGIFactory::EnumAdapters (IUnknown 3 + IDXGIObject 4)
	vtEnumOutputs  = 7 // IDXGIAdapter::EnumOutputs
	vtOutputGetDsc = 7 // IDXGIOutput::GetDesc
	dxgiNotFound   = 0x887A0002
)

type dxgiOutputDesc struct {
	DeviceName [32]uint16
	Desktop    rect
	Attached   int32
	Rotation   uint32
	Monitor    uintptr
}

// comObj is a COM interface pointer: its first field points at the vtable.
type comObj struct{ vtbl *[32]uintptr }

func comCall(obj *comObj, slot int, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(obj.vtbl[slot], append([]uintptr{uintptr(unsafe.Pointer(obj))}, args...)...)
	return r
}

func listScreensDXGI() (out []Screen, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("dxgi: %v", r)
		}
	}()
	if err := procCreateDXGIFactory1.Find(); err != nil {
		return nil, err
	}
	var factory *comObj
	if hr, _, _ := procCreateDXGIFactory1.Call(uintptr(unsafe.Pointer(&iidIDXGIFactory1)), uintptr(unsafe.Pointer(&factory))); hr != 0 || factory == nil {
		return nil, fmt.Errorf("CreateDXGIFactory1 failed: 0x%x", hr)
	}
	defer comCall(factory, vtRelease)

	var adapter *comObj
	if hr := comCall(factory, vtEnumAdapters, 0, uintptr(unsafe.Pointer(&adapter))); hr != 0 || adapter == nil {
		return nil, fmt.Errorf("EnumAdapters failed: 0x%x", hr)
	}
	defer comCall(adapter, vtRelease)

	for i := 0; i < 16; i++ {
		var output *comObj
		hr := comCall(adapter, vtEnumOutputs, uintptr(i), uintptr(unsafe.Pointer(&output)))
		if uint32(hr) == dxgiNotFound || output == nil {
			break
		}
		var d dxgiOutputDesc
		comCall(output, vtOutputGetDsc, uintptr(unsafe.Pointer(&d)))
		comCall(output, vtRelease)
		if d.Attached == 0 {
			continue
		}
		s := Screen{
			Index: i,
			X:     int(d.Desktop.Left), Y: int(d.Desktop.Top),
			W: int(d.Desktop.Right - d.Desktop.Left), H: int(d.Desktop.Bottom - d.Desktop.Top),
		}
		s.Primary = s.X == 0 && s.Y == 0
		out = append(out, s)
	}
	return out, nil
}

func listScreens() []Screen {
	ss, err := listScreensDXGI()
	if err != nil || len(ss) == 0 {
		for i, r := range monitors() {
			ss = append(ss, Screen{Index: i, X: int(r.Left), Y: int(r.Top), W: int(r.Right - r.Left), H: int(r.Bottom - r.Top), Primary: r.Left == 0 && r.Top == 0})
		}
	}
	for i := range ss {
		ss[i].Name = fmt.Sprintf("Screen %d", i+1)
	}
	return ss
}
