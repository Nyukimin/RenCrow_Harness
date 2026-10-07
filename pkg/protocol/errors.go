package protocol

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

var (
	// ErrInvalidInput wraps every rejection of a malformed or out-of-contract input.
	ErrInvalidInput = errors.New("protocol: invalid input")

	// ErrInvalidKeyFile is returned for a key file that is not exactly 64 lowercase
	// hex characters plus an optional final LF.
	ErrInvalidKeyFile = fmt.Errorf("%w: key file must be 64 lowercase hex characters and an optional final LF", ErrInvalidInput)

	// ErrNotMutationMethod is returned when a mutation payload hash is requested for
	// a method outside the seven idempotent native mutations.
	ErrNotMutationMethod = fmt.Errorf("%w: method does not take a mutation payload hash", ErrInvalidInput)

	// ErrOriginProofMAC is returned when an OriginProof MAC is absent, malformed or
	// does not match. It deliberately says nothing about which.
	ErrOriginProofMAC = errors.New("protocol: origin proof MAC verification failed")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}

func invalidWrap(err error, context string) error {
	return fmt.Errorf("%w: %s: %w", ErrInvalidInput, context, err)
}

func checkUTF8(field string, values ...string) error {
	for _, v := range values {
		if !utf8.ValidString(v) {
			return invalid("%s is not valid UTF-8", field)
		}
	}
	return nil
}
