// Package config loads and validates a Harness deployment: the configuration
// file, the policy registry it points at, and the relay key files it names.
//
// Validation is fail-closed and read-only. Nothing is created, repaired or
// defaulted, no environment variable is expanded, and a path that is not an
// explicit absolute path is an error. The result, a Deployment, is an immutable
// snapshot: later edits to any file do not change it, which is what lets a Run
// freeze its policy.
package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Nyukimin/RenCrow_Harness/internal/fsperm"
	"github.com/Nyukimin/RenCrow_Harness/internal/intake"
	"github.com/Nyukimin/RenCrow_Harness/internal/schemacheck"
	"github.com/Nyukimin/RenCrow_Harness/internal/strictjson"
	"github.com/Nyukimin/RenCrow_Harness/pkg/protocol"
)

var (
	// ErrInvalid is wrapped by every rejection of the configuration or of what it refers to.
	ErrInvalid = errors.New("config: invalid configuration")
	// ErrUnavailable is wrapped when something is configured but cannot be provided.
	ErrUnavailable = errors.New("config: unavailable")
)

// Size caps for the files read at startup. They are far above any real file and
// exist so a wrong path (a log, a device) cannot be read into memory.
const (
	maxConfigBytes   = 1 << 20
	maxRegistryBytes = 1 << 20
)

// Config mirrors schemas/config.schema.json.
type Config struct {
	ConfigVersion      string                  `json:"config_version"`
	DataRoot           string                  `json:"data_root"`
	Gateway            GatewayConfig           `json:"gateway"`
	Caller             CallerSection           `json:"caller"`
	Workspaces         []WorkspaceConfig       `json:"workspaces"`
	Bindings           []BindingEntry          `json:"bindings"`
	ProcessProfiles    []ProcessProfile        `json:"process_profiles"`
	Limits             protocol.Limits         `json:"limits"`
	Compaction         CompactionConfig        `json:"compaction"`
	Storage            StorageConfig           `json:"storage"`
	Recovery           protocol.RecoveryPolicy `json:"recovery"`
	PolicyRegistryPath string                  `json:"policy_registry_path"`
	EnvProfiles        []EnvProfile            `json:"env_profiles"`
	Extensions         ExtensionsConfig        `json:"extensions"`
}

// GatewayConfig is the RenCrow_LLM Gateway connection.
type GatewayConfig struct {
	BaseURL          string `json:"base_url"`
	RequiredContract string `json:"required_contract"`
}

// RelayIssuerConfig is one trusted relay issuer.
type RelayIssuerConfig struct {
	Issuer   string `json:"issuer"`
	KeyFile  string `json:"key_file"`
	KeyID    string `json:"key_id"`
	Audience string `json:"audience"`
}

// CallerSection is the profile the process runs as.
type CallerSection struct {
	Principal                 string              `json:"principal"`
	DefaultOrigin             string              `json:"default_origin"`
	ReadableSessionOwners     []string            `json:"readable_session_owners"`
	ControllableSessionOwners []string            `json:"controllable_session_owners"`
	RelayIssuers              []RelayIssuerConfig `json:"relay_issuers"`
}

// WorkspaceConfig is one allowed workspace root.
type WorkspaceConfig struct {
	Root         string   `json:"root"`
	AllowedModes []string `json:"allowed_modes"`
	PolicyRef    string   `json:"policy_ref"`
}

// BindingEntry names a Binding for the CLI and CORE to select.
type BindingEntry struct {
	ProfileName string           `json:"profile_name"`
	Binding     protocol.Binding `json:"binding"`
}

// CompactionConfig is the compaction section.
type CompactionConfig struct {
	Enabled                 bool    `json:"enabled"`
	TriggerRatio            float64 `json:"trigger_ratio"`
	SafetyMarginTokens      int64   `json:"safety_margin_tokens"`
	MaxLogicalStageRequests int64   `json:"max_logical_stage_requests"`
}

// StorageConfig is the store quota and backup location.
type StorageConfig struct {
	MaxDatabaseBytes int64  `json:"max_database_bytes"`
	BackupRoot       string `json:"backup_root"`
}

// ExtensionsConfig is the extensions section. Only its shape is validated here.
type ExtensionsConfig struct {
	Enabled               bool     `json:"enabled"`
	TrustedWorkspaceRoots []string `json:"trusted_workspace_roots"`
	SkillRoots            []string `json:"skill_roots"`
	Hooks                 []string `json:"hooks"`
}

// Workspace is a validated workspace: its real root, the modes it allows, and the
// policy that governs it.
type Workspace struct {
	Root         string
	AllowedModes []string
	PolicyRef    string
}

// Deployment (the output of F30 ValidateDeployment) is the validated, immutable
// runtime profile. All paths in it are real (symbolic links resolved) absolute paths.
type Deployment struct {
	Config           Config
	ConfigPath       string
	DataRoot         string
	BackupRoot       string
	Caller           intake.Caller
	RecoveryRevision string

	workspaces []Workspace
	registry   map[string]Policy
	process    map[string]ProcessProfile
	env        map[string]EnvProfile
	bindings   map[string]protocol.Binding
}

// Workspaces returns a copy of the configured workspaces.
func (d *Deployment) Workspaces() []Workspace { return slices.Clone(d.workspaces) }

// Workspace returns the workspace whose real root is exactly root.
func (d *Deployment) Workspace(root string) (Workspace, bool) {
	for _, w := range d.workspaces {
		if w.Root == root {
			return w, true
		}
	}
	return Workspace{}, false
}

// Binding resolves a CLI or CORE profile name to its Binding. An unknown name is
// an error: there is no default binding to fall back to.
func (d *Deployment) Binding(profileName string) (protocol.Binding, error) {
	b, ok := d.bindings[profileName]
	if !ok {
		return protocol.Binding{}, fmt.Errorf("%w: unknown binding profile", ErrInvalid)
	}
	return b, nil
}

// invalidf wraps ErrInvalid and, through any %w in format, the underlying cause.
func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// Load reads and validates the configuration file at the absolute path.
func Load(path string) (*Deployment, error) {
	if err := checkAbsPath("config path", path); err != nil {
		return nil, err
	}
	data, err := readCapped(path, maxConfigBytes)
	if err != nil {
		return nil, invalidf("config file: %w", fsperm.WithoutPath(err))
	}
	return Parse(path, data)
}

// Parse validates configuration text that was read from configPath.
func Parse(configPath string, data []byte) (*Deployment, error) {
	if err := checkAbsPath("config path", configPath); err != nil {
		return nil, err
	}
	var cfg Config
	if err := decodeInto(schemacheck.Config, data, &cfg); err != nil {
		return nil, err
	}
	d := &Deployment{Config: cfg}

	// 1. Paths: absolute, literal, real. Every directory and file the process will
	//    rely on is resolved once here and used in resolved form from then on.
	realConfig, err := realFilePath("config file", configPath)
	if err != nil {
		return nil, err
	}
	d.ConfigPath = realConfig
	if d.DataRoot, err = realPrivateDir("data_root", cfg.DataRoot); err != nil {
		return nil, err
	}
	if d.BackupRoot, err = realMaybeMissingDir("storage.backup_root", cfg.Storage.BackupRoot); err != nil {
		return nil, err
	}
	registryPath, err := realFilePath("policy_registry_path", cfg.PolicyRegistryPath)
	if err != nil {
		return nil, err
	}
	for _, p := range cfg.Extensions.TrustedWorkspaceRoots {
		if err := checkAbsPath("extensions.trusted_workspace_roots entry", p); err != nil {
			return nil, err
		}
	}
	for _, p := range cfg.Extensions.SkillRoots {
		if err := checkAbsPath("extensions.skill_roots entry", p); err != nil {
			return nil, err
		}
	}
	for _, p := range cfg.ProcessProfiles {
		if err := checkAbsPath("process profile executable", p.Executable); err != nil {
			return nil, err
		}
		for _, a := range p.ArgvPrefix {
			if strings.ContainsRune(a, 0) {
				return nil, invalidf("process profile argv_prefix contains NUL")
			}
		}
	}

	// 2. Workspaces and the separation between workspaces and private locations.
	if d.workspaces, err = loadWorkspaces(cfg.Workspaces); err != nil {
		return nil, err
	}
	if overlaps(d.DataRoot, d.BackupRoot) {
		return nil, invalidf("data_root and storage.backup_root must be separate directories")
	}
	keyFiles := make([]string, len(cfg.Caller.RelayIssuers))
	for i, r := range cfg.Caller.RelayIssuers {
		if keyFiles[i], err = realKeyFile(r.KeyFile); err != nil {
			return nil, err
		}
	}
	for _, w := range d.workspaces {
		for _, dir := range []struct{ name, path string }{{"data_root", d.DataRoot}, {"storage.backup_root", d.BackupRoot}} {
			if overlaps(w.Root, dir.path) {
				return nil, invalidf("%s must be outside every workspace", dir.name)
			}
		}
		for _, file := range append([]string{d.ConfigPath, registryPath}, keyFiles...) {
			if within(file, w.Root) {
				return nil, invalidf("the config, the policy registry and relay key files must be outside every workspace")
			}
		}
	}

	// 3. Policy registry, process and env profiles, and the references between them.
	if err := d.loadProcessAndEnv(); err != nil {
		return nil, err
	}
	if err := d.loadRegistry(registryPath); err != nil {
		return nil, err
	}
	for _, w := range d.workspaces {
		p, ok := d.registry[w.PolicyRef]
		if !ok {
			return nil, ErrUnregisteredPolicy
		}
		if !slices.ContainsFunc(w.AllowedModes, func(m string) bool { return slices.Contains(p.AllowedModes, m) }) {
			return nil, invalidf("workspace and its policy have no execution mode in common")
		}
	}

	// 4. Gateway, bindings, caller profile, recovery policy.
	if err := checkGateway(cfg.Gateway.BaseURL); err != nil {
		return nil, err
	}
	if d.bindings, err = loadBindings(cfg.Bindings); err != nil {
		return nil, err
	}
	if d.Caller, err = buildCaller(cfg.Caller); err != nil {
		return nil, err
	}
	d.RecoveryRevision, err = protocol.RecoveryPolicyRevision(
		cfg.Recovery.ContractVersion, int(cfg.Recovery.MaxAttemptsPerAct), cfg.Recovery.AllowedProfiles)
	if err != nil {
		return nil, invalidf("recovery policy: %w", err)
	}
	return d, nil
}

// decodeInto strictly decodes data, validates it against the schema file and
// reads it into out.
func decodeInto(schemaFile string, data []byte, out any) error {
	v, err := strictjson.Decode(data)
	if err != nil {
		return invalidf("malformed JSON: %w", err)
	}
	if err := schemacheck.Unmarshal(schemaFile, "", v, out); err != nil {
		if errors.Is(err, schemacheck.ErrViolation) || errors.Is(err, schemacheck.ErrDecode) {
			return invalidf("%w", err)
		}
		return fmt.Errorf("config: schema unavailable: %w", err)
	}
	return nil
}

func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file is larger than the allowed size")
	}
	return data, nil
}

// checkAbsPath requires an explicit, absolute, already-clean path with no NUL and
// no ".." segment. "~", "$VAR" and "%VAR%" are relative paths here, so they fail
// the absolute check instead of being expanded.
func checkAbsPath(what, p string) error {
	switch {
	case p == "":
		return invalidf("%s is empty", what)
	case strings.ContainsRune(p, 0):
		return invalidf("%s contains NUL", what)
	case !filepath.IsAbs(p):
		return invalidf("%s must be an absolute path", what)
	case slices.Contains(strings.Split(filepath.ToSlash(p), "/"), ".."):
		return invalidf("%s contains a parent segment", what)
	case filepath.Clean(p) != p:
		return invalidf("%s must be written in clean form", what)
	}
	return nil
}

// realDir resolves p to its real path and requires a directory.
func realDir(what, p string) (string, error) {
	if err := checkAbsPath(what, p); err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", invalidf("%s: %w", what, fsperm.WithoutPath(err))
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", invalidf("%s is not a directory", what)
	}
	return real, nil
}

func realPrivateDir(what, p string) (string, error) {
	real, err := realDir(what, p)
	if err != nil {
		return "", err
	}
	if err := fsperm.CheckOwnerOnlyDir(real); err != nil {
		return "", invalidf("%s: %w", what, fsperm.WithoutPath(err))
	}
	return real, nil
}

// realMaybeMissingDir is realDir for a directory that may not exist yet (the
// backup root is created by the first backup): an existing path must be a
// directory; a missing one is accepted if its parent exists, and is returned as
// the real parent plus its own name.
func realMaybeMissingDir(what, p string) (string, error) {
	if err := checkAbsPath(what, p); err != nil {
		return "", err
	}
	if _, err := os.Lstat(p); err == nil {
		return realDir(what, p)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", invalidf("%s: %w", what, fsperm.WithoutPath(err))
	}
	parent, err := realDir(what+" parent", filepath.Dir(p))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(p)), nil
}

// realFilePath resolves an existing regular file.
func realFilePath(what, p string) (string, error) {
	if err := checkAbsPath(what, p); err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", invalidf("%s: %w", what, fsperm.WithoutPath(err))
	}
	info, err := os.Stat(real)
	if err != nil || !info.Mode().IsRegular() {
		return "", invalidf("%s is not a regular file", what)
	}
	return real, nil
}

// realKeyFile resolves a relay key file. Unlike other files it must not itself be
// a symbolic link, so the path that is checked is the path that is read.
func realKeyFile(p string) (string, error) {
	if err := checkAbsPath("relay key_file", p); err != nil {
		return "", err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		return "", invalidf("relay key_file: %w", fsperm.WithoutPath(err))
	}
	real := filepath.Join(dir, filepath.Base(p))
	if err := fsperm.CheckOwnerOnlyFile(real); err != nil {
		return "", invalidf("relay key_file: %w", fsperm.WithoutPath(err))
	}
	return real, nil
}

func within(child, parent string) bool { return fsperm.Within(child, parent) }

func overlaps(a, b string) bool { return fsperm.Overlaps(a, b) }

func loadWorkspaces(in []WorkspaceConfig) ([]Workspace, error) {
	out := make([]Workspace, 0, len(in))
	for _, w := range in {
		real, err := realDir("workspace root", w.Root)
		if err != nil {
			return nil, err
		}
		if err := checkSet("workspace allowed_modes", w.AllowedModes, oneOf(knownModes, "mode")); err != nil {
			return nil, err
		}
		for _, prev := range out {
			if overlaps(prev.Root, real) {
				return nil, invalidf("workspace roots must be separate directories")
			}
		}
		out = append(out, Workspace{Root: real, AllowedModes: slices.Clone(w.AllowedModes), PolicyRef: w.PolicyRef})
	}
	return out, nil
}

func (d *Deployment) loadProcessAndEnv() error {
	d.process = make(map[string]ProcessProfile, len(d.Config.ProcessProfiles))
	for _, p := range d.Config.ProcessProfiles {
		if _, dup := d.process[p.Name]; dup {
			return invalidf("process profile %q is defined twice", p.Name)
		}
		d.process[p.Name] = p
	}
	d.env = make(map[string]EnvProfile, len(d.Config.EnvProfiles))
	for _, e := range d.Config.EnvProfiles {
		if _, dup := d.env[e.Name]; dup {
			return invalidf("env profile %q is defined twice", e.Name)
		}
		for k, v := range e.Values {
			if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
				return invalidf("env profile %q has an invalid variable", e.Name)
			}
		}
		d.env[e.Name] = e
	}
	// "clean" is the explicit empty profile every deployment must have. An unknown
	// profile is never replaced by it.
	if clean, ok := d.env["clean"]; !ok || len(clean.Values) != 0 {
		return invalidf(`env_profiles must define "clean" with no variables`)
	}
	return nil
}

func (d *Deployment) loadRegistry(path string) error {
	data, err := readCapped(path, maxRegistryBytes)
	if err != nil {
		return invalidf("policy registry: %w", err)
	}
	var reg registryFile
	if err := decodeInto(schemacheck.PolicyRegistry, data, &reg); err != nil {
		return err
	}
	if reg.FormatVersion != "rencrow-policy-registry/v1" {
		return invalidf("unknown policy registry format")
	}
	d.registry = make(map[string]Policy, len(reg.Policies))
	for _, p := range reg.Policies {
		if _, dup := d.registry[p.ID]; dup {
			return invalidf("policy id %q appears twice", p.ID)
		}
		if err := p.validate(); err != nil {
			return err
		}
		for _, n := range p.ProcessProfiles {
			if _, ok := d.process[n]; !ok {
				return invalidf("policy %q names an undefined process profile", p.ID)
			}
		}
		for _, n := range p.EnvProfiles {
			if _, ok := d.env[n]; !ok {
				return invalidf("policy %q names an undefined env profile", p.ID)
			}
		}
		d.registry[p.ID] = p
	}
	return nil
}

func loadBindings(in []BindingEntry) (map[string]protocol.Binding, error) {
	out := make(map[string]protocol.Binding, len(in))
	for _, e := range in {
		if _, dup := out[e.ProfileName]; dup {
			return nil, invalidf("binding profile %q is defined twice", e.ProfileName)
		}
		b := e.Binding
		if b.ProfileRevision == "latest" {
			return nil, invalidf("binding profile_revision must be a resolved revision, not latest")
		}
		if b.Kind == "alias" && (b.AgentID == nil || b.ExecutionRole == nil) {
			return nil, invalidf("an alias binding needs agent_id and execution_role")
		}
		out[e.ProfileName] = b
	}
	return out, nil
}

// checkGateway accepts only a loopback Gateway for now: scheme http or https, a
// literal loopback address or the name localhost, and nothing in the URL that
// carries credentials. A non-loopback Gateway needs TLS and client authentication
// that this version does not provide.
func checkGateway(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return invalidf("gateway.base_url is not an http(s) URL")
	}
	if u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return invalidf("gateway.base_url must not carry credentials, a query or a fragment")
	}
	host := u.Hostname()
	if host != "localhost" {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return invalidf("gateway.base_url must be a loopback address")
		}
	}
	return nil
}

func buildCaller(c CallerSection) (intake.Caller, error) {
	relays := make([]intake.RelayKey, len(c.RelayIssuers))
	for i, r := range c.RelayIssuers {
		real, err := realKeyFile(r.KeyFile)
		if err != nil {
			return intake.Caller{}, err
		}
		raw, err := readCapped(real, 256) // the parser refuses everything but 64 or 65 bytes; the cap only bounds the read
		if err != nil {
			return intake.Caller{}, invalidf("relay key_file: %w", fsperm.WithoutPath(err))
		}
		key, err := protocol.ParseOriginKeyFile(raw)
		if err != nil {
			return intake.Caller{}, invalidf("relay key_file: %w", err)
		}
		relays[i] = intake.RelayKey{Issuer: r.Issuer, KeyID: r.KeyID, Audience: r.Audience, Key: key}
	}
	caller, err := intake.NewCaller(intake.CallerConfig{
		Principal:                 c.Principal,
		DefaultOrigin:             c.DefaultOrigin,
		ReadableSessionOwners:     c.ReadableSessionOwners,
		ControllableSessionOwners: c.ControllableSessionOwners,
		Relays:                    relays,
	})
	if err != nil {
		return intake.Caller{}, invalidf("caller: %w", err)
	}
	return caller, nil
}
