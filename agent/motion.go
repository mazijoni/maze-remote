package main

// Motion recording (camera mode): watch the camera for movement or a big
// change in the picture (a light switched on), and save a clip — starting a
// couple of seconds before it happened — into the clips folder:
//
//	<clips>/2026-11-11/142630.mp4   the video (H.264, plays in any browser)
//	<clips>/2026-11-11/142630.jpg   thumbnail at the moment of most motion
//	<clips>/2026-11-11/142630.json  start, end, how much moved, ...
//
// The server on the same Pi lists these for the website's timeline. The
// website's settings land in <clips>/settings.json, and this writes how it's
// doing to <clips>/status.json.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	preRoll     = 2 * time.Second // saved from before the motion started
	postRoll    = 5 * time.Second // keep recording this long after it stops
	maxClip     = 5 * time.Minute // longer events are split into several clips
	warmUp      = 3 * time.Second // ignore the camera settling after it starts
	analyzeRate = 5               // motion checks per second
	thumbWidth  = 480
)

// RecordSettings are changed from the website (settings.json).
type RecordSettings struct {
	Enabled     bool    `json:"enabled"`
	Sensitivity string  `json:"sensitivity"` // low | medium | high
	KeepDays    int     `json:"keepDays"`    // delete clips older than this
	MaxGB       float64 `json:"maxGB"`       // 0 = no limit (besides keeping 10% of the disk free)
}

func defaultRecordSettings() RecordSettings {
	return RecordSettings{Enabled: true, Sensitivity: "medium", KeepDays: 14}
}

// thresholds: how much a patch of the picture must change in brightness, and
// how much of the picture must change, to count as motion
func (s RecordSettings) thresholds() (level float64, area float64) {
	switch s.Sensitivity {
	case "high":
		return 10, 0.002
	case "low":
		return 24, 0.02
	}
	return 16, 0.006
}

type clipMeta struct {
	ID       string    `json:"id"`
	Start    int64     `json:"start"` // unix ms
	End      int64     `json:"end"`
	Duration float64   `json:"duration"` // seconds
	Peak     float64   `json:"peak"`     // most of the picture that moved at once (0..1)
	Width    int       `json:"width"`
	Height   int       `json:"height"`
	Size     int64     `json:"size"`
	Activity []float64 `json:"activity"` // motion per second
}

type recorder struct {
	dir      string
	fps      int
	settings RecordSettings
	setMod   time.Time

	det                 detector
	ring                []*Frame // pre-roll
	nextKeep, nextCheck time.Time
	hits                int     // motion checks in a row that saw motion
	score               float64 // latest motion score
	clip                *clipWriter
	lastMotion          time.Time
	lastClean           time.Time
}

func startRecorder(c *CameraConfig) {
	if strings.EqualFold(c.Clips, "off") {
		log.Printf("motion recording: off (\"clips\": \"off\" in the config)")
		return
	}
	r := &recorder{dir: c.Clips, fps: c.RecordFPS, settings: defaultRecordSettings()}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		log.Printf("motion recording: can't use %s: %v", r.dir, err)
		return
	}
	go r.run()
}

func (r *recorder) run() {
	r.loadSettings()
	log.Printf("motion recording: clips go to %s (%s)", r.dir, r.describe())
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	var sub *frameSub
	var frames <-chan *Frame
	for {
		if r.settings.Enabled && sub == nil {
			sub = camera.Subscribe(64)
			frames = sub.ch
			r.det.reset()
		} else if !r.settings.Enabled && sub != nil {
			r.finishClip()
			camera.Unsubscribe(sub)
			sub, frames, r.ring = nil, nil, nil
		}
		select {
		case f, ok := <-frames:
			if !ok {
				sub, frames = nil, nil
				continue
			}
			r.frame(f)
		case <-tick.C:
			if r.loadSettings() {
				log.Printf("motion recording: settings changed (%s)", r.describe())
				go cleanupClips(r.dir, r.settings)
			}
			if r.clip != nil && time.Since(r.lastMotion) > postRoll+2*time.Second {
				r.finishClip() // the camera stopped giving pictures mid-clip
			}
			r.writeStatus()
			if time.Since(r.lastClean) > 10*time.Minute {
				r.lastClean = time.Now()
				go cleanupClips(r.dir, r.settings)
			}
		}
	}
}

func (r *recorder) describe() string {
	if !r.settings.Enabled {
		return "turned off on the website"
	}
	return fmt.Sprintf("%s sensitivity, keep %d days", r.settings.Sensitivity, r.settings.KeepDays)
}

func (r *recorder) frame(f *Frame) {
	// a frame for the video? (clips are saved at a lower frame rate)
	every := time.Second / time.Duration(r.fps)
	if r.nextKeep.Sub(f.T) > every {
		r.nextKeep = f.T // the clock jumped
	}
	if !f.T.Before(r.nextKeep.Add(-every / 4)) {
		r.nextKeep = r.nextKeep.Add(every)
		if r.nextKeep.Before(f.T) {
			r.nextKeep = f.T.Add(every)
		}
		if r.clip != nil {
			r.clip.add(f)
		} else {
			r.ring = append(r.ring, f)
			for len(r.ring) > 0 && f.T.Sub(r.ring[0].T) > preRoll {
				r.ring[0] = nil
				r.ring = r.ring[1:]
			}
		}
	}

	// is something moving?
	if f.T.Before(r.nextCheck) {
		return
	}
	r.nextCheck = f.T.Add(time.Second / analyzeRate)
	level, area := r.settings.thresholds()
	score, ready := r.det.check(f, level)
	r.score = score
	if r.clip != nil && (f.W != r.clip.w || f.H != r.clip.h) {
		r.finishClip() // the resolution changed
	}
	if !ready {
		return
	}
	moving := score >= area
	if moving {
		r.hits++
	} else {
		r.hits = 0
	}
	if r.clip != nil {
		r.clip.note(f, score)
	}
	// two checks in a row (0.4 s) so a single noisy frame doesn't count
	if r.hits >= 2 {
		r.lastMotion = f.T
		if r.clip == nil {
			r.startClip(f, score)
		}
	}
	if r.clip != nil {
		switch {
		case f.T.Sub(r.lastMotion) > postRoll:
			r.finishClip()
		case f.T.Sub(r.clip.start) > maxClip:
			r.finishClip()
			if r.hits >= 2 {
				r.startClip(f, score)
			}
		}
	}
}

func (r *recorder) startClip(f *Frame, score float64) {
	if free, total := diskSpace(r.dir); total > 0 && free < 500<<20 {
		cleanupClips(r.dir, r.settings)
		if free, _ = diskSpace(r.dir); free < 500<<20 {
			log.Printf("motion recording: disk almost full — not recording")
			return
		}
	}
	frames := r.ring
	r.ring = nil
	if len(frames) == 0 || frames[len(frames)-1].W != f.W {
		frames = []*Frame{f}
	}
	c, err := newClip(r.dir, r.fps, frames)
	if err != nil {
		log.Printf("motion recording: %v", err)
		return
	}
	r.clip = c
	c.note(f, score)
	log.Printf("● motion — recording %s", c.id)
}

func (r *recorder) finishClip() {
	if r.clip == nil {
		return
	}
	c := r.clip
	r.clip = nil
	r.hits = 0
	go func() {
		if m, err := c.finish(); err != nil {
			log.Printf("motion recording: couldn't save %s: %v", c.id, err)
		} else {
			log.Printf("■ saved %s (%.0f s, %s)", c.id, m.Duration, byteSize(uint64(m.Size)))
			cleanupClips(r.dir, r.settings)
		}
	}()
}

// loadSettings re-reads settings.json when it changed; true if it did.
func (r *recorder) loadSettings() bool {
	p := filepath.Join(r.dir, "settings.json")
	st, err := os.Stat(p)
	if err != nil || st.ModTime().Equal(r.setMod) {
		return false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	s := defaultRecordSettings()
	if json.Unmarshal(b, &s) != nil {
		return false
	}
	if s.KeepDays <= 0 {
		s.KeepDays = 14
	}
	r.setMod = st.ModTime()
	changed := s != r.settings
	r.settings = s
	return changed
}

// writeStatus tells the website whether it's watching or recording right now.
func (r *recorder) writeStatus() {
	st := map[string]any{"updated": time.Now().UnixMilli(), "enabled": r.settings.Enabled,
		"recording": r.clip != nil, "motion": math.Round(r.score*1000) / 1000}
	if r.clip != nil {
		st["since"] = r.clip.start.UnixMilli()
	}
	b, _ := json.Marshal(st)
	writeAtomic(filepath.Join(r.dir, "status.json"), b)
}

func writeAtomic(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ---------- motion detection ----------

// detector compares a coarse grid of brightness patches with a slowly
// updating background, so gradual changes (daylight, the camera adjusting
// exposure) blend in and only real changes count.
type detector struct {
	w, h, gw, gh, cell int
	bg                 []float64
	cur                []float64
	diffs              []float64
	started, last      time.Time
}

func (d *detector) reset() { d.w = 0 }

// check returns how much of the picture changed (0..1). ready is false while
// the camera is still settling.
func (d *detector) check(f *Frame, level float64) (score float64, ready bool) {
	if f.W != d.w || f.H != d.h || f.T.Sub(d.last) > 3*time.Second {
		// new camera, new size, or a gap: start over
		d.w, d.h = f.W, f.H
		d.cell = max(8, f.W/80)
		d.gw, d.gh = f.W/d.cell, f.H/d.cell
		n := d.gw * d.gh
		d.bg, d.cur, d.diffs = make([]float64, n), make([]float64, n), make([]float64, n)
		d.started = f.T
		d.sample(f, d.bg)
	}
	d.last = f.T
	d.sample(f, d.cur)
	if f.T.Sub(d.started) < warmUp {
		copy(d.bg, d.cur)
		return 0, false
	}
	// the whole picture getting brighter/darker together (exposure, clouds)
	// shifts every patch: measure that and leave it out
	for i := range d.cur {
		d.diffs[i] = d.cur[i] - d.bg[i]
	}
	sorted := append([]float64(nil), d.diffs...)
	sort.Float64s(sorted)
	shift := sorted[len(sorted)/2]
	changed := 0
	for _, v := range d.diffs {
		if math.Abs(v-shift) > level {
			changed++
		}
	}
	score = float64(changed) / float64(len(d.diffs))
	if math.Abs(shift) > 40 {
		// a big change of the whole scene (a light switched on/off)
		score = 1
		copy(d.bg, d.cur)
		return score, true
	}
	// follow the scene slowly: something that arrives and stays (a parked
	// car) stops counting after a few seconds
	for i := range d.bg {
		d.bg[i] += 0.08 * (d.cur[i] - d.bg[i])
	}
	return score, true
}

// sample averages the brightness (Y plane) of each grid cell.
func (d *detector) sample(f *Frame, out []float64) {
	c := d.cell
	for gy := 0; gy < d.gh; gy++ {
		for gx := 0; gx < d.gw; gx++ {
			sum, n := 0, 0
			for y := gy * c; y < (gy+1)*c; y += 2 {
				row := f.Data[y*f.W:]
				for x := gx * c; x < (gx+1)*c; x += 2 {
					sum += int(row[x])
					n++
				}
			}
			out[gy*d.gw+gx] = float64(sum) / float64(n)
		}
	}
}

// ---------- writing a clip ----------

type clipWriter struct {
	id, day, base string // base: path without extension
	w, h          int
	start, end    time.Time
	cmd           *exec.Cmd
	ch            chan *Frame
	written       chan error
	peak          float64
	thumb         *Frame
	activity      []float64
	stderr        bytes.Buffer
}

func newClip(dir string, fps int, preroll []*Frame) (*clipWriter, error) {
	first := preroll[0]
	day := first.T.Format("2006-01-02")
	name := first.T.Format("150405")
	if err := os.MkdirAll(filepath.Join(dir, day), 0o755); err != nil {
		return nil, err
	}
	base := filepath.Join(dir, day, name)
	for i := 2; fileExists(base + ".mp4"); i++ { // two clips in the same second
		base = filepath.Join(dir, day, fmt.Sprintf("%s-%d", name, i))
	}
	c := &clipWriter{id: day + "/" + filepath.Base(base), day: day, base: base, w: first.W, h: first.H,
		start: first.T, end: first.T, ch: make(chan *Frame, 256), written: make(chan error, 1)}
	c.cmd = exec.Command(ffmpegPath(), "-hide_banner", "-loglevel", "error",
		"-f", "rawvideo", "-pix_fmt", "yuv420p", "-video_size", fmt.Sprintf("%dx%d", c.w, c.h),
		"-framerate", fmt.Sprint(fps), "-i", "pipe:0",
		"-c:v", "libx264", "-preset", "superfast", "-crf", "27", "-threads", "2",
		"-pix_fmt", "yuv420p", "-g", fmt.Sprint(fps*2), "-movflags", "+faststart",
		"-f", "mp4", "-y", base+".mp4.part")
	c.cmd.Stderr = &c.stderr
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := c.cmd.Start(); err != nil {
		return nil, fmt.Errorf("could not start ffmpeg: %w", err)
	}
	go func() {
		var werr error
		for f := range c.ch {
			if werr == nil {
				_, werr = stdin.Write(f.Data)
			}
		}
		stdin.Close()
		c.written <- werr
	}()
	for _, f := range preroll {
		c.add(f)
	}
	return c, nil
}

func (c *clipWriter) add(f *Frame) {
	if f.W != c.w || f.H != c.h {
		return
	}
	select {
	case c.ch <- f:
		c.end = f.T
	default: // the encoder can't keep up: skip a frame rather than pile up memory
	}
}

// note records how much moved, for the thumbnail and the activity graph.
func (c *clipWriter) note(f *Frame, score float64) {
	sec := int(f.T.Sub(c.start).Seconds())
	if sec >= 0 && sec < 600 {
		for len(c.activity) <= sec {
			c.activity = append(c.activity, 0)
		}
		c.activity[sec] = math.Max(c.activity[sec], math.Round(score*1000)/1000)
	}
	if c.thumb == nil || score > c.peak {
		c.peak = math.Max(c.peak, score)
		c.thumb = f
	}
}

func (c *clipWriter) finish() (clipMeta, error) {
	close(c.ch)
	werr := <-c.written
	err := c.cmd.Wait()
	if err != nil || werr != nil {
		os.Remove(c.base + ".mp4.part")
		if msg := strings.TrimSpace(c.stderr.String()); msg != "" {
			return clipMeta{}, fmt.Errorf("%s", lastLines(msg, 2))
		}
		if err == nil {
			err = werr
		}
		return clipMeta{}, err
	}
	if err := os.Rename(c.base+".mp4.part", c.base+".mp4"); err != nil {
		return clipMeta{}, err
	}
	if c.thumb != nil {
		if err := writeThumb(c.base+".jpg", c.thumb); err != nil {
			log.Printf("thumbnail: %v", err)
		}
	}
	m := clipMeta{ID: c.id, Start: c.start.UnixMilli(), End: c.end.UnixMilli(),
		Duration: math.Round(c.end.Sub(c.start).Seconds()*10) / 10, Peak: math.Min(1, math.Round(c.peak*1000)/1000),
		Width: c.w, Height: c.h, Activity: c.activity}
	if st, err := os.Stat(c.base + ".mp4"); err == nil {
		m.Size = st.Size()
	}
	b, _ := json.Marshal(m)
	return m, writeAtomic(c.base+".json", b)
}

// writeThumb saves a small JPEG straight from the raw frame.
func writeThumb(p string, f *Frame) error {
	step := max(1, f.W/thumbWidth)
	w, h := f.W/step/2*2, f.H/step/2*2
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	uOff, vOff, cw := f.W*f.H, f.W*f.H+f.W*f.H/4, f.W/2
	for y := 0; y < h; y++ {
		src := f.Data[y*step*f.W:]
		for x := 0; x < w; x++ {
			img.Y[y*img.YStride+x] = src[x*step]
		}
	}
	for y := 0; y < h/2; y++ {
		sy := y * step
		for x := 0; x < w/2; x++ {
			sx := x * step
			img.Cb[y*img.CStride+x] = f.Data[uOff+sy*cw+sx]
			img.Cr[y*img.CStride+x] = f.Data[vOff+sy*cw+sx]
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 78}); err != nil {
		return err
	}
	return writeAtomic(p, buf.Bytes())
}

// ---------- keeping the disk tidy ----------

type clipFile struct {
	day, base string
	size      int64
}

// cleanupClips deletes clips older than the settings allow, then the oldest
// ones while they take more than MaxGB or the disk has under 10% free.
func cleanupClips(dir string, s RecordSettings) {
	days, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -s.KeepDays).Format("2006-01-02")
	var clips []clipFile
	var total int64
	for _, d := range days {
		if !d.IsDir() || len(d.Name()) != 10 {
			continue
		}
		dp := filepath.Join(dir, d.Name())
		if d.Name() < cutoff {
			if err := os.RemoveAll(dp); err == nil {
				log.Printf("motion recording: deleted clips from %s (older than %d days)", d.Name(), s.KeepDays)
			}
			continue
		}
		files, _ := os.ReadDir(dp)
		sizes := map[string]int64{}
		for _, f := range files {
			name := f.Name()
			base := strings.TrimSuffix(name, filepath.Ext(name))
			if strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".tmp") {
				// left over from a clip cut off by a restart
				if info, err := f.Info(); err == nil && time.Since(info.ModTime()) > maxClip+time.Minute {
					os.Remove(filepath.Join(dp, name))
				}
				continue
			}
			if info, err := f.Info(); err == nil {
				sizes[base] += info.Size()
				total += info.Size()
			}
		}
		for base, sz := range sizes {
			clips = append(clips, clipFile{day: d.Name(), base: filepath.Join(dp, base), size: sz})
		}
		if len(files) == 0 && d.Name() < time.Now().Format("2006-01-02") {
			os.Remove(dp)
		}
	}
	sort.Slice(clips, func(i, j int) bool { return clips[i].base < clips[j].base }) // oldest first
	limit := int64(s.MaxGB * (1 << 30))
	removed := 0
	for _, c := range clips {
		free, disk := diskSpace(dir)
		tooBig := limit > 0 && total > limit
		tooFull := disk > 0 && free < disk/10
		if !tooBig && !tooFull {
			break
		}
		for _, ext := range []string{".mp4", ".jpg", ".json"} {
			os.Remove(c.base + ext)
		}
		total -= c.size
		removed++
	}
	if removed > 0 {
		log.Printf("motion recording: deleted the %d oldest clips to make room", removed)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ffmpegPath is set from the config in main.
var ffmpegPath = func() string { return "ffmpeg" }
