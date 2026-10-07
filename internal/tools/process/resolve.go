// Package process implements process.exec: resolving a request to exactly one process
// profile, building the child's environment from an explicit profile, running the
// child as a process tree it can stop, capturing its output in bounded chunks, and
// recognising, after a crash, whether a recorded process is still the same process.
//
// Nothing here is a sandbox. A process the Harness starts runs as the same OS user with
// the whole filesystem and network of the host; the workspace gate, the profile and the
// environment are controls on what the Harness asks for, not walls around what the
// child can do. The process tree stop (a process group on Linux and macOS, a Job
// Object on Windows) only reaches children that stayed in the tree.
package process

import (
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/Nyukimin/RenCrow_Harness/internal/config"
	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolerr"
)

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func hasNUL(items ...string) bool {
	for _, s := range items {
		if strings.ContainsRune(s, 0) {
			return true
		}
	}
	return false
}

// Resolve picks the one process profile a request runs under, from the profiles the
// policy allows. A profile matches when its executable is the requested absolute
// executable and its argv_prefix is, element for element, the start of the requested
// argv. No match is a refusal; two or more are POLICY_AMBIGUOUS, a fault of the host's
// configuration that is never settled by guessing. The argv is neither extended nor
// reordered: the profile's prefix is a condition on the request, not text the Host
// adds. Whether a command line is interpreted by a shell is the profile's is_shell, and
// the request has no way to say it.
func Resolve(allowed []config.ProcessProfile, executable string, argv []string) (config.ProcessProfile, error) {
	if executable == "" || !filepath.IsAbs(executable) || hasNUL(append([]string{executable}, argv...)...) {
		return config.ProcessProfile{}, toolerr.Reject(toolerr.CodePolicyRejected, "the executable must be an absolute path and the arguments must not contain NUL")
	}
	var matches []config.ProcessProfile
	for _, p := range allowed {
		if !samePath(p.Executable, executable) {
			continue
		}
		if len(argv) < len(p.ArgvPrefix) || !slices.Equal(argv[:len(p.ArgvPrefix)], p.ArgvPrefix) {
			continue
		}
		matches = append(matches, p)
	}
	switch len(matches) {
	case 0:
		return config.ProcessProfile{}, toolerr.Reject(toolerr.CodePolicyRejected, "no process profile of the policy allows this executable with these arguments")
	case 1:
		return matches[0], nil
	}
	return config.ProcessProfile{}, toolerr.Reject(toolerr.CodePolicyAmbiguous, "more than one process profile of the policy matches this request")
}

// BuildEnv is the child's whole environment: exactly the profile's values, sorted, and
// nothing of the host's. A profile with no values gives an empty, non-nil list, which
// is an empty environment (a nil list would inherit the host's).
func BuildEnv(p config.EnvProfile) ([]string, error) {
	keys := make([]string, 0, len(p.Values))
	for k := range p.Values {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(p.Values[k], 0) {
			return nil, toolerr.Reject(toolerr.CodeEnvUnknown, "the environment profile holds a name or a value that cannot be passed")
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, fmt.Sprintf("%s=%s", k, p.Values[k]))
	}
	return env, nil
}
