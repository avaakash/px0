package main

import (
	"net/http"
	"path/filepath"
	"sort"
	"strings"
)

// Mutations always carry their own checkout; never fall back to the editor root.
func (s *Server) handleWorktreeStage(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) {
		return
	}
	var body struct {
		Worktree string `json:"worktree"`
		Path     string `json:"path"`
		From     string `json:"from"`
		Group    string `json:"group"`
		All      bool   `json:"all"`
	}
	if err := readJSONBody(r, &body); err != nil || body.Worktree == "" {
		fail(w, 400, "select a worktree and changed file")
		return
	}
	unstage := strings.HasSuffix(r.URL.Path, "/unstage")
	if (unstage && body.Group != "staged") || (!unstage && body.Group != "unstaged" && body.Group != "conflicts") {
		fail(w, 400, "invalid change group for this action")
		return
	}
	root, ok := s.selectRegisteredWorktree(w, r, body.Worktree, "")
	if !ok {
		return
	}
	validPath := func(path string) bool {
		clean := filepath.ToSlash(filepath.Clean(path))
		return clean != "." && clean != ".git" && !strings.HasPrefix(clean, ".git/") && worktreeReviewPath(root, path)
	}
	if !body.All && (!validPath(body.Path) || body.From != "" && !validPath(body.From)) {
		fail(w, 403, "file path leaves the selected worktree")
		return
	}
	s.worktreeWriteMu.Lock()
	defer s.worktreeWriteMu.Unlock()
	paths := []string{}
	if !body.All {
		paths = append(paths, body.Path)
		if body.From != "" {
			paths = append(paths, body.From)
		}
	}
	files, err := readWorktreeChanges(r.Context(), root, paths...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	selected := map[string]bool{}
	for _, f := range files {
		if f.Group != body.Group || !body.All && f.Path != body.Path {
			continue
		}
		for _, path := range []string{f.Path, f.From} {
			if path == "" {
				continue
			}
			if !validPath(path) {
				fail(w, 403, "file path leaves the selected worktree")
				return
			}
			selected[path] = true
		}
	}
	if len(selected) == 0 {
		fail(w, 409, "these files are no longer changed in this group; refresh and try again")
		return
	}
	paths = paths[:0]
	for path := range selected {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	args := []string{"add", "-A", "--"}
	if unstage {
		if _, headErr := worktreeGit(r.Context(), root, false, "rev-parse", "--verify", "--quiet", "HEAD"); headErr == nil {
			args = []string{"restore", "--staged", "--source=HEAD", "--"}
		} else {
			args = []string{"rm", "-r", "--cached", "--ignore-unmatch", "--"}
		}
	}
	if _, err := worktreeGit(r.Context(), root, false, append(args, paths...)...); err != nil {
		fail(w, 409, err.Error())
		return
	}
	var refreshPaths []string
	if !body.All {
		refreshPaths = paths
	}
	files, err = readWorktreeChanges(r.Context(), root, refreshPaths...)
	if err != nil {
		fail(w, 500, "Git action completed, but refreshing changes failed: "+err.Error())
		return
	}
	if worktreePath(s.ix.Root()) == worktreePath(root) && s.gitWatcher != nil {
		s.gitWatcher.Trigger()
	}
	writeJSON(w, map[string]any{"worktree": root, "files": files, "partial": !body.All, "paths": paths})
}
