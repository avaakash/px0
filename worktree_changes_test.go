package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeDiffOnlyScansRequestedFile(t *testing.T) {
	newTestStore(t)
	root := gitRepo(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	trace := filepath.Join(t.TempDir(), "git-trace")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PX0_TEST_GIT_TRACE\"\nexec \"$PX0_TEST_REAL_GIT\" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PX0_TEST_GIT_TRACE", trace)
	t.Setenv("PX0_TEST_REAL_GIT", realGit)
	ix := NewIndex(root)
	s := NewServer(ix, nil)
	t.Cleanup(s.gitWatcher.Stop)
	s.gitWatcher.Stop()
	if err := os.WriteFile(trace, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	q := url.Values{"worktree": {root}, "path": {"sub/mod.go"}, "group": {"unstaged"}}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/git/worktrees/diff?"+q.Encode(), nil))
	if w.Code != 200 {
		t.Fatalf("diff: %d %s", w.Code, w.Body)
	}
	b, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, " status ") && !strings.Contains(line, "-- sub/mod.go") {
			t.Fatalf("opening one diff scans the whole checkout: %s", line)
		}
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/git/worktrees?all=1", nil))
	after, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || string(after) != string(b) {
		t.Fatal("warm worktree inventory reruns Git instead of reusing the metadata cache")
	}
}

func TestWorktreeChangesReviewWithoutWorkspaceSwitch(t *testing.T) {
	root := gitRepo(t)
	linked := filepath.Join(root, ".claude", "worktrees", "review")
	addTestWorktree(t, root, linked, "-b", "claude/review")
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(linked, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("keep.go", "staged in selected worktree\n")
	gitTestRun(t, linked, "add", "keep.go")
	write("keep.go", "unstaged in selected worktree\n")
	write("new file.txt", "untracked in selected worktree\n")
	ix := NewIndex(root)
	ix.Build()
	s := NewServer(ix, nil)
	t.Cleanup(s.gitWatcher.Stop)
	request := func(endpoint string, params url.Values) (int, map[string]any) {
		t.Helper()
		params.Set("worktree", linked)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/git/worktrees/"+endpoint+"?"+params.Encode(), nil))
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatalf("%s cannot review without switching: %d %.200s", endpoint, w.Code, w.Body.String())
		}
		return w.Code, result
	}
	code, result := request("changes", url.Values{})
	if code != 200 {
		t.Fatalf("changes: %d %v", code, result)
	}
	files, ok := result["files"].([]any)
	if !ok || len(files) != 3 {
		t.Fatalf("want staged, unstaged, and untracked rows: %v", result)
	}
	for _, group := range []string{"staged", "unstaged"} {
		code, diff := request("diff", url.Values{"path": {"keep.go"}, "group": {group}})
		if code != 200 || !strings.Contains(diff["diff"].(string), "+"+group+" in selected worktree") {
			t.Fatalf("wrong %s diff: %d %v", group, code, diff)
		}
	}
	code, diff := request("diff", url.Values{"path": {"new file.txt"}, "group": {"unstaged"}})
	if code != 200 || !strings.Contains(diff["diff"].(string), "+untracked in selected worktree") {
		t.Fatalf("untracked diff: %d %v", code, diff)
	}
	if s.ix.Root() != root || s.worktreeWB != nil {
		t.Fatal("review changed the active workspace or started a workspace launcher")
	}
}

func BenchmarkWorktreeChanges(b *testing.B) {
	root := gitRepo(b)
	b.ResetTimer()
	for b.Loop() {
		if _, err := readWorktreeChanges(context.Background(), root); err != nil {
			b.Fatal(err)
		}
	}
}

func TestWorktreeChangesFromNonGitFolderFindsRecentRepository(t *testing.T) {
	store := newTestStore(t)
	repo := gitRepo(t)
	linked := filepath.Join(repo, ".claude", "worktrees", "recent-feature")
	addTestWorktree(t, repo, linked, "-b", "claude/recent")
	for _, path := range []string{repo, linked} {
		if err := store.Record(wbDirItem(path)); err != nil {
			t.Fatal(err)
		}
	}
	workspace := t.TempDir()
	ix := NewIndex(workspace)
	ix.Build()
	s := NewServer(ix, nil)
	t.Cleanup(s.gitWatcher.Stop)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/git/worktrees", nil))
	var result struct {
		Repositories []struct {
			Path string `json:"path"`
		} `json:"repositories"`
		Repository string        `json:"repository"`
		Worktrees  []gitWorktree `json:"worktrees"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(result.Repositories) != 1 || len(result.Worktrees) != 2 {
		t.Fatalf("Changes reports no available worktrees when opened outside Git: %d %s", w.Code, w.Body)
	}
	q := url.Values{"repository": {result.Repository}, "worktree": {linked}}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/git/worktrees/changes?"+q.Encode(), nil))
	if w.Code != 200 {
		t.Fatalf("review selected recent repository: %d %s", w.Code, w.Body)
	}
	q.Set("repository", t.TempDir())
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/git/worktrees/changes?"+q.Encode(), nil))
	if w.Code != 400 {
		t.Fatalf("unopened arbitrary repository allowed: %d", w.Code)
	}
	if s.ix.Root() != workspace || s.worktreeWB != nil {
		t.Fatal("repository review changed the original workspace")
	}
}

func TestWorktreeChangesRenameDeleteAndSafePaths(t *testing.T) {
	root := gitRepo(t)
	linked := filepath.Join(root, ".claude", "worktrees", "review-safe")
	addTestWorktree(t, root, linked, "-b", "claude/safe")
	gitTestRun(t, linked, "mv", "keep.go", "renamed file.go")
	gitTestRun(t, linked, "rm", "del.go")
	literal := ":(glob)*.txt"
	if err := os.WriteFile(filepath.Join(linked, literal), []byte("literal pathspec file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "private.txt"), []byte("outside content must not appear"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "private.txt"), filepath.Join(linked, "external-link")); err != nil {
		t.Fatal(err)
	}
	ix := NewIndex(root)
	ix.Build()
	s := NewServer(ix, nil)
	t.Cleanup(s.gitWatcher.Stop)
	request := func(worktree, path, group string) *httptest.ResponseRecorder {
		q := url.Values{"worktree": {worktree}, "path": {path}, "group": {group}}
		if path == "renamed file.go" {
			q.Set("from", "keep.go")
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/git/worktrees/diff?"+q.Encode(), nil))
		return w
	}
	for _, tc := range []struct{ path, group, want string }{
		{"renamed file.go", "staged", "rename from keep.go"},
		{"del.go", "staged", "deleted file mode"},
		{literal, "unstaged", "+literal pathspec file"},
	} {
		w := request(linked, tc.path, tc.group)
		var result struct{ Diff string }
		json.Unmarshal(w.Body.Bytes(), &result)
		if w.Code != 200 || !strings.Contains(result.Diff, tc.want) {
			t.Errorf("%s: %d %s", tc.path, w.Code, w.Body)
		}
	}
	for _, tc := range []struct {
		worktree, path string
		code           int
	}{
		{outside, "private.txt", 400}, {linked, "../private.txt", 403}, {linked, "external-link", 403},
	} {
		if w := request(tc.worktree, tc.path, "unstaged"); w.Code != tc.code {
			t.Errorf("scope guard: %d %s", w.Code, w.Body)
		}
	}
	if err := os.WriteFile(filepath.Join(linked, "huge.txt"), []byte(strings.Repeat("x", (2<<20)+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := request(linked, "huge.txt", "unstaged"); w.Code != 413 {
		t.Errorf("large diff must be bounded: %d", w.Code)
	}
}
