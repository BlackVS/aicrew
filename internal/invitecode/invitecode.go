// Package invitecode is the invitation code's text form
// (docs/ONBOARDING-CONTRACT.md, "Invitations"), shared by the store, which
// issues codes and keeps their digests, and the agent's client, which
// checks a typed code before it spends an invitation attempt on it.
//
// A code is 26 random Crockford base32 characters (130 bits) followed by two
// checksum characters, shown in groups of four: XXXX-XXXX-...-XXXX. Crockford
// base32 has no I, L, O or U, and parsing accepts common confusions (O as 0,
// I and L as 1), any case, and optional separators.
package invitecode

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
)

const (
	Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	BodyLen  = 26 // 26 characters × 5 bits = 130 random bits
	CheckLen = 2
)

// ErrMalformed is a code with a character outside the alphabet, the wrong
// length or a checksum that does not match.
var ErrMalformed = errors.New("invitation code is malformed")

// Generate returns a new code in its grouped display form.
func Generate() (string, error) {
	var raw [BodyLen]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate invitation code: %w", err)
	}
	body := make([]byte, BodyLen)
	for i, b := range raw {
		body[i] = Alphabet[b&31] // 256 is a multiple of 32: uniform
	}
	full := string(body) + Checksum(string(body))
	var groups []string
	for i := 0; i < len(full); i += 4 {
		groups = append(groups, full[i:min(i+4, len(full))])
	}
	return strings.Join(groups, "-"), nil
}

// Normalize returns the code's canonical form (upper case, no separators,
// confusions mapped) after checking its alphabet, length and checksum.
func Normalize(code string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch r {
		case '-', ' ':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		if r > 127 || !strings.ContainsRune(Alphabet, r) {
			return "", fmt.Errorf("%w: it has an invalid character", ErrMalformed)
		}
		b.WriteRune(r)
	}
	norm := b.String()
	if len(norm) != BodyLen+CheckLen {
		return "", fmt.Errorf("%w: it has the wrong length", ErrMalformed)
	}
	if Checksum(norm[:BodyLen]) != norm[BodyLen:] {
		return "", fmt.Errorf("%w: its checksum does not match", ErrMalformed)
	}
	return norm, nil
}

// Checksum is two base32 characters from the first ten bits of the body's
// SHA-256: it catches typing errors, not tampering.
func Checksum(body string) string {
	h := sha256.Sum256([]byte(body))
	return string([]byte{Alphabet[h[0]>>3], Alphabet[(h[0]&7)<<2|h[1]>>6]})
}
