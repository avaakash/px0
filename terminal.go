package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const terminalHistoryLimit = 256 << 10
const terminalSessionLimit = 16

// A bounded byte ring retains output for reconnects without retaining command logs on disk.
type terminalRing struct {
	data [terminalHistoryLimit]byte
	end  int64
}

func (b *terminalRing) write(p []byte) {
	end := b.end + int64(len(p))
	if len(p) > len(b.data) {
		p = p[len(p)-len(b.data):]
	}
	start := end - int64(len(p))
	at := int(start % int64(len(b.data)))
	n := copy(b.data[at:], p)
	copy(b.data[:], p[n:])
	b.end = end
}
func (b *terminalRing) read(offset int64) ([]byte, int64, bool) {
	first := max(int64(0), b.end-int64(len(b.data)))
	truncated := offset < first
	offset = max(first, min(offset, b.end))
	n := min(int64(32768), b.end-offset)
	out := make([]byte, int(n))
	at := int(offset % int64(len(b.data)))
	k := copy(out, b.data[at:])
	copy(out[k:], b.data[:])
	return out, offset + n, truncated
}

type terminalSession struct {
	ID        string `json:"id"`
	Root      string `json:"root"`
	Token     string `json:"token"`
	mu        sync.Mutex
	inputMu   sync.Mutex
	file      *os.File
	cmd       *exec.Cmd
	ring      terminalRing
	changed   chan struct{}
	done      chan struct{}
	exited    bool
	exitCode  int
	closeOnce sync.Once
	name      string
	number    int
	sequence  uint64
}
type terminalManager struct {
	mu           sync.Mutex
	sessions     map[string]*terminalSession
	closed       bool
	nextSequence uint64
	rootNumbers  map[string]int
}
type terminalInfo struct {
	ID       string `json:"id"`
	Root     string `json:"root"`
	Token    string `json:"token"`
	Exited   bool   `json:"exited"`
	Name     string `json:"name"`
	Number   int    `json:"number"`
	Sequence uint64 `json:"sequence"`
}

func (t *terminalSession) info() terminalInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return terminalInfo{ID: t.ID, Root: t.Root, Token: t.Token, Exited: t.exited, Name: t.name, Number: t.number, Sequence: t.sequence}
}
func terminalRandom() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func (t *terminalSession) notify() { close(t.changed); t.changed = make(chan struct{}) }
func (m *terminalManager) open(root string, cols, rows uint16, fresh bool) (*terminalSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errors.New("px0 is shutting down")
	}
	if m.sessions == nil {
		m.sessions = make(map[string]*terminalSession)
	}
	var existing *terminalSession
	for id, t := range m.sessions {
		info := t.info()
		if info.Exited {
			delete(m.sessions, id)
		} else if !fresh && info.Root == root && (existing == nil || info.Sequence < existing.sequence) {
			existing = t
		}
	}
	if existing != nil {
		return existing, nil
	}
	if len(m.sessions) >= terminalSessionLimit {
		return nil, fmt.Errorf("close an existing terminal before opening more (limit %d)", terminalSessionLimit)
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	// SHELL is an executable path, never a shell command supplied by the browser.
	cmd := exec.Command(shell, "-l")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")
	file, err := startTerminalPTY(cmd, cols, rows)
	if err != nil {
		return nil, fmt.Errorf("start terminal: %w", err)
	}
	if m.rootNumbers == nil {
		m.rootNumbers = make(map[string]int)
	}
	m.rootNumbers[root]++
	m.nextSequence++
	t := &terminalSession{number: m.rootNumbers[root], sequence: m.nextSequence, ID: terminalRandom(), Root: root, Token: terminalRandom(), file: file, cmd: cmd, changed: make(chan struct{}), done: make(chan struct{})}
	m.sessions[t.ID] = t
	go func() {
		defer close(t.done)
		buf := make([]byte, 32768)
		for {
			n, err := file.Read(buf)
			if n > 0 {
				t.mu.Lock()
				t.ring.write(buf[:n])
				t.notify()
				t.mu.Unlock()
			}
			if err != nil {
				break
			}
		}
		err := cmd.Wait()
		code := 0
		if err != nil {
			code = -1
			if cmd.ProcessState != nil {
				code = cmd.ProcessState.ExitCode()
			}
		}
		t.mu.Lock()
		t.exited = true
		t.exitCode = code
		t.notify()
		t.mu.Unlock()
		_ = file.Close()
	}()
	return t, nil
}
func (t *terminalSession) close() {
	t.closeOnce.Do(func() {
		stopTerminalPTY(t.cmd, t.file)
		select {
		case <-t.done:
		case <-time.After(time.Second):
			_ = t.cmd.Process.Kill()
		}
	})
}
func (m *terminalManager) closeAll() {
	m.mu.Lock()
	m.closed = true
	sessions := m.sessions
	m.sessions = nil
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, t := range sessions {
		wg.Add(1)
		go func() { defer wg.Done(); t.close() }()
	}
	wg.Wait()
}
func (s *Server) CloseTerminals() { s.terminals.closeAll() }

type terminalRequest struct {
	ID       string `json:"id"`
	Token    string `json:"token"`
	Worktree string `json:"worktree"`
	Data     []byte `json:"data"`
	Cols     uint16 `json:"cols"`
	Rows     uint16 `json:"rows"`
	Offset   int64  `json:"offset"`
	New      bool   `json:"new"`
	Name     string `json:"name"`
}

func terminalSize(cols, rows uint16) (uint16, uint16) {
	return min(400, max(2, cols)), min(200, max(2, rows))
}
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) {
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(host).IsLoopback() {
		fail(w, 403, "terminals are available only on this computer")
		return
	}
	var body terminalRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 128<<10)).Decode(&body); err != nil {
		fail(w, 400, "invalid terminal request")
		return
	}
	action := filepath.Base(r.URL.Path)
	if action == "list" {
		s.terminals.mu.Lock()
		items := make([]terminalInfo, 0, len(s.terminals.sessions))
		for _, t := range s.terminals.sessions {
			items = append(items, t.info())
		}
		s.terminals.mu.Unlock()
		sort.Slice(items, func(i, j int) bool { return items[i].Sequence < items[j].Sequence })
		writeJSON(w, map[string]any{"sessions": items})
		return
	}
	if action == "open" {
		root := s.ix.Root()
		if body.Worktree != "" && worktreePath(body.Worktree) != worktreePath(root) {
			var ok bool
			root, ok = s.selectRegisteredWorktree(w, r, body.Worktree, "")
			if !ok {
				return
			}
		}
		cols, rows := terminalSize(body.Cols, body.Rows)
		t, err := s.terminals.open(worktreePath(root), cols, rows, body.New)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		writeJSON(w, t.info())
		return
	}
	s.terminals.mu.Lock()
	t := s.terminals.sessions[body.ID]
	s.terminals.mu.Unlock()
	if t == nil || subtle.ConstantTimeCompare([]byte(t.Token), []byte(body.Token)) != 1 {
		fail(w, 403, "terminal session is unavailable")
		return
	}
	switch action {
	case "rename":
		name := strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, body.Name))
		if len([]rune(name)) > 120 {
			fail(w, 400, "terminal name must be 120 characters or fewer")
			return
		}
		t.mu.Lock()
		t.name = name
		t.mu.Unlock()
		writeJSON(w, t.info())
	case "stream":
		s.streamTerminal(w, r, t, max(0, body.Offset))
	case "input":
		if len(body.Data) > 65536 {
			fail(w, 400, "terminal input is too large")
			return
		}
		t.inputMu.Lock()
		_ = t.file.SetWriteDeadline(time.Now().Add(3 * time.Second))
		_, err := t.file.Write(body.Data)
		_ = t.file.SetWriteDeadline(time.Time{})
		t.inputMu.Unlock()
		if err != nil {
			fail(w, 409, "terminal has exited")
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	case "resize":
		cols, rows := terminalSize(body.Cols, body.Rows)
		if err := resizeTerminalPTY(t.file, cols, rows); err != nil {
			fail(w, 409, "terminal has exited")
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	case "close":
		s.terminals.mu.Lock()
		delete(s.terminals.sessions, t.ID)
		s.terminals.mu.Unlock()
		t.close()
		writeJSON(w, map[string]bool{"ok": true})
	default:
		fail(w, 404, "unknown terminal action")
	}
}
func (s *Server) streamTerminal(w http.ResponseWriter, r *http.Request, t *terminalSession, offset int64) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "streaming unavailable")
		return
	}
	// A slow or disconnected browser must never block the shell's output reader.
	defer http.NewResponseController(w).SetWriteDeadline(time.Time{})
	encoder := json.NewEncoder(w)
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		t.mu.Lock()
		data, next, truncated := t.ring.read(offset)
		exited, code, changed := t.exited, t.exitCode, t.changed
		caughtUp := next == t.ring.end
		t.mu.Unlock()
		if len(data) > 0 || exited || truncated {
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := encoder.Encode(struct {
				Data      []byte `json:"data,omitempty"`
				Offset    int64  `json:"offset"`
				Truncated bool   `json:"truncated,omitempty"`
				Exited    bool   `json:"exited,omitempty"`
				Code      int    `json:"code,omitempty"`
			}{data, next, truncated, exited && caughtUp, code}); err != nil {
				return
			}
			flusher.Flush()
			offset = next
			if exited && caughtUp {
				return
			}
			if !caughtUp {
				continue
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.streamsDone:
			return
		case <-changed:
			// Coalesce bursts to avoid one HTTP frame per character.
			timer := time.NewTimer(16 * time.Millisecond)
			select {
			case <-timer.C:
			case <-r.Context().Done():
				timer.Stop()
				return
			}
		case <-heartbeat.C:
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprintln(w, "{}"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
