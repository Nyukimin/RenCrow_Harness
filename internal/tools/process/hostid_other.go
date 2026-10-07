//go:build !linux && !darwin && !windows

package process

func hostIncarnation() (string, error) { return "", ErrUnsupported }

func startToken(int) (string, error) { return "", ErrUnsupported }
