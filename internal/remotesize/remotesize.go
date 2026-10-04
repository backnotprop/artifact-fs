// Package remotesize answers the file sizes a mount's snapshot lacks from
// the remote's object-info, so listing or stat never downloads a file just
// to learn its size (2026-10-04: without it one ls of a folder downloaded its
// 56 files). What it learns is saved in the snapshot, so each is asked once.
//
// Two paths use it: a folder listing asks for its page's sizes at once
// (BlobSizes), and Run asks for every size the snapshot still lacks, 1,000
// at a time, in the background after a mount and after each new snapshot.
package remotesize

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

const (
	// lookupsAtOnce is how many lookups run at once (each is one git
	// remote helper and one request to the remote).
	lookupsAtOnce = 4
	// callTimeout bounds one lookup (a folder's page, or one fill batch).
	callTimeout = 30 * time.Second
	// quietAfterFailure is how long after a failed lookup a listing stops
	// asking and downloads instead, so a down remote is not asked per file.
	quietAfterFailure = 30 * time.Second
	// fillBatch is how many ids one background request asks for: Pando's
	// door sizes 1,000 cold blobs from one fetch in about 1.7 s.
	fillBatch = 1000
	// fillRetryFirst and fillRetryMax space the background fill's retries
	// after a failed request.
	fillRetryFirst = 30 * time.Second
	fillRetryMax   = 5 * time.Minute
)

// Source is a fusefs.SizeSource for one mounted repo.
type Source struct {
	git    *gitstore.Store
	repo   model.RepoConfig
	snap   *snapshot.Store
	gen    func() int64
	logger *slog.Logger
	slots  chan struct{}
	kick   chan struct{}

	mu          sync.Mutex
	unsupported bool
	quietUntil  time.Time
	// unanswered holds ids the remote answered without a size; the fill
	// does not ask for them again.
	unanswered map[string]bool
}

// New returns the source for repo; gen is the mount's current generation.
func New(git *gitstore.Store, repo model.RepoConfig, snap *snapshot.Store, gen func() int64, logger *slog.Logger) *Source {
	return &Source{
		git: git, repo: repo, snap: snap, gen: gen, logger: logger,
		slots: make(chan struct{}, lookupsAtOnce), kick: make(chan struct{}, 1),
		unanswered: map[string]bool{},
	}
}

// BlobSizes asks the remote for these blobs' sizes and saves what it learns.
// It returns nothing, without asking, for a while after a failed lookup.
func (r *Source) BlobSizes(ctx context.Context, oids []string) map[string]int64 {
	if !r.asking() {
		return nil
	}
	sizes, _ := r.lookup(ctx, oids)
	return sizes
}

// Kick asks Run for another pass, after a new snapshot is published.
func (r *Source) Kick() {
	if r == nil {
		return
	}
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run fills, in the background, every size the current snapshot lacks: a
// pass now and one after each Kick, until ctx ends.
func (r *Source) Run(ctx context.Context) {
	if r == nil {
		return
	}
	r.Kick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.kick:
		}
		r.fill(ctx)
	}
}

// fill asks for the current generation's unknown sizes, fillBatch at a
// time, until none is left to ask, the remote cannot answer, or a newer
// generation is published (its own Kick starts a fresh pass).
func (r *Source) fill(ctx context.Context) {
	gen := r.gen()
	oids, err := r.snap.UnknownSizeOIDs(ctx, gen)
	if err != nil {
		if ctx.Err() == nil && r.logger != nil {
			r.logger.Warn("listing unknown sizes failed", "repo", r.repo.Name, "error", err)
		}
		return
	}
	todo := r.askable(oids)
	if len(todo) == 0 {
		return
	}
	start, learned := time.Now(), 0
	retry := fillRetryFirst
	for len(todo) > 0 && ctx.Err() == nil && r.gen() == gen {
		batch := todo[:min(fillBatch, len(todo))]
		sizes, err := r.lookup(ctx, batch)
		learned += len(sizes)
		if err != nil {
			if r.isUnsupported() || ctx.Err() != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(retry):
			}
			retry = min(2*retry, fillRetryMax)
			continue
		}
		retry = fillRetryFirst
		r.mu.Lock()
		for _, oid := range batch {
			if _, ok := sizes[oid]; !ok {
				r.unanswered[oid] = true
			}
		}
		r.mu.Unlock()
		todo = todo[len(batch):]
	}
	if r.logger != nil && learned > 0 {
		r.logger.Info("file sizes filled from the remote", "repo", r.repo.Name, "learned", learned, "asked", len(oids), "seconds", time.Since(start).Round(time.Millisecond).Seconds())
	}
}

// lookup asks the remote for oids' sizes, at most lookupsAtOnce at a time,
// and saves what it learns in the current generation.
func (r *Source) lookup(ctx context.Context, oids []string) (map[string]int64, error) {
	if len(oids) == 0 {
		return nil, nil
	}
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-r.slots }()

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	sizes, err := r.git.RemoteBlobSizes(callCtx, r.repo, oids)
	if err != nil && ctx.Err() == nil {
		r.mu.Lock()
		if gitstore.IsObjectInfoUnsupported(err) {
			r.unsupported = true
		} else {
			r.quietUntil = time.Now().Add(quietAfterFailure)
		}
		r.mu.Unlock()
		if r.logger != nil && !gitstore.IsObjectInfoUnsupported(err) {
			r.logger.Warn("remote size lookup failed", "repo", r.repo.Name, "asked", len(oids), "learned", len(sizes), "error", auth.RedactString(err.Error()))
		}
	}
	if len(sizes) > 0 {
		if err := r.snap.UpdateSizes(context.WithoutCancel(ctx), r.gen(), sizes); err != nil && r.logger != nil {
			r.logger.Warn("saving learned sizes failed", "repo", r.repo.Name, "error", err)
		}
	}
	return sizes, err
}

func (r *Source) askable(oids []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unsupported {
		return nil
	}
	out := oids[:0]
	for _, oid := range oids {
		if !r.unanswered[oid] {
			out = append(out, oid)
		}
	}
	return out
}

func (r *Source) isUnsupported() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unsupported
}

func (r *Source) asking() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.unsupported && time.Now().After(r.quietUntil)
}
