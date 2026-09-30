package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/BlackVS/aicrew/internal/invitecode"
)

// Invitation codes are bearer capabilities (docs/ONBOARDING-CONTRACT.md).
// Their text form lives in internal/invitecode, shared with the agent's
// client; the trusted caller generates a code with GenerateInvitationCode
// and shows it once, and the store keeps only its digest.

const (
	codeAlphabet  = invitecode.Alphabet
	codeBodyLen   = invitecode.BodyLen
	codeCheckLen  = invitecode.CheckLen
	codeDigestTag = "aicrew-invitation-v1:"
)

// GenerateInvitationCode returns a new invitation code. The caller shows it
// once and passes it to IssueInvitation; the code is never stored.
func GenerateInvitationCode() (Secret, error) {
	code, err := invitecode.Generate()
	if err != nil {
		return Secret{}, err
	}
	return NewSecret(code), nil
}

// invitationCodeDigest normalizes a code, verifies its checksum and returns
// the digest the store keeps. The code itself is never returned or kept.
func invitationCodeDigest(code Secret) (string, error) {
	norm, err := invitecode.Normalize(code.Reveal())
	if errors.Is(err, invitecode.ErrMalformed) {
		return "", fmt.Errorf("%w: %s", ErrInvalid, err.Error())
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(codeDigestTag + norm))
	return hex.EncodeToString(sum[:]), nil
}

// codeChecksum is invitecode.Checksum, kept for the store's tests.
func codeChecksum(body string) string { return invitecode.Checksum(body) }
