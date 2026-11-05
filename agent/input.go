package main

import "log"

// InputEvent is what the browser sends over the "input" data channel.
//
//	{t:"mm", x, y}          mouse move, x/y normalised 0..1 over the streamed monitor
//	{t:"md"|"mu", b, x, y}  mouse button down/up (0 left, 1 middle, 2 right, 3 back, 4 forward)
//	{t:"wh", dx, dy}        wheel, in browser pixel deltas
//	{t:"kd"|"ku", c}        key down/up, KeyboardEvent.code (e.g. "KeyA", "ShiftLeft")
//	{t:"text", s}           type a string (paste)
//	{t:"screen", n}         switch to monitor n (-1 = all screens)
type InputEvent struct {
	T  string  `json:"t"`
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
	B  int     `json:"b"`
	DX float64 `json:"dx"`
	DY float64 `json:"dy"`
	C  string  `json:"c"`
	S  string  `json:"s"`
	N  int     `json:"n"`
}

type InputInjector interface {
	Handle(ev InputEvent)
	ReleaseAll()        // release any keys/buttons still held when a session ends
	SetTarget(t Screen) // which part of the desktop the picture covers
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// logInjector is used in -test mode: it prints input instead of applying it.
type logInjector struct {
	moves int
	t     Screen
}

func (l *logInjector) Handle(ev InputEvent) {
	if ev.T == "mm" {
		l.moves++
		if l.moves%30 != 1 {
			return
		}
	}
	log.Printf("input: %+v -> desktop (%d,%d)", ev, l.t.X+int(clamp01(ev.X)*float64(l.t.W-1)), l.t.Y+int(clamp01(ev.Y)*float64(l.t.H-1)))
}
func (l *logInjector) SetTarget(t Screen) { l.t = t }
func (l *logInjector) ReleaseAll()        {}

func newInjector(cfg Config) InputInjector {
	if cfg.Test {
		return &logInjector{}
	}
	return NewInputInjector()
}
