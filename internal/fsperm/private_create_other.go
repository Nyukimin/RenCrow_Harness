//go:build !unix && !windows

package fsperm

import (
	"fmt"
	"os"
)

func CreatePrivateDir(path string) error {
	return fmt.Errorf("%w: private directory creation is not implemented", ErrUnverifiable)
}

func CreatePrivateFile(path string) (*os.File, error) {
	return nil, fmt.Errorf("%w: private file creation is not implemented", ErrUnverifiable)
}

func InitializeProcessOwner() error {
	return fmt.Errorf("%w: process owner initialization is not implemented", ErrUnverifiable)
}

func CheckProcessOwner() error {
	return fmt.Errorf("%w: process owner verification is not implemented", ErrUnverifiable)
}
