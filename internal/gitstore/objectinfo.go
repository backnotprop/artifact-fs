package gitstore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/cloudflare/artifact-fs/internal/auth"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// Blob sizes from the remote.
//
// A blobless clone has trees but no blob sizes, and the mount must report a
// size for every file it lists or stats. Without one, getattr downloads the
// blob just to learn its size. Git protocol v2 has a command for exactly this,
// `object-info` (gitprotocol-v2, "object-info"), which answers sizes for
// object IDs without sending contents. When the remote advertises it, the
// snapshot asks once per build for every blob it lacks a size for.
//
// Transports: smart HTTP(S) through git's own remote helper (so credential
// helpers, extra headers and proxies work as for fetch), and local or file://
// remotes through `git upload-pack`. Other remotes keep sizes unknown, and
// the mount learns them by hydrating, as before.

var errObjectInfoUnsupported = errors.New("remote does not offer object-info")

// objectInfoBatch caps the object IDs in one request (about 460 KiB), under
// git's default http.postBuffer so each request is one plain POST.
const objectInfoBatch = 10000

// remoteSizeTimeout bounds the whole size lookup so a slow remote delays a
// snapshot by at most this long; sizes then resolve on demand.
const remoteSizeTimeout = 2 * time.Minute

const (
	pktFlush       = "0000"
	pktDelim       = "0001"
	pktResponseEnd = "0002"
)

// v2Session is one protocol v2 conversation with the remote's upload-pack.
type v2Session interface {
	capabilities() []string
	// call sends one request and returns the response lines up to its flush.
	call(ctx context.Context, request []byte) ([]string, error)
	close() error
}

// remoteBlobSizes returns sizes for the given blob IDs from the remote. A size
// the remote does not know is left out of the map.
func (s *Store) remoteBlobSizes(ctx context.Context, repo model.RepoConfig, oids []string) (map[string]int64, error) {
	if len(oids) == 0 {
		return nil, nil
	}
	remote := strings.TrimSpace(repo.RemoteURL)
	if remote == "" {
		out, err := runGit(ctx, repo.GitDir, "remote", "get-url", "origin")
		if err != nil {
			return nil, errObjectInfoUnsupported
		}
		remote = strings.TrimSpace(out)
	}
	safeURL, credEnv, err := credentialEnv(remote)
	if err != nil {
		return nil, err
	}
	session, err := openV2Session(ctx, repo.GitDir, safeURL, append(nonInteractiveGitEnv(), credEnv...))
	if err != nil {
		return nil, err
	}
	defer session.close()
	objectFormat := ""
	supported := false
	for _, c := range session.capabilities() {
		if c == "object-info" || strings.HasPrefix(c, "object-info=") {
			supported = true
		}
		if v, ok := strings.CutPrefix(c, "object-format="); ok {
			objectFormat = v
		}
	}
	if !supported {
		return nil, errObjectInfoUnsupported
	}
	sizes := make(map[string]int64, len(oids))
	for start := 0; start < len(oids); start += objectInfoBatch {
		batch := oids[start:min(start+objectInfoBatch, len(oids))]
		lines, err := session.call(ctx, objectInfoRequest(objectFormat, batch))
		if err != nil {
			return nil, err
		}
		if err := parseObjectInfo(lines, sizes); err != nil {
			return nil, err
		}
	}
	return sizes, nil
}

func objectInfoRequest(objectFormat string, oids []string) []byte {
	var b bytes.Buffer
	writePkt(&b, "command=object-info\n")
	if objectFormat != "" {
		writePkt(&b, "object-format="+objectFormat+"\n")
	}
	b.WriteString(pktDelim)
	writePkt(&b, "size\n")
	for _, oid := range oids {
		writePkt(&b, "oid "+oid+"\n")
	}
	b.WriteString(pktFlush)
	return b.Bytes()
}

// parseObjectInfo reads "size" and then one "<oid> <size>" line per object.
// An object the remote lacks comes back with an empty size.
func parseObjectInfo(lines []string, sizes map[string]int64) error {
	if len(lines) == 0 {
		return nil
	}
	if lines[0] != "size" {
		return fmt.Errorf("object-info: unexpected attributes %q", lines[0])
	}
	for _, line := range lines[1:] {
		oid, sizeStr, ok := strings.Cut(line, " ")
		if !ok {
			return fmt.Errorf("object-info: malformed line %q", line)
		}
		if sizeStr == "" {
			continue
		}
		size, err := strconv.ParseInt(sizeStr, 10, 64)
		if err != nil || size < 0 {
			return fmt.Errorf("object-info: malformed size in %q", line)
		}
		sizes[oid] = size
	}
	return nil
}

func openV2Session(ctx context.Context, gitDir string, remote string, env []string) (v2Session, error) {
	if path, ok := localRemotePath(remote); ok {
		return openLocalSession(ctx, path, env)
	}
	u, err := url.Parse(remote)
	if err != nil {
		return nil, errObjectInfoUnsupported
	}
	switch scheme := strings.ToLower(u.Scheme); scheme {
	case "http", "https":
		return openHelperSession(ctx, gitDir, scheme, remote, env)
	default:
		return nil, errObjectInfoUnsupported
	}
}

func localRemotePath(remote string) (string, bool) {
	if strings.HasPrefix(remote, "/") {
		return remote, true
	}
	if rest, ok := strings.CutPrefix(remote, "file://"); ok {
		path, err := url.PathUnescape(rest)
		if err != nil || !strings.HasPrefix(path, "/") {
			return "", false
		}
		return path, true
	}
	return "", false
}

// helperSession speaks to `git remote-http(s)` with the stateless-connect
// command, the way git itself runs protocol v2 over HTTP.
type helperSession struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *boundedGitError
	caps   []string
}

func openHelperSession(ctx context.Context, gitDir string, scheme string, remote string, env []string) (*helperSession, error) {
	cmd := exec.CommandContext(ctx, "git", "-c", "protocol.version=2", "remote-"+scheme, "origin", remote)
	configureCancelableCommand(cmd)
	cmd.Env = append(append(os.Environ(), "GIT_DIR="+gitDir), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	h := &helperSession{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), stderr: &boundedGitError{limit: maxGitErrorBytes}}
	cmd.Stderr = h.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if _, err := io.WriteString(stdin, "stateless-connect git-upload-pack\n"); err != nil {
		return nil, h.fail(err)
	}
	reply, err := h.stdout.ReadString('\n')
	if err != nil {
		return nil, h.fail(err)
	}
	if reply != "\n" {
		_ = h.close()
		return nil, errObjectInfoUnsupported // "fallback": the server only speaks protocol v0
	}
	h.caps, _, err = readPktLines(h.stdout)
	if err != nil {
		return nil, h.fail(err)
	}
	return h, nil
}

func (h *helperSession) capabilities() []string { return h.caps }

func (h *helperSession) call(_ context.Context, request []byte) ([]string, error) {
	if _, err := h.stdin.Write(request); err != nil {
		return nil, h.fail(err)
	}
	lines, end, err := readPktLines(h.stdout)
	if err != nil {
		return nil, h.fail(err)
	}
	if end != pktFlush {
		return nil, h.fail(fmt.Errorf("object-info: response ended with %s", end))
	}
	// The helper marks the end of each HTTP response with a response-end packet.
	if _, end, err = readPktLines(h.stdout); err != nil || end != pktResponseEnd {
		return nil, h.fail(fmt.Errorf("object-info: missing response end: %v", err))
	}
	return lines, nil
}

func (h *helperSession) fail(err error) error {
	_ = h.close()
	if msg := strings.TrimSpace(h.stderr.String()); msg != "" {
		return fmt.Errorf("%w: %s", err, auth.RedactString(msg))
	}
	return err
}

func (h *helperSession) close() error {
	_ = h.stdin.Close()
	return h.cmd.Wait()
}

// localSession runs `git upload-pack` against a repository on this machine,
// one process per request, as smart HTTP would.
type localSession struct {
	path string
	env  []string
	caps []string
}

func openLocalSession(ctx context.Context, path string, env []string) (*localSession, error) {
	l := &localSession{path: path, env: env}
	out, err := l.run(ctx, nil, "--advertise-refs")
	if err != nil {
		return nil, err
	}
	l.caps, _, err = readPktLines(bufio.NewReader(bytes.NewReader(out)))
	if err != nil {
		return nil, err
	}
	return l, nil
}

func (l *localSession) capabilities() []string { return l.caps }

func (l *localSession) call(ctx context.Context, request []byte) ([]string, error) {
	out, err := l.run(ctx, request)
	if err != nil {
		return nil, err
	}
	lines, end, err := readPktLines(bufio.NewReader(bytes.NewReader(out)))
	if err != nil {
		return nil, err
	}
	if end != pktFlush {
		return nil, fmt.Errorf("object-info: response ended with %s", end)
	}
	return lines, nil
}

func (l *localSession) close() error { return nil }

func (l *localSession) run(ctx context.Context, stdin []byte, extra ...string) ([]byte, error) {
	args := append([]string{"upload-pack", "--stateless-rpc"}, extra...)
	args = append(args, l.path)
	cmd := exec.CommandContext(ctx, "git", args...)
	configureCancelableCommand(cmd)
	// The remote is another repository: keep this clone's git variables out.
	env := make([]string, 0, len(os.Environ())+len(l.env)+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_DIR=") || strings.HasPrefix(kv, "GIT_WORK_TREE=") || strings.HasPrefix(kv, "GIT_INDEX_FILE=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(append(env, l.env...), "GIT_PROTOCOL=version=2")
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderr := &boundedGitError{limit: maxGitErrorBytes}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("upload-pack: %s", auth.RedactString(strings.TrimSpace(stderr.String()+" "+err.Error())))
	}
	return stdout.Bytes(), nil
}

func writePkt(b *bytes.Buffer, s string) {
	fmt.Fprintf(b, "%04x%s", len(s)+4, s)
}

// readPktLines reads data packets until a flush, delim or response-end
// packet and returns them without their trailing newline, plus the packet
// that ended them. An "ERR" packet becomes an error.
func readPktLines(r *bufio.Reader) ([]string, string, error) {
	var lines []string
	head := make([]byte, 4)
	for {
		if _, err := io.ReadFull(r, head); err != nil {
			return nil, "", fmt.Errorf("read packet: %w", err)
		}
		n, err := strconv.ParseUint(string(head), 16, 16)
		if err != nil {
			return nil, "", fmt.Errorf("bad packet length %q", head)
		}
		switch {
		case n <= 2:
			return lines, string(head), nil
		case n < 4:
			return nil, "", fmt.Errorf("bad packet length %q", head)
		}
		data := make([]byte, n-4)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, "", fmt.Errorf("read packet: %w", err)
		}
		line := strings.TrimSuffix(string(data), "\n")
		if msg, ok := strings.CutPrefix(line, "ERR "); ok {
			return nil, "", fmt.Errorf("remote error: %s", auth.RedactString(msg))
		}
		lines = append(lines, line)
	}
}

// resolveRemoteSizes fills sizes the local object store lacks from the
// remote. Failures leave sizes unknown; the mount then hydrates on demand.
func (s *Store) resolveRemoteSizes(ctx context.Context, repo model.RepoConfig, nodes []model.BaseNode, oids []string, index map[string][]int) error {
	var missing []string
	for _, oid := range oids {
		if idx := index[oid]; len(idx) > 0 && nodes[idx[0]].SizeState != "known" {
			missing = append(missing, oid)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, remoteSizeTimeout)
	defer cancel()
	sizes, err := s.remoteBlobSizes(lookupCtx, repo, missing)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, errObjectInfoUnsupported) {
			s.logger.Debug("remote has no object-info; unknown file sizes resolve on demand", "repo", repo.Name)
		} else {
			s.logger.Warn("remote size lookup failed; unknown file sizes resolve on demand", "repo", repo.Name, "error", auth.RedactString(err.Error()))
		}
		return nil
	}
	for oid, size := range sizes {
		for _, idx := range index[oid] {
			nodes[idx].SizeBytes = size
			nodes[idx].SizeState = "known"
		}
	}
	return nil
}
