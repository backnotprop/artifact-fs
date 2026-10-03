package gitstore

import (
	"context"
	"errors"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// sizedTestRemote makes a bare repo with files of known sizes, a symlink and a
// gitlink, serving blobless clones and object-info. It returns the bare path
// and each blob path's size.
func sizedTestRemote(t *testing.T) (string, map[string]int64) {
	t.Helper()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "remote.git")
	work := filepath.Join(tmp, "work")
	run(t, "git", "init", "-q", "--bare", "-b", "main", bare)
	run(t, "git", "-C", bare, "config", "uploadpack.allowFilter", "true")
	run(t, "git", "-C", bare, "config", "transfer.advertiseObjectInfo", "true")
	run(t, "git", "init", "-q", "-b", "main", work)
	sizes := map[string]int64{}
	write := func(path string, size int) {
		full := filepath.Join(work, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(strings.Repeat("x", size-1)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sizes[path] = int64(size)
	}
	write("README.md", 11)
	write("src/main.go", 4096)
	write("src/deep/data.bin", 70000)
	if err := os.Symlink("README.md", filepath.Join(work, "link.md")); err != nil {
		t.Fatal(err)
	}
	sizes["link.md"] = int64(len("README.md"))
	run(t, "git", "-C", work, "add", "-A")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "files")
	head := strings.TrimSpace(runOutput(t, "git", "-C", work, "rev-parse", "HEAD"))
	run(t, "git", "-C", work, "update-index", "--add", "--cacheinfo", "160000,"+head+",vendor/dep")
	run(t, "git", "-C", work, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "-m", "gitlink")
	run(t, "git", "-C", work, "push", "-q", bare, "main")
	return bare, sizes
}

func missingBlobCount(t *testing.T, gitDir string) int {
	t.Helper()
	cmd := exec.Command("git", "rev-list", "--objects", "--all", "--missing=print")
	cmd.Env = append(os.Environ(), "GIT_DIR="+gitDir, "GIT_NO_LAZY_FETCH=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-list --missing=print: %v", err)
	}
	n := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.HasPrefix(line, "?") {
			n++
		}
	}
	return n
}

func bloblessClone(t *testing.T, remoteURL string) (*Store, model.RepoConfig) {
	t.Helper()
	cfg := model.RepoConfig{ID: "sized", Name: "sized", GitDir: filepath.Join(t.TempDir(), "git"), RemoteURL: remoteURL, Branch: "main"}
	store := New(nil)
	t.Cleanup(store.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.CloneBlobless(ctx, cfg); err != nil {
		t.Fatalf("CloneBlobless: %v", err)
	}
	return store, cfg
}

func TestCloneBloblessMarksFreshIndexFsmonitorValid(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	bare, sizes := sizedTestRemote(t)
	_, cfg := bloblessClone(t, "file://"+bare)

	if got := missingBlobCount(t, cfg.GitDir); got != len(sizes) {
		t.Fatalf("missing blobs after clone = %d, want all %d", got, len(sizes))
	}
	index, err := os.ReadFile(filepath.Join(cfg.GitDir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ext := range []string{"FSMN", "TREE"} {
		if !strings.Contains(string(index), ext) {
			t.Fatalf("fresh index lacks the %s extension", ext)
		}
	}

	// ls-files -f prints a lowercase tag for entries git trusts through
	// fsmonitor. Read it with a hook that reports nothing changed.
	scratch := t.TempDir()
	hook := filepath.Join(scratch, "hook")
	if err := os.WriteFile(hook, []byte(fsmonitorInitHook), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-c", "core.fsmonitor="+hook, "ls-files", "-f")
	cmd.Env = append(os.Environ(), "GIT_DIR="+cfg.GitDir, "GIT_WORK_TREE="+scratch, "GIT_NO_LAZY_FETCH=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ls-files -f: %v: %s", err, out)
	}
	tags := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		tag, path, _ := strings.Cut(line, " ")
		tags[path] = tag
	}
	for path := range sizes {
		if tags[path] != "h" {
			t.Errorf("%s tag = %q, want h (fsmonitor-valid)", path, tags[path])
		}
	}
	if tags["vendor/dep"] != "H" {
		t.Errorf("gitlink tag = %q, want H (never fsmonitor-valid)", tags["vendor/dep"])
	}
	if got := missingBlobCount(t, cfg.GitDir); got != len(sizes) {
		t.Fatalf("missing blobs after reading the index = %d, want all %d", got, len(sizes))
	}
}

func TestReadTreeHEADMarksFreshIndexFsmonitorValid(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	bare, sizes := sizedTestRemote(t)
	store, cfg := bloblessClone(t, "file://"+bare)
	if err := os.Remove(filepath.Join(cfg.GitDir, "index")); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureIndexInitialized(context.Background(), cfg); err != nil {
		t.Fatalf("EnsureIndexInitialized: %v", err)
	}
	index, err := os.ReadFile(filepath.Join(cfg.GitDir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), "FSMN") {
		t.Fatal("rebuilt index lacks the fsmonitor extension")
	}
	if got := missingBlobCount(t, cfg.GitDir); got != len(sizes) {
		t.Fatalf("missing blobs = %d, want all %d", got, len(sizes))
	}
}

func assertSizesFromRemote(t *testing.T, store *Store, cfg model.RepoConfig, sizes map[string]int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	head, _, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.BuildTreeIndex(ctx, cfg, head)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}
	seen := 0
	for _, n := range nodes {
		want, ok := sizes[n.Path]
		if !ok {
			continue
		}
		seen++
		if n.SizeState != "known" || n.SizeBytes != want {
			t.Errorf("%s size = %s/%d, want known/%d", n.Path, n.SizeState, n.SizeBytes, want)
		}
	}
	if seen != len(sizes) {
		t.Fatalf("saw %d sized nodes, want %d", seen, len(sizes))
	}
	if got := missingBlobCount(t, cfg.GitDir); got != len(sizes) {
		t.Fatalf("missing blobs after BuildTreeIndex = %d, want all %d (sizes must not download contents)", got, len(sizes))
	}
}

func TestBuildTreeIndexSizesFromObjectInfoFileRemote(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	bare, sizes := sizedTestRemote(t)
	store, cfg := bloblessClone(t, "file://"+bare)
	assertSizesFromRemote(t, store, cfg, sizes)
}

func TestBuildTreeIndexSizesFromObjectInfoOverHTTP(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	execPath := strings.TrimSpace(runOutput(t, "git", "--exec-path"))
	backend := filepath.Join(execPath, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not available: %v", err)
	}
	bare, sizes := sizedTestRemote(t)
	server := httptest.NewServer(&cgi.Handler{
		Path: backend,
		Env:  []string{"GIT_PROJECT_ROOT=" + filepath.Dir(bare), "GIT_HTTP_EXPORT_ALL=1"},
	})
	defer server.Close()
	store, cfg := bloblessClone(t, server.URL+"/"+filepath.Base(bare))
	assertSizesFromRemote(t, store, cfg, sizes)
}

func TestRemoteBlobSizesSkipsUnsupportedTransports(t *testing.T) {
	for _, remote := range []string{"ssh://git@example.invalid/repo.git", "git@example.invalid:repo.git", "git://example.invalid/repo.git"} {
		_, err := New(nil).remoteBlobSizes(context.Background(), model.RepoConfig{GitDir: t.TempDir(), RemoteURL: remote}, []string{strings.Repeat("a", 40)})
		if !errors.Is(err, errObjectInfoUnsupported) {
			t.Errorf("%s: err = %v, want errObjectInfoUnsupported", remote, err)
		}
	}
}

func TestParseObjectInfo(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	sizes := map[string]int64{}
	if err := parseObjectInfo([]string{"size", a + " 12", b + " "}, sizes); err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 1 || sizes[a] != 12 {
		t.Fatalf("sizes = %v, want only %s=12", sizes, a)
	}
	for _, bad := range [][]string{{"type"}, {"size", a}, {"size", a + " -1"}, {"size", a + " x"}} {
		if err := parseObjectInfo(bad, map[string]int64{}); err == nil {
			t.Errorf("parseObjectInfo(%q) succeeded, want error", bad)
		}
	}
}

func TestObjectInfoRequestFormat(t *testing.T) {
	oid := strings.Repeat("c", 40)
	got := string(objectInfoRequest("sha1", []string{oid}))
	want := "0018command=object-info\n" + "0017object-format=sha1\n" + "0001" + "0009size\n" + "0031oid " + oid + "\n" + "0000"
	if got != want {
		t.Fatalf("request = %q, want %q", got, want)
	}
}

func TestBuildTreeIndexWithoutObjectInfoLeavesSizesUnknown(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	bare, sizes := sizedTestRemote(t)
	run(t, "git", "-C", bare, "config", "transfer.advertiseObjectInfo", "false")
	cmd := exec.Command("git", "upload-pack", "--stateless-rpc", "--advertise-refs", bare)
	cmd.Env = append(os.Environ(), "GIT_PROTOCOL=version=2")
	if adv, err := cmd.Output(); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(adv), "object-info") {
		t.Skip("this git's upload-pack always advertises object-info")
	}
	store, cfg := bloblessClone(t, "file://"+bare)
	ctx := context.Background()
	head, _, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := store.BuildTreeIndex(ctx, cfg, head)
	if err != nil {
		t.Fatalf("BuildTreeIndex: %v", err)
	}
	for _, n := range nodes {
		if _, ok := sizes[n.Path]; ok && n.SizeState == "known" {
			t.Errorf("%s size is known without object-info", n.Path)
		}
	}
	if got := missingBlobCount(t, cfg.GitDir); got != len(sizes) {
		t.Fatalf("missing blobs = %d, want all %d", got, len(sizes))
	}
}
