package main

// Remote maintenance: "update" downloads the newest agent from the GitHub
// release and restarts into it; "restart" just restarts. Both arrive over the
// signaling connection, so they work even when there's no video.

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const releaseBase = "https://github.com/mazijoni/maze-remote/releases/latest/download/"

// exePath is where the agent was started from, read once at startup: after an
// update renames the running file, Linux reports the renamed (.old) path.
var exePath = func() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}()

func assetName() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/arm64":
		return "maze-remote-agent-arm64.exe"
	case "windows/amd64":
		return "maze-remote-agent.exe"
	case "linux/arm64":
		return "maze-remote-agent-linux-arm64"
	case "linux/amd64":
		return "maze-remote-agent-linux"
	}
	return ""
}

func (a *Agent) reply(to string, ok bool, msg string) {
	a.send(wsMsg{Type: "command-result", To: to, OK: ok, Msg: msg})
}

func (a *Agent) runCommand(from, cmd string) {
	switch cmd {
	case "restart":
		log.Printf("restart requested from the website")
		a.reply(from, true, a.cfg.Name+" is restarting — back in a few seconds.")
		a.restartSelf()
	case "update":
		log.Printf("update requested from the website")
		if err := a.selfUpdate(); err != nil {
			log.Printf("update failed: %v", err)
			a.reply(from, false, "Update failed: "+err.Error())
			return
		}
		a.reply(from, true, a.cfg.Name+" downloaded the newest agent and is restarting — back in a few seconds.")
		a.restartSelf()
	}
}

// selfUpdate downloads the newest agent next to the current one and swaps it
// in. A running program can't be overwritten, but it can be renamed, so the
// current one becomes .old (deleted on the next start).
func (a *Agent) selfUpdate() error {
	name := assetName()
	if name == "" {
		return fmt.Errorf("no prebuilt agent for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	exe := exePath
	if exe == "" {
		return fmt.Errorf("can't tell where the agent is installed")
	}

	client := &http.Client{Timeout: 3 * time.Minute, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	resp, err := client.Get(releaseBase + name)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	magic := []byte("\x7fELF")
	if runtime.GOOS == "windows" {
		magic = []byte("MZ")
	}
	if len(data) < 1<<20 || !bytes.HasPrefix(data, magic) {
		return fmt.Errorf("the download doesn't look like an agent (%d bytes)", len(data))
	}

	tmp := exe + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("swap: %w", err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Rename(old, exe) // put the working one back
		return fmt.Errorf("swap: %w", err)
	}
	log.Printf("downloaded the newest agent (%.1f MB)", float64(len(data))/(1<<20))
	return nil
}

// restartSelf starts the agent again (same program path, arguments, folder and
// console) and exits this copy.
func (a *Agent) restartSelf() {
	time.Sleep(500 * time.Millisecond) // let the reply reach the website
	exe := exePath
	if exe == "" {
		log.Printf("restart failed: can't tell where the agent is installed")
		return
	}
	a.closeSession("agent restarting")
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Dir, _ = os.Getwd()
	if err := cmd.Start(); err != nil {
		log.Printf("restart failed: %v", err)
		return
	}
	log.Printf("restarted — the new agent continues in this window")
	os.Exit(0)
}

// cleanupOld removes the previous version left behind by an update.
func cleanupOld() {
	if exePath == "" {
		return
	}
	old := exePath + ".old"
	for i := 0; i < 10; i++ { // the old copy may still be exiting
		if err := os.Remove(old); err == nil || os.IsNotExist(err) {
			return
		}
		time.Sleep(time.Second)
	}
}
