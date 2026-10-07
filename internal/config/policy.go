package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

// Errors of policy resolution. All of them are distinguishable with errors.Is.
var (
	// ErrUnregisteredPolicy is wrapped when a policy_ref is not an id in the registry.
	// The match is exact: not a prefix, not a file name, not a URL.
	ErrUnregisteredPolicy = fmt.Errorf("%w: policy_ref is not registered: %w", ErrInvalid, intake.ErrPolicyUnavailable)
	// ErrModeUnavailable is wrapped when a mode is allowed by configuration but this
	// build cannot provide it. isolated stays unavailable until an isolation
	// adapter has been accepted; it is never replaced by another mode.
	ErrModeUnavailable = fmt.Errorf("%w: execution mode", ErrUnavailable)
)

// Tools a policy may allow (the six built-in Tools).
var knownTools = map[string]bool{
	"file.read": true, "file.search": true, "file.create": true, "file.edit": true, "process.exec": true, "evidence.read": true,
}

// Execution modes.
var knownModes = map[string]bool{
	protocol.ModeStructuredOnly: true, protocol.ModeTrustedHost: true, protocol.ModeIsolated: true,
}

// Policy is one entry of the policy registry (schemas/policy_registry.schema.json).
type Policy struct {
	ID              string          `json:"id"`
	AllowedModes    []string        `json:"allowed_modes"`
	Tools           []string        `json:"tools"`
	ReadPrefixes    []string        `json:"read_prefixes"`
	WritePrefixes   []string        `json:"write_prefixes"`
	ProcessProfiles []string        `json:"process_profiles"`
	EnvProfiles     []string        `json:"env_profiles"`
	Limits          protocol.Limits `json:"limits"`
}

// registryFile is the registry document.
type registryFile struct {
	FormatVersion string   `json:"format_version"`
	Policies      []Policy `json:"policies"`
}

// ProcessProfile is a host-defined way to run one executable.
type ProcessProfile struct {
	Name       string   `json:"name"`
	Executable string   `json:"executable"`
	IsShell    bool     `json:"is_shell"`
	ArgvPrefix []string `json:"argv_prefix"`
}

// EnvProfile is an explicit set of environment variables. Nothing is inherited
// from the host; "clean" is the empty profile.
type EnvProfile struct {
	Name   string            `json:"name"`
	Values map[string]string `json:"values"`
}

// ValidatePrefix checks one read or write prefix: a workspace-relative path
// written as normalized "/"-separated segments, or "." for the whole workspace.
// Absolute paths, drive letters, backslashes, "..", "." inside a longer path,
// empty segments (and so leading or trailing separators), NUL and other control
// characters are refused, so the same text means the same thing on every OS.
// Containment of the real path in the workspace is checked again where a path is
// opened; this is only the grammar.
func ValidatePrefix(p string) error {
	bad := func(why string) error { return fmt.Errorf("%w: prefix %s", ErrInvalid, why) }
	if p == "" {
		return bad("is empty")
	}
	if p == "." {
		return nil
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return bad("contains a control character")
		}
		if r == '\\' {
			return bad("contains a backslash")
		}
	}
	if len(p) >= 2 && p[1] == ':' && (p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z') {
		return bad("starts with a drive letter")
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
			return bad("is absolute or has an empty segment")
		case ".":
			return bad("has a dot segment")
		case "..":
			return bad("has a parent segment")
		}
	}
	return nil
}

func checkSet(what string, items []string, valid func(string) error) error {
	seen := make(map[string]bool, len(items))
	for _, s := range items {
		if valid != nil {
			if err := valid(s); err != nil {
				return err
			}
		}
		if seen[s] {
			return fmt.Errorf("%w: %s has a duplicate entry", ErrInvalid, what)
		}
		seen[s] = true
	}
	return nil
}

func oneOf(table map[string]bool, what string) func(string) error {
	return func(s string) error {
		if !table[s] {
			return fmt.Errorf("%w: unknown %s", ErrInvalid, what)
		}
		return nil
	}
}

// validate checks a registry entry on its own: closed vocabularies, no duplicate
// in any set, prefix grammar. References to process and env profiles are checked
// against the configuration by the caller.
func (p Policy) validate() error {
	if err := checkSet("allowed_modes", p.AllowedModes, oneOf(knownModes, "mode")); err != nil {
		return err
	}
	if err := checkSet("tools", p.Tools, oneOf(knownTools, "tool")); err != nil {
		return err
	}
	if err := checkSet("read_prefixes", p.ReadPrefixes, ValidatePrefix); err != nil {
		return err
	}
	if err := checkSet("write_prefixes", p.WritePrefixes, ValidatePrefix); err != nil {
		return err
	}
	if err := checkSet("process_profiles", p.ProcessProfiles, nil); err != nil {
		return err
	}
	return checkSet("env_profiles", p.EnvProfiles, nil)
}

// EffectivePolicyInput is everything the policy revision covers: the registry
// entry, the definitions it resolves to, and the session's workspace and mode.
type EffectivePolicyInput struct {
	Policy Policy
	// ProcessProfiles and EnvProfiles are the definitions of exactly the profiles
	// the policy names: no fewer (an unresolved name) and no more (a definition
	// the policy does not grant must not shift the revision).
	ProcessProfiles []ProcessProfile
	EnvProfiles     []EnvProfile
	WorkspaceRoot   string // the real, absolute workspace root
	Mode            string
}

// EffectivePolicyRevision is
//
//	D("rencrow-effective-policy/v1", {policy, process_profiles, env_profiles, workspace_root, mode})
//
// Process and env profiles are ordered by name and every set in the policy by
// UTF-8 order, with duplicates refused, so the order of a configuration file
// confers no priority. argv_prefix keeps its order: it is a prefix to match, not
// a set. A Run freezes this value and never re-reads the registry.
func EffectivePolicyRevision(in EffectivePolicyInput) (string, error) {
	p := in.Policy
	if err := p.validate(); err != nil {
		return "", err
	}
	if !filepath.IsAbs(in.WorkspaceRoot) {
		return "", fmt.Errorf("%w: workspace root is not absolute", ErrInvalid)
	}
	if !slices.Contains(p.AllowedModes, in.Mode) {
		return "", fmt.Errorf("%w: mode is not allowed by the policy", ErrInvalid)
	}
	procs, err := sortedProcesses(p.ProcessProfiles, in.ProcessProfiles)
	if err != nil {
		return "", err
	}
	envs, err := sortedEnvs(p.EnvProfiles, in.EnvProfiles)
	if err != nil {
		return "", err
	}
	value := map[string]any{
		"policy": map[string]any{
			"id":               p.ID,
			"allowed_modes":    sortedStrings(p.AllowedModes),
			"tools":            sortedStrings(p.Tools),
			"read_prefixes":    sortedStrings(p.ReadPrefixes),
			"write_prefixes":   sortedStrings(p.WritePrefixes),
			"process_profiles": sortedStrings(p.ProcessProfiles),
			"env_profiles":     sortedStrings(p.EnvProfiles),
			"limits": map[string]any{
				"max_model_steps":         p.Limits.MaxModelSteps,
				"max_tool_calls_per_step": p.Limits.MaxToolCallsPerStep,
				"deadline_seconds":        p.Limits.DeadlineSeconds,
				"max_capture_bytes":       p.Limits.MaxCaptureBytes,
				"max_generation_attempts": p.Limits.MaxGenerationAttempts,
			},
		},
		"process_profiles": procs,
		"env_profiles":     envs,
		"workspace_root":   in.WorkspaceRoot,
		"mode":             in.Mode,
	}
	rev, err := canon.D("rencrow-effective-policy/v1", value)
	if err != nil {
		return "", fmt.Errorf("%w: policy revision: %w", ErrInvalid, err)
	}
	return rev, nil
}

func sortedStrings(items []string) []any {
	s := slices.Clone(items)
	slices.Sort(s)
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func sortedProcesses(names []string, defs []ProcessProfile) ([]any, error) {
	byName := make(map[string]ProcessProfile, len(defs))
	for _, d := range defs {
		if _, dup := byName[d.Name]; dup {
			return nil, fmt.Errorf("%w: process profile %q is defined twice", ErrInvalid, d.Name)
		}
		byName[d.Name] = d
	}
	if len(byName) != len(names) {
		return nil, fmt.Errorf("%w: process profile definitions do not match the policy", ErrInvalid)
	}
	sorted := sortedStrings(names)
	out := make([]any, len(sorted))
	for i, n := range sorted {
		d, ok := byName[n.(string)]
		if !ok {
			return nil, fmt.Errorf("%w: process profile %q is not defined", ErrInvalid, n)
		}
		argv := make([]any, len(d.ArgvPrefix))
		for j, a := range d.ArgvPrefix {
			argv[j] = a
		}
		out[i] = map[string]any{"name": d.Name, "executable": d.Executable, "is_shell": d.IsShell, "argv_prefix": argv}
	}
	return out, nil
}

func sortedEnvs(names []string, defs []EnvProfile) ([]any, error) {
	byName := make(map[string]EnvProfile, len(defs))
	for _, d := range defs {
		if _, dup := byName[d.Name]; dup {
			return nil, fmt.Errorf("%w: env profile %q is defined twice", ErrInvalid, d.Name)
		}
		byName[d.Name] = d
	}
	if len(byName) != len(names) {
		return nil, fmt.Errorf("%w: env profile definitions do not match the policy", ErrInvalid)
	}
	sorted := sortedStrings(names)
	out := make([]any, len(sorted))
	for i, n := range sorted {
		d, ok := byName[n.(string)]
		if !ok {
			return nil, fmt.Errorf("%w: env profile %q is not defined", ErrInvalid, n)
		}
		values := make(map[string]any, len(d.Values))
		for k, v := range d.Values {
			values[k] = v
		}
		out[i] = map[string]any{"name": d.Name, "values": values}
	}
	return out, nil
}

// Policy returns the registry entry whose id is exactly ref.
func (d *Deployment) Policy(ref string) (Policy, error) {
	p, ok := d.registry[ref]
	if !ok {
		return Policy{}, ErrUnregisteredPolicy
	}
	return p, nil
}

// EffectiveCaps is the most a Run under the policy may ask for: the smaller of the
// host configuration's limits and the policy's, field by field.
func (d *Deployment) EffectiveCaps(policyRef string) (protocol.Limits, error) {
	p, err := d.Policy(policyRef)
	if err != nil {
		return protocol.Limits{}, err
	}
	return intake.MinLimits(d.Config.Limits, p.Limits), nil
}

// ResolveMode returns the requested mode if the workspace, the policy and this
// build all allow it, and an error otherwise. It never substitutes another mode:
// a refusal stays a refusal. The session's policy_ref must be the one the
// workspace is configured with.
func (d *Deployment) ResolveMode(workspaceRoot, policyRef, mode string) (string, error) {
	ws, ok := d.Workspace(workspaceRoot)
	if !ok {
		return "", fmt.Errorf("%w: path is not a configured workspace root", ErrInvalid)
	}
	p, err := d.Policy(policyRef)
	if err != nil {
		return "", err
	}
	if ws.PolicyRef != policyRef {
		return "", fmt.Errorf("%w: policy_ref is not the one this workspace is configured with", ErrInvalid)
	}
	if !knownModes[mode] {
		return "", fmt.Errorf("%w: unknown execution mode", ErrInvalid)
	}
	if !slices.Contains(ws.AllowedModes, mode) || !slices.Contains(p.AllowedModes, mode) {
		return "", fmt.Errorf("%w: mode is not allowed for this workspace and policy", ErrInvalid)
	}
	if mode == protocol.ModeIsolated {
		return "", fmt.Errorf("%w: isolated needs an accepted isolation adapter", ErrModeUnavailable)
	}
	return mode, nil
}

// PolicyRevision resolves the policy's process and env profiles from this
// deployment and returns EffectivePolicyRevision for the workspace and mode. The
// deployment holds its own copy of everything: editing a file after load changes
// nothing here.
func (d *Deployment) PolicyRevision(policyRef, workspaceRoot, mode string) (string, error) {
	if _, err := d.ResolveMode(workspaceRoot, policyRef, mode); err != nil {
		return "", err
	}
	p := d.registry[policyRef]
	in := EffectivePolicyInput{Policy: p, WorkspaceRoot: workspaceRoot, Mode: mode}
	for _, n := range p.ProcessProfiles {
		in.ProcessProfiles = append(in.ProcessProfiles, d.process[n])
	}
	for _, n := range p.EnvProfiles {
		in.EnvProfiles = append(in.EnvProfiles, d.env[n])
	}
	return EffectivePolicyRevision(in)
}

// EffectivePolicy is everything a Run's tool authority is made of, resolved from this
// deployment for one workspace and mode: the registry entry, the process and env
// profile definitions it grants, and the revision that covers them all. The Run
// freezes it; nothing re-reads the registry afterwards.
type EffectivePolicy struct {
	Policy          Policy
	ProcessProfiles []ProcessProfile
	EnvProfiles     []EnvProfile
	WorkspaceRoot   string
	Mode            string
	Revision        string
}

// EffectivePolicy resolves the policy, the mode and the revision, with the same checks
// as PolicyRevision (a mode the workspace, the policy or this build does not allow is
// refused, never replaced). The deployment holds its own copy of every definition:
// the returned slices are copies.
func (d *Deployment) EffectivePolicy(policyRef, workspaceRoot, mode string) (EffectivePolicy, error) {
	rev, err := d.PolicyRevision(policyRef, workspaceRoot, mode)
	if err != nil {
		return EffectivePolicy{}, err
	}
	p := d.registry[policyRef]
	out := EffectivePolicy{WorkspaceRoot: workspaceRoot, Mode: mode, Revision: rev}
	out.Policy = p
	out.Policy.AllowedModes = slices.Clone(p.AllowedModes)
	out.Policy.Tools = slices.Clone(p.Tools)
	out.Policy.ReadPrefixes = slices.Clone(p.ReadPrefixes)
	out.Policy.WritePrefixes = slices.Clone(p.WritePrefixes)
	out.Policy.ProcessProfiles = slices.Clone(p.ProcessProfiles)
	out.Policy.EnvProfiles = slices.Clone(p.EnvProfiles)
	for _, n := range p.ProcessProfiles {
		pp := d.process[n]
		pp.ArgvPrefix = slices.Clone(pp.ArgvPrefix)
		out.ProcessProfiles = append(out.ProcessProfiles, pp)
	}
	for _, n := range p.EnvProfiles {
		e := d.env[n]
		vals := make(map[string]string, len(e.Values))
		for k, v := range e.Values {
			vals[k] = v
		}
		e.Values = vals
		out.EnvProfiles = append(out.EnvProfiles, e)
	}
	return out, nil
}
