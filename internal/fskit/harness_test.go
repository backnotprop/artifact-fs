package fskit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/gitstore"
	"github.com/cloudflare/artifact-fs/internal/hydrator"
	"github.com/cloudflare/artifact-fs/internal/model"
	"github.com/cloudflare/artifact-fs/internal/overlay"
	"github.com/cloudflare/artifact-fs/internal/snapshot"
)

// harness runs a Server over a real stack: a local git remote, a blobless
// clone, a published snapshot, an overlay and a hydrator with workers.
type harness struct {
	t     *testing.T
	srv   *Server
	sock  string
	repo  model.RepoConfig
	ov    *overlay.Store
	h     *hydrator.Service
	git   *gitstore.Store
	snap  *snapshot.Store
	res   *fusefs.Resolver
	gen   int64
	work  string            // the remote's working copy
	files map[string]string // base files and their content
}

type harnessOptions struct {
	readOnly bool
	wrap     func(model.Hydrator) model.Hydrator
	extra    func(work string) // add files to the remote before it is pushed
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "remote.git")
	work := filepath.Join(tmp, "work")
	gitRun(t, tmp, "init", "--bare", "-b", "main", bare)
	gitRun(t, tmp, "init", "-b", "main", work)

	files := map[string]string{
		"README.md":      "# test repo\n",
		"src/main.go":    "package main\n\nfunc main() {}\n",
		"src/util.go":    "package main\n",
		"bin/run.sh":     "#!/bin/sh\necho hi\n",
		"docs/guide.md":  strings.Repeat("guide line\n", 500),
		"empty/.keep":    "",
		"nested/a/b.txt": "deep\n",
	}
	for i := 0; i < 40; i++ {
		files[fmt.Sprintf("many/f%03d.txt", i)] = fmt.Sprintf("file %d\n", i)
	}
	for p, c := range files {
		full := filepath.Join(work, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(work, "bin", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("README.md", filepath.Join(work, "link")); err != nil {
		t.Fatal(err)
	}
	if opts.extra != nil {
		opts.extra(work)
	}
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "commit", "-q", "-m", "seed")
	gitRun(t, work, "remote", "add", "origin", bare)
	gitRun(t, work, "push", "-q", "origin", "main")
	gitRun(t, bare, "config", "uploadpack.allowFilter", "true")

	state := filepath.Join(tmp, "state")
	cfg := model.RepoConfig{
		ID:            "repo",
		Name:          "repo",
		RemoteURL:     "file://" + bare,
		Branch:        "main",
		MountPath:     filepath.Join(state, "mnt"),
		GitDir:        filepath.Join(state, "git"),
		OverlayDir:    filepath.Join(state, "overlay"),
		BlobCacheDir:  filepath.Join(state, "blobs"),
		MetaDBPath:    filepath.Join(state, "meta.sqlite"),
		OverlayDBPath: filepath.Join(state, "overlay", "meta.sqlite"),
	}
	for _, d := range []string{cfg.OverlayDir, cfg.BlobCacheDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git := gitstore.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(git.Close)
	if err := git.CloneBlobless(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	oid, ref, err := git.ResolveHEAD(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := git.BuildTreeIndex(ctx, cfg, oid)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := snapshot.New(ctx, cfg.MetaDBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { snap.Close() })
	gen, err := snap.PublishGeneration(ctx, oid, ref, nodes)
	if err != nil {
		t.Fatal(err)
	}
	ov, err := overlay.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ov.Close() })
	h := hydrator.New(git)
	h.Start(2, cfg)
	t.Cleanup(h.Stop)
	var hyd model.Hydrator = h
	if opts.wrap != nil {
		hyd = opts.wrap(h)
	}
	resolver := &fusefs.Resolver{Snapshot: snap, Overlay: ov}
	resolver.SetGeneration(gen)
	if ts, err := git.CommitTimestamp(ctx, cfg, oid); err == nil {
		resolver.SetCommitTime(ts)
	}
	engine := &fusefs.Engine{Resolver: resolver, Repo: cfg, Overlay: ov, Hydrator: hyd}
	srv := NewServer(Config{
		Repo:       cfg,
		Resolver:   resolver,
		Engine:     engine,
		ReadOnly:   opts.readOnly,
		MountPoint: cfg.MountPath,
		Build:      "test",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// A short folder: unix socket paths are limited to 104 bytes on macOS.
	sockDir, err := os.MkdirTemp("", "fsk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, SocketName)
	ln, err := ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return &harness{t: t, srv: srv, sock: sock, repo: cfg, ov: ov, h: h, git: git, snap: snap, res: resolver, gen: gen, work: work, files: files}
}

// wireClient is an in-process client of the wire, built the way the
// extension and broker use it: one persistent connection, many requests in
// flight, replies matched by id.
type wireClient struct {
	t       *testing.T
	conn    net.Conn
	wmu     sync.Mutex
	mu      sync.Mutex
	next    uint64
	pending map[uint64]chan wireResult
	closed  chan struct{}
}

type wireResult struct {
	rep     reply
	payload []byte
}

func (h *harness) client() *wireClient {
	h.t.Helper()
	conn, err := net.Dial("unix", h.sock)
	if err != nil {
		h.t.Fatal(err)
	}
	c := &wireClient{t: h.t, conn: conn, pending: map[uint64]chan wireResult{}, closed: make(chan struct{})}
	go c.readLoop()
	h.t.Cleanup(func() { conn.Close() })
	return c
}

func (c *wireClient) readLoop() {
	defer close(c.closed)
	r := bufio.NewReader(c.conn)
	for {
		hdr, payload, err := readFrame(r)
		if err != nil {
			return
		}
		var rep reply
		if err := json.Unmarshal(hdr, &rep); err != nil {
			return
		}
		c.mu.Lock()
		ch := c.pending[rep.ID]
		delete(c.pending, rep.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- wireResult{rep: rep, payload: payload}
		}
	}
}

// call sends one request and waits for its reply.
func (c *wireClient) call(fields map[string]any, payload []byte) (reply, []byte) {
	c.t.Helper()
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan wireResult, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	fields["id"] = id
	hdr, err := json.Marshal(fields)
	if err != nil {
		c.t.Fatal(err)
	}
	c.wmu.Lock()
	err = writeFrame(c.conn, hdr, payload)
	c.wmu.Unlock()
	if err != nil {
		c.t.Fatalf("send %v: %v", fields["op"], err)
	}
	select {
	case res := <-ch:
		if res.rep.ID != id {
			c.t.Fatalf("reply id %d, want %d", res.rep.ID, id)
		}
		return res.rep, res.payload
	case <-c.closed:
		c.t.Fatalf("connection closed while waiting for %v", fields["op"])
	case <-time.After(30 * time.Second):
		c.t.Fatalf("no reply to %v", fields["op"])
	}
	return reply{}, nil
}

func (c *wireClient) lookup(parent uint64, name string) (Attr, int32) {
	c.t.Helper()
	rep, _ := c.call(map[string]any{"op": "lookup", "parent": parent, "name": name}, nil)
	if rep.Attr == nil {
		return Attr{}, rep.Errno
	}
	return *rep.Attr, rep.Errno
}

func (c *wireClient) mustLookup(parent uint64, name string) Attr {
	c.t.Helper()
	a, errno := c.lookup(parent, name)
	if errno != 0 {
		c.t.Fatalf("lookup %q: errno %d", name, errno)
	}
	return a
}

func (c *wireClient) getattr(ino uint64) (Attr, int32) {
	c.t.Helper()
	rep, _ := c.call(map[string]any{"op": "getattr", "ino": ino}, nil)
	if rep.Attr == nil {
		return Attr{}, rep.Errno
	}
	return *rep.Attr, rep.Errno
}

func (c *wireClient) create(op string, parent uint64, name string, extra map[string]any) (Attr, int32) {
	c.t.Helper()
	fields := map[string]any{"op": op, "parent": parent, "name": name}
	for k, v := range extra {
		fields[k] = v
	}
	rep, _ := c.call(fields, nil)
	if rep.Attr == nil {
		return Attr{}, rep.Errno
	}
	return *rep.Attr, rep.Errno
}

func (c *wireClient) mustCreate(parent uint64, name string, content string) Attr {
	c.t.Helper()
	a, errno := c.create("create", parent, name, map[string]any{"mode": 0o644})
	if errno != 0 {
		c.t.Fatalf("create %q: errno %d", name, errno)
	}
	if content != "" {
		c.mustWrite(a.Ino, 0, []byte(content))
	}
	return a
}

func (c *wireClient) mustMkdir(parent uint64, name string) Attr {
	c.t.Helper()
	a, errno := c.create("mkdir", parent, name, map[string]any{"mode": 0o755})
	if errno != 0 {
		c.t.Fatalf("mkdir %q: errno %d", name, errno)
	}
	return a
}

func (c *wireClient) read(ino uint64, off int64, size int) ([]byte, int32) {
	c.t.Helper()
	rep, payload := c.call(map[string]any{"op": "read", "ino": ino, "offset": off, "size": size}, nil)
	return payload, rep.Errno
}

func (c *wireClient) readAll(ino uint64) string {
	c.t.Helper()
	var out bytes.Buffer
	const chunk = 1 << 20
	for off := int64(0); ; off += chunk {
		data, errno := c.read(ino, off, chunk)
		if errno != 0 {
			c.t.Fatalf("read ino %d: errno %d", ino, errno)
		}
		out.Write(data)
		if len(data) < chunk {
			return out.String()
		}
	}
}

func (c *wireClient) write(ino uint64, off int64, data []byte) (reply, int32) {
	c.t.Helper()
	rep, _ := c.call(map[string]any{"op": "write", "ino": ino, "offset": off}, data)
	return rep, rep.Errno
}

func (c *wireClient) mustWrite(ino uint64, off int64, data []byte) {
	c.t.Helper()
	rep, errno := c.write(ino, off, data)
	if errno != 0 {
		c.t.Fatalf("write ino %d: errno %d", ino, errno)
	}
	if rep.Written == nil || int(*rep.Written) != len(data) {
		c.t.Fatalf("write ino %d: written %v, want %d", ino, rep.Written, len(data))
	}
}

func (c *wireClient) op(op string, fields map[string]any) int32 {
	c.t.Helper()
	fields["op"] = op
	rep, _ := c.call(fields, nil)
	return rep.Errno
}

func (c *wireClient) unlink(parent uint64, name string) int32 {
	return c.op("unlink", map[string]any{"parent": parent, "name": name})
}

func (c *wireClient) rmdir(parent uint64, name string) int32 {
	return c.op("rmdir", map[string]any{"parent": parent, "name": name})
}

func (c *wireClient) rename(parent uint64, name string, newParent uint64, newName string) int32 {
	return c.op("rename", map[string]any{"parent": parent, "name": name, "newparent": newParent, "newname": newName})
}

func (c *wireClient) forget(inos ...uint64) {
	c.t.Helper()
	if errno := c.op("forget", map[string]any{"inos": inos}); errno != 0 {
		c.t.Fatalf("forget: errno %d", errno)
	}
}

// listAll pages through a directory and returns the entries in order.
func (c *wireClient) listAll(ino uint64, page int, withAttrs bool) ([]DirEntry, uint64) {
	c.t.Helper()
	var out []DirEntry
	cookie, verifier := uint64(0), uint64(0)
	for {
		rep, _ := c.call(map[string]any{"op": "readdir", "ino": ino, "cookie": cookie, "verifier": verifier, "max_entries": page, "with_attrs": withAttrs}, nil)
		if rep.Errno != 0 {
			c.t.Fatalf("readdir ino %d cookie %d: errno %d", ino, cookie, rep.Errno)
		}
		if verifier != 0 && rep.Verifier != verifier {
			c.t.Fatalf("verifier changed during a listing: %d then %d", verifier, rep.Verifier)
		}
		verifier = rep.Verifier
		if rep.Entries == nil || rep.EOF == nil {
			c.t.Fatalf("readdir reply without entries or eof: %+v", rep)
		}
		entries := *rep.Entries
		out = append(out, entries...)
		if *rep.EOF {
			return out, verifier
		}
		if len(entries) == 0 {
			c.t.Fatal("readdir returned no entries and no eof")
		}
		cookie = entries[len(entries)-1].NextCookie
	}
}

func names(entries []DirEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}
