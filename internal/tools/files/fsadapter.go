package files

import (
	"os"

	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolerr"
)

// isLink reports anything that is not a plain file or directory by its own right: a
// symbolic link, a reparse point, a device, a socket or a pipe.
func isLink(fi os.FileInfo) bool {
	if fi.Mode()&(os.ModeSymlink|os.ModeIrregular|os.ModeDevice|os.ModeNamedPipe|os.ModeSocket|os.ModeCharDevice) != 0 {
		return true
	}
	return isReparsePoint(fi)
}

// openRegular opens a resolved regular file for reading and checks that what was
// opened is what was inspected: a regular file, not a link, and the same file.
func openRegular(r Resolved) (*os.File, os.FileInfo, error) {
	if !r.Exists || r.Info == nil {
		return nil, nil, toolerr.Fail(toolerr.CodeNotFound, "the path does not exist")
	}
	if !r.Info.Mode().IsRegular() {
		if r.Info.IsDir() {
			return nil, nil, toolerr.Fail(toolerr.CodeNotFile, "the path is a directory")
		}
		return nil, nil, toolerr.Reject(toolerr.CodePathEscape, "the path is not a regular file")
	}
	f, err := openNoFollow(r.Abs)
	if err != nil {
		return nil, nil, toolerr.Fail(toolerr.CodeIO, "the file could not be opened")
	}
	post, err := f.Stat()
	if err != nil || !post.Mode().IsRegular() || isLink(post) || !os.SameFile(r.Info, post) {
		_ = f.Close()
		return nil, nil, toolerr.Reject(toolerr.CodePathEscape, "the file changed while it was being opened")
	}
	return f, post, nil
}

// afterOpen is a test seam: it runs between opening a file and the second look at its
// path, where a concurrent change of a directory of the path would have to land.
var afterOpen func()

// openExisting resolves a path and opens the regular file there, and looks at the path
// a second time once the file is open: the walk, the open and the second walk must all
// reach the same file. A directory of the path that was replaced by a link while the
// file was being opened is then caught by the second walk (which refuses links), where
// an open that follows intermediate links would have taken it for the file it expected.
// A change that is undone before the second look is not caught: containment is judged
// at two instants, not held, and a writer outside the workspace lock is outside what
// the Harness promises.
func (s Scope) openExisting(p string, a Access) (Resolved, *os.File, os.FileInfo, error) {
	r, err := s.Resolve(p, a)
	if err != nil {
		return Resolved{}, nil, nil, err
	}
	f, info, err := openRegular(r)
	if err != nil {
		return Resolved{}, nil, nil, err
	}
	if afterOpen != nil {
		afterOpen()
	}
	again, err := s.walk(r.Segs, false)
	if err != nil || again.Info == nil || !os.SameFile(again.Info, info) {
		_ = f.Close()
		return Resolved{}, nil, nil, toolerr.Reject(toolerr.CodePathEscape, "the path changed while the file was being opened")
	}
	return r, f, info, nil
}
