package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"px0/internal/render"
)

type worktreeChange struct {
	Path   string `json:"path"`
	From   string `json:"from,omitempty"`
	Status string `json:"status"`
	Group  string `json:"group"` // staged, unstaged, or conflicts
}

var errWorktreeOutputLimit = errors.New("change output exceeds the 2 MiB review limit")

type worktreeOutput struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *worktreeOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > b.limit {
		b.exceeded = true
		return 0, errWorktreeOutputLimit
	}
	return b.buffer.Write(p)
}

// No index, language server, child process, or workspace mutation is needed to
// review another checkout. Bound all Git output/time and disable diff drivers.
func worktreeGit(ctx context.Context, root string, diffExit bool, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"--literal-pathspecs", "-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	cmd.WaitDelay = 250 * time.Millisecond
	out := &worktreeOutput{limit: 2 << 20}
	stderr := &worktreeOutput{limit: 8192}
	cmd.Stdout, cmd.Stderr = out, stderr
	err := cmd.Run()
	if out.exceeded {
		return nil, errWorktreeOutputLimit
	}
	var exit *exec.ExitError
	if err != nil && !(diffExit && errors.As(err, &exit) && exit.ExitCode() == 1 && ctx.Err() == nil) {
		return nil, fmt.Errorf("read worktree changes: %w: %s", err, strings.TrimSpace(stderr.buffer.String()))
	}
	return out.buffer.Bytes(), nil
}

func readWorktreeChanges(ctx context.Context, root string, paths ...string) ([]worktreeChange, error) {
	args := []string{"status", "--porcelain=v2", "-z", "--untracked-files=all"}
	if len(paths) > 0 {
		args = append(args, "--")
		args = append(args, paths...)
	}
	b, err := worktreeGit(ctx, root, false, args...)
	if err != nil {
		return nil, err
	}
	files := []worktreeChange{}
	fields := strings.Split(string(b), "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 2 {
			continue
		}
		if f[0] == '?' {
			files = append(files, worktreeChange{Path: f[2:], Status: "U", Group: "unstaged"})
			continue
		}
		count := 9
		if f[0] == '2' {
			count = 10
		}
		if f[0] == 'u' {
			count = 11
		}
		if f[0] != '1' && f[0] != '2' && f[0] != 'u' {
			continue
		}
		p := strings.SplitN(f, " ", count)
		if len(p) != count || len(p[1]) != 2 {
			continue
		}
		path, from := p[count-1], ""
		if f[0] == '2' && i+1 < len(fields) {
			i++
			from = fields[i]
		}
		if f[0] == 'u' {
			files = append(files, worktreeChange{Path: path, Status: "!", Group: "conflicts"})
			continue
		}
		for k, group := range []string{"staged", "unstaged"} {
			if p[1][k] == '.' {
				continue
			}
			change := worktreeChange{Path: path, Status: string(p[1][k]), Group: group}
			if change.Status == "R" || change.Status == "C" {
				change.From = from
			}
			files = append(files, change)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Group != files[j].Group {
			return files[i].Group < files[j].Group
		}
		return files[i].Path < files[j].Path
	})
	return files, nil
}

func (s *Server) reviewWorktree(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Method != http.MethodGet {
		fail(w, 405, "GET only")
		return "", false
	}
	if !trustedGet(w, r) {
		return "", false
	}
	return s.selectRegisteredWorktree(w, r, r.URL.Query().Get("worktree"), r.URL.Query().Get("repository"))
}

func (s *Server) selectRegisteredWorktree(w http.ResponseWriter, r *http.Request, path, repository string) (string, bool) {
	inventory, err := s.cachedWorktreeInventory(r.Context(), false)
	if err != nil {
		fail(w, 500, err.Error())
		return "", false
	}
	if repository != "" {
		known := false
		for _, repo := range inventory.Repositories {
			if repo.Path == repository {
				known = true
				break
			}
		}
		if !known {
			fail(w, 400, "choose a Git repository already opened in px0")
			return "", false
		}
	}
	canonical := path
	if path != "" {
		canonical = worktreePath(path)
	}
	for _, wt := range inventory.Worktrees {
		if wt.Available && (repository == "" || wt.Repository == repository) && (path != "" && wt.Path == canonical || path == "" && wt.Current && (wt.Repository == inventory.Current || len(inventory.Repositories) == 1)) {
			return wt.Path, true
		}
	}
	fail(w, 400, "select an available registered worktree of this repository")
	return "", false
}

func (s *Server) handleWorktreeChanges(w http.ResponseWriter, r *http.Request) {
	root, ok := s.reviewWorktree(w, r)
	if !ok {
		return
	}
	files, err := readWorktreeChanges(r.Context(), root)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"worktree": root, "files": files})
}

// Check the nearest existing ancestor too: a deleted file can have no current
// path, and a replaced parent symlink must not expose another directory.
func worktreeReviewPath(root, rel string) bool {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if rel == "" || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	for abs := filepath.Join(root, clean); ; abs = filepath.Dir(abs) {
		real, err := filepath.EvalSymlinks(abs)
		if err == nil {
			within, err := filepath.Rel(worktreePath(root), real)
			return err == nil && within != ".." && !strings.HasPrefix(within, ".."+string(filepath.Separator))
		}
		if !os.IsNotExist(err) || abs == filepath.Dir(abs) {
			return false
		}
	}
}

func (s *Server) handleWorktreeDiff(w http.ResponseWriter, r *http.Request) {
	root, ok := s.reviewWorktree(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if !worktreeReviewPath(root, q.Get("path")) || q.Get("from") != "" && !worktreeReviewPath(root, q.Get("from")) {
		fail(w, 403, "file path leaves the selected worktree")
		return
	}
	paths := []string{q.Get("path")}
	if q.Get("from") != "" {
		paths = append(paths, q.Get("from"))
	}
	files, err := readWorktreeChanges(r.Context(), root, paths...)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	var selected *worktreeChange
	for i := range files {
		if files[i].Path == q.Get("path") && files[i].Group == q.Get("group") {
			selected = &files[i]
			break
		}
	}
	if selected == nil {
		fail(w, 404, "file is no longer changed in this group")
		return
	}
	if !worktreeReviewPath(root, selected.Path) || selected.From != "" && !worktreeReviewPath(root, selected.From) {
		fail(w, 403, "file path leaves the selected worktree")
		return
	}
	args := []string{"diff", "--no-color", "--no-ext-diff", "--no-textconv"}
	if selected.Status == "U" {
		args = append(args, "--no-index", "--", os.DevNull, selected.Path)
	} else {
		if selected.Group == "staged" {
			args = append(args, "--cached")
		}
		args = append(args, "--", selected.Path)
		if selected.From != "" {
			args = append(args, selected.From)
		}
	}
	b, err := worktreeGit(r.Context(), root, selected.Status == "U", args...)
	if err != nil {
		status := 500
		if errors.Is(err, errWorktreeOutputLimit) {
			status = 413
		}
		fail(w, status, err.Error())
		return
	}
	diff := string(b)
	writeJSON(w, map[string]any{"worktree": root, "path": selected.Path, "group": selected.Group, "status": selected.Status, "diff": diff, "hunks": render.HighlightDiff(selected.Path, diff)})
}
