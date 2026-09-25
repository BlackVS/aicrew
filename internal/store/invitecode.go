package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Invitation codes are bearer capabilities (docs/ONBOARDING-CONTRACT.md).
//
// A code is 26 random Crockford base32 characters (130 bits) followed by two
// checksum characters, shown in groups of four: XXXX-XXXX-...-XXXX. The
// trusted caller generates it with GenerateInvitationCode and shows it once;
// the store keeps only its digest. Crockford base32 has no I, L, O or U, and
// parsing accepts common confusions (O as 0, I and L as 1), any case, and
// optional separators.

const (
	codeAlphabet  = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	codeBodyLen   = 26 // 26 characters × 5 bits = 130 random bits
	codeCheckLen  = 2
	codeDigestTag = "aicrew-invitation-v1:"
)

// GenerateInvitationCode returns a new invitation code. The caller shows it
// once and passes it to IssueInvitation; the code is never stored.
func GenerateInvitationCode() (Secret, error) {
	var raw [codeBodyLen]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Secret{}, fmt.Errorf("generate invitation code: %w", err)
	}
	body := make([]byte, codeBodyLen)
	for i, b := range raw {
		body[i] = codeAlphabet[b&31] // 256 is a multiple of 32: uniform
	}
	full := string(body) + codeChecksum(string(body))
	var groups []string
	for i := 0; i < len(full); i += 4 {
		groups = append(groups, full[i:min(i+4, len(full))])
	}
	return NewSecret(strings.Join(groups, "-")), nil
}

// invitationCodeDigest normalizes a code, verifies its checksum and returns
// the digest the store keeps. The code itself is never returned or kept.
func invitationCodeDigest(code Secret) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(code.Reveal()) {
		switch r {
		case '-', ' ':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		if r > 127 || !strings.ContainsRune(codeAlphabet, r) {
			return "", fmt.Errorf("%w: invitation code has an invalid character", ErrInvalid)
		}
		b.WriteRune(r)
	}
	norm := b.String()
	if len(norm) != codeBodyLen+codeCheckLen {
		return "", fmt.Errorf("%w: invitation code has the wrong length", ErrInvalid)
	}
	body, check := norm[:codeBodyLen], norm[codeBodyLen:]
	if codeChecksum(body) != check {
		return "", fmt.Errorf("%w: invitation code checksum does not match", ErrInvalid)
	}
	sum := sha256.Sum256([]byte(codeDigestTag + norm))
	return hex.EncodeToString(sum[:]), nil
}

// codeChecksum is two base32 characters from the first ten bits of the
// body's SHA-256: it catches typing errors, not tampering.
func codeChecksum(body string) string {
	h := sha256.Sum256([]byte(body))
	return string([]byte{codeAlphabet[h[0]>>3], codeAlphabet[(h[0]&7)<<2|h[1]>>6]})
}
