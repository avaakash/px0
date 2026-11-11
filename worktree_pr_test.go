package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise real HTTP serialization and status handling against a local GitHub
// stand-in. No test sends credentials or writes to GitHub.
func worktreePRMock(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	original := githubHTTPClient.Transport
	u, _ := url.Parse(server.URL)
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.github.com" {
			return nil, fmt.Errorf("unexpected upstream host: %s", req.URL.Host)
		}
		copy := req.Clone(req.Context())
		target := *req.URL
		target.Scheme, target.Host = u.Scheme, u.Host
		copy.URL = &target
		return http.DefaultTransport.RoundTrip(copy)
	})
	t.Cleanup(func() { githubHTTPClient.Transport = original })
}

func worktreePRFixture(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	switch {
	case path == "/graphql":
		fmt.Fprint(w, `{"data":{"repository":{"pullRequest":{"number":7,"state":"OPEN","headRefOid":"head","reviewDecision":"REVIEW_REQUIRED"}}}}`)
	case path == "/repos/me/widget/pulls":
		fmt.Fprint(w, `[]`)
	case path == "/repos/acme/widget/pulls":
		fmt.Fprint(w, `[{"number":7,"state":"open","title":"A feature","html_url":"https://github.com/acme/widget/pull/7","merge_commit_sha":"merge","user":{"login":"me"},"head":{"ref":"main","sha":"head","repo":{"full_name":"me/widget"}},"requested_reviewers":[{"login":"alice"}],"requested_teams":[{"slug":"backend"}]}]`)
	case path == "/repos/acme/widget/pulls/7/reviews":
		fmt.Fprint(w, `[{"user":{"login":"bob"},"state":"APPROVED"},{"user":{"login":"bob"},"state":"COMMENTED"},{"user":{"login":"carol"},"state":"CHANGES_REQUESTED"},{"user":{"login":"me"},"state":"APPROVED"}]`)
	case path == "/repos/acme/widget/commits/head/check-runs":
		fmt.Fprint(w, `{"check_runs":[{"id":12,"name":"unit","status":"completed","conclusion":"failure","app":{"slug":"github-actions"},"details_url":"https://github.com/acme/widget/actions/runs/77/job/101"},{"id":13,"name":"lint","status":"completed","conclusion":"failure","app":{"slug":"github-actions"},"details_url":"https://github.com/acme/widget/actions/runs/77/job/102"},{"id":14,"name":"external","status":"completed","conclusion":"failure","app":{"slug":"other-ci"},"details_url":"https://github.com/acme/widget/actions/runs/77/job/103"}]}`)
	case path == "/repos/acme/widget/commits/merge/check-runs":
		fmt.Fprint(w, `{"check_runs":[{"id":15,"name":"merge build","status":"completed","conclusion":"success"}]}`)
	case strings.HasSuffix(path, "/status"):
		fmt.Fprint(w, `{"statuses":[{"id":16,"context":"legacy-ci","state":"success","target_url":"https://ci.example.invalid/build/16"}]}`)
	case path == "/repos/acme/widget/actions/jobs/101":
		fmt.Fprint(w, `{"head_sha":"head","check_run_url":"https://api.github.com/repos/acme/widget/check-runs/12","status":"completed","conclusion":"failure"}`)
	case path == "/repos/acme/widget/actions/jobs/102":
		fmt.Fprint(w, `{"head_sha":"merge","check_run_url":"https://api.github.com/repos/acme/widget/check-runs/13","status":"completed","conclusion":"failure"}`)
	default:
		return false
	}
	return true
}

func TestWorktreePRStatusAndSelectedReruns(t *testing.T) {
	newTestStore(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "fixture-token")
	root := gitRepo(t)
	gitTestRun(t, root, "branch", "-M", "main")
	gitTestRun(t, root, "remote", "add", "origin", "git@github.com:me/widget.git")
	gitTestRun(t, root, "remote", "add", "upstream", "https://github.com/acme/widget.git")
	s := NewServer(NewIndex(root), nil)
	defer s.CloseThreads()
	s.gitWatcher.Stop()
	var writes []string
	deniedCode := 403
	worktreePRMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("wrong auth")
		}
		if r.URL.Path == "/graphql" {
			worktreePRFixture(w, r)
			return // A read-only GraphQL query, not a rerun.
		}
		if r.Method == "POST" {
			writes = append(writes, r.URL.Path)
			if strings.HasSuffix(r.URL.Path, "/102/rerun") {
				http.Error(w, `{"message":"forbidden fixture-token"}`, deniedCode)
			} else {
				w.WriteHeader(201)
			}
			return
		}
		if !worktreePRFixture(w, r) {
			t.Errorf("unexpected read: %s", r.URL)
			http.Error(w, "unexpected", 501)
		}
	})
	request := func(method, path string, body any, origin string) *httptest.ResponseRecorder {
		t.Helper()
		b, _ := json.Marshal(body)
		r := httptest.NewRequest(method, "http://localhost/api/git/worktrees/pr"+path, bytes.NewReader(b))
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	w := request("GET", "?worktree="+url.QueryEscape(root), nil, "")
	if w.Code != 200 {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}
	var status worktreePRStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Number != 7 || status.Repository != "acme/widget" || status.ReviewState != "reviewed" || status.ReviewRequirements != "required" || len(status.Approved) != 1 || len(status.Changes) != 1 || len(status.Requested) != 2 || status.ChecksState != "failed" || len(status.Checks) != 5 {
		t.Fatalf("incorrect status: %+v", status)
	}
	for _, check := range status.Checks {
		if check.ID == 14 && check.Rerunnable {
			t.Fatal("third-party check rerunnable")
		}
	}
	body := map[string]any{"worktree": root, "number": 7, "headSHA": "head", "checks": []int{12, 13}}
	if w := request("POST", "/rerun", body, "https://evil.invalid"); w.Code != 403 {
		t.Fatal("foreign origin accepted")
	}
	body["worktree"] = t.TempDir()
	if w := request("POST", "/rerun", body, "http://localhost"); w.Code != 400 {
		t.Fatal("foreign worktree accepted")
	}
	body["worktree"] = root
	body["headSHA"] = "old-head"
	if w := request("POST", "/rerun", body, "http://localhost"); w.Code != 409 {
		t.Fatal("stale head accepted")
	}
	body["headSHA"] = "head"
	for _, ids := range [][]int{{12, 999}, {12, 15}, {14}} {
		body["checks"] = ids
		if w := request("POST", "/rerun", body, "http://localhost"); w.Code != 409 {
			t.Fatalf("invalid selection accepted: %v %d %s", ids, w.Code, w.Body)
		}
		if len(writes) != 0 {
			t.Fatal("wrote before validating all selections")
		}
	}
	body["checks"] = []int{12, 13}
	w = request("POST", "/rerun", body, "http://localhost")
	if w.Code != 200 || len(writes) != 2 || !strings.Contains(w.Body.String(), `"accepted":true`) || !strings.Contains(w.Body.String(), `"accepted":false`) || strings.Contains(w.Body.String(), "fixture-token") {
		t.Fatalf("partial results: %d %s %v", w.Code, w.Body, writes)
	}
	body["checks"] = []int{12}
	if w := request("POST", "/rerun", body, "http://localhost"); w.Code != 409 || len(writes) != 2 {
		t.Fatal("duplicate rerun sent")
	}
	deniedCode = 502
	body["checks"] = []int{13}
	w = request("POST", "/rerun", body, "http://localhost")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"unconfirmed":true`) {
		t.Fatalf("uncertain rerun not reported: %s", w.Body)
	}
	if w := request("POST", "/rerun", body, "http://localhost"); w.Code != 409 || len(writes) != 3 {
		t.Fatal("unconfirmed rerun was repeated")
	}
}

func TestWorktreePRCacheSharesConcurrentRequests(t *testing.T) {
	var calls atomic.Int32
	worktreePRMock(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `[]`) })
	p := &worktreePRService{}
	source := worktreePRSource{branch: "feature", headRepo: "me/widget", repos: []string{"me/widget"}, key: "test"}
	var group sync.WaitGroup
	for range 20 {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := p.status(context.Background(), source, "", false); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("duplicate reads: %d", calls.Load())
	}
	p.status(context.Background(), source, "", false)
	if calls.Load() != 1 {
		t.Fatal("warm cache missed")
	}
	p.status(context.Background(), source, "", true)
	if calls.Load() != 2 {
		t.Fatal("refresh failed to bypass cache")
	}
	p.status(context.Background(), source, "new-token", false)
	if calls.Load() != 3 {
		t.Fatal("new credentials reused old cache")
	}
}

func TestWorktreeReviewSummary(t *testing.T) {
	for _, tc := range []struct {
		states             string
		want               string
		approvals, changes int
	}{
		{"APPROVED,COMMENTED", "reviewed", 1, 0},
		{"APPROVED,CHANGES_REQUESTED", "reviewed", 0, 1},
		{"APPROVED,DISMISSED", "reviewed", 0, 0},
		{"PENDING", "requested", 0, 0},
		{"COMMENTED", "reviewed", 0, 0},
	} {
		var items []json.RawMessage
		for _, state := range strings.Split(tc.states, ",") {
			items = append(items, json.RawMessage(fmt.Sprintf(`{"state":%q,"user":{"login":"reviewer"}}`, state)))
		}
		status := worktreePRStatus{Requested: []string{"reviewer"}}
		worktreeReviewSummary(&status, items)
		if status.ReviewState != tc.want || len(status.Approved) != tc.approvals || len(status.Changes) != tc.changes {
			t.Errorf("%s: %+v", tc.states, status)
		}
	}
}

func TestWorktreePRSourceAndJobValidation(t *testing.T) {
	root := gitRepo(t)
	gitTestRun(t, root, "checkout", "-b", "FeatureX")
	gitTestRun(t, root, "remote", "add", "origin", "git@github.com:me/widget.git")
	gitTestRun(t, root, "config", "branch.FeatureX.remote", "origin")
	gitTestRun(t, root, "config", "branch.FeatureX.merge", "refs/heads/published-feature")
	source, err := worktreePRSourceFor(context.Background(), root)
	if err != nil || source.headRepo != "me/widget" || source.branch != "published-feature" {
		t.Fatalf("renamed local branch: %+v %v", source, err)
	}
	for _, raw := range []string{"https://evil.invalid/acme/widget/actions/runs/1/job/2", "https://github.com/other/widget/actions/runs/1/job/2", "https://github.com/acme/widget/actions/runs/no/job/2", "https://github.com/acme/widget/actions/runs/1/job/-2"} {
		if worktreeActionJob("acme/widget", raw) != 0 {
			t.Errorf("invalid job URL: %s", raw)
		}
	}
	if worktreeActionJob("acme/widget", "https://github.com/acme/widget/actions/runs/1/job/2?pr=7") != 2 {
		t.Fatal("valid job URL rejected")
	}
}

func TestWorktreePRPaginationAndFailures(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		switch r.URL.Path {
		case "/limited":
			http.Error(w, `{"message":"rate limited"}`, 429)
		case "/bad":
			fmt.Fprint(w, `{"not-an-array":true}`)
		default:
			items := make([]map[string]int, 100)
			if r.URL.Query().Get("page") == "2" && r.URL.Path != "/endless" {
				items = items[:1]
			}
			json.NewEncoder(w).Encode(items)
		}
	}))
	defer server.Close()
	api := forgeAPI{base: server.URL}
	items, err := worktreePRPages(context.Background(), api, "/pages", "")
	if err != nil || len(items) != 101 || pages != 2 {
		t.Fatalf("pagination: %d %v %d", len(items), err, pages)
	}
	for _, path := range []string{"/limited", "/bad", "/endless"} {
		if _, err := worktreePRPages(context.Background(), api, path, ""); err == nil {
			t.Errorf("incomplete result accepted: %s", path)
		}
	}
}

func BenchmarkWorktreeReviewSummary(b *testing.B) {
	items := []json.RawMessage{json.RawMessage(`{"state":"APPROVED","user":{"login":"reviewer"}}`)}
	for b.Loop() {
		status := worktreePRStatus{}
		worktreeReviewSummary(&status, items)
	}
}

func TestWorktreePRCancelledCacheCanRetry(t *testing.T) {
	worktreePRMock(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `[]`) })
	p := &worktreePRService{}
	source := worktreePRSource{branch: "feature", headRepo: "me/widget", repos: []string{"me/widget"}, key: "test"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.status(ctx, source, "", false)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := p.status(ctx, source, "", false); err != nil {
		t.Fatal("cancelled request poisoned cache:", err)
	}
}

func TestWorktreeApprovalGate(t *testing.T) {
	for _, tc := range []struct {
		name, decision, head, state, requirements, want string
		code                                            int
		graphErrors, missingPR, noApproval, anonymous   bool
	}{
		{name: "required reviews green", decision: "APPROVED", requirements: "satisfied", want: "approved"},
		{name: "ordinary approval but owner or count still required", decision: "REVIEW_REQUIRED", requirements: "required", want: "reviewed"},
		{name: "changes requested", decision: "CHANGES_REQUESTED", requirements: "changes_requested", want: "reviewed"},
		{name: "pending decision", decision: "", requirements: "unknown", want: "reviewed"},
		{name: "unknown enum", decision: "FUTURE_STATE", requirements: "unknown", want: "reviewed"},
		{name: "head moved", decision: "APPROVED", head: "new-head", requirements: "unknown", want: "reviewed"},
		{name: "closed PR", decision: "APPROVED", state: "CLOSED", requirements: "unknown", want: "reviewed"},
		{name: "no submitted approval", decision: "APPROVED", noApproval: true, requirements: "unknown", want: "reviewed"},
		{name: "GraphQL partial error", decision: "APPROVED", graphErrors: true, requirements: "unknown", want: "reviewed"},
		{name: "missing PR", missingPR: true, requirements: "unknown", want: "reviewed"},
		{name: "forbidden", code: 403, requirements: "unknown", want: "reviewed"},
		{name: "rate limited", code: 429, requirements: "unknown", want: "reviewed"},
		{name: "server error", code: 502, requirements: "unknown", want: "reviewed"},
		{name: "anonymous", anonymous: true, requirements: "unknown", want: "reviewed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			worktreePRMock(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.URL.Path != "/graphql" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var request struct {
					Query     string
					Variables struct {
						Owner, Name string
						Number      int
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(request.Query, "query WorktreeApproval") || strings.Contains(request.Query, "mutation") || request.Variables.Owner != "acme" || request.Variables.Name != "widget" || request.Variables.Number != 7 {
					t.Fatalf("wrong query: %+v", request)
				}
				if tc.code != 0 {
					http.Error(w, "fixture denial", tc.code)
					return
				}
				head, state := tc.head, tc.state
				if head == "" {
					head = "head"
				}
				if state == "" {
					state = "OPEN"
				}
				var pr any = map[string]any{"number": 7, "state": state, "headRefOid": head, "reviewDecision": tc.decision}
				if tc.missingPR {
					pr = nil
				}
				response := map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": pr}}}
				if tc.graphErrors {
					response["errors"] = []any{map[string]any{"message": "not authorized"}}
				}
				json.NewEncoder(w).Encode(response)
			})
			status := worktreePRStatus{Repository: "acme/widget", Number: 7, HeadSHA: "head", ReviewState: "reviewed", Reviewed: []string{"bob"}, Approved: []string{"bob"}, ChecksState: "failed"}
			if tc.noApproval {
				status.Approved = nil
			}
			token := "fixture-token"
			if tc.anonymous {
				token = ""
			}
			fetchWorktreeApproval(context.Background(), githubAPI(token), token, &status)
			if status.ReviewState != tc.want || status.ReviewRequirements != tc.requirements || status.ChecksState != "failed" {
				t.Fatalf("incorrect gate: %+v", status)
			}
			if tc.anonymous && calls != 0 {
				t.Fatal("anonymous GraphQL call")
			}
		})
	}
}
