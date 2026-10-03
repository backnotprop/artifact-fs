//go:build !linux && !darwin

package daemon

import (
	"errors"

	"github.com/cloudflare/artifact-fs/internal/fusefs"
	"github.com/cloudflare/artifact-fs/internal/model"
)

func (s *Service) mountFrontEnd(model.RepoConfig, *fusefs.Resolver, *fusefs.Engine, *fusefs.ReadyGate) (fusefs.MountedFS, error) {
	return nil, errors.New("no file system front end for this platform")
}
