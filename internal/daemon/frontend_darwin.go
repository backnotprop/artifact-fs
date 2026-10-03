//go:build darwin

package daemon

import (
	"github.com/cloudflare/artifact-fs/internal/fskit"
	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
)

// mountFrontEnd mounts a repo through the Pando FSKit extension. The Mac
// build has no FUSE (macFUSE) code.
func (s *Service) mountFrontEnd(cfg model.RepoConfig, resolver *fusefs.Resolver, engine *fusefs.Engine, gate *fusefs.ReadyGate) (fusefs.MountedFS, error) {
	return fskit.MountRepo(fskit.MountOptions{
		Repo:     cfg,
		Resolver: resolver,
		Engine:   engine,
		Gate:     gate,
		Logger:   s.logger,
	})
}
