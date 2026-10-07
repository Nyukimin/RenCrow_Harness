package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/files"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ErrPolicyChanged is returned when the policy a Run would use is not the one it froze
// at admission: the registry, a profile or the workspace's configuration changed. The
// Run does not take the new authority, and does not keep using the old one.
var ErrPolicyChanged = errors.New("tools: the effective policy is not the one the run froze")

// RunPolicy is a Run's Tool authority, resolved once from the deployment and frozen:
// which Tools it may use, which paths, which processes and which environments. Nothing
// in it is re-read while the Run goes on.
type RunPolicy struct {
	// Revision is the effective policy revision (HOST_ASSETS section 1).
	Revision string
	Mode     string
	// Workspace is the real workspace root.
	Workspace string

	policy    config.Policy
	processes []config.ProcessProfile
	envs      map[string]config.EnvProfile
	enabled   []string
}

// newRunPolicy freezes an effective policy and decides which Tools are available under
// it. A Tool is available when the policy lists it and the mode and the policy give it
// something to work on: process.exec only in trusted_host and only with a process
// profile, the file Tools only with a prefix of the kind they need. structured_only
// never has process.exec.
func newRunPolicy(ep config.EffectivePolicy) *RunPolicy {
	p := &RunPolicy{Revision: ep.Revision, Mode: ep.Mode, Workspace: ep.WorkspaceRoot, policy: ep.Policy, processes: ep.ProcessProfiles, envs: map[string]config.EnvProfile{}}
	for _, e := range ep.EnvProfiles {
		p.envs[e.Name] = e
	}
	for _, name := range AllTools() {
		if !slices.Contains(ep.Policy.Tools, name) {
			continue
		}
		switch name {
		case ProcessExec:
			if ep.Mode != protocol.ModeTrustedHost || len(ep.ProcessProfiles) == 0 {
				continue
			}
		case FileRead, FileSearch:
			if len(ep.Policy.ReadPrefixes) == 0 {
				continue
			}
		case FileCreate, FileEdit:
			if len(ep.Policy.WritePrefixes) == 0 {
				continue
			}
		}
		p.enabled = append(p.enabled, name)
	}
	return p
}

// Names are the Tools available to the Run, in name order.
func (p *RunPolicy) Names() []string { return slices.Clone(p.enabled) }

// Enabled reports whether a Tool is available to the Run.
func (p *RunPolicy) Enabled(name string) bool { return slices.Contains(p.enabled, name) }

// CanMutate reports whether the Run may change anything outside the store: it has a
// Tool that writes files or runs a process. Such a Run holds the workspace lock.
func (p *RunPolicy) CanMutate() bool {
	return p.Enabled(FileCreate) || p.Enabled(FileEdit) || p.Enabled(ProcessExec)
}

// Scope is the workspace and prefixes the file Tools work within.
func (p *RunPolicy) Scope() files.Scope {
	return files.Scope{Root: p.Workspace, ReadPrefixes: slices.Clone(p.policy.ReadPrefixes), WritePrefixes: slices.Clone(p.policy.WritePrefixes)}
}
