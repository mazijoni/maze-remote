package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
)

// Capture runs ffmpeg, which grabs the screen and encodes H.264 (Annex B) to stdout.
// We split the stream into access units (frames) and feed them to the WebRTC track.
type Capture struct {
	cmd  *exec.Cmd
	stop sync.Once
	done chan struct{}
}

// Largest picture we send. H.264 hardware encoders top out around 4096 wide,
// and browsers decode 4K comfortably, so "All screens" is scaled to fit this.
const maxW, maxH = 3840, 2160

func ffmpegArgs(cfg Config, ss []Screen, sel int) []string {
	fps := fmt.Sprint(cfg.FPS)
	gop := fmt.Sprint(cfg.FPS * 2)
	args := []string{"-hide_banner", "-loglevel", "error", "-fflags", "nobuffer", "-flags", "low_delay"}
	fitScale := fmt.Sprintf("scale=w='min(%d,iw)':h='min(%d,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2", maxW, maxH)

	// ---- all screens: one input per monitor, stitched by their desktop positions ----
	if sel == AllScreens && len(ss) > 1 {
		b := bounds(ss)
		var graph, stack, layout string
		for i, sc := range ss {
			switch {
			case cfg.Test:
				src := []string{"testsrc2", "smptebars", "rgbtestsrc", "yuvtestsrc"}[i%4]
				w, h := nativeSize(sc)
				args = append(args, "-re", "-f", "lavfi", "-i", fmt.Sprintf("%s=size=%dx%d:rate=%s", src, w, h, fps))
				graph += fmt.Sprintf("[%d:v]%sformat=yuv420p[s%d];", i, withComma(sc.Fix), i)
			case runtime.GOOS == "windows":
				args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("ddagrab=output_idx=%d:framerate=%s:draw_mouse=1", sc.Index, fps))
				graph += fmt.Sprintf("[%d:v]hwdownload,format=bgra,%sformat=yuv420p[s%d];", i, withComma(sc.Fix), i)
			}
			stack += fmt.Sprintf("[s%d]", i)
			if i > 0 {
				layout += "|"
			}
			layout += fmt.Sprintf("%d_%d", sc.X-b.X, sc.Y-b.Y)
		}
		graph += fmt.Sprintf("%sxstack=inputs=%d:layout=%s:fill=black,%s,format=yuv420p[out]", stack, len(ss), layout, fitScale)
		args = append(args, "-filter_complex", graph, "-map", "[out]")
		return append(args, encoderArgs(cfg, gop)...)
	}

	// ---- one screen ----
	sc := target(ss, sel)
	hw := false // frames stay on the GPU (d3d11) and go straight to a hardware encoder
	switch {
	case cfg.Test:
		src := []string{"testsrc2", "smptebars"}[sc.Index%2]
		w, h := nativeSize(sc)
		args = append(args, "-re", "-f", "lavfi", "-i", fmt.Sprintf("%s=size=%dx%d:rate=%s", src, w, h, fps))
	case runtime.GOOS == "windows":
		// Desktop Duplication API — fast, low-overhead, needs ffmpeg 6.0+
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("ddagrab=output_idx=%d:framerate=%s:draw_mouse=1", sc.Index, fps))
		hw = (cfg.Encoder == "nvenc" || cfg.Encoder == "amf") && sc.W <= maxW && sc.H <= maxH && sc.Fix == ""
	case runtime.GOOS == "darwin":
		args = append(args, "-f", "avfoundation", "-framerate", fps, "-capture_cursor", "1", "-i", fmt.Sprintf("%d:none", sc.Index+1))
	default: // linux / X11
		args = append(args, "-f", "x11grab", "-framerate", fps, "-draw_mouse", "1", "-i", displayName())
	}

	// ---- filters ----
	if runtime.GOOS == "windows" && !cfg.Test && !hw {
		args = append(args, "-vf", "hwdownload,format=bgra,"+withComma(sc.Fix)+fitScale+",format=yuv420p")
	} else if !hw {
		args = append(args, "-vf", withComma(sc.Fix)+fitScale+",format=yuv420p")
	}
	return append(args, encoderArgs(cfg, gop)...)
}

// nativeSize is the frame size before rotation (what a sideways capture gives).
func nativeSize(sc Screen) (int, int) {
	if sc.Rotation == 90 || sc.Rotation == 270 {
		return sc.H, sc.W
	}
	return sc.W, sc.H
}

func withComma(f string) string {
	if f == "" {
		return ""
	}
	return f + ","
}

func encoderArgs(cfg Config, gop string) []string {
	var args []string
	// ---- encoder (no B-frames, short GOP, constant-ish bitrate — tuned for latency) ----
	br := cfg.Bitrate
	switch cfg.Encoder {
	case "nvenc":
		args = append(args, "-c:v", "h264_nvenc", "-preset", "p1", "-tune", "ull", "-zerolatency", "1",
			"-rc", "cbr", "-b:v", br, "-maxrate", br, "-bufsize", br, "-profile:v", "baseline", "-delay", "0")
	case "amf":
		args = append(args, "-c:v", "h264_amf", "-usage", "ultralowlatency", "-quality", "speed",
			"-rc", "cbr", "-b:v", br, "-profile:v", "constrained_baseline")
	case "qsv":
		args = append(args, "-c:v", "h264_qsv", "-preset", "veryfast", "-low_power", "1",
			"-b:v", br, "-maxrate", br, "-profile:v", "baseline")
	default:
		args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
			"-profile:v", "baseline", "-b:v", br, "-maxrate", br, "-bufsize", br)
	}
	return append(args, "-g", gop, "-bf", "0", "-an", "-f", "h264", "-")
}

func StartCapture(cfg Config, ss []Screen, sel int, track *webrtc.TrackLocalStaticSample) (*Capture, error) {
	args := ffmpegArgs(cfg, ss, sel)
	cmd := exec.Command(cfg.FFmpeg, args...)
	hideWindow(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not start ffmpeg (%s): %w — is ffmpeg installed and in PATH?", cfg.FFmpeg, err)
	}
	c := &Capture{cmd: cmd, done: make(chan struct{})}

	go func() {
		defer close(c.done)
		err := pump(bufio.NewReaderSize(stdout, 1<<20), track)
		_ = cmd.Wait()
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			log.Printf("ffmpeg: %s", msg)
		}
		if err != nil && err != io.EOF {
			log.Printf("capture ended: %v", err)
		}
	}()
	return c, nil
}

// pump groups NAL units into frames: a new frame starts at a VCL slice whose
// first_mb_in_slice == 0 (first bit of the slice header is 1), or at an AUD.
func pump(r io.Reader, track *webrtc.TrackLocalStaticSample) error {
	reader, err := h264reader.NewReader(r)
	if err != nil {
		return err
	}
	var frame []byte
	hasVCL := false
	last := time.Now()
	startCode := []byte{0, 0, 0, 1}

	flush := func() error {
		if len(frame) == 0 {
			return nil
		}
		now := time.Now()
		dur := now.Sub(last)
		last = now
		if dur <= 0 || dur > time.Second {
			dur = time.Millisecond * 16
		}
		err := track.WriteSample(media.Sample{Data: frame, Duration: dur})
		frame = nil
		hasVCL = false
		return err
	}

	for {
		nal, err := reader.NextNAL()
		if err != nil {
			_ = flush()
			return err
		}
		t := nal.UnitType
		isVCL := t == h264reader.NalUnitTypeCodedSliceNonIdr || t == h264reader.NalUnitTypeCodedSliceIdr
		firstSlice := isVCL && len(nal.Data) > 1 && nal.Data[1]&0x80 != 0
		newAU := t == h264reader.NalUnitTypeAUD ||
			(hasVCL && (firstSlice || t == h264reader.NalUnitTypeSPS || t == h264reader.NalUnitTypePPS || t == h264reader.NalUnitTypeSEI))
		if newAU {
			if err := flush(); err != nil {
				return err
			}
		}
		if t == h264reader.NalUnitTypeAUD {
			continue
		}
		frame = append(frame, startCode...)
		frame = append(frame, nal.Data...)
		if isVCL {
			hasVCL = true
		}
	}
}

func (c *Capture) Stop() {
	c.stop.Do(func() {
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		select {
		case <-c.done:
		case <-time.After(3 * time.Second):
		}
	})
}
