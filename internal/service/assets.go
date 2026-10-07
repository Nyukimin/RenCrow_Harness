package service

import (
	"context"
	"errors"
	"slices"

	"github.com/Nyukimin/RenCrow_Harness/internal/extensions"
	"github.com/Nyukimin/RenCrow_Harness/internal/state/sqlite"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// hostAssets loads what the host gives a Run of the Thread beyond the blocks the caller
// passed (F27): the AGENTS.md of the trusted workspace and the skill catalog, for a Run whose
// policy lets it read Evidence. It is read once, here, and the same bytes are what the
// admission seals: nothing is read again between the look and the Run. It is nil when the
// extensions are off, the workspace is not trusted and no skill root is configured, or there
// is nothing to give.
//
// An asset over a limit is CONTEXT_ASSET_TOO_LARGE and one that is not acceptable is
// INVALID_EXTENSION: either way the Run is not admitted, and nothing is cut to fit or left
// out to make it fit.
func (s *Service) hostAssets(ctx context.Context, threadID string) (*sqlite.HostAssets, error) {
	ext := s.dep.Config.Extensions
	if !ext.Enabled {
		return nil, nil
	}
	info, err := s.store.GetSession(ctx, s.caller, threadID)
	if err != nil {
		return nil, err
	}
	// The catalog is for a Run that can follow it. The session's policy and mode are fixed when
	// it is opened and the deployment is immutable for the life of the process, so what is
	// read here is what the admission will freeze; a policy that cannot be resolved is the
	// admission's own refusal, not a Run without skills.
	ep, err := s.dep.EffectivePolicy(info.PolicyRef, info.WorkspacePath, info.ExecutionMode)
	if err != nil {
		return nil, protocol.NewError(protocol.CodeForbidden, "the session's policy is not available").Wrap(err)
	}
	withSkills := slices.Contains(ep.Policy.Tools, "evidence.read")
	a, err := extensions.Load(ext, info.WorkspacePath, extensions.LoadOptions{WithSkills: withSkills})
	switch {
	case errors.Is(err, extensions.ErrAssetTooLarge):
		return nil, protocol.NewError(protocol.CodeContextAssetTooLarge, "an AGENTS.md or SKILL.md asset is over its limit, so the run is not admitted").Wrap(err)
	case errors.Is(err, extensions.ErrInvalidAsset):
		return nil, protocol.NewError(protocol.CodeInvalidExtension, "an AGENTS.md or SKILL.md asset is not acceptable, so the run is not admitted").Wrap(err)
	case err != nil:
		return nil, protocol.NewError(protocol.CodeInternal, "the host assets could not be loaded").Wrap(err)
	case a == nil:
		return nil, nil
	}
	out := &sqlite.HostAssets{Blocks: a.Blocks, Manifest: a.Manifest}
	for _, f := range a.Files {
		out.Files = append(out.Files, sqlite.HostAssetFile{EvidenceID: f.EvidenceID, Purpose: f.Purpose, MediaType: f.MediaType, Data: f.Data})
	}
	return out, nil
}
