package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stageWorktreePOST(s *Server, action, root, path, from, group string, all bool) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]any{"worktree": root, "path": path, "from": from, "group": group, "all": all})
	r := httptest.NewRequest("POST", "http://localhost/api/git/worktrees/"+action, bytes.NewReader(b))
	r.Header.Set("Origin", "http://localhost")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func TestWorktreeStageUnstageIsolatedAndLiteral(t *testing.T) {
	newTestStore(t)
	root := gitRepo(t)
	linked := filepath.Join(root, ".claude", "worktrees", "stage")
	addTestWorktree(t, root, linked, "-b", "stage-test")
	s := NewServer(NewIndex(root), nil)
	s.gitWatcher.Stop()
	before := gitTestRun(t, root, "status", "--porcelain=v2")
	literal := ":(glob)*.txt"
	for _, path := range []string{"keep.go", literal, "other.txt"} {
		if err := os.WriteFile(filepath.Join(linked, path), []byte("new content\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	request := func(action, path, from, group string, all bool) {
		t.Helper()
		w := stageWorktreePOST(s, action, linked, path, from, group, all)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", action, path, w.Code, w.Body)
		}
	}
	request("stage", literal, "", "unstaged", false)
	staged := gitTestRun(t, linked, "diff", "--cached", "--name-only")
	if !strings.Contains(staged, literal) || strings.Contains(staged, "other.txt") {
		t.Fatalf("literal path affected wrong files: %s", staged)
	}
	request("unstage", literal, "", "staged", false)
	if gitTestRun(t, linked, "diff", "--cached", "--name-only") != "" {
		t.Fatal("unstage left staged files")
	}
	request("stage", "keep.go", "", "unstaged", false)
	os.WriteFile(filepath.Join(linked, "keep.go"), []byte("partial content\n"), 0600)
	request("unstage", "keep.go", "", "staged", false)
	if got, _ := os.ReadFile(filepath.Join(linked, "keep.go")); string(got) != "partial content\n" {
		t.Fatal("unstage changed working file")
	}
	request("stage", "", "", "unstaged", true)
	request("unstage", "", "", "staged", true)
	gitTestRun(t, linked, "mv", "sub/ren.go", "sub/renamed.go")
	request("unstage", "sub/renamed.go", "sub/ren.go", "staged", false)
	if gitTestRun(t, linked, "diff", "--cached", "--name-only") != "" {
		t.Fatal("rename source remained staged")
	}
	if got := gitTestRun(t, root, "status", "--porcelain=v2"); got != before {
		t.Fatal("stage modified original checkout")
	}
	for _, path := range []string{"../keep.go", "/etc/passwd", ".git", "."} {
		if w := stageWorktreePOST(s, "stage", linked, path, "", "unstaged", false); w.Code != 403 {
			t.Fatalf("unsafe path %q accepted: %d", path, w.Code)
		}
	}
	if w := stageWorktreePOST(s, "stage", t.TempDir(), "keep.go", "", "unstaged", false); w.Code != 400 {
		t.Fatal("unrelated checkout accepted")
	}
}
func TestWorktreeUnstageUnbornRepository(t *testing.T) {
	newTestStore(t)
	root := t.TempDir()
	gitTestRun(t, root, "init", "-b", "main")
	os.WriteFile(filepath.Join(root, "new.txt"), []byte("keep me\n"), 0600)
	gitTestRun(t, root, "add", "new.txt")
	s := NewServer(NewIndex(root), nil)
	s.gitWatcher.Stop()
	w := stageWorktreePOST(s, "unstage", root, "new.txt", "", "staged", false)
	if w.Code != 200 {
		t.Fatalf("unstage unborn: %d %s", w.Code, w.Body)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "new.txt")); string(data) != "keep me\n" {
		t.Fatal("unstage removed file")
	}
	if gitTestRun(t, root, "ls-files") != "" {
		t.Fatal("unborn index not cleared")
	}
}
