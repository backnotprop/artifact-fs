//go:build linux

package daemon

import (
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// mountFrontEnd mounts a repo through FUSE.
func (s *Service) mountFrontEnd(cfg model.RepoConfig, resolver *fusefs.Resolver, engine *fusefs.Engine, gate *fusefs.ReadyGate) (fusefs.MountedFS, error) {
	return fusefs.MountRepoWithGate(cfg, resolver, engine, gate)
}
