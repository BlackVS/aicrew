package store

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The shared coordination.v1 fixture, vendored once for the coordination
// route's tests; see its PROVENANCE.md.
const (
	coordFixture     = "../server/testdata/coordination-v1/examples.json"
	coordFixtureBlob = "26d408a0e04e1bb27a44ff7fe8a754c5214194de" // aimem a9b9b6f
)

type evidenceFixture struct {
	Scheme          string `json:"scheme"`
	ConfirmedDigest string `json:"confirmed_digest"`
	Vectors         []struct {
		Case             string   `json:"case"`
		TerminalEvidence []string `json:"terminal_evidence"`
		Digest           string   `json:"digest"`
	} `json:"vectors"`
	Cases []struct {
		Case             string   `json:"case"`
		TerminalEvidence []string `json:"terminal_evidence"`
		Outcome          string   `json:"outcome"`
	} `json:"cases"`
}

func loadEvidenceFixture(t *testing.T) evidenceFixture {
	t.Helper()
	raw, err := os.ReadFile(coordFixture)
	if err != nil {
		t.Fatal(err)
	}
	// The file is aimem's, byte for byte: its Git blob at the pinned commit.
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(raw))
	h.Write(raw)
	if got := hex.EncodeToString(h.Sum(nil)); got != coordFixtureBlob {
		t.Fatalf("the vendored fixture is blob %s, not aimem's %s", got, coordFixtureBlob)
	}
	var fx struct {
		EvidenceDigest evidenceFixture `json:"evidence_digest"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.EvidenceDigest.Vectors) < 15 || len(fx.EvidenceDigest.Cases) < 6 {
		t.Fatalf("the fixture has %d vectors and %d cases", len(fx.EvidenceDigest.Vectors), len(fx.EvidenceDigest.Cases))
	}
	return fx.EvidenceDigest
}

var evidenceDigestForm = regexp.MustCompile(`^e1_[A-Za-z0-9_-]{43}$`)

// The e1_ digest is aimem's, on every shared vector (empty, one reference,
// reordered, trailing space, case, dropped, extra, duplicate, the ab|c and
// a|bc pair, a newline inside a reference, non-ASCII), and on the outcome
// cases a mismatch is exactly a different digest from the confirmed one.
func TestEvidenceDigestVectors(t *testing.T) {
	fx := loadEvidenceFixture(t)
	seen := map[string]string{}
	for _, v := range fx.Vectors {
		got := evidenceDigest(v.TerminalEvidence)
		if got != v.Digest || !evidenceDigestForm.MatchString(got) {
			t.Errorf("%s: digest %s, want %s", v.Case, got, v.Digest)
		}
		if other, dup := seen[got]; dup {
			t.Errorf("%s and %s share a digest", v.Case, other)
		}
		seen[got] = v.Case
	}
	for _, c := range fx.Cases {
		match := evidenceDigest(c.TerminalEvidence) == fx.ConfirmedDigest
		if want := c.Outcome != "evidence_mismatch"; match != want {
			t.Errorf("%s: matches the confirmed digest %v, outcome %s", c.Case, match, c.Outcome)
		}
	}
}

// A delivery reference aimem would refuse as terminal evidence is refused
// when it is confirmed or offered on the one-shot path, and so are more than
// 16 references.
func TestDeliveryEvidenceShape(t *testing.T) {
	ok := []string{"https://forge.example/pull/7", "line one\nline two", "tab\there", "Überprüfung ✓ 検証",
		strings.Repeat("r", maxRefLen)}
	bad := map[string]string{
		"blank": "   ", "invalid UTF-8": "ref-\xff", "a control character": "ref\x1b[0m", "NUL": "ref\x00",
		"a bidirectional override": "ref‮", "an isolate": "ref⁦x⁩", "too long": strings.Repeat("r", maxRefLen+1),
	}
	with := func(ref string) TrustedDelivery {
		d := TrustedDelivery{Required: DevelopmentDelivery, Evidence: append([]Evidence(nil), deliveryEvidence...)}
		d.Evidence[1].Ref = ref
		return d
	}
	for _, ref := range ok {
		if err := with(ref).check(); err != nil {
			t.Errorf("%q: %v", ref, err)
		}
	}
	for name, ref := range bad {
		if err := with(ref).check(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	many := TrustedDelivery{Required: DevelopmentDelivery, Evidence: append([]Evidence(nil), deliveryEvidence...)}
	for len(many.Evidence) <= maxDeliveryEvidence {
		many.Evidence = append(many.Evidence, Evidence{Kind: "note", Ref: fmt.Sprintf("ref-%d", len(many.Evidence))})
	}
	if err := many.check(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%d references: %v", len(many.Evidence), err)
	}

	// Both paths use that check: a confirmation, and the one-shot finalize.
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, seq := e.submitted(t, "task-1")
	if _, err := e.s.ReviewWithToken(ctx, "review-1", e.leadTok, a.ID, seq, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-bidi", e.leadTok, a.ID, seq, with("ref‮").Evidence); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a confirmation with a bidirectional override: %v", err)
	}
	if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-many", e.leadTok, a.ID, seq, many.Evidence); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a confirmation of %d references: %v", len(many.Evidence), err)
	}
}

// A finalize begun from a confirmation serves the e1_ digest of exactly the
// references it sends aimem, in their confirmed order; a stop release's fact
// carries none (every kind is checked end to end by the coordination route's
// tests).
func TestFinalizeFactCarriesTheEvidenceDigest(t *testing.T) {
	ctx := context.Background()
	e := newClaimStopEnv(t)
	a, seq := e.submitted(t, "task-1")
	if _, err := e.s.ReviewWithToken(ctx, "review-1", e.leadTok, a.ID, seq, ReviewAccept); err != nil {
		t.Fatal(err)
	}
	evidence := []Evidence{deliveryEvidence[2], deliveryEvidence[0], deliveryEvidence[1]}
	if _, err := e.s.ConfirmDeliveryWithToken(ctx, "confirm-1", e.leadTok, a.ID, seq, evidence); err != nil {
		t.Fatal(err)
	}
	a, st, err := e.s.BeginFinalizeWithToken(ctx, "fin-1", e.indepTk, a.ID, seq)
	if err != nil {
		t.Fatal(err)
	}
	f := e.fact(t, st.CoordinationProof)
	sent := reservationRequest(a, "").TerminalEvidence
	identity := "aicrew attempt " + a.ID + " by member " + a.WorkerAgentID
	if !f.Active || f.Kind != FactAcceptedForFinalization ||
		f.EvidenceDigest != evidenceDigest(append(refsOf(evidence), identity)) ||
		f.EvidenceDigest != evidenceDigest(sent) {
		t.Fatalf("the finalize fact: %+v, sent %q", f, sent)
	}

	// Another kind carries none: a stop release.
	e2 := newClaimStopEnv(t)
	r, _ := e2.runningClaim(t, "task-1")
	if _, err := e2.s.RequestStopWithToken(ctx, "stop-1", e2.leadTok, r.ID, "priorities changed"); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.s.ConfirmStopWithToken(ctx, "confirm-stop-1", e2.indepTk, r.ID); err != nil {
		t.Fatal(err)
	}
	_, rst, err := e2.s.BeginStopReleaseWithToken(ctx, "release-1", e2.indepTk, r.ID, ReleaseBlocked, "waiting")
	if err != nil {
		t.Fatal(err)
	}
	if f := e2.fact(t, rst.CoordinationProof); !f.Active || f.EvidenceDigest != "" {
		t.Fatalf("a stop release fact: %+v", f)
	}
}
