package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"

	"github.com/Nyukimin/RenCrow_Harness/internal/tools/toolerr"
)

// CreateArgs are the arguments of file.create. expected_absent is always true: the
// schema allows no other value.
type CreateArgs struct {
	Path string
	Text string
}

// CreateResult is the result of file.create.
type CreateResult struct {
	Path         string `json:"path"`
	BytesWritten int64  `json:"bytes_written"`
	RawHash      string `json:"raw_hash"`
}

// EditArgs are the arguments of file.edit.
type EditArgs struct {
	Path         string
	ExpectedHash string
	OldText      string
	NewText      string
}

// EditResult is the result of file.edit.
type EditResult struct {
	Path          string `json:"path"`
	OldHash       string `json:"old_hash"`
	NewHash       string `json:"new_hash"`
	NewTotalBytes int64  `json:"new_total_bytes"`
	ReplacedAt    int64  `json:"replaced_at"`
}

// tempName is a fresh name for the file a write is staged in, in the same directory
// as its target so the final step is a rename or link within one filesystem.
func tempName(dir string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", toolerr.Fail(toolerr.CodeIO, "a temporary name could not be made")
	}
	return filepath.Join(dir, ".rencrow-tmp-"+hex.EncodeToString(b[:])), nil
}

// stage writes data to a new file next to the target and flushes it to disk.
func stage(dir string, data []byte, perm os.FileMode, preservePerm bool) (string, error) {
	tmp, err := tempName(dir)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, stageCreatePerm(perm, preservePerm))
	if err != nil {
		return "", toolerr.Fail(toolerr.CodeIO, "a temporary file could not be created")
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", toolerr.Fail(toolerr.CodeIO, "a temporary file could not be written")
	}
	if preservePerm {
		if err := restoreStagePerm(f, perm); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return "", toolerr.Fail(toolerr.CodeIO, "a temporary file's permissions could not be set")
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", toolerr.Fail(toolerr.CodeIO, "a temporary file could not be flushed")
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", toolerr.Fail(toolerr.CodeIO, "a temporary file could not be closed")
	}
	return tmp, nil
}

// CreateFile is file.create: the file is written whole to a staging file and
// published under its name only if no file has that name, so a reader never sees a
// partial file and an existing file is never touched. The parent directory must exist;
// none is created.
func CreateFile(ctx context.Context, sc Scope, a CreateArgs) (CreateResult, error) {
	if len(a.Text) > 1<<20 {
		return CreateResult{}, toolerr.Reject(toolerr.CodeFileTooLarge, "the text is longer than file.create takes")
	}
	r, err := sc.resolve(a.Path, resolveOpts{access: Write, missingOK: true})
	if err != nil {
		return CreateResult{}, err
	}
	if r.Exists {
		return CreateResult{}, toolerr.Fail(toolerr.CodeAlreadyExists, "a file or directory already has this name")
	}
	if len(r.Segs) == 0 {
		return CreateResult{}, toolerr.Reject(toolerr.CodePathInvalid, "the workspace root cannot be created")
	}
	if err := ctx.Err(); err != nil {
		return CreateResult{}, toolerr.Fail(toolerr.CodeCancelled, "the write was stopped")
	}
	dir := filepath.Dir(r.Abs)
	data := []byte(a.Text)
	tmp, err := stage(dir, data, 0o644, false)
	if err != nil {
		return CreateResult{}, err
	}
	// link(2) fails if the name exists: the check and the publication are one step.
	// A filesystem without hard links falls back to an exclusive create, which has the
	// same refusal but is not published atomically.
	if lerr := os.Link(tmp, r.Abs); lerr != nil {
		_ = os.Remove(tmp)
		if errors.Is(lerr, os.ErrExist) {
			return CreateResult{}, toolerr.Fail(toolerr.CodeAlreadyExists, "a file or directory already has this name")
		}
		if err := createExclusive(r.Abs, data); err != nil {
			return CreateResult{}, err
		}
	} else {
		_ = os.Remove(tmp)
	}
	sum := sha256.Sum256(data)
	if err := syncDir(dir); err != nil {
		return CreateResult{}, toolerr.Uncertain(toolerr.CodeDurability, "the file was created but its directory could not be flushed")
	}
	return CreateResult{Path: r.Rel, BytesWritten: int64(len(data)), RawHash: hex.EncodeToString(sum[:])}, nil
}

func createExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return toolerr.Fail(toolerr.CodeAlreadyExists, "a file or directory already has this name")
		}
		return toolerr.Fail(toolerr.CodeIO, "the file could not be created")
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return toolerr.Fail(toolerr.CodeIO, "the file could not be written")
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return toolerr.Uncertain(toolerr.CodeDurability, "the file was written but could not be flushed")
	}
	if err := f.Close(); err != nil {
		return toolerr.Uncertain(toolerr.CodeDurability, "the file was written but could not be closed")
	}
	return nil
}

// uniqueIndex finds old in data and requires exactly one occurrence, overlapping
// ones included: "aa" in "aaa" is not unique.
func uniqueIndex(data, old []byte) (int, error) {
	i := bytes.Index(data, old)
	if i < 0 {
		return 0, toolerr.Fail(toolerr.CodeOldTextMissing, "old_text does not occur in the file")
	}
	if bytes.Contains(data[i+1:], old) {
		return 0, toolerr.Fail(toolerr.CodeOldTextAmbig, "old_text occurs more than once in the file")
	}
	return i, nil
}

// beforeReplace is a test seam: it runs after the new content is staged and before the
// last look at the target, where a change by someone else would have to land.
var beforeReplace func()

var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// EditFile is file.edit. The file is read once and must hash to expected_hash; old_text
// must occur exactly once, byte for byte (no fuzzy matching, no line-number
// correction); the new content is staged next to the file and flushed, the file is
// hashed again immediately before the rename (a change by someone else in between is a
// conflict, not overwritten), and the rename replaces it atomically. A failure to
// flush the directory after the rename leaves the outcome unknown.
//
// This is not a filesystem compare-and-swap: a writer that ignores the workspace lock
// can still slip in between the last check and the rename.
func EditFile(ctx context.Context, sc Scope, a EditArgs) (EditResult, error) {
	if !hashRE.MatchString(a.ExpectedHash) {
		return EditResult{}, toolerr.Reject(toolerr.CodeHashMismatch, "expected_hash is not a SHA-256 in lowercase hex")
	}
	if a.OldText == "" {
		return EditResult{}, toolerr.Reject(toolerr.CodeOldTextMissing, "old_text is empty")
	}
	r, data, hash, info, err := readWhole(sc, a.Path, Write, MaxEditBytes)
	if err != nil {
		return EditResult{}, err
	}
	if hash != a.ExpectedHash {
		return EditResult{}, toolerr.Fail(toolerr.CodeHashMismatch, "the file's hash is not expected_hash (it is now %s)", hash)
	}
	at, err := uniqueIndex(data, []byte(a.OldText))
	if err != nil {
		return EditResult{}, err
	}
	out := make([]byte, 0, len(data)-len(a.OldText)+len(a.NewText))
	out = append(out, data[:at]...)
	out = append(out, a.NewText...)
	out = append(out, data[at+len(a.OldText):]...)
	if int64(len(out)) > MaxEditBytes {
		return EditResult{}, toolerr.Fail(toolerr.CodeFileTooLarge, "the edited file would be larger than file.edit handles")
	}
	if err := ctx.Err(); err != nil {
		return EditResult{}, toolerr.Fail(toolerr.CodeCancelled, "the write was stopped")
	}
	dir := filepath.Dir(r.Abs)
	tmp, err := stage(dir, out, info.Mode().Perm(), true)
	if err != nil {
		return EditResult{}, err
	}
	if beforeReplace != nil {
		beforeReplace()
	}
	// The last look before the file is replaced.
	_, _, again, _, err := readWhole(sc, a.Path, Write, MaxEditBytes)
	if err == nil && again != hash {
		err = toolerr.Fail(toolerr.CodeHashMismatch, "the file changed while the edit was being prepared")
	}
	if err != nil {
		_ = os.Remove(tmp)
		return EditResult{}, err
	}
	if err := os.Rename(tmp, r.Abs); err != nil {
		_ = os.Remove(tmp)
		return EditResult{}, toolerr.Fail(toolerr.CodeIO, "the edited file could not replace the original")
	}
	sum := sha256.Sum256(out)
	if err := syncDir(dir); err != nil {
		return EditResult{}, toolerr.Uncertain(toolerr.CodeDurability, "the file was replaced but its directory could not be flushed")
	}
	return EditResult{Path: r.Rel, OldHash: hash, NewHash: hex.EncodeToString(sum[:]), NewTotalBytes: int64(len(out)), ReplacedAt: int64(at)}, nil
}
