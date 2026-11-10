package main

// Adaptive quality, the way game-streaming tools do it: the browser reports
// how every packet arrived (transport-wide congestion control feedback), the
// GCC estimator turns that into "how many bits per second fit through right
// now", and Auto moves the stream up or down a ladder of encode settings.
// Down quickly when the link gets worse, up slowly once there's clear headroom.

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pion/interceptor/pkg/cc"
)

// MR_TEST_SLOW_KBPS fakes a slow link for testing Auto.
var testSlowKbps, _ = strconv.Atoi(os.Getenv("MR_TEST_SLOW_KBPS"))

type rung struct {
	Kbps, MaxW, MaxH, FPS int
}

func (r rung) label() string {
	res := fmt.Sprintf("%dp", r.MaxH)
	if r.MaxH >= 2160 {
		res = "Full"
	}
	return fmt.Sprintf("%s · %d fps · %.1f Mbps", res, r.FPS, float64(r.Kbps)/1000)
}

// ladder lists the Auto steps this machine can use, lowest first, never above
// what its config allows (bitrate, fps, camera resolution).
func ladder(cfg Config) []rung {
	maxKbps := parseKbps(cfg.Bitrate)
	maxFPS := cfg.FPS
	capW, capH := 3840, 2160
	if cfg.Camera != nil {
		capW, capH = cfg.Camera.Width, cfg.Camera.Height
		maxFPS = min(maxFPS, cfg.Camera.FPS)
	}
	base := []rung{
		{700, 960, 540, 20},
		{1500, 1280, 720, 24},
		{2500, 1280, 720, 30},
		{4000, 1920, 1080, 30},
		{6000, 1920, 1080, 60},
		{maxKbps, 3840, 2160, 60},
	}
	var out []rung
	for _, r := range base {
		r.Kbps = min(r.Kbps, maxKbps)
		r.FPS = min(r.FPS, maxFPS)
		r.MaxW, r.MaxH = min(r.MaxW, capW), min(r.MaxH, capH)
		if n := len(out); n > 0 && out[n-1] == r {
			continue // identical after capping
		}
		if n := len(out); n > 0 && r.Kbps <= out[n-1].Kbps && r.MaxH <= out[n-1].MaxH && r.FPS <= out[n-1].FPS {
			continue // not actually better than the previous step
		}
		out = append(out, r)
	}
	return out
}

func rungSettings(r rung) encodeSettings {
	return encodeSettings{FPS: r.FPS, Kbps: r.Kbps, MaxW: r.MaxW, MaxH: r.MaxH, GOP: r.FPS}
}

// startRung picks where Auto begins: the top on a direct (home) connection,
// around 4 Mbps through the relay.
func startRung(rs []rung, relayed bool) int {
	if !relayed {
		return len(rs) - 1
	}
	pick := 0
	for i, r := range rs {
		if r.Kbps <= 4000 {
			pick = i
		}
	}
	return pick
}

// runAdaptive steers Auto by congestion signals rather than the raw estimate:
// GCC only "sees" up to ~1.5x what's being sent, and an idle desktop sends
// almost nothing, so the estimate alone would wrongly drag quality down.
//
//	down: real congestion (delay building up, estimate falling, or >5% loss) for ~1.5s
//	up:   5s clean while the stream is actually using its budget (proves the link carries it)
//	idle: no change
func (a *Agent) runAdaptive(s *Session, est cc.BandwidthEstimator) {
	type bweStats interface{ GetStats() map[string]any }
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	down, up, tick := 0, 0, 0
	var lastBytes int64
	var lastCap *Capture
	sent := 0.0                 // smoothed kbps actually sent
	backoff := 15 * time.Second // how long to wait before re-trying a step that just failed
	var probedAt time.Time      // when we last stepped up
	for range t.C {
		a.mu.Lock()
		if a.sess != s {
			a.mu.Unlock()
			return
		}
		auto, r, rs, last, hold, dc, capt := s.quality == "auto", s.rung, s.rungs, s.lastSwitch, s.holdUntil, s.dc, s.cap
		a.mu.Unlock()

		// what the encoder is really sending
		if capt != nil {
			b := capt.Bytes()
			if capt == lastCap && b >= lastBytes {
				kbps := float64(b-lastBytes) * 8 / 1000 / 0.5
				sent = sent*0.6 + kbps*0.4
			} else {
				sent = 0 // new encoder: start measuring fresh
			}
			lastCap, lastBytes = capt, b
		}

		loss, usage, state := 0.0, "", ""
		if bs, ok := est.(bweStats); ok {
			st := bs.GetStats()
			loss, _ = st["averageLoss"].(float64)
			usage, _ = st["usage"].(string)
			state, _ = st["state"].(string)
		}
		usable := int(float64(est.GetTargetBitrate()) * 0.85 / 1000)
		if testSlowKbps > 0 && sent > float64(testSlowKbps) { // test: pretend the link is slower
			usage, loss, usable = "overuse", 0.08, testSlowKbps
		}
		congested := strings.EqualFold(usage, "overuse") || strings.EqualFold(state, "decrease") || loss > 0.05

		if tick++; tick%4 == 0 && dc != nil {
			b, _ := json.Marshal(map[string]any{"t": "bwe", "sent": int(sent), "loss": loss, "congested": congested})
			_ = dc.SendText(string(b))
		}
		if !auto || len(rs) == 0 {
			down, up = 0, 0
			continue
		}

		settling := time.Since(last) < 3*time.Second // the restart's first keyframe is a burst
		if congested && !settling {
			down++
			up = 0
		} else {
			down = 0
		}
		if down >= 3 && r > 0 {
			// jump to the best step that fits the current estimate (at least one down)
			next := 0
			for i := r - 1; i >= 0; i-- {
				if rs[i].Kbps <= max(usable, rs[0].Kbps) {
					next = i
					break
				}
			}
			if !probedAt.IsZero() && time.Since(probedAt) < 20*time.Second {
				backoff = min(backoff*2, 5*time.Minute) // the step up didn't hold: wait longer next time
			} else if time.Since(probedAt) > 2*time.Minute {
				backoff = 15 * time.Second
			}
			probedAt = time.Time{}
			a.setHold(s, backoff)
			a.switchRung(s, next, fmt.Sprintf("congestion: %s/%s, %.0f%% loss, ~%.1f Mbps", usage, state, loss*100, float64(usable)/1000))
			down, up = 0, 0
			continue
		}

		busy := sent >= float64(rs[r].Kbps)*0.6
		if !congested && loss < 0.01 && busy && r < len(rs)-1 && time.Now().After(hold) {
			up++
		} else if !busy {
			up = 0 // idle screen: nothing proves there's room for more
		}
		if up >= 10 && time.Since(last) > 8*time.Second {
			probedAt = time.Now()
			a.switchRung(s, r+1, fmt.Sprintf("clean for 5s at %.1f Mbps", sent/1000))
			down, up = 0, 0
		}
	}
}

func (a *Agent) switchRung(s *Session, to int, why string) {
	a.mu.Lock()
	if a.sess != s || to == s.rung || to < 0 || to >= len(s.rungs) {
		a.mu.Unlock()
		return
	}
	from := s.rung
	s.rung, s.lastSwitch = to, time.Now()
	if to < from {
		if h := time.Now().Add(15 * time.Second); h.After(s.holdUntil) {
			s.holdUntil = h // don't bounce straight back up
		}
	}
	r := s.rungs[to]
	a.mu.Unlock()
	log.Printf("auto quality: %s (%s)", r.label(), why)
	if err := a.restartCapture(s); err != nil {
		log.Printf("could not change quality: %v", err)
	}
	a.sendQuality(s)
}

func (a *Agent) setHold(s *Session, d time.Duration) {
	a.mu.Lock()
	s.holdUntil = time.Now().Add(d)
	a.mu.Unlock()
}

// sendQuality tells the viewer what's streaming now.
func (a *Agent) sendQuality(s *Session) {
	a.mu.Lock()
	msg := map[string]any{"t": "quality", "quality": s.quality, "level": s.level, "relayed": s.relayed, "label": a.qualityLabel(s)}
	dc := s.dc
	a.mu.Unlock()
	if dc != nil {
		b, _ := json.Marshal(msg)
		_ = dc.SendText(string(b))
	}
}

// qualityLabel describes the current encode settings; caller holds a.mu.
func (a *Agent) qualityLabel(s *Session) string {
	if s.quality == "auto" && len(s.rungs) > 0 {
		return s.rungs[s.rung].label()
	}
	if q, ok := qualityLevels[s.level]; ok {
		return q.describe
	}
	return ""
}
