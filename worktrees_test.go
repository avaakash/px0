package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func BenchmarkWorktreeDiscovery(b *testing.B) {
	root := gitRepo(b)
	b.ResetTimer()
	for b.Loop() {
		if _, err := listGitWorktrees(context.Background(), root); err != nil {
			b.Fatal(err)
		}
	}
}

// Exercise discovery through HTTP, with the same ignored, nested checkout
// layout Claude Code creates. File search must remain scoped to its checkout.
func TestWorktreesDiscoverIgnoredClaudeCheckout(t *testing.T) {
	root := gitRepo(t)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".claude/worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(root, ".claude", "worktrees", "feature with spaces")
	cmd := exec.Command("git", "-C", root, "worktree", "add", "-b", "claude/feature", linked, "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add worktree: %v\n%s", err, out)
	}
	for _, opened := range []string{root, linked} {
		t.Run(filepath.Base(opened), func(t *testing.T) {
			ix := NewIndex(opened)
			ix.Build()
			s := NewServer(ix, nil)
			t.Cleanup(s.gitWatcher.Stop)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost/api/git/worktrees", nil))
			var body struct {
				Worktrees []struct {
					Path    string `json:"path"`
					Branch  string `json:"branch"`
					Current bool   `json:"current"`
				} `json:"worktrees"`
			}
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
				t.Fatalf("worktree picker cannot discover checkouts: status %d, body %.200s", rec.Code, rec.Body.String())
			}
			if len(body.Worktrees) != 2 {
				t.Fatalf("got %d worktrees, want main plus ignored Claude checkout: %+v", len(body.Worktrees), body)
			}
			found := false
			for _, wt := range body.Worktrees {
				if wt.Current != (wt.Path == opened) {
					t.Errorf("wrong current marker for %+v (opened %s)", wt, opened)
				}
				if wt.Path == linked {
					found = true
					if wt.Branch != "claude/feature" {
						t.Errorf("branch = %q", wt.Branch)
					}
				}
			}
			if !found {
				t.Fatal("ignored Claude checkout is missing")
			}
		})
	}
}

func addTestWorktree(t *testing.T, root, path string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root, "worktree", "add"}, append(args, path, "HEAD")...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add worktree: %v\n%s", err, out)
	}
}

func TestWorktreesSpecialPathsAndMissingCheckout(t *testing.T) {
	root := gitRepo(t)
	linked := filepath.Join(t.TempDir(), "unicode-λ\tquote\"\ncheckout")
	addTestWorktree(t, root, linked, "--detach")
	cmd := exec.Command("git", "-C", root, "worktree", "lock", "--reason", "keep it", linked)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("lock: %v\n%s", err, out)
	}
	items, err := listGitWorktrees(t.Context(), linked)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[1].Path != worktreePath(linked) || !items[1].Detached || !items[1].Locked || !items[1].Current || !items[1].Available {
		t.Fatalf("special worktree not preserved: %+v", items)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	addTestWorktree(t, root, missing, "--detach")
	if err := os.RemoveAll(missing); err != nil {
		t.Fatal(err)
	}
	items, err = listGitWorktrees(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Path == worktreePath(missing) && (!item.Prunable || item.Available) {
			t.Errorf("missing worktree should be unavailable: %+v", item)
		}
	}
	items, err = listGitWorktrees(t.Context(), filepath.Join(root, "sub"))
	if err != nil || !items[0].Current {
		t.Fatalf("subdirectory current checkout: %+v, %v", items, err)
	}
	if gitCurrentBranch(root) == "HEAD" {
		t.Fatal("discovery changed the main checkout")
	}
}

func TestWorktreeOpenScopeAndRequestGuards(t *testing.T) {
	newTestStore(t)
	root := gitRepo(t)
	linked := filepath.Join(root, ".claude", "worktrees", "feature")
	addTestWorktree(t, root, linked, "-b", "claude/feature")
	ix := NewIndex(root)
	ix.Build()
	s := NewServer(ix, nil, "/base/")
	s.workbenchURL = "/hub/"
	t.Cleanup(s.gitWatcher.Stop)
	t.Cleanup(s.CloseWorktrees)
	request := func(method, path, origin, host, target string) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"path": target})
		r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(string(body)))
		r.Host = host
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		name, method, origin, host string
		status                     int
	}{
		{"get", "GET", "http://localhost", "localhost", 405},
		{"cross origin", "POST", "https://example.com", "localhost", 403},
		{"no origin", "POST", "", "localhost", 403},
		{"rebound host", "POST", "http://example.com", "example.com", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request(tc.method, "/base/api/git/worktrees/open", tc.origin, tc.host, linked)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d", w.Code, tc.status)
			}
		})
	}
	w := request("GET", "/base/api/git/worktrees", "", "example.com", "")
	if w.Code != 403 {
		t.Fatalf("discovery allows rebound host: %d", w.Code)
	}
	for _, path := range []string{t.TempDir(), filepath.Join(linked, "sub"), ""} {
		w = request("POST", "/base/api/git/worktrees/open", "http://localhost", "localhost", path)
		if w.Code != 400 {
			t.Fatalf("arbitrary path %q accepted: %d", path, w.Code)
		}
	}
	w = request("POST", "/base/api/git/worktrees/open", "http://localhost", "localhost", linked)
	var result struct {
		Href string `json:"href"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Href != "/hub/w/"+wbDirItem(linked).ID+"/" {
		t.Fatalf("workbench handoff: %d %s", w.Code, w.Body)
	}
	w = request("POST", "/base/api/git/worktrees/open", "http://localhost", "localhost", root)
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Href != "/base/" {
		t.Fatalf("current checkout: %s", w.Body)
	}
	if err := os.RemoveAll(linked); err != nil {
		t.Fatal(err)
	}
	w = request("POST", "/base/api/git/worktrees/open", "http://localhost", "localhost", linked)
	if w.Code != 400 {
		t.Fatalf("missing checkout accepted: %d", w.Code)
	}
}

func TestStandaloneWorktreeProxyRejectsUnrelatedStoredDirectory(t *testing.T) {
	st := newTestStore(t)
	root := gitRepo(t)
	foreign := wbDirItem(t.TempDir())
	if err := st.Record(foreign); err != nil {
		t.Fatal(err)
	}
	ix := NewIndex(root)
	s := NewServer(ix, nil)
	t.Cleanup(s.gitWatcher.Stop)
	t.Cleanup(s.CloseWorktrees)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/w/"+foreign.ID+"/", nil))
	if w.Code != 404 || s.worktreeWB != nil {
		t.Fatalf("unrelated workspace exposed: %d", w.Code)
	}
	for _, path := range []string{"/api/wb/browse", "/api/wb/open", "/api/wb/state"} {
		w = httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost"+path, nil))
		if w.Code != 404 {
			t.Errorf("standalone viewer exposes %s: %d", path, w.Code)
		}
	}
}

func TestWorktreeSwitchThroughRealChild(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the px0 binary")
	}
	newTestStore(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	exe := filepath.Join(t.TempDir(), "px0")
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	root := gitRepo(t)
	linked := filepath.Join(root, ".claude", "worktrees", "feature with spaces")
	addTestWorktree(t, root, linked, "-b", "claude/feature")
	if err := os.WriteFile(filepath.Join(linked, "keep.go"), []byte("linked checkout only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"standalone", "workbench"} {
		t.Run(mode, func(t *testing.T) {
			args := []string{"-no-lsp", "-no-agent", "-no-telemetry"}
			var handler http.Handler
			var cleanup func()
			base := "/base/"
			if mode == "standalone" {
				ix := NewIndex(root)
				ix.Build()
				s := NewServer(ix, nil, base)
				s.worktreeWB = newWorkbench(base, root, exe, append(args, "-worktree-host"))
				handler = s
				cleanup = func() { s.CloseWorktrees(); s.gitWatcher.Stop() }
			} else {
				wb := newWorkbench(base, root, exe, args)
				handler = wb
				cleanup = wb.Close
			}
			srv := httptest.NewServer(handler)
			defer srv.Close()
			defer cleanup()
			post := func(path string, body any) map[string]any {
				t.Helper()
				b, _ := json.Marshal(body)
				r, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(string(b)))
				r.Header.Set("Origin", srv.URL)
				resp, err := http.DefaultClient.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				var result map[string]any
				if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || resp.StatusCode != 200 {
					t.Fatalf("post %s: status %d, %v, %v", path, resp.StatusCode, result, err)
				}
				return result
			}
			waitReady := func(href string) map[string]any {
				t.Helper()
				resp, err := http.Get(srv.URL + href)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				deadline := time.Now().Add(20 * time.Second)
				lastStatus, lastBody := 0, ""
				for time.Now().Before(deadline) {
					resp, err := http.Get(srv.URL + href + "api/meta")
					if err != nil {
						t.Fatal(err)
					}
					var meta map[string]any
					b, _ := io.ReadAll(resp.Body)
					json.Unmarshal(b, &meta)
					lastStatus, lastBody = resp.StatusCode, string(b)
					resp.Body.Close()
					if resp.StatusCode == 200 && meta["ready"] == true {
						return meta
					}
					time.Sleep(50 * time.Millisecond)
				}
				t.Fatalf("worktree child did not become ready: status %d, body %.500s", lastStatus, lastBody)
				return nil
			}
			if mode == "workbench" {
				opened := post(base+"api/wb/open", map[string]string{"target": root})
				base = opened["href"].(string)
				waitReady(base)
			}
			opened := post(base+"api/git/worktrees/open", map[string]string{"path": linked})
			href := opened["href"].(string)
			meta := waitReady(href)
			if meta["root"] != linked {
				t.Fatalf("wrong checkout: %v", meta["root"])
			}
			if mode == "standalone" && (meta["worktreeHost"] != "/base/" || meta["workbench"] != "") {
				t.Fatalf("wrong standalone parent: %v %v", meta["worktreeHost"], meta["workbench"])
			}
			resp, err := http.Get(srv.URL + href + "api/raw?path=keep.go")
			if err != nil {
				t.Fatal(err)
			}
			content, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(content) != "linked checkout only\n" {
				t.Fatalf("file came from wrong checkout: %q", content)
			}
			back := post(href+"api/git/worktrees/open", map[string]string{"path": root})
			backHref := back["href"].(string)
			if mode == "standalone" {
				client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
				resp, err := client.Get(srv.URL + backHref)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != 302 || resp.Header.Get("Location") != "/base/" {
					t.Fatalf("return to original: %d %s", resp.StatusCode, resp.Header.Get("Location"))
				}
			} else if backHref != base {
				t.Fatalf("return to main: %s, want %s", backHref, base)
			}
		})
	}
}
