package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"px0/internal/settings"
)

// This service reads remote PR metadata only. It never checks out, fetches or
// changes a worktree, and does not turn a local checkout into a PR session.
type worktreePRCheck struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	State      string `json:"state"`
	Conclusion string `json:"conclusion"`
	URL        string `json:"url,omitempty"`
	JobID      int64  `json:"-"`
	Rerunnable bool   `json:"rerunnable"`
}

type worktreePRStatus struct {
	State                    string            `json:"state"`
	Reason                   string            `json:"reason,omitempty"`
	Connected                bool              `json:"connected"`
	Repository               string            `json:"repository,omitempty"`
	Number                   int               `json:"number,omitempty"`
	Title                    string            `json:"title,omitempty"`
	URL                      string            `json:"url,omitempty"`
	HeadSHA                  string            `json:"headSHA,omitempty"`
	MergeSHA                 string            `json:"-"`
	Author                   string            `json:"-"`
	Draft                    bool              `json:"draft"`
	ReviewState              string            `json:"reviewState,omitempty"`
	ReviewRequirements       string            `json:"reviewRequirements,omitempty"`
	ReviewRequirementsReason string            `json:"reviewRequirementsReason,omitempty"`
	Requested                []string          `json:"requested"`
	Reviewed                 []string          `json:"reviewed"`
	Approved                 []string          `json:"approved"`
	Changes                  []string          `json:"changesRequested"`
	ChecksState              string            `json:"checksState,omitempty"`
	Checks                   []worktreePRCheck `json:"checks"`
	UpdatedAt                string            `json:"updatedAt"`
}

type worktreePREntry struct {
	loading chan struct{}
	at      time.Time
	value   worktreePRStatus
	err     error
}

type worktreePRService struct {
	mu      sync.Mutex
	entries map[string]*worktreePREntry
	slots   chan struct{}
	authMu  sync.Mutex
	authAt  time.Time
	token   string
	writeMu sync.Mutex
	reruns  map[int64]time.Time
}

// A single bounded gh invocation is shared across worktrees. Explicit settings
// and environment changes take effect immediately, without persisting tokens.
func (p *worktreePRService) auth(ctx context.Context) string {
	cfg := settings.Read()
	if cfg.GitHubToken != nil && strings.TrimSpace(*cfg.GitHubToken) != "" {
		return strings.TrimSpace(*cfg.GitHubToken)
	}
	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(key)); token != "" {
			return token
		}
	}
	p.authMu.Lock()
	defer p.authMu.Unlock()
	if time.Since(p.authAt) < time.Minute {
		return p.token
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	cmd.WaitDelay = 250 * time.Millisecond
	b, err := cmd.Output()
	if ctx.Err() == nil {
		p.authAt, p.token = time.Now(), ""
		if err == nil {
			p.token = strings.TrimSpace(string(b))
		}
	}
	return p.token
}

var worktreeGHRepoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func worktreeGitHubRepo(raw string) string {
	host, path, ok := remoteHostPath(strings.TrimSpace(raw))
	path = strings.TrimSuffix(path, "/")
	if !ok || !strings.EqualFold(host, "github.com") || !worktreeGHRepoRE.MatchString(path) {
		return ""
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return ""
		}
	}
	return path
}

type worktreePRSource struct {
	branch, headRepo string
	repos            []string
	key              string
}

func worktreePRSourceFor(ctx context.Context, root string) (worktreePRSource, error) {
	b, err := worktreeGit(ctx, root, false, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return worktreePRSource{}, nil // detached/unborn checkouts have no PR branch
	}
	branch := strings.TrimSpace(string(b))
	b, err = worktreeGit(ctx, root, true, "config", "--get-regexp", `^(remote\..*\.url|branch\..*\.(remote|merge|pushremote)|remote\.pushdefault)$`)
	if err != nil {
		return worktreePRSource{}, err
	}
	config := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		key, value, ok := strings.Cut(line, " ")
		if ok {
			config[key] = value
		}
	}
	key := "branch." + branch + "."
	remote := config[key+"pushremote"]
	if remote == "" {
		remote = config["remote.pushdefault"]
	}
	if remote == "" {
		remote = config[key+"remote"]
	}
	if remote == "" || remote == "." {
		remote = "origin"
	}
	head := worktreeGitHubRepo(config["remote."+remote+".url"])
	if head == "" {
		return worktreePRSource{}, nil
	}
	if remote == config[key+"remote"] && strings.HasPrefix(config[key+"merge"], "refs/heads/") {
		branch = strings.TrimPrefix(config[key+"merge"], "refs/heads/")
	}
	source := worktreePRSource{branch: branch, headRepo: head}
	for _, name := range []string{"upstream", remote, "origin"} {
		repo := worktreeGitHubRepo(config["remote."+name+".url"])
		if repo != "" {
			found := false
			for _, existing := range source.repos {
				found = found || strings.EqualFold(repo, existing)
			}
			if !found {
				source.repos = append(source.repos, repo)
			}
		}
	}
	source.key = fmt.Sprintf("%x", sha256.Sum256([]byte(root+"\x00"+branch+"\x00"+string(b))))
	return source, nil
}

func (p *worktreePRService) status(ctx context.Context, source worktreePRSource, token string, force bool) (worktreePRStatus, error) {
	key := source.key + fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
	for {
		p.mu.Lock()
		if p.entries == nil {
			p.entries, p.slots = map[string]*worktreePREntry{}, make(chan struct{}, 4)
		}
		entry := p.entries[key]
		if entry != nil && entry.loading != nil {
			ready := entry.loading
			p.mu.Unlock()
			select {
			case <-ready:
				force = false
				continue
			case <-ctx.Done():
				return worktreePRStatus{}, ctx.Err()
			}
		}
		if entry != nil && !force && time.Since(entry.at) < 2*time.Minute {
			value, err := entry.value, entry.err
			p.mu.Unlock()
			return value, err
		}
		if len(p.entries) >= 100 {
			for k, e := range p.entries {
				if e.loading == nil {
					delete(p.entries, k)
				}
			}
		}
		entry = &worktreePREntry{loading: make(chan struct{})}
		p.entries[key] = entry
		slots := p.slots
		p.mu.Unlock()
		var value worktreePRStatus
		var err error
		select {
		case slots <- struct{}{}:
			value, err = fetchWorktreePR(ctx, source, token)
			<-slots
		case <-ctx.Done():
			err = ctx.Err()
		}
		p.mu.Lock()
		entry.value, entry.err, entry.at = value, err, time.Now()
		if ctx.Err() != nil {
			delete(p.entries, key)
		}
		close(entry.loading)
		entry.loading = nil
		p.mu.Unlock()
		return value, err
	}
}

// Use numbered, bounded pages on the fixed GitHub API host, never a server's
// arbitrary pagination URL. Do not silently report green on truncated data.
func worktreePRPages(ctx context.Context, api forgeAPI, path, field string) ([]json.RawMessage, error) {
	var result []json.RawMessage
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; page <= 10; page++ {
		var raw json.RawMessage
		_, err := api.do(ctx, "GET", fmt.Sprintf("%s%sper_page=100&page=%d", path, sep, page), nil, &raw)
		if err != nil {
			return nil, err
		}
		if field != "" {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(raw, &object); err != nil {
				return nil, err
			}
			raw = object[field]
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
		result = append(result, items...)
		if len(items) < 100 {
			return result, nil
		}
	}
	return nil, fmt.Errorf("too many GitHub results; open the PR on GitHub for the complete status")
}

type worktreeGHPull struct {
	Number         int
	Title, State   string
	HTMLURL        string `json:"html_url"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Draft          bool
	User           struct{ Login string }
	Head           struct {
		Ref, SHA string
		Repo     struct {
			FullName string `json:"full_name"`
		}
	}
	RequestedReviewers []struct{ Login string } `json:"requested_reviewers"`
	RequestedTeams     []struct{ Slug string }  `json:"requested_teams"`
}

func fetchWorktreePR(ctx context.Context, source worktreePRSource, token string) (worktreePRStatus, error) {
	status := worktreePRStatus{State: "none", Connected: token != "", Requested: []string{}, Reviewed: []string{}, Approved: []string{}, Changes: []string{}, Checks: []worktreePRCheck{}, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	if source.headRepo == "" {
		status.State, status.Reason = "unsupported", "No GitHub branch"
		return status, nil
	}
	api := githubAPI(token)
	owner, _, _ := strings.Cut(source.headRepo, "/")
	var firstError error
	var match *worktreeGHPull
	for _, repo := range source.repos {
		items, err := worktreePRPages(ctx, api, "/repos/"+repo+"/pulls?state=open&head="+url.QueryEscape(owner+":"+source.branch), "")
		if err != nil {
			if firstError == nil {
				firstError = err
			}
			continue
		}
		for _, item := range items {
			var pr worktreeGHPull
			if err := json.Unmarshal(item, &pr); err != nil {
				return status, err
			}
			if pr.State != "open" || pr.Head.Ref != source.branch || !strings.EqualFold(pr.Head.Repo.FullName, source.headRepo) {
				continue
			}
			if match != nil {
				status.State, status.Reason = "ambiguous", "Multiple open PRs for this branch"
				return status, nil
			}
			copy := pr
			match, status.Repository = &copy, repo
		}
	}
	if match == nil {
		return status, firstError
	}
	pr := match
	status.State, status.Number, status.Title, status.URL = "open", pr.Number, pr.Title, pr.HTMLURL
	status.HeadSHA, status.MergeSHA, status.Author, status.Draft = pr.Head.SHA, pr.MergeCommitSHA, pr.User.Login, pr.Draft
	if status.Number <= 0 || status.HeadSHA == "" {
		return status, fmt.Errorf("GitHub returned an incomplete PR")
	}
	for _, user := range pr.RequestedReviewers {
		status.Requested = append(status.Requested, user.Login)
	}
	for _, team := range pr.RequestedTeams {
		status.Requested = append(status.Requested, "team/"+team.Slug)
	}
	items, err := worktreePRPages(ctx, api, fmt.Sprintf("/repos/%s/pulls/%d/reviews", status.Repository, status.Number), "")
	if err != nil {
		return status, err
	}
	worktreeReviewSummary(&status, items)
	fetchWorktreeApproval(ctx, api, token, &status)
	if err := fetchWorktreePRChecks(ctx, api, &status); err != nil {
		return status, err
	}
	return status, nil
}

func worktreeReviewSummary(status *worktreePRStatus, items []json.RawMessage) {
	opinions, reviewed := map[string]string{}, map[string]bool{}
	for _, raw := range items {
		var review struct {
			State string
			User  struct{ Login string }
		}
		if json.Unmarshal(raw, &review) != nil || review.User.Login == "" || strings.EqualFold(review.User.Login, status.Author) {
			continue
		}
		login := review.User.Login
		switch review.State {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			opinions[login], reviewed[login] = review.State, true
		case "COMMENTED":
			reviewed[login] = true // a later comment does not revoke approval
		}
	}
	for login := range reviewed {
		status.Reviewed = append(status.Reviewed, login)
	}
	for login, opinion := range opinions {
		if opinion == "APPROVED" {
			status.Approved = append(status.Approved, login)
		}
		if opinion == "CHANGES_REQUESTED" {
			status.Changes = append(status.Changes, login)
		}
	}
	for _, names := range [][]string{status.Requested, status.Reviewed, status.Approved, status.Changes} {
		sort.Strings(names)
	}
	status.ReviewState = "none"
	if len(status.Requested) > 0 {
		status.ReviewState = "requested"
	}
	if len(status.Reviewed) > 0 {
		status.ReviewState = "reviewed"
	}
}

func worktreeCheckState(state, conclusion string) string {
	if state != "completed" {
		return "pending"
	}
	switch conclusion {
	case "success":
		return "passed"
	case "neutral", "skipped":
		return "skipped"
	case "failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale":
		return "failed"
	default:
		return "pending"
	}
}

func worktreeActionJob(repo, raw string) int64 {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" {
		return 0
	}
	prefix := "/" + repo + "/actions/runs/"
	if !strings.HasPrefix(strings.ToLower(u.Path), strings.ToLower(prefix)) {
		return 0
	}
	parts := strings.Split(strings.TrimPrefix(u.Path[len(prefix):], "/"), "/")
	if len(parts) != 3 || parts[1] != "job" {
		return 0
	}
	run, _ := strconv.ParseInt(parts[0], 10, 64)
	job, _ := strconv.ParseInt(parts[2], 10, 64)
	if run <= 0 || job <= 0 {
		return 0
	}
	return job
}

func fetchWorktreePRChecks(ctx context.Context, api forgeAPI, status *worktreePRStatus) error {
	// Head checks and the current synthetic merge commit can both carry PR CI.
	refs := []string{status.HeadSHA}
	if status.MergeSHA != "" && status.MergeSHA != status.HeadSHA {
		refs = append(refs, status.MergeSHA)
	}
	seen := map[int64]bool{}
	for _, ref := range refs {
		items, err := worktreePRPages(ctx, api, "/repos/"+status.Repository+"/commits/"+url.PathEscape(ref)+"/check-runs?filter=latest", "check_runs")
		if err != nil {
			return err
		}
		for _, raw := range items {
			var check struct {
				ID                       int64
				Name, Status, Conclusion string
				DetailsURL               string `json:"details_url"`
				HTMLURL                  string `json:"html_url"`
				App                      struct{ Slug string }
			}
			if err := json.Unmarshal(raw, &check); err != nil {
				return err
			}
			if seen[check.ID] {
				continue
			}
			seen[check.ID] = true
			c := worktreePRCheck{ID: check.ID, Name: check.Name, State: worktreeCheckState(check.Status, check.Conclusion), Conclusion: check.Conclusion, URL: check.HTMLURL}
			if check.App.Slug == "github-actions" {
				c.JobID = worktreeActionJob(status.Repository, check.DetailsURL)
			}
			c.Rerunnable = status.Connected && c.JobID > 0 && c.State == "failed"
			status.Checks = append(status.Checks, c)
		}
		items, err = worktreePRPages(ctx, api, "/repos/"+status.Repository+"/commits/"+url.PathEscape(ref)+"/status", "statuses")
		if err != nil {
			return err
		}
		for _, raw := range items {
			var item struct {
				ID             int64
				Context, State string
				TargetURL      string `json:"target_url"`
			}
			if err := json.Unmarshal(raw, &item); err != nil {
				return err
			}
			if seen[-item.ID] {
				continue
			}
			seen[-item.ID] = true
			state := "pending"
			if item.State == "success" {
				state = "passed"
			} else if item.State == "error" || item.State == "failure" {
				state = "failed"
			}
			status.Checks = append(status.Checks, worktreePRCheck{ID: -item.ID, Name: item.Context, State: state, Conclusion: item.State, URL: item.TargetURL})
		}
	}
	status.ChecksState = "none"
	if len(status.Checks) > 0 {
		status.ChecksState = "passed"
	}
	for _, check := range status.Checks {
		if check.State == "pending" && status.ChecksState != "failed" {
			status.ChecksState = "pending"
		}
		if check.State == "failed" {
			status.ChecksState = "failed"
		}
	}
	sort.SliceStable(status.Checks, func(i, j int) bool { return status.Checks[i].Name < status.Checks[j].Name })
	return nil
}

func (s *Server) handleWorktreePR(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		fail(w, 405, "GET only")
		return
	}
	if r.URL.Query().Get("worktree") == "" {
		fail(w, 400, "select a worktree")
		return
	}
	root, ok := s.reviewWorktree(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	source, err := worktreePRSourceFor(ctx, root)
	if err != nil {
		fail(w, 400, "could not read the worktree's GitHub remote")
		return
	}
	token := ""
	if source.headRepo != "" {
		token = s.worktreePR.auth(ctx)
	}
	status, err := s.worktreePR.status(ctx, source, token, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		fail(w, 502, redactToken(err.Error(), token))
		return
	}
	writeJSON(w, status)
}

func (s *Server) handleWorktreePRRerun(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) {
		return
	}
	var body struct {
		Worktree string  `json:"worktree"`
		Number   int     `json:"number"`
		HeadSHA  string  `json:"headSHA"`
		Checks   []int64 `json:"checks"`
	}
	if err := readJSONBody(r, &body); err != nil || body.Worktree == "" || body.Number <= 0 || body.HeadSHA == "" || len(body.Checks) == 0 || len(body.Checks) > 20 {
		fail(w, 400, "select up to 20 failed checks from a PR")
		return
	}
	root, ok := s.selectRegisteredWorktree(w, r, body.Worktree, "")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	token := s.worktreePR.auth(ctx)
	if token == "" {
		fail(w, 403, "connect GitHub with Actions write access to rerun jobs")
		return
	}
	source, err := worktreePRSourceFor(ctx, root)
	if err != nil {
		fail(w, 400, "could not read the worktree's GitHub remote")
		return
	}
	p := &s.worktreePR
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	status, err := p.status(ctx, source, token, true)
	if err != nil {
		fail(w, 502, redactToken(err.Error(), token))
		return
	}
	if status.State != "open" || status.Number != body.Number || status.HeadSHA != body.HeadSHA {
		fail(w, 409, "the PR changed; refresh before rerunning checks")
		return
	}
	api := githubAPI(token)
	selected, jobs := []worktreePRCheck{}, map[int64]bool{}
	for _, id := range body.Checks {
		found := false
		for _, check := range status.Checks {
			if check.ID != id {
				continue
			}
			if !check.Rerunnable {
				fail(w, 409, "a selected check is no longer a failed GitHub Actions job")
				return
			}
			if jobs[check.JobID] {
				found = true
				break
			}
			if at := p.reruns[check.JobID]; !at.IsZero() && time.Since(at) < 90*time.Second {
				fail(w, 409, "rerun already requested; wait for GitHub to update")
				return
			}
			var job struct {
				HeadSHA            string `json:"head_sha"`
				CheckRunURL        string `json:"check_run_url"`
				Status, Conclusion string
			}
			_, err := api.do(ctx, "GET", fmt.Sprintf("/repos/%s/actions/jobs/%d", status.Repository, check.JobID), nil, &job)
			if err != nil {
				fail(w, 502, redactToken(err.Error(), token))
				return
			}
			want := fmt.Sprintf("https://api.github.com/repos/%s/check-runs/%d", status.Repository, check.ID)
			if !strings.EqualFold(job.CheckRunURL, want) || (job.HeadSHA != status.HeadSHA && job.HeadSHA != status.MergeSHA) || worktreeCheckState(job.Status, job.Conclusion) != "failed" {
				fail(w, 409, "a selected job changed; refresh and select its latest attempt")
				return
			}
			selected, jobs[check.JobID], found = append(selected, check), true, true
			break
		}
		if !found {
			fail(w, 409, "a selected check is not on this PR's current commit")
			return
		}
	}
	// All selections are verified before the first remote write. Record partial
	// success so the user never retries jobs that GitHub already accepted.
	type result struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Accepted    bool   `json:"accepted"`
		Unconfirmed bool   `json:"unconfirmed,omitempty"`
		Error       string `json:"error,omitempty"`
	}
	results := []result{}
	if p.reruns == nil {
		p.reruns = map[int64]time.Time{}
	}
	for id, at := range p.reruns {
		if time.Since(at) > 90*time.Second {
			delete(p.reruns, id)
		}
	}
	for _, check := range selected {
		code, err := api.do(ctx, "POST", fmt.Sprintf("/repos/%s/actions/jobs/%d/rerun", status.Repository, check.JobID), map[string]bool{"enable_debug_logging": false}, nil)
		entry := result{ID: check.ID, Name: check.Name, Accepted: err == nil}
		if err != nil {
			entry.Error = redactToken(err.Error(), token)
			if code == 0 || code >= 500 {
				entry.Unconfirmed = true
				entry.Error = "GitHub did not confirm whether the rerun started. Refresh its status before retrying."
				p.reruns[check.JobID] = time.Now()
			}
			if strings.Contains(entry.Error, "403") {
				entry.Error = "GitHub denied the rerun. Your account and token need Actions write access."
			}
		} else {
			p.reruns[check.JobID] = time.Now()
		}
		results = append(results, entry)
	}
	p.mu.Lock()
	for _, entry := range p.entries {
		entry.at = time.Time{}
	}
	p.mu.Unlock()
	writeJSON(w, map[string]any{"results": results})
}

const worktreeApprovalQuery = `query WorktreeApproval($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) { number state headRefOid reviewDecision }
  }
}`

// GitHub's aggregate decision accounts for approval count, code owners and
// stale/last-push review rules. Individual APPROVED reviews are not sufficient.
func fetchWorktreeApproval(ctx context.Context, api forgeAPI, token string, status *worktreePRStatus) {
	if status.ReviewState == "approved" {
		status.ReviewState = "reviewed"
	}
	status.ReviewRequirements = "unknown"
	status.ReviewRequirementsReason = "Required-review status could not be verified."
	if token == "" {
		status.ReviewRequirementsReason = "Connect GitHub to verify required reviews."
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	owner, repo, ok := strings.Cut(status.Repository, "/")
	if !ok {
		return
	}
	var response struct {
		Data struct {
			Repository *struct {
				PullRequest *struct {
					Number                            int
					State, HeadRefOid, ReviewDecision string
				}
			}
		}
		Errors []json.RawMessage
	}
	_, err := api.do(ctx, "POST", "/graphql", map[string]any{
		"query":     worktreeApprovalQuery,
		"variables": map[string]any{"owner": owner, "name": repo, "number": status.Number},
	}, &response)
	if err != nil || len(response.Errors) > 0 || response.Data.Repository == nil || response.Data.Repository.PullRequest == nil {
		return
	}
	pr := response.Data.Repository.PullRequest
	if pr.Number != status.Number || pr.State != "OPEN" || pr.HeadRefOid != status.HeadSHA {
		return
	}
	switch pr.ReviewDecision {
	case "APPROVED":
		if len(status.Approved) > 0 {
			status.ReviewState, status.ReviewRequirements = "approved", "satisfied"
			status.ReviewRequirementsReason = "GitHub confirms required reviews are satisfied, including Code Owners when required."
		}
	case "REVIEW_REQUIRED":
		status.ReviewRequirements = "required"
		status.ReviewRequirementsReason = "GitHub still requires approval before merging."
	case "CHANGES_REQUESTED":
		status.ReviewRequirements = "changes_requested"
		status.ReviewRequirementsReason = "GitHub requires changes before the review requirement is satisfied."
	}
}
