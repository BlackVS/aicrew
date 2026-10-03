// Package optoken is the operator credential: the bearer that authorizes
// aicrewd's operator API. It lives only in owner-only files: the one
// aicrewd.json names as operator_token_file, which aicrewd reads on every
// operator call, and the copy the operator's client reads. aicrewd keeps no
// trace of it in its store. Replacing the file rotates the credential.
package optoken

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/BlackVS/aicrew/internal/privatefile"
)

// Prefix marks an operator token, so it is told apart from every other
// aicrew and aimem bearer at a glance.
const Prefix = "aop_"

// shape is a token: the prefix and 32 random bytes in lowercase hex. Only a
// generated token has it, so a weak hand-made value is refused.
var shape = regexp.MustCompile(`^aop_[0-9a-f]{64}$`)

// Generate returns a new token.
func Generate() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return Prefix + hex.EncodeToString(b[:]), nil
}

// Valid reports whether s has a token's shape.
func Valid(s string) bool { return shape.MatchString(s) }

// Write stores a new token in path, which must not exist, as a new
// owner-only file. A failed write removes the file.
func Write(path, token string) error {
	if !Valid(token) {
		return errors.New("not an operator token")
	}
	f, err := privatefile.Create(path)
	if err != nil {
		return err
	}
	_, werr := io.WriteString(f, token+"\n")
	if werr == nil {
		werr = f.Sync()
	}
	if err := errors.Join(werr, f.Close()); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// Read returns the token in path, which must be an owner-only file holding
// one token alone on its line. Its errors name the path and the fault, never
// the content.
func Read(path string) (string, error) {
	if err := privatefile.Check(path); err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return "", err
	}
	s := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if !Valid(s) {
		return "", fmt.Errorf("%s does not hold an operator token alone on one line (create one with `aicrew operator-token new`)", path)
	}
	return s, nil
}
