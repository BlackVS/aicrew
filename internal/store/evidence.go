package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// evidenceDigest is coordination.v1's e1_ digest of a finalize's terminal
// evidence (aimem C5-w3): e1_ and the unpadded base64url SHA-256 of, for
// each reference in order, the 4-byte big-endian length of its UTF-8 bytes
// and then the bytes. Nothing is normalised: order, case, spaces and
// duplicates all count. aimem recomputes it over the finalize's
// terminal_evidence and refuses a mismatch.
func evidenceDigest(refs []string) string {
	h := sha256.New()
	var n [4]byte
	for _, r := range refs {
		binary.BigEndian.PutUint32(n[:], uint32(len(r)))
		h.Write(n[:])
		h.Write([]byte(r))
	}
	return "e1_" + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// pendingRefs are the references a pending finalize sends aimem as its
// terminal evidence, in order: the evidence recorded when it began.
func pendingRefs(a Attempt) ([]string, error) {
	var evidence []Evidence
	if err := json.Unmarshal([]byte(a.PendingEvidence), &evidence); err != nil {
		return nil, fmt.Errorf("attempt %s: pending evidence: %w", a.ID, err)
	}
	refs := make([]string, 0, len(evidence))
	for _, e := range evidence {
		refs = append(refs, e.Ref)
	}
	return refs, nil
}

// checkEvidenceRef refuses a reference aimem would refuse as terminal
// evidence for its shape (aimem's task text rule), so that a confirmed set
// is never refused at finalize for it: valid UTF-8, not blank, at most
// maxRefLen bytes (aimem allows 512), no control character but tab, newline
// and carriage return, and no bidirectional override. aimem also refuses
// secret-shaped text, which aicrew does not mirror.
func checkEvidenceRef(ref string) error {
	switch {
	case !utf8.ValidString(ref):
		return fmt.Errorf("%w: a delivery reference is not valid UTF-8", ErrInvalid)
	case strings.TrimSpace(ref) == "":
		return fmt.Errorf("%w: a delivery reference is blank", ErrInvalid)
	case len(ref) > maxRefLen:
		return fmt.Errorf("%w: a delivery reference is longer than %d bytes", ErrInvalid, maxRefLen)
	}
	for _, r := range ref {
		if unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r' {
			return fmt.Errorf("%w: a delivery reference has a control character", ErrInvalid)
		}
		if (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			return fmt.Errorf("%w: a delivery reference has a bidirectional override", ErrInvalid)
		}
	}
	return nil
}
