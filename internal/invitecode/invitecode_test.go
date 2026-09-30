package invitecode

import (
	"errors"
	"strings"
	"testing"
)

func TestGenerateNormalizeRoundTrip(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		code, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != 34 || strings.Count(code, "-") != 6 {
			t.Fatalf("display form %q", code)
		}
		norm, err := Normalize(code)
		if err != nil || norm != strings.ReplaceAll(code, "-", "") || seen[norm] {
			t.Fatalf("%q: %q, %v", code, norm, err)
		}
		seen[norm] = true
		// Case, separators and the common confusions are accepted.
		loose := strings.NewReplacer("0", "o", "1", "l").Replace(strings.ToLower(strings.ReplaceAll(code, "-", " ")))
		if again, err := Normalize(loose); err != nil || again != norm {
			t.Fatalf("%q: %q, %v", loose, again, err)
		}
	}
}

func TestNormalizeRefuses(t *testing.T) {
	code, _ := Generate()
	norm, _ := Normalize(code)
	flip := func(i int) string {
		b := []byte(norm)
		b[i] = Alphabet[(strings.IndexByte(Alphabet, b[i])+1)%len(Alphabet)]
		return string(b)
	}
	for name, in := range map[string]string{
		"empty":         "",
		"short":         norm[:BodyLen+CheckLen-1],
		"long":          norm + "0",
		"invalid char":  norm[:5] + "U" + norm[6:],
		"non-ASCII":     norm[:5] + "é" + norm[6:],
		"body typo":     flip(3),
		"checksum typo": flip(BodyLen + 1),
	} {
		if _, err := Normalize(in); !errors.Is(err, ErrMalformed) {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := Normalize(in); err != nil && strings.Contains(err.Error(), norm) {
			t.Fatalf("%s: the error quotes the code", name)
		}
	}
}
