package fskit

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"path"
	"sort"
	"sync"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// A listing is one directory as it was at one verifier. Pages of a listing
// in progress come from the same listing, so other changes cannot skip or
// repeat entries.
type listing struct {
	dirIno    uint64
	parentIno uint64
	verifier  uint64
	children  []listEntry
}

type listEntry struct {
	name string
	ino  uint64
	kind uint8
	attr Attr
	need *model.BaseNode // size still unknown
}

type listKey struct {
	ino      uint64
	verifier uint64
}

// listingCache keeps the most recent listings, oldest evicted first.
type listingCache struct {
	mu    sync.Mutex
	m     map[listKey]*listing
	order []listKey
}

const listingCacheSize = 128

func newListingCache() *listingCache {
	return &listingCache{m: map[listKey]*listing{}}
}

func (c *listingCache) get(k listKey) *listing {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k]
}

func (c *listingCache) put(l *listing) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := listKey{l.dirIno, l.verifier}
	if _, ok := c.m[k]; ok {
		c.m[k] = l
		return
	}
	c.m[k] = l
	c.order = append(c.order, k)
	for len(c.order) > listingCacheSize {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
}

func (c *listingCache) drop(k listKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, k)
}

// verifierFor derives the directory verifier from the snapshot generation and
// the overlay change counter. It is never 0.
func verifierFor(gen int64, changes uint64) uint64 {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(gen))
	binary.BigEndian.PutUint64(b[8:], changes)
	h := fnv.New64a()
	h.Write(b[:])
	if v := h.Sum64(); v != 0 {
		return v
	}
	return 1
}

func (s *Server) currentVerifier() uint64 {
	return verifierFor(s.res.Generation(), s.eng.Overlay.ChangeCount())
}

// buildListing lists dir now. Caller holds s.ns for reading.
func (s *Server) buildListing(ctx context.Context, v inodeView) (*listing, error) {
	// Read the counter first: a change during the listing then yields a new
	// current verifier, so the kernel never trusts a listing older than it.
	changes := s.eng.Overlay.ChangeCount()
	entries, gen, ct, err := s.res.ReaddirSnapshot(ctx, v.path)
	if err != nil {
		return nil, err
	}
	l := &listing{dirIno: v.ino, verifier: verifierFor(gen, changes)}
	if v.ino == inoRoot {
		l.parentIno = inoParentOfRoot
		entries = withSynthesized(entries)
	} else {
		parent := path.Dir(v.path)
		if parent == "" {
			parent = "."
		}
		l.parentIno = s.inodes.assign(model.CleanPath(parent), kindDir, false)
	}
	l.children = make([]listEntry, 0, len(entries))
	for _, e := range entries {
		cp := childPathOf(v.path, e.Name)
		kind := kindOf(e.Type)
		ino := s.inodes.assign(cp, kind, false)
		le := listEntry{name: e.Name, ino: ino, kind: kind}
		switch {
		case v.ino == inoRoot && isSynthesized(e.Name):
			le.attr = s.synthAttr(ino, e.Name)
		case e.FromOverlay:
			le.attr = Attr{Ino: ino, Kind: kind, Mode: wireMode(e.Mode, kind), Size: uint64(max(e.SizeBytes, 0)), MtimeNs: e.MtimeUnixNs, CtimeNs: e.CtimeUnixNs}
			if kind == kindDir && le.attr.Size == 0 {
				le.attr.Size = 4096
			}
		default:
			le.attr, le.need = baseAttr(ino, model.BaseNode{Path: cp, Type: e.Type, Mode: e.Mode, ObjectOID: e.ObjectOID, SizeState: e.SizeState, SizeBytes: e.SizeBytes}, gen, ct)
		}
		l.children = append(l.children, le)
	}
	return l, nil
}

// withSynthesized adds .git and .metadata_never_index to the root listing,
// replacing any real entry of the same name, and keeps it sorted.
func withSynthesized(entries []fusefs.ReaddirEntry) []fusefs.ReaddirEntry {
	out := make([]fusefs.ReaddirEntry, 0, len(entries)+2)
	for _, e := range entries {
		if !isSynthesized(e.Name) {
			out = append(out, e)
		}
	}
	out = append(out,
		fusefs.ReaddirEntry{Name: gitfileName, Type: "file", Mode: 0o444, SizeState: "known"},
		fusefs.ReaddirEntry{Name: neverIndexName, Type: "file", Mode: 0o444, SizeState: "known"},
	)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Server) opReaddir(ctx context.Context, req *request) (*reply, error) {
	limit := int(req.MaxEntries)
	if limit == 0 {
		limit = defaultEntries
	}
	limit = min(limit, maxEntries)

	s.ns.RLock()
	v, err := s.node(req.Ino)
	if err != nil {
		s.ns.RUnlock()
		return nil, err
	}
	var l *listing
	switch {
	case v.removed != nil:
		s.ns.RUnlock()
		if v.kind != kindDir {
			return nil, wireErr(errENOTDIR)
		}
		// A removed directory is empty for whoever still holds it.
		l = &listing{dirIno: v.ino, parentIno: v.ino, verifier: 1}
	default:
		if v.kind != kindDir {
			s.ns.RUnlock()
			return nil, wireErr(errENOTDIR)
		}
		if v.ino != inoRoot {
			n, err := s.res.ResolvePath(v.path)
			if err != nil {
				s.ns.RUnlock()
				return nil, err
			}
			if n.Type() != "dir" {
				s.ns.RUnlock()
				return nil, wireErr(errENOTDIR)
			}
		}
		if req.Cookie != 0 {
			l = s.lists.get(listKey{v.ino, req.Verifier})
		}
		if l == nil {
			if req.Cookie != 0 && req.Verifier != s.currentVerifier() {
				s.ns.RUnlock()
				return nil, wireErr(errBadCookie)
			}
			l, err = s.buildListing(ctx, v)
			if err != nil {
				s.ns.RUnlock()
				return nil, err
			}
			if req.Cookie != 0 && l.verifier != req.Verifier {
				s.ns.RUnlock()
				return nil, wireErr(errBadCookie)
			}
			s.lists.put(l)
		}
		s.ns.RUnlock()
	}

	// Positions: 0 ".", 1 "..", 2+i child i.
	total := uint64(len(l.children)) + 2
	pos := req.Cookie
	if req.WithAttrs && pos < 2 {
		pos = 2
	}
	if pos > total {
		return nil, wireErr(errBadCookie)
	}
	entries := make([]DirEntry, 0, min(limit, int(total-pos)))
	var needs []int
	for ; pos < total && len(entries) < limit; pos++ {
		switch pos {
		case 0:
			entries = append(entries, DirEntry{Name: ".", Ino: l.dirIno, Kind: kindDir, NextCookie: 1})
		case 1:
			entries = append(entries, DirEntry{Name: "..", Ino: l.parentIno, Kind: kindDir, NextCookie: 2})
		default:
			c := l.children[pos-2]
			e := DirEntry{Name: c.name, Ino: c.ino, Kind: c.kind, NextCookie: pos + 1}
			if req.WithAttrs {
				a := c.attr
				e.Attr = &a
				if c.need != nil {
					needs = append(needs, len(entries))
				}
			}
			entries = append(entries, e)
		}
	}
	if err := s.fillListingSizes(ctx, l, entries, needs); err != nil {
		return nil, err
	}
	eof := pos >= total
	return &reply{Entries: &entries, Verifier: l.verifier, EOF: &eof}, nil
}

// fillListingSizes fills the sizes the snapshot lacks for one page, without
// holding the namespace lock: first with one remote lookup for the whole
// page, then, for any the remote did not answer, by downloading the blobs a
// few at a time.
func (s *Server) fillListingSizes(ctx context.Context, l *listing, entries []DirEntry, needs []int) error {
	if len(needs) == 0 {
		return nil
	}
	if s.eng != nil && s.eng.Sizes != nil {
		oids := make([]string, 0, len(needs))
		for _, idx := range needs {
			oids = append(oids, l.children[entries[idx].NextCookie-3].need.ObjectOID)
		}
		sizes := s.eng.Sizes.BlobSizes(ctx, oids)
		rest := needs[:0]
		for _, idx := range needs {
			if size, ok := sizes[l.children[entries[idx].NextCookie-3].need.ObjectOID]; ok {
				entries[idx].Attr.Size = uint64(size)
			} else {
				rest = append(rest, idx)
			}
		}
		needs = rest
	}
	sem := make(chan struct{}, sizeFillWorkers)
	var wg sync.WaitGroup
	errs := make([]error, len(needs))
	for i, idx := range needs {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			child := l.children[entries[idx].NextCookie-3]
			errs[i] = s.downloadSize(ctx, entries[idx].Attr, child.need)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
