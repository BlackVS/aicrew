package store

import (
	"context"
	"fmt"
	"time"
)

// A step's scan progress (crew-execution b3b). Settling a pending step reads
// the read scope once per lookup: a receipt by each of its proofs, or, for a
// proofless update, by each of its keys. A lookup whose "none" is final can
// never show a commit afterwards, so it is recorded here and not read again:
// a step with more lookups than one read window finishes across windows and
// restarts, whatever its kind.
//
// A "none" is final, and recorded, only by the read scope's finality rules
// (none_finality):
//   - a proof's, when read at least NoneFinalAfter after the proof ended or
//     expired;
//   - an update key's, when read after the hold's fence or task revision was
//     observed past the request.

// proofLookup names the receipt lookup by a proof, by its stored digest.
func proofLookup(digest string) string { return "proof:" + digest }

// keyLookup names the receipt lookup by an update's request key.
func keyLookup(key string) string { return "key:" + key }

// scanFinals are attemptID's lookups whose "none" is final.
func (s *Store) scanFinals(ctx context.Context, attemptID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT lookup FROM scan_finals WHERE attempt_id = ?`, attemptID)
	if err != nil {
		return nil, fmt.Errorf("read scan progress: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out[l] = true
	}
	return out, rows.Err()
}

// markScanFinal records that lookup's "none", read at at, is final. It
// caches a read-scope observation and changes no attempt state; the settle
// that uses it is audited as always.
func (s *Store) markScanFinal(ctx context.Context, attemptID, lookup string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO scan_finals (attempt_id, lookup, final_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
		attemptID, lookup, formatTime(at)); err != nil {
		return fmt.Errorf("record scan progress: %w", err)
	}
	return nil
}
