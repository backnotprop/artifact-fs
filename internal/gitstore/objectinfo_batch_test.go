package gitstore

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// objectInfoServer serves bare over smart HTTP (git-http-backend) and counts
// object-info requests; stall, when set, holds the request with that number
// (1-based) until the client gives up.
func objectInfoServer(t *testing.T, bare string, stall int64) (string, *atomic.Int64) {
	t.Helper()
	backend := filepath.Join(strings.TrimSpace(runOutput(t, "git", "--exec-path")), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not available: %v", err)
	}
	cgiHandler := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + filepath.Dir(bare), "GIT_HTTP_EXPORT_ALL=1"}}
	var count atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if bytes.Contains(body, []byte("command=object-info")) && count.Add(1) == stall {
				<-r.Context().Done()
				return
			}
		}
		cgiHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/" + filepath.Base(bare), &count
}

// One slow request ends the lookup at its own deadline, and the sizes the
// requests before it learned are kept.
func TestRemoteBlobSizesKeepsEarlierBatchesWhenOneIsSlow(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	batch, timeout := objectInfoBatch, objectInfoBatchTimeout
	objectInfoBatch, objectInfoBatchTimeout = 2, time.Second
	t.Cleanup(func() { objectInfoBatch, objectInfoBatchTimeout = batch, timeout })

	bare, _ := sizedTestRemote(t)
	url, count := objectInfoServer(t, bare, 2)
	store, cfg := bloblessClone(t, url)
	oids := strings.Fields(runOutput(t, "git", "--git-dir", cfg.GitDir, "ls-tree", "-r", "--object-only", "HEAD"))
	var blobs []string
	for _, oid := range oids {
		if typ := strings.TrimSpace(runOutput(t, "git", "--git-dir", bare, "cat-file", "-t", oid)); typ == "blob" {
			blobs = append(blobs, oid)
		}
	}
	if len(blobs) < 3 {
		t.Fatalf("want at least 3 blobs, got %d", len(blobs))
	}

	start := time.Now()
	sizes, err := store.RemoteBlobSizes(context.Background(), cfg, blobs)
	if err == nil || !strings.Contains(err.Error(), "took over") {
		t.Fatalf("err = %v, want the slow request's deadline", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("lookup took %s, want it to end at the 1 s request deadline", took)
	}
	if len(sizes) != 2 || count.Load() != 2 {
		t.Fatalf("kept %d sizes after %d requests, want the first request's 2 after 2", len(sizes), count.Load())
	}
}

// A new snapshot takes the sizes an earlier one knew and asks the remote for
// none of them.
func TestBuildTreeIndexKnowingAsksOnlyForNewSizes(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	bare, want := sizedTestRemote(t)
	url, count := objectInfoServer(t, bare, 0)
	store, cfg := bloblessClone(t, url)
	ctx := context.Background()
	head, _, err := store.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.BuildTreeIndex(ctx, cfg, head)
	if err != nil || count.Load() != 1 {
		t.Fatalf("first index: err %v after %d object-info requests, want 1", err, count.Load())
	}
	known := map[string]int64{}
	for _, n := range first {
		if n.SizeState == "known" && n.ObjectOID != "" && n.Type != "dir" {
			known[n.ObjectOID] = n.SizeBytes
		}
	}

	again, err := store.BuildTreeIndexKnowing(ctx, cfg, head, known)
	if err != nil {
		t.Fatal(err)
	}
	if count.Load() != 1 {
		t.Fatalf("object-info requests = %d, want still 1", count.Load())
	}
	for _, n := range again {
		if size, ok := want[n.Path]; ok && (n.SizeState != "known" || n.SizeBytes != size) {
			t.Errorf("%s = %s/%d, want known/%d", n.Path, n.SizeState, n.SizeBytes, size)
		}
	}
	if got := missingBlobCount(t, cfg.GitDir); got != len(want) {
		t.Fatalf("missing blobs = %d, want all %d (nothing downloaded)", got, len(want))
	}
}
