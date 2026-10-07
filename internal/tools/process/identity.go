package process

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// ErrNoSuchProcess is returned by a Prober for a process that does not exist.
var ErrNoSuchProcess = errors.New("process: no such process")

// ErrUnsupported is returned where an OS has no way to read a process's start time or
// the host's incarnation: what cannot be verified is reported as unverifiable, never
// guessed.
var ErrUnsupported = errors.New("process: not supported on this OS")

// Identity is what makes a PID mean one particular process: the incarnation of the
// host (a PID of another boot is another process), the process's start time as the OS
// reports it (a PID reused later starts later), and a nonce the Harness drew for this
// attempt. It is recorded with the Attempt right after the process starts.
//
// The nonce ties the record to the attempt that made it. It cannot be read back from a
// foreign process on every OS, so matching a live process rests on the incarnation,
// the PID and the start time.
type Identity struct {
	Incarnation string `json:"incarnation"`
	PID         int    `json:"pid"`
	Start       string `json:"start"`
	Nonce       string `json:"nonce"`
}

// Encode is the text stored as the Attempt's process_token.
func (i Identity) Encode() string {
	b, _ := json.Marshal(i)
	return string(b)
}

// DecodeIdentity reads a stored process_token. Anything that is not exactly an
// Identity is an error: a token that cannot be read is not matched loosely.
func DecodeIdentity(s string) (Identity, error) {
	var i Identity
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&i); err != nil {
		return Identity{}, errors.New("process: the token is not an identity")
	}
	return i, nil
}

// identify reads the identity of a process that has just started. It never fails: an
// OS that cannot give a field leaves it empty, which makes the identity unverifiable
// later, not wrong.
func identify(pid int) Identity {
	id := Identity{PID: pid}
	id.Incarnation, _ = hostIncarnation()
	id.Start, _ = startToken(pid)
	var n [8]byte
	if _, err := rand.Read(n[:]); err == nil {
		id.Nonce = hex.EncodeToString(n[:])
	}
	return id
}

// Identify is identify for the current process tree's owner: the identity of pid now.
func Identify(pid int) Identity { return identify(pid) }

// OSProber is the Prober of the running OS.
type OSProber struct{}

// HostIncarnation identifies this boot of the host.
func (OSProber) HostIncarnation() (string, error) { return hostIncarnation() }

// StartToken is the start time token of a process.
func (OSProber) StartToken(pid int) (string, error) { return startToken(pid) }

// StopTree stops the process and, where the OS has process groups, the group it leads,
// provided its start time is still start.
func (OSProber) StopTree(pid int, start string) error { return stopTree(pid, start) }
