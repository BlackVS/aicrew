package server

import "github.com/BlackVS/aicrew/internal/store"

// The step routes' bodies besides the offer's and the claim's, named so the
// agent's role guidance can be checked against them: every example body it
// shows decodes strictly into its route's type (pilot G2).

type acceptBody struct {
	InstructionDigest string `json:"instruction_digest"`
}

type stopBody struct {
	Reason string `json:"reason"`
}

type releaseBody struct {
	Target  store.ReleaseTarget `json:"target"`
	Blocker string              `json:"blocker,omitempty"`
}

type reviewBody struct {
	ResultSeq int64                `json:"result_seq"`
	Decision  store.ReviewDecision `json:"decision"`
}

type deliveryBody struct {
	ResultSeq int64            `json:"result_seq"`
	Evidence  []store.Evidence `json:"evidence"`
}

type finalizeBody struct {
	ResultSeq int64 `json:"result_seq"`
}

type workBody struct {
	Intent     store.WorkIntent `json:"intent"`
	Detail     string           `json:"detail,omitempty"`
	Supersedes string           `json:"supersedes,omitempty"`
}
