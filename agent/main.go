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
	"time"

	"github.com/gorilla/websocket"
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

	screens []Screen // monitors, refreshed at the start of each session
	sel     int      // current screen index, or AllScreens
	capMu   sync.Mutex
}

type Session struct {
	sid     string
	client  string
	pc      *webrtc.PeerConnection
	track   *webrtc.TrackLocalStaticSample
	cap     *Capture
	dc      *webrtc.DataChannel
	once    sync.Once
	pending []webrtc.ICECandidateInit
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
		a.closeSession("replaced by new viewer")
		if err := a.startSession(m.From, m.Sid, *d.SDP); err != nil {
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

func (a *Agent) startSession(client, sid string, offer webrtc.SessionDescription) error {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: a.ice})
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
	a.screens = getScreens(a.cfg)
	a.sel = validSel(a.screens, a.sel)
	a.input.SetTarget(target(a.screens, a.sel))
	s := &Session{sid: sid, client: client, pc: pc, track: track}
	a.mu.Lock()
	a.sess = s
	a.mu.Unlock()

	// Read RTCP so interceptors (NACK etc.) work; request keyframe on PLI.
	go func() {
		for {
			pkts, _, err := sender.ReadRTCP()
			if err != nil {
				return
			}
			_ = pkts
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
				default:
					a.input.Handle(ev)
				}
			}
		})
		dc.OnOpen(func() {
			a.mu.Lock()
			s.dc = dc
			a.mu.Unlock()
			info, _ := json.Marshal(map[string]any{"t": "hello", "name": a.cfg.Name, "os": runtime.GOOS, "version": version,
				"encoder": a.cfg.Encoder, "screens": a.screens, "screen": a.sel})
			_ = dc.SendText(string(info))
		})
	})

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		log.Printf("peer connection: %s", st)
		switch st {
		case webrtc.PeerConnectionStateConnected:
			s.once.Do(func() {
				a.capMu.Lock()
				defer a.capMu.Unlock()
				capt, err := StartCapture(a.cfg, a.screens, a.sel, track)
				if err != nil {
					log.Printf("capture failed: %v", err)
					a.send(wsMsg{Type: "bye", To: s.client, Sid: s.sid})
					go a.closeSession("capture failed")
					return
				}
				a.mu.Lock()
				s.cap = capt
				a.mu.Unlock()
				log.Printf("▶ streaming %s to viewer (session %s)", screenLabel(a.screens, a.sel), s.sid)
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

// switchScreen restarts capture on another monitor (or all of them) mid-session.
func (a *Agent) switchScreen(s *Session, sel int) {
	a.capMu.Lock()
	defer a.capMu.Unlock()
	a.mu.Lock()
	if a.sess != s {
		a.mu.Unlock()
		return
	}
	a.screens = getScreens(a.cfg)
	sel = validSel(a.screens, sel)
	old := s.cap
	s.cap = nil
	a.sel = sel
	a.mu.Unlock()

	if old != nil {
		old.Stop()
	}
	a.input.ReleaseAll()
	a.input.SetTarget(target(a.screens, sel))
	capt, err := StartCapture(a.cfg, a.screens, sel, s.track)
	a.mu.Lock()
	if err == nil && a.sess == s {
		s.cap = capt
	}
	dc := s.dc
	a.mu.Unlock()
	if err != nil {
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
	log.Printf("■ session %s closed: %s", s.sid, reason)
}

func main() {
	log.SetFlags(log.Ltime)
	cfg := loadConfig()
	platformInit()
	a := &Agent{cfg: cfg, input: newInjector(cfg), sel: cfg.Monitor}
	a.screens = getScreens(cfg)
	for _, sc := range a.screens {
		log.Printf("found %s: %dx%d at (%d,%d)", sc.Name, sc.W, sc.H, sc.X, sc.Y)
	}
	log.Printf("Maze Remote agent %s — %s (%s), encoder %s, %d fps", version, cfg.Name, runtime.GOOS, cfg.Encoder, cfg.FPS)
	if cfg.Test {
		log.Println("TEST MODE: streaming a test pattern; input is logged, not applied")
	}
	a.run()
}
