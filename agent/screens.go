package main

// Screen is one monitor, in virtual-desktop pixel coordinates.
// Index is the capture index (DXGI output index on Windows), which is what
// ffmpeg's ddagrab output_idx expects.
type Screen struct {
	Index   int    `json:"i"`
	Name    string `json:"name"`
	X       int    `json:"x"`
	Y       int    `json:"y"`
	W       int    `json:"w"`
	H       int    `json:"h"`
	Primary bool   `json:"primary"`
	// Rotation in degrees (0, 90, 180, 270) as set in Windows display settings.
	Rotation int `json:"rot"`
	// Fix is the ffmpeg filter that turns this screen's captured frames
	// upright ("" when they already are).
	Fix string `json:"-"`
}

// AllScreens selects every monitor stitched into one picture.
const AllScreens = -1

// bounds returns the rectangle covering all screens (the "All screens" view).
func bounds(ss []Screen) Screen {
	if len(ss) == 0 {
		return Screen{W: 1920, H: 1080}
	}
	minX, minY := ss[0].X, ss[0].Y
	maxX, maxY := ss[0].X+ss[0].W, ss[0].Y+ss[0].H
	for _, s := range ss[1:] {
		minX, minY = min(minX, s.X), min(minY, s.Y)
		maxX, maxY = max(maxX, s.X+s.W), max(maxY, s.Y+s.H)
	}
	return Screen{Index: AllScreens, Name: "All screens", X: minX, Y: minY, W: maxX - minX, H: maxY - minY}
}

// target returns the area the picture covers for a selection, used to map
// mouse input back onto the real desktop.
func target(ss []Screen, sel int) Screen {
	if sel == AllScreens && len(ss) > 1 {
		return bounds(ss)
	}
	for _, s := range ss {
		if s.Index == sel {
			return s
		}
	}
	if len(ss) > 0 {
		return ss[0]
	}
	return Screen{W: 1920, H: 1080}
}

// validSel clamps a selection to something that exists.
func validSel(ss []Screen, sel int) int {
	if sel == AllScreens {
		if len(ss) > 1 {
			return AllScreens
		}
		sel = 0
	}
	for _, s := range ss {
		if s.Index == sel {
			return sel
		}
	}
	if len(ss) > 0 {
		return ss[0].Index
	}
	return 0
}

// getScreens lists monitors; -test mode fakes two side-by-side screens.
func getScreens(cfg Config) []Screen {
	if cfg.Test {
		return []Screen{
			{Index: 0, Name: "Test screen 1", X: 0, Y: 0, W: 1280, H: 720, Primary: true},
			// portrait monitor, like a rotated real one: frames arrive sideways and need Fix
			{Index: 1, Name: "Test screen 2", X: 1280, Y: -200, W: 720, H: 1280, Rotation: 90, Fix: "transpose=cclock"},
		}
	}
	ss := listScreens(cfg.FFmpeg)
	if len(ss) == 0 {
		ss = []Screen{{Index: 0, Name: "Screen", W: 1920, H: 1080, Primary: true}}
	}
	return ss
}
