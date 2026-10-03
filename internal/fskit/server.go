package fskit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// Config is what a Server serves.
type Config struct {
	Repo     model.RepoConfig
	Resolver *fusefs.Resolver
	Engine   *fusefs.Engine
	// Gate, when set, holds every operation except hello, forget and a root
	// getattr until the repo is prepared (async add-repo).
	Gate *fusefs.ReadyGate
	// ReadOnly refuses every change with EROFS, except the daemon's own
	// write-through changes (WriteThrough).
	ReadOnly bool
	// MountPoint is where the volume is mounted; WriteThrough writes there.
	MountPoint string
	// Build is reported in the hello reply.
	Build  string
	Logger *slog.Logger
}

// Server serves the Pando FSKit wire for one mounted repo.
type Server struct {
	cfg     Config
	log     *slog.Logger
	res     *fusefs.Resolver
	eng     *fusefs.Engine
	env     blobEnv
	inodes  *inodeTable
	lists   *listingCache
	allow   *allowList
	gitfile []byte
	started int64

	// ns orders namespace changes. unlink, rmdir and rename take it for
	// writing, because they move or retire item numbers; every other
	// operation takes it for reading, and nothing slow (a blob download)
	// runs while it is held.
	ns sync.RWMutex

	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	lns    map[net.Listener]struct{}
	conns  map[*conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// NewServer returns a server for cfg. Call Serve with a listener.
func NewServer(cfg Config) *Server {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		cfg:     cfg,
		log:     log,
		res:     cfg.Resolver,
		eng:     cfg.Engine,
		env:     blobEnv{hydrator: cfg.Engine.Hydrator, repo: cfg.Repo, scratchDir: scratchDirFor(cfg.Repo)},
		inodes:  newInodeTable(),
		lists:   newListingCache(),
		allow:   newAllowList(),
		gitfile: fmt.Appendf(nil, "gitdir: %s\n", cfg.Repo.GitDir),
		started: time.Now().UnixNano(),
		ctx:     ctx,
		cancel:  cancel,
		lns:     map[net.Listener]struct{}{},
		conns:   map[*conn]struct{}{},
	}
}

// Serve accepts connections until the listener closes. Each connection is
// persistent: one reader goroutine, one goroutine per request, replies in
// any order under a write lock.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return net.ErrClosed
	}
	s.lns[ln] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.lns, ln)
		s.mu.Unlock()
	}()
	for {
		nc, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		c := &conn{nc: nc}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			nc.Close()
			return nil
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serveConn(c)
	}
}

// Close stops listening, drops connections, waits for requests in flight,
// and frees every removed item.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	for ln := range s.lns {
		ln.Close()
	}
	for c := range s.conns {
		c.nc.Close()
	}
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
	s.inodes.closeAll()
	return nil
}

type conn struct {
	nc      net.Conn
	writeMu sync.Mutex
	reqs    sync.WaitGroup
}

func (c *conn) send(rep *reply, payload []byte) error {
	hdr, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeFrame(c.nc, hdr, payload)
}

func (s *Server) serveConn(c *conn) {
	defer s.wg.Done()
	defer func() {
		c.nc.Close()
		c.reqs.Wait()
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}()
	r := bufio.NewReaderSize(c.nc, 64<<10)
	for {
		header, payload, err := readFrame(r)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.log.Warn("fskit: closing connection after a bad frame", "error", err)
			}
			return
		}
		req := new(request)
		if err := json.Unmarshal(header, req); err != nil {
			s.log.Warn("fskit: closing connection after a header that is not a JSON object", "error", err)
			return
		}
		c.reqs.Add(1)
		go func() {
			defer c.reqs.Done()
			rep, data := s.handle(req, payload)
			rep.ID = req.ID
			if err := c.send(rep, data); err != nil && !errors.Is(err, net.ErrClosed) {
				s.log.Warn("fskit: reply failed", "op", req.Op, "error", err)
			}
		}()
	}
}

// handle runs one request and never panics into the connection.
func (s *Server) handle(req *request, payload []byte) (rep *reply, data []byte) {
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("fskit: operation panicked", "op", req.Op, "panic", p)
			rep, data = &reply{Errno: errEIO}, nil
		}
	}()
	if s.cfg.Gate != nil && req.Op != "hello" && req.Op != "forget" && !(req.Op == "getattr" && req.Ino == inoRoot) {
		if err := s.cfg.Gate.Wait(s.ctx); err != nil {
			return &reply{Errno: errEIO}, nil
		}
	}
	ctx := s.ctx
	var err error
	switch req.Op {
	case "hello":
		rep = s.opHello(req)
	case "lookup":
		rep, err = s.opLookup(ctx, req)
	case "getattr":
		rep, err = s.opGetattr(ctx, req)
	case "readdir":
		rep, err = s.opReaddir(ctx, req)
	case "read":
		data, err = s.opRead(ctx, req)
		rep = &reply{}
	case "write":
		rep, err = s.opWrite(ctx, req, payload)
	case "readlink":
		rep, err = s.opReadlink(ctx, req)
	case "create":
		rep, err = s.opCreate(ctx, req, kindFile)
	case "mkdir":
		rep, err = s.opCreate(ctx, req, kindDir)
	case "symlink":
		rep, err = s.opCreate(ctx, req, kindSymlink)
	case "unlink":
		err = s.opRemove(ctx, req, false)
		rep = &reply{}
	case "rmdir":
		err = s.opRemove(ctx, req, true)
		rep = &reply{}
	case "rename":
		err = s.opRename(ctx, req)
		rep = &reply{}
	case "setattr":
		rep, err = s.opSetattr(ctx, req)
	case "forget":
		for _, ino := range req.Inos {
			s.inodes.forget(ino)
		}
		rep = &reply{}
	default:
		return &reply{Errno: errENOSYS}, nil
	}
	if err != nil {
		errno := errnoFor(err)
		if errno == errEIO {
			s.log.Warn("fskit: operation failed", "op", req.Op, "error", err)
		}
		return &reply{Errno: errno}, nil
	}
	return rep, data
}

// allowList holds the paths the daemon is writing through the mount right
// now. A read-only mount accepts changes to these paths only.
type allowList struct {
	mu    sync.Mutex
	paths map[string]int
}

func newAllowList() *allowList { return &allowList{paths: map[string]int{}} }

func (a *allowList) add(paths []string) {
	a.mu.Lock()
	for _, p := range paths {
		a.paths[p]++
	}
	a.mu.Unlock()
}

func (a *allowList) remove(paths []string) {
	a.mu.Lock()
	for _, p := range paths {
		if a.paths[p] <= 1 {
			delete(a.paths, p)
		} else {
			a.paths[p]--
		}
	}
	a.mu.Unlock()
}

func (a *allowList) has(path string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.paths[path] > 0
}

func (s *Server) checkWritable(paths ...string) error {
	if !s.cfg.ReadOnly {
		return nil
	}
	for _, p := range paths {
		if !s.allow.has(p) {
			return wireErr(errEROFS)
		}
	}
	return nil
}
