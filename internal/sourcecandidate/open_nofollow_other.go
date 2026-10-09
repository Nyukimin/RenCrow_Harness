//go:build !unix && !windows

package sourcecandidate

import (
	"errors"
	"os"
)

func openRegularNoFollow(string) (*os.File, error) {
	return nil, errors.New("no-follow file open is unavailable")
}
