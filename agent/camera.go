package main

// Camera mode: instead of a desktop, stream a Raspberry Pi camera.
// rpicam-vid captures raw frames and pipes them into ffmpeg, which encodes
// low-latency H.264 (the Pi 5 has no hardware H.264 encoder). Enabled by a
// "camera" section in the config file; the machine shows up as its own card.

import (
	"fmt"
	"os/exec"
)

type CameraConfig struct {
	Width    int    `json:"width"`    // default 1280 (rounded to a multiple of 64)
	Height   int    `json:"height"`   // default 720
	FPS      int    `json:"fps"`      // default 30
	Rotation int    `json:"rotation"` // 0 or 180
	HFlip    bool   `json:"hflip"`
	VFlip    bool   `json:"vflip"`
	Command  string `json:"command"` // capture program, default "rpicam-vid"
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
}

func cameraScreens(cfg Config) []Screen {
	c := cfg.Camera
	return []Screen{{Index: 0, Name: "Camera", W: c.Width, H: c.Height, Primary: true}}
}

// cameraCommands builds the capture → encode pipeline.
func cameraCommands(cfg Config, es encodeSettings) (*exec.Cmd, *exec.Cmd) {
	c := cfg.Camera
	fps := min(c.FPS, es.FPS)
	camArgs := []string{
		"-t", "0", "-n", "--flush",
		"--codec", "yuv420",
		"--width", fmt.Sprint(c.Width), "--height", fmt.Sprint(c.Height),
		"--framerate", fmt.Sprint(fps),
		"-o", "-",
	}
	if c.Rotation == 180 {
		camArgs = append(camArgs, "--rotation", "180")
	}
	if c.HFlip {
		camArgs = append(camArgs, "--hflip")
	}
	if c.VFlip {
		camArgs = append(camArgs, "--vflip")
	}
	cam := exec.Command(c.Command, camArgs...)

	fitScale := fmt.Sprintf("scale=w='min(%d,iw)':h='min(%d,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2", es.MaxW, es.MaxH)
	ffArgs := []string{"-hide_banner", "-loglevel", "error", "-fflags", "nobuffer", "-flags", "low_delay",
		"-f", "rawvideo", "-pix_fmt", "yuv420p", "-video_size", fmt.Sprintf("%dx%d", c.Width, c.Height),
		"-framerate", fmt.Sprint(fps), "-i", "pipe:0",
		"-vf", fitScale + ",format=yuv420p"}
	ffArgs = append(ffArgs, encoderArgs(cfg, es, fmt.Sprint(es.GOP))...)
	return cam, exec.Command(cfg.FFmpeg, ffArgs...)
}

// noInput is the input handler for a camera: there's nothing to control.
type noInput struct{}

func (noInput) Handle(InputEvent)  {}
func (noInput) ReleaseAll()        {}
func (noInput) SetTarget(t Screen) {}
