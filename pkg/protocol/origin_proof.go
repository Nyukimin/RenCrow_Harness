package protocol

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"

	"github.com/Nyukimin/RenCrow_Harness/internal/canon"
)

// OriginKey is a 32-byte HMAC-SHA256 key. It prints as a redaction under every
// fmt verb so it cannot reach a log by accident.
type OriginKey struct{ key [32]byte }

// Format implements fmt.Formatter.
func (OriginKey) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, "OriginKey[REDACTED]") }

// ParseOriginKeyFile reads the one accepted key file form: 64 lowercase hex
// characters (32 bytes) with at most one final LF. A BOM, CRLF, whitespace,
// uppercase, raw binary and base64 are rejected; there is no format guessing.
func ParseOriginKeyFile(data []byte) (OriginKey, error) {
	text := data
	if len(text) == 65 && text[64] == '\n' {
		text = text[:64]
	}
	var k OriginKey
	if len(text) != 64 {
		return k, ErrInvalidKeyFile
	}
	for _, c := range text {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return k, ErrInvalidKeyFile
		}
	}
	if _, err := hex.Decode(k.key[:], text); err != nil {
		return OriginKey{}, ErrInvalidKeyFile
	}
	return k, nil
}

// OriginProof is the Human relay proof a trusted issuer attaches to a mutation.
// Timestamps are UTC seconds as "YYYY-MM-DDTHH:MM:SSZ"; Sequence is the accepted
// original's sequence.
type OriginProof struct {
	Issuer              string `json:"issuer"`
	KeyID               string `json:"key_id"`
	Audience            string `json:"audience"`
	Origin              string `json:"origin"`
	SourceMessageID     string `json:"source_message_id"`
	SourceThreadID      string `json:"source_thread_id"`
	DestinationThreadID string `json:"destination_thread_id"`
	MutationKey         string `json:"mutation_key"`
	RawHash             string `json:"raw_hash"`
	Sequence            uint64 `json:"sequence"`
	IssuedAt            string `json:"issued_at"`
	ExpiresAt           string `json:"expires_at"`
	Nonce               string `json:"nonce"`
	MAC                 string `json:"mac"`
}

// originProofMACInput is "rencrow-origin-proof/v1\0" followed by LP of each field
// except mac, in the fixed order, with the sequence as a plain decimal.
func originProofMACInput(p OriginProof) ([]byte, error) {
	fields := []string{
		p.Issuer, p.KeyID, p.Audience, p.Origin, p.SourceMessageID, p.SourceThreadID, p.DestinationThreadID,
		p.MutationKey, p.RawHash, strconv.FormatUint(p.Sequence, 10), p.IssuedAt, p.ExpiresAt, p.Nonce,
	}
	if err := checkUTF8("origin proof", fields...); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString("rencrow-origin-proof/v1\x00")
	for _, f := range fields {
		b.Write(canon.LP([]byte(f)))
	}
	return b.Bytes(), nil
}

// OriginProofMAC computes the proof's MAC: lowercase hex HMAC-SHA256 over the MAC
// input. The proof's own MAC field is ignored.
func OriginProofMAC(key OriginKey, p OriginProof) (string, error) {
	input, err := originProofMACInput(p)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, key.key[:])
	m.Write(input)
	return hex.EncodeToString(m.Sum(nil)), nil
}

// VerifyOriginProofMAC recomputes the MAC and compares it with the proof's MAC in
// constant time. A MAC that is not exactly 64 lowercase hex characters fails the
// same way as a wrong one. This checks the MAC only: issuer and key allowlists,
// audience, thread and mutation key binding, raw hash, time window and nonce
// uniqueness belong to the intake.
func VerifyOriginProofMAC(key OriginKey, p OriginProof) error {
	want, err := OriginProofMAC(key, p)
	if err != nil {
		return err
	}
	if len(p.MAC) != 64 {
		return ErrOriginProofMAC
	}
	for i := 0; i < len(p.MAC); i++ {
		if c := p.MAC[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return ErrOriginProofMAC
		}
	}
	got, err := hex.DecodeString(p.MAC)
	if err != nil {
		return ErrOriginProofMAC
	}
	wantBytes, err := hex.DecodeString(want)
	if err != nil {
		return ErrOriginProofMAC
	}
	if !hmac.Equal(got, wantBytes) {
		return ErrOriginProofMAC
	}
	return nil
}

// OriginProofDigest is the lowercase hex SHA-256 of the proof's MAC input: the
// digest that IntakeReceipt.proof_digest carries for a verified relay. It is the
// digest the design vector publishes as mac_input_sha256. The MAC input holds every
// proof field except the MAC, so the digest identifies the proof (issuer, key id,
// nonce, bindings, times) and neither contains nor reveals the MAC or the key.
func OriginProofDigest(p OriginProof) (string, error) {
	input, err := originProofMACInput(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(input)
	return hex.EncodeToString(sum[:]), nil
}
