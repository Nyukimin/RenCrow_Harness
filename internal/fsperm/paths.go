package fsperm

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// fold makes comparison case-insensitive where the default file systems are
// (macOS and Windows). It can only over-report overlap on a case-sensitive volume,
// which is the safe direction.
func fold(p string) string {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// Within reports whether child is parent or lies inside it. Both must be absolute
// and already resolved (see ResolveAhead for paths that do not exist yet).
func Within(child, parent string) bool {
	rel, err := filepath.Rel(fold(parent), fold(child))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// Overlaps reports whether either path is, or lies inside, the other.
func Overlaps(a, b string) bool { return Within(a, b) || Within(b, a) }

// ResolveAhead returns the real path p would have if it were created: the deepest
// existing ancestor is resolved through symbolic links and the missing remainder is
// appended. Nothing is created. It lets a caller judge where a directory would land
// before making it.
func ResolveAhead(p string) (string, error) {
	var rest []string
	cur := filepath.Clean(p)
	for {
		if _, err := os.Lstat(cur); err == nil {
			real, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			for i := len(rest) - 1; i >= 0; i-- {
				real = filepath.Join(real, rest[i])
			}
			return real, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", os.ErrNotExist
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// WithoutPath returns err without the file path an operating-system error carries
// ("open /x/y: no such file"), keeping the operation and the cause, so errors.Is
// against os.ErrNotExist, os.ErrExist and the like still works. Messages that may
// reach a client or a shared log must not carry the operator's directory layout.
func WithoutPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return fmt.Errorf("%s: %w", le.Op, le.Err)
	}
	return err
}
