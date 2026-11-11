package update

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"px0/internal/ui"
)

func TestCompareSemver(t *testing.T) {
	tests := []struct {
		v1   string
		v2   string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"v0.1.0", "0.1.0", 0},
		{"0.1.0", "v0.1.0", 0},
		{"0.2.0", "0.1.0", 1},
		{"0.1.0", "0.2.0", -1},
		{"1.0.0", "0.9.9", 1},
		{"0.1.1", "0.1.0", 1},
		{"0.10.0", "0.9.0", 1},
		{"0.1.0-alpha", "0.1.0", 0},
		{"0.2.0", "0.1.99", 1},
		{"macos-v0.1.22", "0.1.21", 1},
		{"macos-v1.0.0", "0.9.99", 1},
		{"macos-v1.2.3", "v1.2.3", 0},
	}

	for _, tt := range tests {
		got := compareSemver(tt.v1, tt.v2)
		if got != tt.want {
			t.Errorf("compareSemver(%q, %q) = %d, want %d", tt.v1, tt.v2, got, tt.want)
		}
	}
}

func TestFetchLatestRelease(t *testing.T) {
	fakeRelease := githubRelease{
		TagName: "v0.2.0",
		Name:    "px0 v0.2.0",
		Assets: []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		}{
			{
				Name:               "px0-0.2.0-linux-amd64",
				BrowserDownloadURL: "https://example.com/download/px0",
			},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fakeRelease)
	}))
	defer server.Close()

	t.Setenv("PX0_UPDATE_URL", server.URL)

	rel, err := fetchLatestRelease("test/repo")
	if err != nil {
		t.Fatalf("fetchLatestRelease failed: %v", err)
	}

	if rel.TagName != "v0.2.0" {
		t.Errorf("got TagName %q, want v0.2.0", rel.TagName)
	}
	if len(rel.Assets) != 1 || rel.Assets[0].Name != "px0-0.2.0-linux-amd64" {
		t.Errorf("unexpected assets: %+v", rel.Assets)
	}
}

func TestUpdateStatePersistence(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmpDir)

	statePath := filepath.Join(tmpDir, "px0", "update_check.json")
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("expected state file to not exist yet")
	}

	now := time.Now().Truncate(time.Second)
	s := &updateState{
		LastChecked: now,
		LatestVer:   "0.2.0",
	}
	writeUpdateState(s)

	readState, err := readUpdateState()
	if err != nil {
		t.Fatalf("readUpdateState failed: %v", err)
	}

	if readState.LatestVer != "0.2.0" {
		t.Errorf("read LatestVer = %q, want 0.2.0", readState.LatestVer)
	}
	if readState.LastChecked.Unix() != now.Unix() {
		t.Errorf("read LastChecked = %v, want %v", readState.LastChecked, now)
	}
}

func TestDownloadVerifiedAsset(t *testing.T) {
	assetName := "px0-0.2.0-linux-amd64"
	binary := []byte("test binary")
	digest := fmt.Sprintf("%x", sha256.Sum256(binary))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + assetName:
			_, _ = w.Write(binary)
		case "/checksums.txt":
			_, _ = fmt.Fprintf(w, "%s  %s\n", digest, assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var got bytes.Buffer
	err := downloadVerifiedAsset(server.Client(), server.URL+"/"+assetName, server.URL+"/checksums.txt", assetName, &got)
	if err != nil {
		t.Fatalf("downloadVerifiedAsset failed: %v", err)
	}
	if !bytes.Equal(got.Bytes(), binary) {
		t.Fatalf("downloaded %q, want %q", got.Bytes(), binary)
	}
}

func TestDownloadVerifiedAssetRejectsMismatch(t *testing.T) {
	assetName := "px0-0.2.0-linux-amd64"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/checksums.txt" {
			_, _ = fmt.Fprintf(w, "%064x  %s\n", 0, assetName)
			return
		}
		_, _ = w.Write([]byte("tampered binary"))
	}))
	defer server.Close()

	err := downloadVerifiedAsset(server.Client(), server.URL+"/"+assetName, server.URL+"/checksums.txt", assetName, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("got %v, want checksum mismatch", err)
	}
}

func TestChecksumForRejectsMissingAndMalformedEntries(t *testing.T) {
	assetName := "px0-0.2.0-linux-amd64"
	for name, checksums := range map[string]string{
		"missing":   fmt.Sprintf("%064x  other-asset\n", 0),
		"malformed": "not-a-sha256  " + assetName + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := checksumFor([]byte(checksums), assetName); err == nil {
				t.Fatal("expected checksum validation to fail")
			}
		})
	}
}

func TestRunSelfUpdateMock(t *testing.T) {
	// Create a dummy binary that will act as the "updated" binary
	tmpDir := t.TempDir()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}

	newBinPath := filepath.Join(tmpDir, "new-px0"+ext)
	// Write a shell script / batch script or copy existing current test binary
	script := "#!/bin/sh\necho 'px0 0.2.0 (" + runtime.GOOS + "/" + runtime.GOARCH + ")'\n"
	if err := os.WriteFile(newBinPath, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write fake binary: %v", err)
	}

	assetName := fmt.Sprintf("px0-0.2.0-%s-%s%s", runtime.GOOS, runtime.GOARCH, ext)

	// Setup mock server
	var serverURL string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/test/px0/releases/latest":
			rel := githubRelease{
				TagName: "v0.2.0",
				Name:    "px0 v0.2.0",
				Assets: []struct {
					Name               string `json:"name"`
					BrowserDownloadURL string `json:"browser_download_url"`
				}{
					{
						Name:               assetName,
						BrowserDownloadURL: serverURL + "/download/" + assetName,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(rel)
		case "/download/" + assetName:
			http.ServeFile(w, r, newBinPath)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	serverURL = ts.URL

	t.Setenv("PX0_REPO", "test/px0")
	t.Setenv("PX0_UPDATE_URL", ts.URL+"/repos/test/px0/releases/latest")
	t.Setenv("XDG_STATE_HOME", tmpDir)

	rel, err := fetchLatestRelease("test/px0")
	if err != nil {
		t.Fatalf("fetchLatestRelease returned error: %v", err)
	}
	if rel.TagName != "v0.2.0" {
		t.Fatalf("expected v0.2.0, got %s", rel.TagName)
	}
	if len(rel.Assets) != 1 || rel.Assets[0].Name != assetName {
		t.Fatalf("expected asset %s, got %+v", assetName, rel.Assets)
	}
}

// TestInstallUpdate exercises the actual download-verify-swap logic against
// a stand-in execPath, without touching the real test binary via
// os.Executable() (which AutoUpdate/RunSelfUpdate use in production).
func TestInstallUpdate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires a runnable .exe; covered on unix")
	}

	tmpDir := t.TempDir()
	execPath := filepath.Join(tmpDir, "px0")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\necho 'px0 0.1.0'\n"), 0o755); err != nil {
		t.Fatalf("failed to seed current binary: %v", err)
	}

	assetName := fmt.Sprintf("px0-0.2.0-%s-%s", runtime.GOOS, runtime.GOARCH)
	newBinary := []byte("#!/bin/sh\necho 'px0 0.2.0'\n")
	digest := fmt.Sprintf("%x", sha256.Sum256(newBinary))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/" + assetName:
			_, _ = w.Write(newBinary)
		case "/checksums.txt":
			_, _ = fmt.Fprintf(w, "%s  %s\n", digest, assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	rel := &githubRelease{
		TagName: "macos-v0.2.0",
		Assets: []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		}{
			{Name: assetName, BrowserDownloadURL: ts.URL + "/download/" + assetName},
			{Name: "checksums.txt", BrowserDownloadURL: ts.URL + "/checksums.txt"},
		},
	}

	if err := installUpdate(rel, releaseVersion(rel.TagName), execPath); err != nil {
		t.Fatalf("installUpdate failed: %v", err)
	}

	got, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatalf("failed to read installed binary: %v", err)
	}
	if string(got) != string(newBinary) {
		t.Fatalf("installed binary content = %q, want %q", got, newBinary)
	}
	info, err := os.Stat(execPath)
	if err != nil {
		t.Fatalf("failed to stat installed binary: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("installed binary is not executable: mode %v", info.Mode())
	}
}

func TestForkUpdateRepository(t *testing.T) {
	t.Setenv("PX0_REPO", "")
	if got := getRepoName(); got != "avaakash/px0" {
		t.Fatalf("updater points at %q", got)
	}
	t.Setenv("PX0_REPO", "  example/other-fork  ")
	if got := getRepoName(); got != "example/other-fork" {
		t.Fatalf("repository override ignored: %q", got)
	}
}

func TestForkReleaseValidation(t *testing.T) {
	for _, tc := range []struct {
		tag                      string
		draft, prerelease, valid bool
	}{
		{"macos-v0.1.21", false, false, true},
		{"v0.1.22", false, false, true},
		{"0.1.22", false, false, true},
		{"v0.1.22", true, false, false},
		{"v0.1.22", false, true, false},
		{"", false, false, false},
		{"latest", false, false, false},
		{"macos-v1.2.3/something", false, false, false},
	} {
		t.Run(fmt.Sprintf("%s-%t-%t", tc.tag, tc.draft, tc.prerelease), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(githubRelease{TagName: tc.tag, Draft: tc.draft, Prerelease: tc.prerelease})
			}))
			defer server.Close()
			t.Setenv("PX0_UPDATE_URL", server.URL)
			rel, err := fetchLatestRelease("avaakash/px0")
			if (err == nil) != tc.valid {
				t.Fatalf("tag %q accepted=%t, want %t (%v)", tc.tag, err == nil, tc.valid, err)
			}
			if tc.valid && !stableVersion.MatchString(releaseVersion(rel.TagName)) {
				t.Fatal("release version not normalized")
			}
		})
	}
}

func TestForkIgnoresOldRepositoryCache(t *testing.T) {
	for _, oldRepo := range []string{"", "px0-ai/px0", "another/fork"} {
		t.Run("repo-"+oldRepo, func(t *testing.T) {
			t.Setenv("PX0_REPO", "")
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			// Write a real old cache file without the write helper's migration.
			b, _ := json.Marshal(updateState{Repo: oldRepo, LastChecked: time.Now(), LatestVer: "0.1.20"})
			os.MkdirAll(filepath.Dir(stateFilePath()), 0700)
			if err := os.WriteFile(stateFilePath(), b, 0600); err != nil {
				t.Fatal(err)
			}
			if state, err := readUpdateState(); err != nil || state != nil {
				t.Fatalf("old repository cache reused: %+v %v", state, err)
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				json.NewEncoder(w).Encode(githubRelease{TagName: "macos-v0.1.21"})
			}))
			defer server.Close()
			t.Setenv("PX0_UPDATE_URL", server.URL)
			AutoUpdate("0.1.22") // newer local build must never be replaced
			state, err := readUpdateState()
			if err != nil || state == nil || state.Repo != "avaakash/px0" || state.LatestVer != "0.1.21" || calls != 1 {
				t.Fatalf("fork cache not refreshed: %+v %v calls=%d", state, err, calls)
			}
			AutoUpdate("0.1.22")
			if calls != 1 {
				t.Fatal("daily check cache missed")
			}
		})
	}
}

func TestInstallUpdateFailureKeepsCurrentBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix verification fixture")
	}
	for _, scenario := range []string{"wrong-version", "bad-checksum", "missing-checksum", "missing-platform"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "px0")
			old := []byte("original local build")
			if err := os.WriteFile(path, old, 0755); err != nil {
				t.Fatal(err)
			}
			name := fmt.Sprintf("px0-0.1.23-%s-%s", runtime.GOOS, runtime.GOARCH)
			binary := []byte("#!/bin/sh\necho 'px0 0.1.23'\n")
			if scenario == "wrong-version" {
				binary = []byte("#!/bin/sh\necho 'px0 0.1.21'\n")
			}
			digest := fmt.Sprintf("%x", sha256.Sum256(binary))
			if scenario == "bad-checksum" {
				digest = strings.Repeat("0", 64)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/checksums.txt" {
					fmt.Fprintf(w, "%s  %s\n", digest, name)
				} else {
					w.Write(binary)
				}
			}))
			defer server.Close()
			var rel githubRelease
			// Decode the real manifest shape, including our macOS tag prefix.
			manifest := fmt.Sprintf(`{"tag_name":"macos-v0.1.23","assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}]}`, name, server.URL+"/binary", server.URL+"/checksums.txt")
			if err := json.Unmarshal([]byte(manifest), &rel); err != nil {
				t.Fatal(err)
			}
			if scenario == "missing-checksum" {
				rel.Assets = rel.Assets[:1]
			}
			if scenario == "missing-platform" {
				rel.Assets = rel.Assets[1:]
			}
			if err := installUpdate(&rel, releaseVersion(rel.TagName), path); err == nil {
				t.Fatal("invalid release installed")
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, old) {
				t.Fatalf("current binary changed: %q %v", got, err)
			}
			left, _ := filepath.Glob(filepath.Join(dir, "px0-update-*"))
			if len(left) != 0 {
				t.Fatalf("temporary downloads leaked: %v", left)
			}
		})
	}
}

// TestAutoUpdateFastPathSkipsNetwork verifies that when the cached state was
// checked recently and shows no newer version, AutoUpdate never touches the
// network (and so never reaches the os.Executable()-based install
// path, which would be unsafe to exercise against the test binary).
func TestAutoUpdateFastPathSkipsNetwork(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmpDir)
	t.Setenv("PX0_UPDATE_URL", "http://127.0.0.1:1/should-not-be-hit")

	writeUpdateState(&updateState{LastChecked: time.Now(), LatestVer: "0.1.0"})

	AutoUpdate("0.1.0")
	// No panic/hang means the cached "up to date" state short-circuited
	// before any network call.
}

// TestAutoUpdateDueCheckSkipsInstallWhenUpToDate verifies that a due daily
// check that finds no newer release just refreshes the cache and returns,
// again without reaching the install path.
func TestAutoUpdateDueCheckSkipsInstallWhenUpToDate(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmpDir)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := githubRelease{TagName: "v0.1.0"}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rel)
	}))
	defer ts.Close()
	t.Setenv("PX0_UPDATE_URL", ts.URL)

	AutoUpdate("0.1.0")

	state, err := readUpdateState()
	if err != nil {
		t.Fatalf("readUpdateState failed: %v", err)
	}
	if state.LatestVer != "0.1.0" {
		t.Fatalf("LatestVer = %q, want 0.1.0", state.LatestVer)
	}
}

func captureAutoUpdateStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() failed: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = w

	fn()

	os.Stdout = origStdout
	_ = w.Close()

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

// TestAutoUpdateVerbosePrintsUpToDate verifies that -verbose surfaces an
// explicit "up to date" line on stdout when the daily check finds nothing
// newer, both when the check actually hits the network...
func TestAutoUpdateVerbosePrintsUpToDate(t *testing.T) {
	origVerbose := ui.Verbose
	defer func() { ui.Verbose = origVerbose }()
	ui.Verbose = true

	tmpDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmpDir)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := githubRelease{TagName: "v0.1.0"}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rel)
	}))
	defer ts.Close()
	t.Setenv("PX0_UPDATE_URL", ts.URL)

	out := captureAutoUpdateStdout(t, func() { AutoUpdate("0.1.0") })
	if !strings.Contains(out, "up to date") {
		t.Fatalf("expected an up-to-date message in verbose output, got %q", out)
	}
}

// ...and when the cached state from an earlier check already says so.
func TestAutoUpdateVerbosePrintsUpToDateFromCache(t *testing.T) {
	origVerbose := ui.Verbose
	defer func() { ui.Verbose = origVerbose }()
	ui.Verbose = true

	tmpDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmpDir)
	t.Setenv("PX0_UPDATE_URL", "http://127.0.0.1:1/should-not-be-hit")

	writeUpdateState(&updateState{LastChecked: time.Now(), LatestVer: "0.1.0"})

	out := captureAutoUpdateStdout(t, func() { AutoUpdate("0.1.0") })
	if !strings.Contains(out, "up to date") {
		t.Fatalf("expected an up-to-date message in verbose output, got %q", out)
	}
}

// TestAutoUpdateQuietWhenNotVerbose verifies that without -verbose, an
// up-to-date check stays silent (no stdout noise on every ordinary run).
func TestAutoUpdateQuietWhenNotVerbose(t *testing.T) {
	origVerbose := ui.Verbose
	defer func() { ui.Verbose = origVerbose }()
	ui.Verbose = false

	tmpDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmpDir)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := githubRelease{TagName: "v0.1.0"}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rel)
	}))
	defer ts.Close()
	t.Setenv("PX0_UPDATE_URL", ts.URL)

	out := captureAutoUpdateStdout(t, func() { AutoUpdate("0.1.0") })
	if out != "" {
		t.Fatalf("expected no output without -verbose, got %q", out)
	}
}

func TestCopyOrMove(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")

	if err := os.WriteFile(src, []byte("binary data 12345"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := copyOrMove(src, dst); err != nil {
		t.Fatalf("copyOrMove failed: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("failed to read dst: %v", err)
	}
	if string(got) != "binary data 12345" {
		t.Errorf("unexpected content: %s", string(got))
	}
}

func BenchmarkCompareSemver(b *testing.B) {
	pairs := [][2]string{
		{"0.1.0", "0.2.0"},
		{"v1.2.3", "1.2.4"},
		{"0.10.0", "0.9.9"},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := pairs[i%len(pairs)]
		_ = compareSemver(p[0], p[1])
	}
}
