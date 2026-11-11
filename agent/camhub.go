package main

// The camera can only be opened by one program at a time, so a single
// rpicam-vid runs for everything that needs pictures — the live view and
// motion recording — and its raw frames (YUV 4:2:0) are handed to each
// subscriber. It runs while anyone is subscribed.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"time"
)

// Frame is one raw picture from the camera. Never modified after it's made,
// so subscribers can keep it around (pre-roll, thumbnails).
type Frame struct {
	Data []byte // Y plane, then U, then V (yuv420p)
	W, H int
	T    time.Time
}

type frameSub struct {
	ch chan *Frame
}

type camHub struct {
	mu        sync.Mutex
	cfg       CameraConfig
	subs      map[*frameSub]struct{}
	cmd       *exec.Cmd
	done      chan struct{} // closed when the running camera process has exited
	gen       int           // bumps on every start, so stale restarts do nothing
	lastFrame time.Time
	lastFail  time.Time
	errText   string
}

// camera is the shared camera, set in main when the agent runs in camera mode.
var camera *camHub

func newCamHub(cfg CameraConfig) *camHub {
	return &camHub{cfg: cfg, subs: map[*frameSub]struct{}{}}
}

// Subscribe starts receiving frames. buf is how many may queue up before
// frames are dropped for this subscriber (a slow one never holds others up).
func (h *camHub) Subscribe(buf int) *frameSub {
	s := &frameSub{ch: make(chan *Frame, buf)}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[s] = struct{}{}
	if h.cmd == nil {
		h.start()
	}
	return s
}

// Unsubscribe stops the frames and closes the subscriber's channel. The
// camera turns off when nobody is left.
func (h *camHub) Unsubscribe(s *frameSub) {
	h.mu.Lock()
	if _, ok := h.subs[s]; !ok {
		h.mu.Unlock()
		return
	}
	delete(h.subs, s)
	close(s.ch)
	var cmd *exec.Cmd
	var done chan struct{}
	if len(h.subs) == 0 {
		cmd, done = h.stopLocked()
	}
	h.mu.Unlock()
	wait(cmd, done)
}

// Restart applies new camera settings (resolution, HDR, ...).
func (h *camHub) Restart(cfg CameraConfig) {
	h.mu.Lock()
	cmd, done := h.stopLocked()
	h.cfg = cfg
	h.mu.Unlock()
	wait(cmd, done) // the camera must be fully closed before it can open again
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) > 0 && h.cmd == nil {
		h.start()
	}
}

// Size is the frame size the camera is set to.
func (h *camHub) Size() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.Width, h.cfg.Height
}

// WorksSince reports whether the camera gave a picture after t (true),
// failed after t (false), or hasn't done either yet (ok == false).
func (h *camHub) WorksSince(t time.Time) (works, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastFrame.After(t) {
		return true, true
	}
	if h.lastFail.After(t) {
		return false, true
	}
	return false, false
}

// ErrText is the camera program's last error output.
func (h *camHub) ErrText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.errText
}

// stopLocked turns the camera off (called with h.mu held); wait() for it after unlocking.
func (h *camHub) stopLocked() (*exec.Cmd, chan struct{}) {
	cmd, done := h.cmd, h.done
	h.cmd = nil
	h.gen++
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return cmd, done
}

func wait(cmd *exec.Cmd, done chan struct{}) {
	if cmd == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// start launches the camera. Called with h.mu held.
func (h *camHub) start() {
	c := h.cfg
	args := []string{
		"-t", "0", "-n", "--flush",
		"--codec", "yuv420",
		"--width", fmt.Sprint(c.Width), "--height", fmt.Sprint(c.Height),
		"--framerate", fmt.Sprint(c.FPS),
		"-o", "-",
	}
	if c.Rotation == 180 {
		args = append(args, "--rotation", "180")
	}
	if c.HFlip {
		args = append(args, "--hflip")
	}
	if c.VFlip {
		args = append(args, "--vflip")
	}
	if c.HDR {
		args = append(args, "--hdr", "auto")
	}
	if c.Night {
		args = append(args, "--exposure", "long")
	}
	if win := c.focusWindow(); win != "" {
		// keep focusing continuously, but only on that spot
		args = append(args, "--autofocus-mode", "continuous", "--autofocus-window", win)
	}
	cmd := exec.Command(c.Command, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	h.gen++
	gen := h.gen
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		h.errText = fmt.Sprintf("could not start the camera (%s): %v — is rpicam-apps installed?", c.Command, err)
		h.lastFail = time.Now()
		log.Print(h.errText)
		go h.retry(gen)
		return
	}
	h.cmd = cmd
	h.done = make(chan struct{})
	go h.read(cmd, out, &stderr, c.Width, c.Height, gen, h.done)
}

// read splits the raw stream into frames and hands them out.
func (h *camHub) read(cmd *exec.Cmd, out io.Reader, stderr *bytes.Buffer, w, ht, gen int, done chan struct{}) {
	defer close(done)
	r := bufio.NewReaderSize(out, 1<<20)
	size := w * ht * 3 / 2
	for {
		buf := make([]byte, size)
		if _, err := io.ReadFull(r, buf); err != nil {
			break
		}
		f := &Frame{Data: buf, W: w, H: ht, T: time.Now()}
		h.mu.Lock()
		if h.gen != gen {
			h.mu.Unlock()
			break
		}
		h.lastFrame = f.T
		for s := range h.subs {
			select {
			case s.ch <- f:
			default: // that subscriber is behind: it misses this one
			}
		}
		h.mu.Unlock()
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gen != gen {
		return // stopped on purpose
	}
	// it died by itself: note why, and try again in a moment
	h.cmd = nil
	h.lastFail = time.Now()
	h.errText = lastLines(stderr.String(), 3)
	log.Printf("camera stopped: %s", h.errText)
	go h.retry(gen)
}

func (h *camHub) retry(gen int) {
	time.Sleep(3 * time.Second)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gen == gen && h.cmd == nil && len(h.subs) > 0 {
		h.start()
	}
}
