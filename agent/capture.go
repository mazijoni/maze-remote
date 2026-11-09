package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"
)

// Capture runs ffmpeg, which grabs the screen and encodes H.264 (Annex B) to stdout.
// We split the stream into access units (frames) and feed them to the WebRTC track.
type Capture struct {
	stopping atomic.Bool  // set when we stop it on purpose
	errMsg   string       // ffmpeg's error output if it exited by itself
	frames   atomic.Int64 // frames sent so far
	started  time.Time
	cmd      *exec.Cmd
	stop     sync.Once
	done     chan struct{}
}

// The picture is scaled to fit es.MaxW x es.MaxH (at most 4K: H.264 hardware
// encoders top out around 4096 wide).
func ffmpegArgs(cfg Config, ss []Screen, sel int, es encodeSettings) []string {
	fps := fmt.Sprint(es.FPS)
	gop := fmt.Sprint(es.GOP)
	maxW, maxH := es.MaxW, es.MaxH
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
				args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("ddagrab=output_idx=%d:framerate=%s:draw_mouse=%d", sc.Index, fps, es.mouse()))
				graph += fmt.Sprintf("[%d:v]hwdownload,format=bgra,%sformat=yuv420p[s%d];", i, withComma(sc.Fix), i)
			}
			stack += fmt.Sprintf("[s%d]", i)
			if i > 0 {
				layout += "|"
			}
			layout += fmt.Sprintf("%d_%d", sc.X-b.X, sc.Y-b.Y)
		}
		graph += fmt.Sprintf("%sxstack=inputs=%d:layout=%s:fill=black,%s,format=yuv420p[out]", stack, len(ss), layout, fitScale)
		if cfg.Test && testBlackout() {
			graph += ";[out]select=0[blk]"
			args = append(args, "-filter_complex", graph, "-map", "[blk]")
			return append(args, encoderArgs(cfg, es, gop)...)
		}
		args = append(args, "-filter_complex", graph, "-map", "[out]")
		return append(args, encoderArgs(cfg, es, gop)...)
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
		args = append(args, "-f", "lavfi", "-i", fmt.Sprintf("ddagrab=output_idx=%d:framerate=%s:draw_mouse=%d", sc.Index, fps, es.mouse()))
		hw = (cfg.Encoder == "nvenc" || cfg.Encoder == "amf") && sc.W <= maxW && sc.H <= maxH && sc.Fix == ""
	case runtime.GOOS == "darwin":
		args = append(args, "-f", "avfoundation", "-framerate", fps, "-capture_cursor", fmt.Sprint(es.mouse()), "-i", fmt.Sprintf("%d:none", sc.Index+1))
	default: // linux / X11
		args = append(args, "-f", "x11grab", "-framerate", fps, "-draw_mouse", fmt.Sprint(es.mouse()), "-i", displayName())
	}

	// ---- filters ----
	if cfg.Test && testBlackout() {
		args = append(args, "-vf", "select=0")
		return append(args, encoderArgs(cfg, es, gop)...)
	}
	if runtime.GOOS == "windows" && !cfg.Test && !hw {
		args = append(args, "-vf", "hwdownload,format=bgra,"+withComma(sc.Fix)+fitScale+",format=yuv420p")
	} else if !hw {
		args = append(args, "-vf", withComma(sc.Fix)+fitScale+",format=yuv420p")
	}
	return append(args, encoderArgs(cfg, es, gop)...)
}

// nativeSize is the frame size before rotation (what a sideways capture gives).
func nativeSize(sc Screen) (int, int) {
	if sc.Rotation == 90 || sc.Rotation == 270 {
		return sc.H, sc.W
	}
	return sc.W, sc.H
}

// testBlackout simulates a screen that gives no picture (like a locked PC) in -test mode.
func testBlackout() bool {
	f := os.Getenv("MR_TEST_BLACKOUT")
	if f == "" {
		return false
	}
	_, err := os.Stat(f)
	return err == nil
}

func withComma(f string) string {
	if f == "" {
		return ""
	}
	return f + ","
}

func encoderArgs(cfg Config, es encodeSettings, gop string) []string {
	var args []string
	// ---- encoder (no B-frames, short GOP, constant-ish bitrate — tuned for latency) ----
	br := es.bitrate()
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

func StartCapture(cfg Config, ss []Screen, sel int, es encodeSettings, track *webrtc.TrackLocalStaticSample) (*Capture, error) {
	args := ffmpegArgs(cfg, ss, sel, es)
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
	c := &Capture{cmd: cmd, done: make(chan struct{}), started: time.Now()}

	go func() {
		defer close(c.done)
		err := pump(bufio.NewReaderSize(stdout, 1<<20), track, &c.frames)
		_ = cmd.Wait()
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			c.errMsg = msg
			if !c.stopping.Load() {
				log.Printf("ffmpeg: %s", msg)
			}
		}
		if err != nil && err != io.EOF {
			log.Printf("capture ended: %v", err)
		}
	}()
	return c, nil
}

// pump groups NAL units into frames: a new frame starts at a VCL slice whose
// first_mb_in_slice == 0 (first bit of the slice header is 1), or at an AUD.
func pump(r io.Reader, track *webrtc.TrackLocalStaticSample, count *atomic.Int64) error {
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
		count.Add(1)
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

// Frames is how many frames have been sent; ErrText is ffmpeg's error output.
func (c *Capture) Frames() int64   { return c.frames.Load() }
func (c *Capture) ErrText() string { return c.errMsg }

// Done is closed when ffmpeg exits, whether stopped or crashed.
func (c *Capture) Done() <-chan struct{} { return c.done }

// Crashed reports an exit we didn't ask for (e.g. the display went to sleep).
func (c *Capture) Crashed() bool { return !c.stopping.Load() }

func (c *Capture) Stop() {
	c.stopping.Store(true)
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
