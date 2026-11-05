package main

import (
	"fmt"
	"strconv"
	"strings"
)

// Quality levels. "auto" picks one from the connection: High on a direct
// (local network) connection, Balanced when the video goes through the TURN
// relay (mobile data, other networks). The browser can step auto down to Low
// if frames still aren't getting through.
var qualityLevels = map[string]struct {
	fps      int
	kbps     int
	maxW     int
	maxH     int
	gopSecs  float64
	describe string
}{
	"high":     {0, 0, 3840, 2160, 2, "as configured, up to 4K"},
	"balanced": {30, 4000, 1920, 1080, 1, "1080p, 30 fps, 4 Mbps"},
	"low":      {24, 1500, 1280, 720, 1, "720p, 24 fps, 1.5 Mbps"},
}

func validLevel(l string) bool { _, ok := qualityLevels[l]; return ok }

// parseKbps turns "8M", "1500k" or "6000000" into kilobits per second.
func parseKbps(s string) int {
	s = strings.TrimSpace(strings.ToLower(s))
	mult := 1.0 / 1000
	switch {
	case strings.HasSuffix(s, "m"):
		mult, s = 1000, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "k"):
		mult, s = 1, strings.TrimSuffix(s, "k")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 8000
	}
	return int(v * mult)
}

// Encode settings for a level, never above what the config allows.
type encodeSettings struct {
	FPS, Kbps, MaxW, MaxH, GOP int
}

func settingsFor(cfg Config, level string) encodeSettings {
	q, ok := qualityLevels[level]
	if !ok {
		q = qualityLevels["high"]
	}
	e := encodeSettings{FPS: cfg.FPS, Kbps: parseKbps(cfg.Bitrate), MaxW: q.maxW, MaxH: q.maxH}
	if q.fps > 0 && q.fps < e.FPS {
		e.FPS = q.fps
	}
	if q.kbps > 0 && q.kbps < e.Kbps {
		e.Kbps = q.kbps
	}
	e.GOP = int(float64(e.FPS) * q.gopSecs)
	if e.GOP < 1 {
		e.GOP = 1
	}
	return e
}

func (e encodeSettings) bitrate() string { return fmt.Sprintf("%dk", e.Kbps) }
