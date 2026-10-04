package hydrator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/model"
)

// batchingFetcher records each FetchBlobs request.
type batchingFetcher struct {
	*fakeBlobFetcher
	batchMu  sync.Mutex
	batches  [][]string
	batchErr error
}

func (f *batchingFetcher) FetchBlobs(_ context.Context, _ model.RepoConfig, oids []string) (int, error) {
	f.batchMu.Lock()
	defer f.batchMu.Unlock()
	f.batches = append(f.batches, append([]string(nil), oids...))
	return len(oids), f.batchErr
}

// Reads that miss while a worker is busy are fetched together in one
// request, and every reader gets its blob. A failed batch falls back to one
// fetch per blob.
func TestQueuedMissesAreFetchedInOneRequest(t *testing.T) {
	for _, batchErr := range []error{nil, errors.New("remote refused")} {
		t.Run(fmt.Sprint(batchErr), func(t *testing.T) {
			payload := []byte("content")
			cfg := model.RepoConfig{ID: "repo", BlobCacheDir: t.TempDir()}
			node := func(i int) model.BaseNode {
				return model.BaseNode{RepoID: cfg.ID, Path: fmt.Sprintf("f%d", i), ObjectOID: fmt.Sprintf("blob%d", i), SizeState: "known", SizeBytes: int64(len(payload))}
			}
			release := make(chan struct{})
			started := make(chan struct{})
			fetcher := &batchingFetcher{fakeBlobFetcher: &fakeBlobFetcher{payload: payload, fetchStarted: started, fetchWait: release}, batchErr: batchErr}
			h := New(fetcher)
			h.Start(1, cfg)
			defer h.Stop()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			const queued = 20
			errCh := make(chan error, queued+1)
			read := func(i int) {
				_, _, err := h.EnsureHydrated(ctx, cfg, node(i))
				errCh <- err
			}
			go read(0)
			<-started // the only worker is busy with blob0
			for i := 1; i <= queued; i++ {
				go read(i)
			}
			for h.QueueDepth(cfg.ID) < queued {
				if ctx.Err() != nil {
					t.Fatalf("queue depth %d, want %d", h.QueueDepth(cfg.ID), queued)
				}
				time.Sleep(time.Millisecond)
			}
			close(release)
			for range queued + 1 {
				if err := <-errCh; err != nil {
					t.Fatalf("EnsureHydrated: %v", err)
				}
			}
			fetcher.batchMu.Lock()
			defer fetcher.batchMu.Unlock()
			if len(fetcher.batches) != 1 || len(fetcher.batches[0]) != queued {
				sizes := []int{}
				for _, b := range fetcher.batches {
					sizes = append(sizes, len(b))
				}
				t.Fatalf("batch sizes = %v, want one batch of %d", sizes, queued)
			}
			if fetcher.Calls() != queued+1 {
				t.Fatalf("blobs copied to the cache = %d, want %d", fetcher.Calls(), queued+1)
			}
		})
	}
}
