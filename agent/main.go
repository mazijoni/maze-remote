// Maze Remote — host agent
// Runs on the PC you want to control. Connects out to the signaling server,
// and when a browser asks for a session it streams the screen over WebRTC
// and applies the mouse/keyboard input it receives.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

var version = "0.1.0"

type Config struct {
	Server  string `json:"server"`
	Secret  string `json:"secret"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Encoder string `json:"encoder"`
	FPS     int    `json:"fps"`
	Bitrate string `json:"bitrate"`
	Monitor int    `json:"monitor"`
	FFmpeg  string `json:"ffmpeg"`
	Test    bool   `json:"test"`
}

type wsMsg struct {
	Type   string          `json:"type"`
	To     string          `json:"to,omitempty"`
	From   string          `json:"from,omitempty"`
	Sid    string          `json:"sid,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	ICE    []iceServer     `json:"ice,omitempty"`
	ID     string          `json:"id,omitempty"`
	Name   string          `json:"name,omitempty"`
	OS     string          `json:"os,omitempty"`
	Secret string          `json:"secret,omitempty"`
}

type iceServer struct {
	URLs       any    `json:"urls"`
	Username   string `json:"username,omitempty"`
	Credential string `json:"credential,omitempty"`
}

type signalData struct {
	SDP       *webrtc.SessionDescription `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit   `json:"candidate,omitempty"`
	// The viewer draws its own mouse pointer (it has a real mouse), so the
	// agent leaves the pointer out of the video and sends its shape instead.
	LocalCursor bool `json:"localCursor,omitempty"`
}

func loadConfig() Config {
	cfg := Config{Encoder: "x264", FPS: 60, Bitrate: "8M", FFmpeg: "ffmpeg"}
	hn, _ := os.Hostname()
	cfg.ID, cfg.Name = hn, hn

	cfgPath := flag.String("config", "maze-remote.json", "path to config file (optional)")
	server := flag.String("server", "", "signaling server WebSocket URL, e.g. ws://raspberrypi.local:8080/ws")
	secret := flag.String("secret", "", "host secret (MR_HOST_SECRET on the server)")
	id := flag.String("id", "", "stable host id (default: hostname)")
	name := flag.String("name", "", "display name (default: hostname)")
	enc := flag.String("encoder", "", "x264 | nvenc | amf | qsv")
	fps := flag.Int("fps", 0, "capture framerate")
	br := flag.String("bitrate", "", "video bitrate, e.g. 8M")
	mon := flag.Int("monitor", -2, "screen to show first: 0 = first screen, -1 = all screens")
	ff := flag.String("ffmpeg", "", "path to ffmpeg")
	test := flag.Bool("test", false, "stream a test pattern instead of the screen")
	flag.Parse()

	if b, err := os.ReadFile(*cfgPath); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			log.Fatalf("bad config %s: %v", *cfgPath, err)
		}
	}
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&cfg.Server, *server)
	set(&cfg.Secret, *secret)
	set(&cfg.ID, *id)
	set(&cfg.Name, *name)
	set(&cfg.Encoder, *enc)
	set(&cfg.Bitrate, *br)
	set(&cfg.FFmpeg, *ff)
	set(&cfg.Secret, os.Getenv("MR_HOST_SECRET"))
	if *fps > 0 {
		cfg.FPS = *fps
	}
	if *mon >= -1 {
		cfg.Monitor = *mon
	}
	if *test {
		cfg.Test = true
	}

	if cfg.Server == "" || cfg.Secret == "" {
		fmt.Println("Maze Remote agent", version)
		fmt.Println("Missing -server or -secret. Example:")
		fmt.Println(`  maze-remote-agent -server ws://raspberrypi.local:8080/ws -secret YOUR_HOST_SECRET`)
		fmt.Println("Or put them in maze-remote.json next to the agent.")
		os.Exit(2)
	}
	return cfg
}

// ---------- agent ----------

type Agent struct {
	cfg   Config
	conn  *websocket.Conn
	wmu   sync.Mutex
	ice   []webrtc.ICEServer
	mu    sync.Mutex
	sess  *Session
	input InputInjector

	screens    []Screen // monitors, refreshed at the start of each session
	maxScreens int      // most screens seen, so we know when sleeping ones are back
	sel        int      // current screen index, or AllScreens
	capMu      sync.Mutex
}

type Session struct {
	sid          string
	client       string
	pc           *webrtc.PeerConnection
	track        *webrtc.TrackLocalStaticSample
	cap          *Capture
	dc           *webrtc.DataChannel
	quality      string       // what the viewer chose: auto, high, balanced, low
	level        string       // what is actually streaming: high, balanced, low
	relayed      bool         // video goes through the TURN relay
	started      time.Time    // when the current capture started
	lastKF       time.Time    // last keyframe-driven restart
	stalled      bool         // screen is giving no picture; don't restart for keyframes
	localCursor  bool         // viewer draws the pointer itself
	moveSeq      atomic.Int64 // newest mouse position applied (positions can arrive out of order)
	stallStage   int          // 0 fine, 1 trying to wake, 2 told the viewer what's wrong
	vIdx         int          // virtual screen plugged in for this session (-1 none)
	triedVirtual bool         // already tried adding one
	lastFrame    time.Time    // when the screen last produced a picture
	once         sync.Once
	pending      []webrtc.ICECandidateInit
}

func (a *Agent) send(m wsMsg) {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	if a.conn != nil {
		_ = a.conn.WriteJSON(m)
	}
}

func (a *Agent) signal(s *Session, d signalData) {
	b, _ := json.Marshal(d)
	a.send(wsMsg{Type: "signal", To: s.client, Sid: s.sid, Data: b})
}

func (a *Agent) run() {
	backoff := time.Second
	for {
		err := a.connectOnce()
		a.closeSession("server connection lost")
		log.Printf("disconnected from server: %v — retrying in %s", err, backoff)
		time.Sleep(backoff + time.Duration(rand.Intn(500))*time.Millisecond)
		if backoff < 30*time.Second {
			backoff *= 2
		}
		if err == errAuth {
			log.Println("server rejected the host secret — check MR_HOST_SECRET")
			backoff = 30 * time.Second
		}
	}
}

var errAuth = fmt.Errorf("auth rejected")

func (a *Agent) connectOnce() error {
	d := websocket.Dialer{HandshakeTimeout: 10 * time.Second, Proxy: http.ProxyFromEnvironment}
	c, _, err := d.Dial(a.cfg.Server, nil)
	if err != nil {
		return err
	}
	a.wmu.Lock()
	a.conn = c
	a.wmu.Unlock()
	defer func() {
		a.wmu.Lock()
		a.conn = nil
		a.wmu.Unlock()
		c.Close()
	}()

	a.send(wsMsg{Type: "host-hello", ID: a.cfg.ID, Name: a.cfg.Name, OS: runtime.GOOS, Secret: a.cfg.Secret})

	// keepalive
	c.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.SetPongHandler(func(string) error { c.SetReadDeadline(time.Now().Add(60 * time.Second)); return nil })
	c.SetPingHandler(func(s string) error {
		c.SetReadDeadline(time.Now().Add(60 * time.Second))
		a.wmu.Lock()
		defer a.wmu.Unlock()
		return c.WriteControl(websocket.PongMessage, []byte(s), time.Now().Add(5*time.Second))
	})

	for {
		var m wsMsg
		if err := c.ReadJSON(&m); err != nil {
			if ce, ok := err.(*websocket.CloseError); ok && ce.Code == 4003 {
				return errAuth
			}
			return err
		}
		c.SetReadDeadline(time.Now().Add(60 * time.Second))
		switch m.Type {
		case "welcome":
			a.ice = toICE(m.ICE)
			log.Printf("online as %q on %s — waiting for connections", a.cfg.Name, a.cfg.Server)
		case "signal":
			a.onSignal(m)
		case "bye":
			a.mu.Lock()
			s := a.sess
			a.mu.Unlock()
			if s != nil && s.sid == m.Sid {
				a.closeSession("viewer left")
			}
		}
	}
}

func toICE(in []iceServer) []webrtc.ICEServer {
	var out []webrtc.ICEServer
	for _, s := range in {
		var urls []string
		switch u := s.URLs.(type) {
		case string:
			urls = []string{u}
		case []any:
			for _, x := range u {
				if xs, ok := x.(string); ok {
					urls = append(urls, xs)
				}
			}
		}
		out = append(out, webrtc.ICEServer{URLs: urls, Username: s.Username, Credential: s.Credential})
	}
	return out
}

func (a *Agent) onSignal(m wsMsg) {
	var d signalData
	if err := json.Unmarshal(m.Data, &d); err != nil {
		return
	}
	if d.SDP != nil && d.SDP.Type == webrtc.SDPTypeOffer {
		if len(m.ICE) > 0 {
			a.ice = toICE(m.ICE) // fresh relay credentials from the server
		}
		a.closeSession("replaced by new viewer")
		if err := a.startSession(m.From, m.Sid, *d.SDP, d.LocalCursor); err != nil {
			log.Printf("session failed: %v", err)
			a.send(wsMsg{Type: "bye", To: m.From, Sid: m.Sid})
			a.closeSession("setup failed")
		}
		return
	}
	if d.Candidate != nil {
		a.mu.Lock()
		s := a.sess
		a.mu.Unlock()
		if s == nil || s.sid != m.Sid {
			return
		}
		if s.pc.RemoteDescription() == nil {
			s.pending = append(s.pending, *d.Candidate)
			return
		}
		_ = s.pc.AddICECandidate(*d.Candidate)
	}
}

func (a *Agent) startSession(client, sid string, offer webrtc.SessionDescription, localCursor bool) error {
	// Mobile networks often go quiet for a few seconds; don't call the link
	// "disconnected" too early, and keep the path warm with frequent keepalives.
	se := webrtc.SettingEngine{}
	se.SetICETimeouts(10*time.Second, 40*time.Second, time.Second)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: a.ice})
	if err != nil {
		return err
	}
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"},
		"screen", "maze-remote")
	if err != nil {
		pc.Close()
		return err
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		pc.Close()
		return err
	}
	// Sleeping monitors often count as disconnected, so wake them before
	// listing screens, then give them a moment to come back.
	a.screens = a.wakeAndListScreens()
	a.sel = validSel(a.screens, a.sel)
	a.input.SetTarget(target(a.screens, a.sel))
	s := &Session{sid: sid, client: client, pc: pc, track: track, localCursor: localCursor, vIdx: -1}
	a.mu.Lock()
	a.sess = s
	a.mu.Unlock()

	// Read RTCP so interceptors (NACK etc.) work. When the viewer can't decode
	// (lost packets, often a lost keyframe on mobile) it sends a PLI/FIR: answer
	// with a fresh keyframe by restarting the encoder.
	go func() {
		for {
			pkts, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			for _, p := range pkts {
				switch p.(type) {
				case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
					go a.keyframe(s)
				}
			}
		}
	}()

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		ci := c.ToJSON()
		a.signal(s, signalData{Candidate: &ci})
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("data channel %q open", dc.Label())
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			var ev InputEvent
			if json.Unmarshal(msg.Data, &ev) == nil {
				switch ev.T {
				case "ping":
					_ = dc.SendText(string(msg.Data))
				case "screen":
					go a.switchScreen(s, ev.N)
				case "quality":
					go a.setQuality(s, ev.S, ev.C)
				case "mm":
					// numbered positions on the unordered channel: skip any older than the last one
					if n := int64(ev.N); n > 0 {
						if n <= s.moveSeq.Load() {
							return
						}
						s.moveSeq.Store(n)
					}
					a.input.Handle(ev)
				default:
					a.input.Handle(ev)
				}
			}
		})
		dc.OnOpen(func() {
			if dc.Label() != "input" {
				return // the unreliable "move" channel only carries mouse positions
			}
			a.mu.Lock()
			s.dc = dc
			a.mu.Unlock()
			info, _ := json.Marshal(map[string]any{"t": "hello", "name": a.cfg.Name, "os": runtime.GOOS, "version": version,
				"encoder": a.cfg.Encoder, "screens": a.screens, "screen": a.sel,
				"quality": s.quality, "level": s.level, "relayed": s.relayed})
			_ = dc.SendText(string(info))
		})
	})

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		log.Printf("peer connection: %s%s", st, routeInfo(pc))
		switch st {
		case webrtc.PeerConnectionStateConnected:
			s.once.Do(func() {
				// Through the relay (mobile data, other networks) start at Balanced.
				if pair, err := pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair(); err == nil && pair != nil {
					s.relayed = pair.Local.Typ == webrtc.ICECandidateTypeRelay || pair.Remote.Typ == webrtc.ICECandidateTypeRelay
				}
				s.quality = "auto"
				s.level = autoLevel(s)
				keepAwake(true) // screens were woken when the viewer asked to connect
				a.mu.Lock()
				none := len(a.screens) == 0
				a.mu.Unlock()
				if none {
					log.Printf("no monitors found (asleep or unplugged)")
					a.ensureVirtualScreen(s)
				}
				if err := a.restartCapture(s); err != nil {
					log.Printf("capture failed: %v", err)
					a.send(wsMsg{Type: "bye", To: s.client, Sid: s.sid})
					go a.closeSession("capture failed")
					return
				}
				if s.localCursor {
					go a.syncCursor(s)
				}
				route := "direct"
				if s.relayed {
					route = "relayed (TURN)"
				}
				log.Printf("▶ streaming %s to viewer, %s, quality %s (session %s)", screenLabel(a.screens, a.sel), route, s.level, s.sid)
			})
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			a.mu.Lock()
			cur := a.sess
			a.mu.Unlock()
			if cur == s {
				go a.closeSession("connection " + st.String())
			}
		}
	})

	if err := pc.SetRemoteDescription(offer); err != nil {
		return err
	}
	for _, c := range s.pending {
		_ = pc.AddICECandidate(c)
	}
	s.pending = nil
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return err
	}
	a.signal(s, signalData{SDP: pc.LocalDescription()})
	log.Printf("viewer connecting (session %s)", sid)
	return nil
}

// wakeAndListScreens wakes the displays and waits (up to ~6s) until as many
// screens are back as the agent has seen before.
func (a *Agent) wakeAndListScreens() []Screen {
	asleep := wakeDisplays()
	ss := getScreens(a.cfg)
	if len(ss) >= a.maxScreens && !asleep {
		a.maxScreens = max(a.maxScreens, len(ss))
		return ss
	}
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) && len(ss) < max(a.maxScreens, 1) {
		time.Sleep(500 * time.Millisecond)
		wakeDisplays()
		ss = getScreens(a.cfg)
	}
	if asleep {
		time.Sleep(700 * time.Millisecond) // let a freshly woken screen settle
		ss = getScreens(a.cfg)
	}
	if len(ss) < a.maxScreens {
		log.Printf("only %d of %d screens came back after waking", len(ss), a.maxScreens)
	}
	a.maxScreens = max(a.maxScreens, len(ss))
	return ss
}

// refreshScreens re-reads the monitors mid-session (they may have been asleep
// or disconnected) and tells the viewer if the list changed.
func (a *Agent) refreshScreens(s *Session) {
	ss := a.wakeAndListScreens()
	a.mu.Lock()
	if a.sess != s {
		a.mu.Unlock()
		return
	}
	changed := len(ss) != len(a.screens)
	for i := range ss {
		if !changed && (ss[i].Index != a.screens[i].Index || ss[i].W != a.screens[i].W || ss[i].H != a.screens[i].H) {
			changed = true
		}
	}
	a.screens = ss
	a.sel = validSel(ss, a.sel)
	sel, dc := a.sel, s.dc
	a.mu.Unlock()
	a.input.SetTarget(target(ss, sel))
	if changed {
		log.Printf("screens changed: now %d", len(ss))
		if dc != nil {
			msg, _ := json.Marshal(map[string]any{"t": "screen", "screen": sel, "screens": ss})
			_ = dc.SendText(string(msg))
		}
	}
}

// routeInfo describes the network path in use, e.g.
// " — this PC: srflx udp 84.x.x.x:51234 ↔ viewer: relay udp 104.x.x.x:3478 | sent 3.1 MB, got 12 kB"
func routeInfo(pc *webrtc.PeerConnection) string {
	sctp := pc.SCTP()
	if sctp == nil || sctp.Transport() == nil || sctp.Transport().ICETransport() == nil {
		return ""
	}
	pair, err := sctp.Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return ""
	}
	out := fmt.Sprintf(" — this PC: %s %s %s:%d ↔ viewer: %s %s %s:%d",
		pair.Local.Typ, pair.Local.Protocol, pair.Local.Address, pair.Local.Port,
		pair.Remote.Typ, pair.Remote.Protocol, pair.Remote.Address, pair.Remote.Port)
	for _, st := range pc.GetStats() {
		if t, ok := st.(webrtc.TransportStats); ok {
			out += fmt.Sprintf(" | sent %s, got %s", byteSize(t.BytesSent), byteSize(t.BytesReceived))
			break
		}
	}
	return out
}

func byteSize(n uint64) string {
	switch {
	case n > 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n > 1<<10:
		return fmt.Sprintf("%d kB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// ensureVirtualScreen plugs in a virtual screen when the real monitors give
// nothing to capture (asleep, or treated as unplugged), like RustDesk, Parsec
// and Sunshine do. Needs the Parsec Virtual Display driver. Tried once per session.
func (a *Agent) ensureVirtualScreen(s *Session) bool {
	a.mu.Lock()
	if a.sess != s || s.triedVirtual || s.vIdx >= 0 {
		a.mu.Unlock()
		return false
	}
	s.triedVirtual = true
	old := map[int]bool{}
	for _, sc := range a.screens {
		old[sc.Index] = true
	}
	dc := s.dc
	a.mu.Unlock()

	if !vscreenAvailable() {
		log.Printf("tip: install the virtual screen driver so the PC can be shown while its monitors sleep (see README)")
		return false
	}
	a.status(dc, "Monitors are asleep — adding a virtual screen…")
	idx, err := vscreenAdd()
	if err != nil {
		log.Printf("virtual screen: %v", err)
		return false
	}
	var ss []Screen
	pick := -1
	for i := 0; i < 24 && pick < 0; i++ { // Windows needs a moment to bring it up
		time.Sleep(250 * time.Millisecond)
		ss = getScreens(a.cfg)
		for _, sc := range ss {
			if !old[sc.Index] {
				pick = sc.Index
			}
		}
	}
	if pick < 0 {
		log.Printf("the virtual screen didn't show up — removing it")
		vscreenRemove(idx)
		return false
	}
	a.mu.Lock()
	if a.sess != s {
		a.mu.Unlock()
		vscreenRemove(idx)
		return false
	}
	s.vIdx = idx
	for i := range ss {
		if ss[i].Index == pick {
			ss[i].Name = "Virtual screen"
		}
	}
	a.screens, a.sel = ss, pick
	s.lastFrame = time.Now() // give the new screen time to produce a picture
	a.mu.Unlock()
	a.input.SetTarget(target(ss, pick))
	log.Printf("▶ added a virtual screen (%s) — showing it while the monitors are off", screenLabel(ss, pick))
	if dc != nil {
		msg, _ := json.Marshal(map[string]any{"t": "screen", "screen": pick, "screens": ss})
		_ = dc.SendText(string(msg))
	}
	return true
}

func autoLevel(s *Session) string {
	if s.relayed {
		return "balanced"
	}
	return "high"
}

// restartCapture (re)starts the encoder for the session with its current
// screen and quality. A new encoder always begins with a keyframe.
func (a *Agent) restartCapture(s *Session) error {
	a.capMu.Lock()
	defer a.capMu.Unlock()
	a.mu.Lock()
	if a.sess != s {
		a.mu.Unlock()
		return nil
	}
	old := s.cap
	s.cap = nil
	ss, sel, level := a.screens, a.sel, s.level
	a.mu.Unlock()

	if old != nil {
		old.Stop()
	}
	es := settingsFor(a.cfg, level)
	es.DrawMouse = !s.localCursor
	capt, err := StartCapture(a.cfg, ss, sel, es, s.track)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		return err
	}
	if a.sess != s {
		capt.Stop()
		return nil
	}
	s.cap = capt
	s.started = time.Now()
	if s.lastFrame.IsZero() {
		s.lastFrame = s.started
	}
	go a.watch(s, capt)
	go a.watchFrames(s, capt)
	return nil
}

// watchFrames notices when capture runs but no picture comes out — e.g. the
// monitors are off or Windows is showing the lock screen — tries to fix it,
// and tells the viewer why. Its state lives on the session, so capture
// restarts don't reset it.
func (a *Agent) watchFrames(s *Session, capt *Capture) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	var last int64
	kicks := 0
	for {
		select {
		case <-capt.Done():
			return
		case <-t.C:
		}
		// No first picture yet? Nudge Windows to repaint (at 0.8s, 1.6s, 2.4s).
		if n := capt.Frames(); n == 0 {
			if el := time.Since(capt.started); kicks < 3 && el > time.Duration(kicks+1)*800*time.Millisecond {
				kicks++
				log.Printf("no picture yet after %s — asking Windows to repaint (%d/3)", el.Round(100*time.Millisecond), kicks)
				kickScreen()
			}
		} else if last == 0 {
			log.Printf("first picture after %s", time.Since(capt.started).Round(10*time.Millisecond))
		}
		a.mu.Lock()
		if a.sess != s || s.cap != capt {
			a.mu.Unlock()
			return
		}
		dc := s.dc
		if n := capt.Frames(); n != last {
			last = n
			s.lastFrame = time.Now()
			back := s.stallStage > 0
			s.stallStage, s.stalled = 0, false
			a.mu.Unlock()
			if back {
				log.Printf("picture is back")
				a.status(dc, "")
			}
			continue
		}
		quiet := time.Since(s.lastFrame)
		stage := s.stallStage
		a.mu.Unlock()

		switch {
		case stage == 0 && quiet > 4*time.Second:
			a.setStall(s, 1)
			log.Printf("no picture from the screen for %s — waking the displays", quiet.Round(time.Second))
			a.status(dc, "Waking the screen…")
			a.refreshScreens(s)
			go a.restartCapture(s) // a fresh capture after waking; a new watcher takes over
			return
		case stage == 1 && quiet > 8*time.Second && !s.triedVirtual:
			if a.ensureVirtualScreen(s) {
				go a.restartCapture(s)
				return
			}
		case stage == 1 && quiet > 12*time.Second:
			a.setStall(s, 2)
			msg := screenProblem("")
			log.Printf("still no picture: %s", msg)
			a.statusErr(dc, msg)
		case stage >= 2 && int(quiet.Seconds())%15 == 0:
			// keep trying quietly: the moment the PC is unlocked the picture comes back
			a.refreshScreens(s)
			go a.restartCapture(s)
			return
		}
	}
}

func (a *Agent) setStall(s *Session, stage int) {
	a.mu.Lock()
	s.stallStage, s.stalled = stage, true
	a.mu.Unlock()
}

// screenProblem explains why there's no picture, as well as the agent can tell.
func screenProblem(ffmpegErr string) string {
	if desk := inputDesktop(); desk != "" && desk != "Default" {
		return "Your PC is locked (or showing a Windows security prompt). Windows doesn't let apps capture the lock screen — " +
			"unlock it at the PC, or turn off \"require sign-in\" in Windows settings so it doesn't lock while you're away."
	}
	if ffmpegErr != "" {
		e := ffmpegErr
		if len(e) > 160 {
			e = e[:160] + "…"
		}
		return "Screen capture failed on the PC: " + e
	}
	msg := "The PC's screen isn't giving a picture (monitors off or disconnected?). Try again in a moment."
	if runtime.GOOS == "windows" && !vscreenAvailable() {
		msg += " Tip: install the virtual screen driver on the PC so it can be shown while its monitors sleep."
	}
	return msg
}

// syncCursor tells the viewer which pointer shape the PC shows (arrow, text
// cursor, hand, resize...), so its locally drawn pointer matches.
func (a *Agent) syncCursor(s *Session) {
	t := time.NewTicker(40 * time.Millisecond)
	defer t.Stop()
	last := "?"
	for range t.C {
		a.mu.Lock()
		alive, dc := a.sess == s, s.dc
		a.mu.Unlock()
		if !alive {
			return
		}
		c := cursorShape()
		if c == last || dc == nil {
			continue
		}
		last = c
		b, _ := json.Marshal(map[string]any{"t": "cursor", "c": c})
		_ = dc.SendText(string(b))
	}
}

func (a *Agent) statusErr(dc *webrtc.DataChannel, msg string) {
	if dc == nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"t": "status", "s": msg, "err": true})
	_ = dc.SendText(string(b))
}

// watch restarts capture if ffmpeg dies on its own — typically because the
// monitors were asleep or just woke up, which interrupts screen duplication.
func (a *Agent) watch(s *Session, capt *Capture) {
	<-capt.Done()
	if !capt.Crashed() {
		return
	}
	for attempt := 1; ; attempt++ {
		a.mu.Lock()
		alive := a.sess == s && (s.cap == capt || s.cap == nil)
		dc := s.dc
		a.mu.Unlock()
		if !alive {
			return
		}
		if attempt == 1 {
			log.Printf("screen capture stopped — waking the displays and retrying")
		}
		if attempt == 2 && a.ensureVirtualScreen(s) {
			if err := a.restartCapture(s); err == nil {
				return
			}
		}
		if attempt >= 3 {
			msg := screenProblem(capt.ErrText())
			if attempt == 3 {
				log.Printf("capture keeps failing: %s", msg)
			}
			a.statusErr(dc, msg)
		} else {
			a.status(dc, "Waking the screen…")
		}
		a.refreshScreens(s)
		time.Sleep(time.Duration(min(attempt, 5)) * time.Second)
		a.mu.Lock()
		still := a.sess == s && (s.cap == capt || s.cap == nil)
		a.mu.Unlock()
		if !still {
			return
		}
		if err := a.restartCapture(s); err == nil {
			// restartCapture started a new watcher; see if it survives a moment
			time.Sleep(1500 * time.Millisecond)
			a.mu.Lock()
			cur := s.cap
			a.mu.Unlock()
			if cur != nil {
				select {
				case <-cur.Done():
					if !cur.Crashed() {
						return
					}
					capt = cur // crashed again: keep trying from here
					continue
				default:
					a.status(dc, "")
					return
				}
			}
		}
	}
}

// status shows a short message on the viewer's screen ("" clears it).
func (a *Agent) status(dc *webrtc.DataChannel, msg string) {
	if dc == nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"t": "status", "s": msg})
	_ = dc.SendText(string(b))
}

// keyframe answers a picture-loss report, at most every 2 seconds.
func (a *Agent) keyframe(s *Session) {
	a.mu.Lock()
	// A fresh keyframe only helps if the screen is producing pictures at all.
	if a.sess != s || s.cap == nil || s.stalled || time.Since(s.lastFrame) > 2*time.Second ||
		time.Since(s.started) < 1500*time.Millisecond || time.Since(s.lastKF) < 2*time.Second {
		a.mu.Unlock()
		return
	}
	s.lastKF = time.Now()
	a.mu.Unlock()
	log.Printf("viewer lost the picture — sending a fresh keyframe")
	if err := a.restartCapture(s); err != nil {
		log.Printf("keyframe restart failed: %v", err)
	}
}

// setQuality applies the viewer's choice. For "auto" the browser may pass a
// level it wants to step down to when frames aren't getting through.
func (a *Agent) setQuality(s *Session, q, autoStep string) {
	if q != "auto" && !validLevel(q) {
		return
	}
	a.mu.Lock()
	if a.sess != s {
		a.mu.Unlock()
		return
	}
	level := q
	if q == "auto" {
		level = autoLevel(s)
		if validLevel(autoStep) {
			level = autoStep
		}
	}
	changed := level != s.level
	s.quality, s.level = q, level
	dc := s.dc
	a.mu.Unlock()

	if changed {
		if err := a.restartCapture(s); err != nil {
			log.Printf("could not change quality: %v", err)
			return
		}
		log.Printf("quality: %s (%s)", level, qualityLevels[level].describe)
	}
	if dc != nil {
		msg, _ := json.Marshal(map[string]any{"t": "quality", "quality": q, "level": level, "relayed": s.relayed})
		_ = dc.SendText(string(msg))
	}
}

// switchScreen restarts capture on another monitor (or all of them) mid-session.
func (a *Agent) switchScreen(s *Session, sel int) {
	a.mu.Lock()
	if a.sess != s {
		a.mu.Unlock()
		return
	}
	a.screens = getScreens(a.cfg)
	sel = validSel(a.screens, sel)
	a.sel = sel
	dc := s.dc
	a.mu.Unlock()

	a.input.ReleaseAll()
	a.input.SetTarget(target(a.screens, sel))
	if err := a.restartCapture(s); err != nil {
		log.Printf("could not switch screen: %v", err)
		return
	}
	log.Printf("switched to %s", screenLabel(a.screens, sel))
	if dc != nil {
		msg, _ := json.Marshal(map[string]any{"t": "screen", "screen": sel, "screens": a.screens})
		_ = dc.SendText(string(msg))
	}
}

func screenLabel(ss []Screen, sel int) string {
	t := target(ss, sel)
	if sel == AllScreens && len(ss) > 1 {
		return fmt.Sprintf("all %d screens (%dx%d)", len(ss), t.W, t.H)
	}
	return fmt.Sprintf("%s (%dx%d)", t.Name, t.W, t.H)
}

func (a *Agent) closeSession(reason string) {
	defer keepAwake(false)
	a.mu.Lock()
	s := a.sess
	a.sess = nil
	a.mu.Unlock()
	if s == nil {
		return
	}
	if s.cap != nil {
		s.cap.Stop()
	}
	s.pc.Close()
	a.input.ReleaseAll()
	if s.vIdx >= 0 {
		vscreenRemove(s.vIdx)
		log.Printf("removed the virtual screen")
	}
	log.Printf("■ session %s closed: %s", s.sid, reason)
}

func main() {
	log.SetFlags(log.Ltime)
	cfg := loadConfig()
	platformInit()
	a := &Agent{cfg: cfg, input: newInjector(cfg), sel: cfg.Monitor}
	a.screens = getScreens(cfg)
	a.maxScreens = len(a.screens)
	if runtime.GOOS == "windows" && !cfg.Test && !vscreenAvailable() {
		log.Printf("virtual screen driver not installed — the PC can't be shown while its monitors sleep (see README)")
	}
	for _, sc := range a.screens {
		log.Printf("found %s: %dx%d at (%d,%d), rotation %d°", sc.Name, sc.W, sc.H, sc.X, sc.Y, sc.Rotation)
	}
	log.Printf("Maze Remote agent %s — %s (%s), encoder %s, %d fps", version, cfg.Name, runtime.GOOS, cfg.Encoder, cfg.FPS)
	if cfg.Test {
		log.Println("TEST MODE: streaming a test pattern; input is logged, not applied")
	}
	a.run()
}
