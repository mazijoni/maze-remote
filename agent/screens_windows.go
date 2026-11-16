//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"
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

// dxgiOutputs returns the attached outputs of one adapter, or ok=false when
// the adapter doesn't exist.
func dxgiOutputs(factory *comObj, ai int) (out []dxgiOutputDesc, idx []int, ok bool) {
	var adapter *comObj
	if hr := comCall(factory, vtEnumAdapters, uintptr(ai), uintptr(unsafe.Pointer(&adapter))); hr != 0 || adapter == nil {
		return nil, nil, false
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
		if d.Attached != 0 {
			out, idx = append(out, d), append(idx, i)
		}
	}
	return out, idx, true
}

// listScreensDXGI lists the outputs of every GPU. Outputs of the default GPU
// are captured with ddagrab (Index = output_idx, as before); outputs of other
// GPUs get the next free Index and are captured with gdigrab.
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

	next := 0
	for ai := 0; ai < 8; ai++ {
		descs, idx, ok := dxgiOutputs(factory, ai)
		if !ok {
			if ai == 0 {
				return nil, fmt.Errorf("EnumAdapters failed")
			}
			break
		}
		for k, d := range descs {
			s := Screen{
				X: int(d.Desktop.Left), Y: int(d.Desktop.Top),
				W: int(d.Desktop.Right - d.Desktop.Left), H: int(d.Desktop.Bottom - d.Desktop.Top),
			}
			if hasRect(out, s) {
				continue
			}
			if ai == 0 {
				s.Index, s.Output = idx[k], idx[k]
				// DXGI_MODE_ROTATION: 1 identity, 2 = 90°, 3 = 180°, 4 = 270°
				s.Rotation = map[uint32]int{2: 90, 3: 180, 4: 270}[d.Rotation]
			} else {
				s.GDI = true // gdigrab grabs the desktop already upright
			}
			s.Primary = s.X == 0 && s.Y == 0
			out = append(out, s)
			next = max(next, s.Index+1)
		}
	}
	for i := range out {
		if out[i].GDI {
			out[i].Index, next = next, next+1
		}
	}
	return out, nil
}

func hasRect(ss []Screen, s Screen) bool {
	for _, o := range ss {
		if o.X == s.X && o.Y == s.Y && o.W == s.W && o.H == s.H {
			return true
		}
	}
	return false
}

// Desktop Duplication hands over a rotated monitor's image in the panel's
// native (unrotated) orientation. Work out which filter turns it upright.
// Newer ffmpeg builds may already rotate, so for 90/270 we probe the frame
// size ffmpeg actually produces and only rotate if it's still sideways.
var (
	probeMu    sync.Mutex
	probeCache = map[string]string{}
	sizeRe     = regexp.MustCompile(`Stream #0:0.*?, (\d{2,5})x(\d{2,5})`)
)

func fixFor(ffmpeg string, s Screen) string {
	if s.GDI {
		return ""
	}
	switch s.Rotation {
	case 180:
		return "hflip,vflip"
	case 90, 270:
	default:
		return ""
	}
	// For DXGI_MODE_ROTATION_ROTATE90 the duplicated texture holds the desktop
	// turned 90° counter-clockwise, so it is turned back clockwise (and vice versa).
	// (Confirmed on a real portrait monitor: the other way round comes out upside down.)
	fix := "transpose=clock"
	if s.Rotation == 270 {
		fix = "transpose=cclock"
	}
	key := fmt.Sprintf("%d:%d:%dx%d", s.Index, s.Rotation, s.W, s.H)
	probeMu.Lock()
	defer probeMu.Unlock()
	if v, ok := probeCache[key]; ok {
		return v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-f", "lavfi", "-i",
		fmt.Sprintf("ddagrab=output_idx=%d:framerate=5", s.Output), "-frames:v", "1", "-f", "null", "-")
	hideWindow(cmd)
	out, _ := cmd.CombinedOutput()
	if m := sizeRe.FindSubmatch(out); m != nil {
		pw, _ := strconv.Atoi(string(m[1]))
		ph, _ := strconv.Atoi(string(m[2]))
		if (pw > ph) == (s.W > s.H) {
			fix = "" // ffmpeg already delivers it upright
		}
		log.Printf("Screen at (%d,%d) is rotated %d°: ffmpeg gives %dx%d, desktop is %dx%d -> %s",
			s.X, s.Y, s.Rotation, pw, ph, s.W, s.H, map[bool]string{true: "rotating it upright", false: "already upright"}[fix != ""])
	} else {
		log.Printf("Screen at (%d,%d) is rotated %d°: couldn't probe ffmpeg, rotating it upright", s.X, s.Y, s.Rotation)
	}
	probeCache[key] = fix
	return fix
}

func listScreens(ffmpeg string) []Screen {
	ss, err := listScreensDXGI()
	if err != nil {
		log.Printf("listing screens via DXGI failed (%v), using the Windows monitor list", err)
	}
	if len(ss) == 0 {
		for i, r := range monitors() {
			ss = append(ss, Screen{Index: i, Output: i, X: int(r.Left), Y: int(r.Top), W: int(r.Right - r.Left), H: int(r.Bottom - r.Top), Primary: r.Left == 0 && r.Top == 0})
		}
	} else {
		// anything Windows shows that DXGI didn't (odd drivers, DisplayLink…)
		next := 0
		for _, s := range ss {
			next = max(next, s.Index+1)
		}
		for _, r := range monitors() {
			s := Screen{X: int(r.Left), Y: int(r.Top), W: int(r.Right - r.Left), H: int(r.Bottom - r.Top), GDI: true}
			if s.W > 0 && s.H > 0 && !hasRect(ss, s) {
				s.Index, s.Primary = next, s.X == 0 && s.Y == 0
				next++
				ss = append(ss, s)
			}
		}
	}
	for i := range ss {
		ss[i].Name = fmt.Sprintf("Screen %d", i+1)
		ss[i].Fix = fixFor(ffmpeg, ss[i])
	}
	return ss
}
