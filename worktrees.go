package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Git is the source of truth, rather than the file index: linked checkouts may
// live in ignored/hidden directories, outside the repo, or have quoted names.
type gitWorktree struct {
	Repository string `json:"repository,omitempty"`
	Path       string `json:"path"`
	Head       string `json:"head"`
	Branch     string `json:"branch"`
	Current    bool   `json:"current"`
	Detached   bool   `json:"detached"`
	Bare       bool   `json:"bare"`
	Locked     bool   `json:"locked"`
	Prunable   bool   `json:"prunable"`
	Available  bool   `json:"available"`
}

type worktreeRepository struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Current bool   `json:"current"`
}

// Only repositories already opened in px0 are offered; never scan the home
// directory recursively or accept an arbitrary repository supplied by a GET.
func (s *Server) worktreeRepositoryCandidates() []string {
	paths := []string{s.ix.Root()}
	for _, it := range (&wbStore{path: workbenchStorePath()}).Load().Items {
		if it.Kind == "dir" && it.Path != "" {
			paths = append(paths, it.Path)
		}
		if len(paths) >= 100 {
			break
		}
	}
	return paths
}

type worktreeInventory struct {
	Repositories []worktreeRepository
	Worktrees    []gitWorktree
	Current      string
}

type worktreeInventoryCache struct {
	mu      sync.Mutex
	data    *worktreeInventory
	at      time.Time
	loading chan struct{}
}

func (s *Server) cachedWorktreeInventory(ctx context.Context, force bool) (*worktreeInventory, error) {
	c := &s.worktreeInventory
	for {
		c.mu.Lock()
		if c.data != nil && !force && time.Since(c.at) < 15*time.Second {
			data := c.data
			c.mu.Unlock()
			return data, nil
		}
		if ready := c.loading; ready != nil {
			c.mu.Unlock()
			select {
			case <-ready:
				force = false
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		c.loading = make(chan struct{})
		ready := c.loading
		c.mu.Unlock()
		data, err := s.buildWorktreeInventory(ctx)
		c.mu.Lock()
		if err == nil {
			c.data = data
			c.at = time.Now()
		}
		c.loading = nil
		close(ready)
		c.mu.Unlock()
		return data, err
	}
}

func (s *Server) buildWorktreeInventory(ctx context.Context) (*worktreeInventory, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out := &worktreeInventory{Repositories: []worktreeRepository{}, Worktrees: []gitWorktree{}}
	current := gitProbe(s.ix.Root())
	out.Current = current.toplevel
	seen := map[string]bool{}
	for _, path := range s.worktreeRepositoryCandidates() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		info := current
		if path != s.ix.Root() {
			info = gitProbe(path)
		}
		if !info.ok {
			continue
		}
		common := info.gitdir
		if b, err := os.ReadFile(filepath.Join(common, "commondir")); err == nil {
			value := strings.TrimSpace(string(b))
			if filepath.IsAbs(value) {
				common = value
			} else {
				common = filepath.Join(common, value)
			}
		}
		identity := worktreePath(common)
		if seen[identity] {
			continue
		}
		seen[identity] = true
		items, err := listGitWorktrees(ctx, info.toplevel)
		if err != nil || len(items) == 0 {
			continue
		}
		available := false
		for _, wt := range items {
			available = available || wt.Available
		}
		if !available {
			continue
		}
		out.Repositories = append(out.Repositories, worktreeRepository{Path: info.toplevel, Name: filepath.Base(items[0].Path), Current: info.toplevel == current.toplevel})
		for _, wt := range items {
			wt.Repository = info.toplevel
			out.Worktrees = append(out.Worktrees, wt)
		}
	}
	return out, nil
}

func worktreePath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}

func listGitWorktrees(ctx context.Context, root string) ([]gitWorktree, error) {
	out := []gitWorktree{}
	if gitDisabled || !gitAvailable(root) {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "worktree", "list", "--porcelain", "-z")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list git worktrees: %w", err)
	}
	current := worktreePath(gitProbe(root).toplevel)
	var wt *gitWorktree
	for _, field := range strings.Split(string(b), "\x00") {
		key, value, _ := strings.Cut(field, " ")
		switch key {
		case "worktree":
			out = append(out, gitWorktree{Path: value, Current: worktreePath(value) == current})
			wt = &out[len(out)-1]
		case "HEAD":
			if wt != nil {
				wt.Head = value
			}
		case "branch":
			if wt != nil {
				wt.Branch = strings.TrimPrefix(value, "refs/heads/")
			}
		case "detached":
			if wt != nil {
				wt.Detached = true
			}
		case "bare":
			if wt != nil {
				wt.Bare = true
			}
		case "locked":
			if wt != nil {
				wt.Locked = true
			}
		case "prunable":
			if wt != nil {
				wt.Prunable = true
			}
		}
	}
	for i := range out {
		st, err := os.Stat(out[i].Path)
		out[i].Available = err == nil && st.IsDir() && !out[i].Bare && !out[i].Prunable
	}
	return out, nil
}

func (s *Server) handleWorktrees(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if !trustedGet(w, r) {
		return
	}
	inventory, err := s.cachedWorktreeInventory(r.Context(), r.URL.Query().Get("refresh") == "1")
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	repository := r.URL.Query().Get("repository")
	if repository == "" {
		repository = inventory.Current
		if repository == "" && len(inventory.Repositories) == 1 {
			repository = inventory.Repositories[0].Path
		}
	}
	known := repository == ""
	for _, repo := range inventory.Repositories {
		if repo.Path == repository {
			known = true
			break
		}
	}
	if !known {
		fail(w, 400, "choose a Git repository already opened in px0")
		return
	}
	items := []gitWorktree{}
	for _, wt := range inventory.Worktrees {
		if r.URL.Query().Get("all") == "1" || wt.Repository == repository {
			items = append(items, wt)
		}
	}
	reason := ""
	if len(inventory.Repositories) == 0 {
		reason = "The open folder is not a Git repository. Open a project with px0 /path/to/repository, then return to Changes."
	}
	if gitDisabled {
		reason = "Git awareness is disabled. Restart px0 without -no-git."
	}
	writeJSON(w, map[string]any{"worktrees": items, "repositories": inventory.Repositories, "repository": repository, "reason": reason})
}

func (s *Server) handleOpenWorktree(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) {
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := readJSONBody(r, &body); err != nil || body.Path == "" {
		fail(w, http.StatusBadRequest, "worktree path is required")
		return
	}
	items, err := listGitWorktrees(r.Context(), s.ix.Root())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, wt := range items {
		if worktreePath(body.Path) != worktreePath(wt.Path) {
			continue
		}
		if !wt.Available {
			fail(w, http.StatusBadRequest, "worktree is missing, prunable, or bare")
			return
		}
		if wt.Current {
			writeJSON(w, map[string]any{"href": s.BasePath()})
			return
		}
		it := wbDirItem(wt.Path)
		parent := s.workbenchURL
		if parent == "" {
			parent = s.worktreeHostURL
		}
		if parent == "" {
			if _, err := s.worktreeWorkbench(); err != nil {
				fail(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			parent = s.BasePath()
		}
		if err := (&wbStore{path: workbenchStorePath()}).Record(it); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Navigation starts/reuses the child. Merely opening the picker, or
		// cancelling a browser's unsaved-edits prompt, never starts a process.
		writeJSON(w, map[string]any{"href": parent + "w/" + it.ID + "/"})
		return
	}
	fail(w, http.StatusBadRequest, "path is not a registered worktree of this repository")
}

// A standalone viewer reuses the workbench's child launcher/proxy, but exposes
// only registered worktrees, never its unrestricted disk browser or open API.
func (s *Server) worktreeWorkbench() (*workbench, error) {
	s.worktreeMu.Lock()
	defer s.worktreeMu.Unlock()
	if s.worktreeClosed {
		return nil, fmt.Errorf("workspace is shutting down")
	}
	if s.worktreeWB == nil {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		args := append(append([]string{}, s.worktreeArgs...), "-worktree-host")
		s.worktreeWB = newWorkbench(s.BasePath(), s.ix.Root(), exe, args)
	}
	return s.worktreeWB, nil
}

func (s *Server) handleWorktreeWorkspace(w http.ResponseWriter, r *http.Request) {
	if !trustedGet(w, r) {
		return
	}
	// A child is already proxied by its parent. Do not allow nested launchers.
	if s.workbenchURL != "" || s.worktreeHostURL != "" {
		http.NotFound(w, r)
		return
	}
	id, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, s.routePath("/w/")), "/")
	s.worktreeMu.Lock()
	wb := s.worktreeWB
	s.worktreeMu.Unlock()
	known := false
	if wb != nil {
		wb.mu.Lock()
		known = wb.children[id] != nil
		wb.mu.Unlock()
	}
	if !known {
		items, err := listGitWorktrees(r.Context(), s.ix.Root())
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, wt := range items {
			it := wbDirItem(wt.Path)
			if it.ID != id || !wt.Available {
				continue
			}
			if wt.Current {
				http.Redirect(w, r, s.BasePath(), http.StatusFound)
				return
			}
			if err := (&wbStore{path: workbenchStorePath()}).Record(it); err != nil {
				fail(w, http.StatusInternalServerError, err.Error())
				return
			}
			known = true
			break
		}
	}
	if !known {
		http.NotFound(w, r)
		return
	}
	var err error
	if wb, err = s.worktreeWorkbench(); err != nil {
		fail(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	wb.handleWorkspace(w, r)
}

func (s *Server) CloseWorktrees() {
	s.worktreeMu.Lock()
	s.worktreeClosed = true
	wb := s.worktreeWB
	s.worktreeMu.Unlock()
	if wb != nil {
		wb.Close()
	}
}
