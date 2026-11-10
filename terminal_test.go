package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func terminalPOST(s *Server, action string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "http://localhost/api/terminal/"+action, bytes.NewReader(b))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Origin", "http://localhost")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func terminalTestShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix PTY")
	}
	shell := filepath.Join(t.TempDir(), "shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nexec /bin/sh -i\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)
}
func terminalWaitOutput(t *testing.T, session *terminalSession, want string) {
	t.Helper()
	deadline := time.NewTimer(4 * time.Second)
	defer deadline.Stop()
	for {
		session.mu.Lock()
		var all []byte
		off := max(int64(0), session.ring.end-terminalHistoryLimit)
		for off < session.ring.end {
			b, next, _ := session.ring.read(off)
			all = append(all, b...)
			off = next
		}
		changed := session.changed
		session.mu.Unlock()
		if strings.Contains(string(all), want) {
			return
		}
		select {
		case <-changed:
		case <-deadline.C:
			t.Fatalf("missing terminal output %q: %.400s", want, all)
		}
	}
}
func TestTerminalRingBoundedReplay(t *testing.T) {
	var ring terminalRing
	input := bytes.Repeat([]byte("0123456789abcdef"), 50000)
	ring.write(input[:123])
	ring.write(input[123:])
	var got []byte
	off := int64(0)
	trimmed := false
	for off < ring.end {
		b, next, gap := ring.read(off)
		got = append(got, b...)
		off = next
		trimmed = trimmed || gap
	}
	if !trimmed || len(got) != terminalHistoryLimit || !bytes.Equal(got, input[len(input)-terminalHistoryLimit:]) {
		t.Fatal("ring does not retain exactly the newest bounded output")
	}
}
func BenchmarkTerminalRing(b *testing.B) {
	var ring terminalRing
	data := bytes.Repeat([]byte("x"), 4096)
	b.ReportAllocs()
	for b.Loop() {
		ring.write(data)
		ring.read(ring.end - 4096)
	}
}
func TestTerminalLifecycleReconnectScopeAndControls(t *testing.T) {
	terminalTestShell(t)
	newTestStore(t)
	root := gitRepo(t)
	linked := filepath.Join(root, ".claude", "worktrees", "terminal")
	addTestWorktree(t, root, linked, "-b", "terminal-test")
	s := NewServer(NewIndex(root), nil)
	s.gitWatcher.Stop()
	defer s.CloseTerminals()
	w := terminalPOST(s, "open", map[string]any{"worktree": linked, "cols": 90, "rows": 25})
	if w.Code != 200 {
		t.Fatalf("open %d %s", w.Code, w.Body)
	}
	var info terminalInfo
	json.Unmarshal(w.Body.Bytes(), &info)
	w = terminalPOST(s, "open", map[string]any{"worktree": linked})
	var again terminalInfo
	json.Unmarshal(w.Body.Bytes(), &again)
	if again.ID != info.ID {
		t.Fatal("opening an existing worktree replaced its shell")
	}
	session := s.terminals.sessions[info.ID]
	command := []byte("export PX0_TERMINAL_TEST=alive; printf 'working-dir:'; pwd; printf 'session:%s\\n' \"$PX0_TERMINAL_TEST\"\n")
	w = terminalPOST(s, "input", terminalRequest{ID: info.ID, Token: info.Token, Data: command})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	terminalWaitOutput(t, session, "session:alive")
	session.mu.Lock()
	data, _, _ := session.ring.read(0)
	session.mu.Unlock()
	if !strings.Contains(string(data), linked) {
		t.Fatal("shell started in wrong checkout")
	}
	// New browser obtains the same session and capability, not a new process.
	w = terminalPOST(s, "list", map[string]any{})
	if !strings.Contains(w.Body.String(), info.ID) {
		t.Fatal("reconnect cannot find live shell")
	}
	w = terminalPOST(s, "resize", terminalRequest{ID: info.ID, Token: info.Token, Cols: 83, Rows: 24})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	w = terminalPOST(s, "input", terminalRequest{ID: info.ID, Token: info.Token, Data: []byte("stty size\n")})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	terminalWaitOutput(t, session, "24 83")
	// Ctrl-C must interrupt a foreground command and return to the same shell.
	terminalPOST(s, "input", terminalRequest{ID: info.ID, Token: info.Token, Data: []byte("sleep 30\n")})
	time.Sleep(50 * time.Millisecond)
	terminalPOST(s, "input", terminalRequest{ID: info.ID, Token: info.Token, Data: []byte{3}})
	terminalPOST(s, "input", terminalRequest{ID: info.ID, Token: info.Token, Data: []byte("printf 'after-interrupt:%s\\n' \"$PX0_TERMINAL_TEST\"\n")})
	terminalWaitOutput(t, session, "after-interrupt:alive")
	w = terminalPOST(s, "input", terminalRequest{ID: info.ID, Token: "wrong", Data: []byte("bad")})
	if w.Code != 403 {
		t.Fatal("wrong capability accepted")
	}
	w = terminalPOST(s, "open", map[string]any{"worktree": t.TempDir()})
	if w.Code != 400 {
		t.Fatal("unregistered terminal root accepted")
	}
	w = terminalPOST(s, "close", terminalRequest{ID: info.ID, Token: info.Token})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	select {
	case <-session.done:
	case <-time.After(time.Second):
		t.Fatal("closed shell was not reaped")
	}
	s.CloseTerminals()
	if _, err := s.terminals.open(root, 80, 24, false); err == nil {
		t.Fatal("terminal created after shutdown")
	}
}
func TestTerminalRequestsRejectRemoteOriginAndMethod(t *testing.T) {
	s := NewServer(NewIndex(t.TempDir()), nil)
	s.gitWatcher.Stop()
	defer s.CloseTerminals()
	for _, tc := range []struct{ method, origin, remote string }{{"GET", "http://localhost", "127.0.0.1:1"}, {"POST", "https://other.example", "127.0.0.1:1"}, {"POST", "http://localhost", "192.0.2.1:1"}} {
		r := httptest.NewRequest(tc.method, "http://localhost/api/terminal/list", strings.NewReader("{}"))
		r.Header.Set("Origin", tc.origin)
		r.RemoteAddr = tc.remote
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 403 && w.Code != 405 {
			t.Fatalf("request accepted: %+v", tc)
		}
	}
}
func TestTerminalOutputStreamReplayAndExit(t *testing.T) {
	terminalTestShell(t)
	s := NewServer(NewIndex(t.TempDir()), nil)
	s.gitWatcher.Stop()
	defer s.CloseTerminals()
	w := terminalPOST(s, "open", map[string]any{})
	var info terminalInfo
	json.Unmarshal(w.Body.Bytes(), &info)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	terminalPOST(s, "input", terminalRequest{ID: info.ID, Token: info.Token, Data: []byte("printf 'stream-replay-marker\\n'; exit 7\n")})
	session := s.terminals.sessions[info.ID]
	select {
	case <-session.done:
	case <-time.After(4 * time.Second):
		t.Fatal("shell did not exit")
	}
	w = terminalPOST(s, "stream", terminalRequest{ID: info.ID, Token: info.Token})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	decoder := json.NewDecoder(w.Body)
	var output []byte
	var exited bool
	var code int
	for {
		var frame struct {
			Data   []byte
			Exited bool
			Code   int
		}
		err := decoder.Decode(&frame)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		output = append(output, frame.Data...)
		if frame.Exited {
			exited = true
			code = frame.Code
		}
	}
	if !strings.Contains(string(output), "stream-replay-marker") || !exited || code != 7 {
		t.Fatalf("incomplete replay / exit: %v %d %.100s", exited, code, output)
	}
}

// The toolbar's + request must create another shell even when its worktree
// already has a terminal. Reopening the worktree itself should still reuse one.
func TestTerminalPlusCreatesIndependentSessions(t *testing.T) {
	terminalTestShell(t)
	s := NewServer(NewIndex(t.TempDir()), nil)
	s.gitWatcher.Stop()
	defer s.CloseTerminals()
	var ids []string
	for range 2 {
		w := terminalPOST(s, "open", map[string]any{"new": true})
		if w.Code != 200 {
			t.Fatalf("+ open: %d", w.Code)
		}
		var info terminalInfo
		if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, info.ID)
	}
	if ids[0] == ids[1] {
		t.Fatal("+ returned the existing shell instead of creating a second terminal")
	}
}

func TestTerminalNamesAndMultipleShellsSurviveReconnect(t *testing.T) {
	terminalTestShell(t)
	s := NewServer(NewIndex(t.TempDir()), nil)
	s.gitWatcher.Stop()
	defer s.CloseTerminals()
	open := func() terminalInfo {
		t.Helper()
		w := terminalPOST(s, "open", map[string]any{"new": true})
		if w.Code != 200 {
			t.Fatal(w.Body)
		}
		var info terminalInfo
		if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
			t.Fatal(err)
		}
		return info
	}
	first, second := open(), open()
	if first.ID == second.ID || first.Number != 1 || second.Number != 2 {
		t.Fatal("new terminals must have distinct identities and default numbers")
	}
	w := terminalPOST(s, "rename", terminalRequest{ID: first.ID, Token: first.Token, Name: "  Build & test <local>  "})
	if w.Code != 200 {
		t.Fatalf("rename %d %s", w.Code, w.Body)
	}
	var renamed terminalInfo
	json.Unmarshal(w.Body.Bytes(), &renamed)
	if renamed.Name != "Build & test <local>" || renamed.ID != first.ID {
		t.Fatal("rename changed identity or lost literal text")
	}
	w = terminalPOST(s, "rename", terminalRequest{ID: first.ID, Token: "wrong", Name: "unauthorized"})
	if w.Code != 403 {
		t.Fatal("rename accepted a foreign capability")
	}
	w = terminalPOST(s, "rename", terminalRequest{ID: second.ID, Token: second.Token, Name: strings.Repeat("a", 121)})
	if w.Code != 400 {
		t.Fatal("oversized name accepted")
	}
	// A fresh browser restores both shells, their creation order and their own names.
	w = terminalPOST(s, "list", map[string]any{})
	var listing struct {
		Sessions []terminalInfo `json:"sessions"`
	}
	json.Unmarshal(w.Body.Bytes(), &listing)
	if len(listing.Sessions) != 2 || listing.Sessions[0].ID != first.ID || listing.Sessions[0].Name != renamed.Name || listing.Sessions[1].ID != second.ID || listing.Sessions[1].Name != "" {
		t.Fatal("reconnect lost distinct names or session order")
	}
	// Each shell has its own environment even though the checkout is the same.
	terminalPOST(s, "input", terminalRequest{ID: first.ID, Token: first.Token, Data: []byte("export PX0_DISTINCT_SHELL=first\nprintf 'first:%s\\n' \"$PX0_DISTINCT_SHELL\"\n")})
	terminalWaitOutput(t, s.terminals.sessions[first.ID], "first:first")
	terminalPOST(s, "input", terminalRequest{ID: second.ID, Token: second.Token, Data: []byte("printf 'second:%s\\n' \"${PX0_DISTINCT_SHELL:-separate}\"\n")})
	terminalWaitOutput(t, s.terminals.sessions[second.ID], "second:separate")
	// Returning to a worktree reuses a shell; resetting its name never recreates it.
	w = terminalPOST(s, "open", map[string]any{})
	var reopened terminalInfo
	json.Unmarshal(w.Body.Bytes(), &reopened)
	if reopened.ID != first.ID {
		t.Fatal("reopen created an extra shell")
	}
	w = terminalPOST(s, "rename", terminalRequest{ID: first.ID, Token: first.Token, Name: ""})
	json.Unmarshal(w.Body.Bytes(), &renamed)
	if w.Code != 200 || renamed.Name != "" || renamed.ID != first.ID {
		t.Fatal("default-name reset replaced the session")
	}
	terminalPOST(s, "close", terminalRequest{ID: second.ID, Token: second.Token})
	terminalPOST(s, "input", terminalRequest{ID: first.ID, Token: first.Token, Data: []byte("printf 'remaining:%s\\n' \"$PX0_DISTINCT_SHELL\"\n")})
	terminalWaitOutput(t, s.terminals.sessions[first.ID], "remaining:first")
}
