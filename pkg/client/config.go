package client

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// DefaultExitGrace is how long the client waits for the child to exit after its output
// ended, or after a shutdown it asked for has run its time, before it kills it.
const DefaultExitGrace = 5 * time.Second

// Config says how the Harness child process is started and what the client requires of
// it. Everything the child gets comes from here: nothing is inherited from the calling
// process (not its environment, not its working directory), and no secret is put in an
// argument. The arguments are always `serve --stdio --config <ConfigPath>`.
type Config struct {
	// Binary is the absolute path of the rencrow-harness executable.
	Binary string
	// ConfigPath is the absolute path of the Harness configuration file. It is the only
	// thing passed on the command line; secrets are referenced from the file, never
	// carried here.
	ConfigPath string
	// Dir is the absolute working directory of the child.
	Dir string
	// Env is the whole environment of the child, as "KEY=value" entries. A nil or empty
	// Env means an empty environment: the child never inherits the environment of the
	// calling process. To pass something on, say so (for instance os.Environ()).
	Env []string
	// Stderr receives the diagnostics of the child (they carry no request content, key or
	// path). Nil discards them.
	Stderr io.Writer

	// ClientName and ClientVersion are sent in initialize (1 to 80 characters each). They
	// identify the client in diagnostics and are not an authentication.
	ClientName    string
	ClientVersion string

	// RequireCapabilities lists the capabilities that must be ready after initialize;
	// Start fails with ErrIncompatible if one is not. Nil means
	// DefaultRequiredCapabilities; an empty non-nil slice requires none.
	RequireCapabilities []string

	// Notifications bound what is held for a consumer of Notifications that does not keep
	// up. IgnoreNotifications says nobody reads them: they are discarded as they arrive
	// (they are durable, EventsRead returns them), and Notifications carries nothing.
	// Without it, a consumer that never reads makes the connection end with
	// ErrNotificationOverflow once the limit is reached.
	Notifications       NotificationLimits
	IgnoreNotifications bool

	// ExitGrace is the time given to the child to exit once it was asked to, beyond the
	// time a shutdown allows; then it is killed. Default DefaultExitGrace.
	ExitGrace time.Duration
}

// DefaultRequiredCapabilities are what a delegating caller needs from a Harness: it can
// open a Thread, admit and execute a Run on a model, stop it, and report on it.
func DefaultRequiredCapabilities() []string {
	return []string{
		"session/open", "turn/start", "turn/interrupt", "run/get", "receipt/get", "events/read", "service/shutdown",
		"turn.execution", "model.generation",
	}
}

// validate returns the Config with its defaults filled in.
func (cfg Config) validate() (Config, error) {
	bad := func(format string, args ...any) (Config, error) {
		return cfg, fmt.Errorf("%w: %s", ErrInvalidConfig, fmt.Sprintf(format, args...))
	}
	for _, f := range []struct{ name, value string }{{"Binary", cfg.Binary}, {"ConfigPath", cfg.ConfigPath}, {"Dir", cfg.Dir}} {
		if f.value == "" || !filepath.IsAbs(f.value) || strings.ContainsRune(f.value, 0) {
			return bad("%s must be an absolute path", f.name)
		}
	}
	for _, f := range []struct{ name, value string }{{"ClientName", cfg.ClientName}, {"ClientVersion", cfg.ClientVersion}} {
		if n := len([]rune(f.value)); n < 1 || n > 80 {
			return bad("%s must be 1 to 80 characters", f.name)
		}
	}
	for _, e := range cfg.Env {
		i := strings.IndexByte(e, '=')
		if i < 1 || strings.ContainsRune(e, 0) {
			return bad("Env entries must be KEY=value")
		}
	}
	if cfg.RequireCapabilities == nil {
		cfg.RequireCapabilities = DefaultRequiredCapabilities()
	}
	if cfg.ExitGrace <= 0 {
		cfg.ExitGrace = DefaultExitGrace
	}
	cfg.Env = append([]string{}, cfg.Env...)
	return cfg, nil
}
