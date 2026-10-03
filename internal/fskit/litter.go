package fskit

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
)

// Without xattr support macOS writes AppleDouble "._" files through the
// mount, and Finder and fseventsd add their own folders. These lines keep
// them out of git status and git add -A.
const (
	excludeBegin = "# pando: macOS files on FSKit mounts (begin; managed by artifact-fs)"
	excludeEnd   = "# pando: macOS files on FSKit mounts (end)"
)

var macOSExcludes = []string{
	"._*",
	".DS_Store",
	".fseventsd/",
	".Spotlight-V100/",
	".Trashes/",
	".TemporaryItems/",
	"/" + neverIndexName,
}

// EnsureMacOSExcludes adds the macOS litter patterns to gitDir/info/exclude
// once, between marker lines, and refreshes the block if it changed.
func EnsureMacOSExcludes(gitDir string) error {
	path := filepath.Join(gitDir, "info", "exclude")
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var block bytes.Buffer
	block.WriteString(excludeBegin + "\n")
	for _, p := range macOSExcludes {
		block.WriteString(p + "\n")
	}
	block.WriteString(excludeEnd + "\n")

	var out []byte
	if i := bytes.Index(old, []byte(excludeBegin)); i >= 0 {
		rest := old[i:]
		j := bytes.Index(rest, []byte(excludeEnd))
		if j < 0 {
			return errors.New("fskit: info/exclude has a begin marker without an end marker")
		}
		tail := rest[j+len(excludeEnd):]
		tail = bytes.TrimPrefix(tail, []byte("\n"))
		out = append(append(append([]byte{}, old[:i]...), block.Bytes()...), tail...)
	} else {
		out = append([]byte{}, old...)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		out = append(out, block.Bytes()...)
	}
	if bytes.Equal(out, old) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".exclude-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
