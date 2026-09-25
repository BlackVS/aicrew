package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrForbidden           = errors.New("forbidden")
	ErrInvalid             = errors.New("invalid input")
	ErrNotFound            = errors.New("not found")
	ErrAmbiguous           = errors.New("ambiguous label")
	ErrExists              = errors.New("already exists")
	ErrRevisionConflict    = errors.New("revision conflict")
	ErrIdempotencyConflict = errors.New("idempotency key reused with different input")
)

const maxKeyLen = 128

// command is one mutation. run authorizes it, replays an earlier receipt for
// the same (caller, op, scope, key), or applies it and commits its state
// change, audit record and receipt in a single transaction. Only committed
// commands get a receipt: a refusal (forbidden, invalid, conflict) records
// nothing, so a retry with the same key is evaluated again.
type command struct {
	op        string
	scope     string
	key       string
	input     any
	authorize func(Caller) error
	validate  func() error
	apply     func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error)
}

func (s *Store) run(ctx context.Context, c Caller, cmd command, out any) error {
	// Authorization comes first, on every call including replays, so a
	// receipt never answers a caller the current policy would refuse.
	if err := cmd.authorize(c); err != nil {
		return err
	}
	if cmd.key == "" || len(cmd.key) > maxKeyLen {
		return fmt.Errorf("%w: idempotency key must be 1-%d characters", ErrInvalid, maxKeyLen)
	}
	if cmd.validate != nil {
		if err := cmd.validate(); err != nil {
			return err
		}
	}
	input, err := json.Marshal(cmd.input)
	if err != nil {
		return fmt.Errorf("encode input: %w", err)
	}
	sum := sha256.Sum256(input)
	digest := hex.EncodeToString(sum[:])

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var prevDigest, prevResult string
	err = tx.QueryRowContext(ctx,
		`SELECT input_digest, result FROM receipts
		 WHERE caller_kind = ? AND caller_id = ? AND operation = ? AND scope = ? AND key = ?`,
		c.kind.String(), c.id, cmd.op, cmd.scope, cmd.key).Scan(&prevDigest, &prevResult)
	switch {
	case err == nil:
		if prevDigest != digest {
			return fmt.Errorf("%w: %s key %q", ErrIdempotencyConflict, cmd.op, cmd.key)
		}
		return decodeResult(prevResult, out)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("read receipt: %w", err)
	}

	now := s.now()
	res, err := cmd.apply(ctx, tx, now)
	if err != nil {
		return err
	}
	result, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	at := formatTime(now)
	auditRes, err := tx.ExecContext(ctx,
		`INSERT INTO audit (at, caller_kind, caller_id, operation, scope, key, input_digest, input, result)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		at, c.kind.String(), c.id, cmd.op, cmd.scope, cmd.key, digest, string(input), string(result))
	if err != nil {
		return fmt.Errorf("write audit: %w", err)
	}
	auditID, err := auditRes.LastInsertId()
	if err != nil {
		return fmt.Errorf("audit id: %w", err)
	}
	if s.beforeReceipt != nil {
		if err := s.beforeReceipt(cmd.op); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO receipts (caller_kind, caller_id, operation, scope, key, input_digest, result, audit_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.kind.String(), c.id, cmd.op, cmd.scope, cmd.key, digest, string(result), auditID, at); err != nil {
		return fmt.Errorf("write receipt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return decodeResult(string(result), out)
}

func decodeResult(result string, out any) error {
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(result), out); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	return nil
}
