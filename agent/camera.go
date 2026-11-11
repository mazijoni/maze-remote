package main

// Camera mode: instead of a desktop, stream a Raspberry Pi camera.
// rpicam-vid captures raw frames and pipes them into ffmpeg, which encodes
// low-latency H.264 (the Pi 5 has no hardware H.264 encoder). Enabled by a
// "camera" section in the config file; the machine shows up as its own card.

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type CameraConfig struct {
	Width    int    `json:"width"`    // default 1280 (rounded to a multiple of 64)
	Height   int    `json:"height"`   // default 720
	FPS      int    `json:"fps"`      // default 30
	Rotation int    `json:"rotation"` // 0 or 180
	HFlip    bool   `json:"hflip"`
	VFlip    bool   `json:"vflip"`
	HDR      bool   `json:"hdr"`     // high dynamic range (bright windows + dark room)
	Night    bool   `json:"night"`   // allow long exposures for dark rooms
	Focus    string `json:"focus"`   // "x,y" (0..1 of the picture) to focus on, "" = whole picture
	Command  string `json:"command"` // capture program, default "rpicam-vid"
	// Motion recording: where clips are saved ("off" = don't record), and the
	// frame rate they're saved at.
	Clips     string `json:"clips"`     // default ~/maze-remote-clips
	RecordFPS int    `json:"recordFps"` // default 15
}

func (c *CameraConfig) defaults() {
	if c.Width <= 0 {
		c.Width = 1280
	}
	// raw frames are only tightly packed when the width is a multiple of 64
	c.Width = max(64, c.Width/64*64)
	if c.Height <= 0 {
		c.Height = 720
	}
	c.Height = max(2, c.Height/2*2)
	if c.FPS <= 0 {
		c.FPS = 30
	}
	if c.Command == "" {
		c.Command = "rpicam-vid"
	}
	if c.Clips == "" {
		home, _ := os.UserHomeDir()
		c.Clips = filepath.Join(home, "maze-remote-clips")
	}
	if c.RecordFPS <= 0 {
		c.RecordFPS = 15
	}
	c.RecordFPS = min(c.RecordFPS, c.FPS)
}

func cameraScreens(cfg Config) []Screen {
	c := cfg.Camera
	return []Screen{{Index: 0, Name: "Camera", W: c.Width, H: c.Height, Primary: true}}
}

// cameraEncoder builds the live-view encoder: raw frames from the shared
// camera go in on stdin, low-latency H.264 comes out.
func cameraEncoder(cfg Config, es encodeSettings, w, h int) *exec.Cmd {
	c := cfg.Camera
	vf := fmt.Sprintf("scale=w='min(%d,iw)':h='min(%d,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2", es.MaxW, es.MaxH)
	if es.FPS < c.FPS {
		vf = fmt.Sprintf("fps=%d,", es.FPS) + vf
	}
	ffArgs := []string{"-hide_banner", "-loglevel", "error", "-fflags", "nobuffer", "-flags", "low_delay",
		"-f", "rawvideo", "-pix_fmt", "yuv420p", "-video_size", fmt.Sprintf("%dx%d", w, h),
		"-framerate", fmt.Sprint(c.FPS), "-i", "pipe:0",
		"-vf", vf + ",format=yuv420p"}
	ffArgs = append(ffArgs, encoderArgs(cfg, es, fmt.Sprint(es.GOP))...)
	return exec.Command(cfg.FFmpeg, ffArgs...)
}

// focusWindow turns the tapped point into rpicam's autofocus window
// "x,y,w,h" (fractions of the sensor). The sensor sees the picture before
// flips/rotation, so mirror the point back.
func (c *CameraConfig) focusWindow() string {
	x, y, ok := parseFocus(c.Focus)
	if !ok {
		return ""
	}
	if c.Rotation == 180 {
		x, y = 1-x, 1-y
	}
	if c.HFlip {
		x = 1 - x
	}
	if c.VFlip {
		y = 1 - y
	}
	const size = 0.2
	l := min(max(x-size/2, 0), 1-size)
	t := min(max(y-size/2, 0), 1-size)
	return fmt.Sprintf("%.3f,%.3f,%.3f,%.3f", l, t, size, size)
}

func parseFocus(s string) (x, y float64, ok bool) {
	if _, err := fmt.Sscanf(s, "%f,%f", &x, &y); err != nil || x < 0 || x > 1 || y < 0 || y > 1 {
		return 0, 0, false
	}
	return x, y, true
}

// Camera settings changed from the website are saved next to the config, so
// they survive restarts.
func cameraSettingsPath() string {
	if configPath == "" {
		return "camera-settings.json"
	}
	return filepath.Join(filepath.Dir(configPath), "camera-settings.json")
}

type cameraOpts struct {
	Res   string `json:"res"` // "720p" or "1080p"
	HDR   bool   `json:"hdr"`
	Night bool   `json:"night"`
	Flip  bool   `json:"flip"`  // rotated 180°
	Focus string `json:"focus"` // tapped focus point, "" = whole picture
}

func (c *CameraConfig) opts() cameraOpts {
	res := "720p"
	if c.Height >= 1080 {
		res = "1080p"
	}
	return cameraOpts{Res: res, HDR: c.HDR, Night: c.Night, Flip: c.Rotation == 180, Focus: c.Focus}
}

func (c *CameraConfig) apply(o cameraOpts) {
	if o.Res == "1080p" {
		c.Width, c.Height = 1920, 1080
	} else {
		c.Width, c.Height = 1280, 720
	}
	c.HDR, c.Night, c.Focus = o.HDR, o.Night, o.Focus
	c.Rotation = 0
	if o.Flip {
		c.Rotation = 180
	}
	c.defaults()
}

func loadCameraSettings(c *CameraConfig) {
	b, err := os.ReadFile(cameraSettingsPath())
	if err != nil {
		return
	}
	o := c.opts()
	if json.Unmarshal(b, &o) == nil {
		c.apply(o)
	}
}

func saveCameraSettings(c *CameraConfig) {
	b, _ := json.MarshalIndent(c.opts(), "", "  ")
	if err := os.WriteFile(cameraSettingsPath(), b, 0o644); err != nil {
		log.Printf("couldn't save camera settings: %v", err)
	}
}

// setCamera changes one camera setting from the website and restarts capture.
func (a *Agent) setCamera(s *Session, key, val string) {
	if a.cfg.Camera == nil {
		return
	}
	a.mu.Lock()
	if a.sess != s {
		a.mu.Unlock()
		return
	}
	prev := a.cfg.Camera.opts()
	o := prev
	on := val == "1" || val == "true"
	switch key {
	case "res":
		if val == "720p" || val == "1080p" {
			o.Res = val
		}
	case "hdr":
		o.HDR = on
	case "night":
		o.Night = on
	case "flip":
		o.Flip = on
	case "focus":
		if _, _, ok := parseFocus(val); ok || val == "" {
			o.Focus = val
		}
	default:
		a.mu.Unlock()
		return
	}
	a.cfg.Camera.apply(o)
	saveCameraSettings(a.cfg.Camera)
	a.screens = cameraScreens(a.cfg)
	s.rungs = ladder(a.cfg)
	s.rung = min(s.rung, len(s.rungs)-1)
	dc := s.dc
	newCfg := *a.cfg.Camera
	a.mu.Unlock()
	log.Printf("camera: %s=%s", key, val)
	since := time.Now()
	camera.Restart(newCfg)
	ok := a.restartCapture(s) == nil && cameraWorks(since)
	if !ok {
		// this camera doesn't support that setting: put it back
		log.Printf("camera didn't start with %s=%s — reverting", key, val)
		a.mu.Lock()
		a.cfg.Camera.apply(prev)
		saveCameraSettings(a.cfg.Camera)
		a.screens = cameraScreens(a.cfg)
		s.rungs = ladder(a.cfg)
		s.rung = min(s.rung, len(s.rungs)-1)
		s.stallStage, s.stalled, s.lastFrame = 0, false, time.Now()
		oldCfg := *a.cfg.Camera
		a.mu.Unlock()
		camera.Restart(oldCfg)
		a.restartCapture(s)
		names := map[string]string{"res": "1080p", "hdr": "HDR", "night": "Night mode", "flip": "Flip", "focus": "tap to focus"}
		a.status(dc, "")
		if dc != nil {
			msg := "Your camera doesn't support " + names[key] + " — turned it back off"
			if key == "focus" {
				msg = "This camera can't focus on a spot (only Camera Module 3 has autofocus)"
			}
			b, _ := json.Marshal(map[string]any{"t": "toast", "s": msg, "bad": true})
			_ = dc.SendText(string(b))
		}
	}
	if dc != nil {
		b, _ := json.Marshal(map[string]any{"t": "camera", "opts": a.cfg.Camera.opts()})
		_ = dc.SendText(string(b))
	}
	a.sendQuality(s)
}

// cameraWorks waits up to ~6s for the restarted camera to produce a picture.
func cameraWorks(since time.Time) bool {
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if works, ok := camera.WorksSince(since); ok {
			return works
		}
	}
	return false
}

// noInput is the input handler for a camera: there's nothing to control.
type noInput struct{}

func (noInput) Handle(InputEvent)  {}
func (noInput) ReleaseAll()        {}
func (noInput) SetTarget(t Screen) {}
