package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"
	"unicode/utf8"
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
	// check runs inside the transaction before a new command applies, for
	// rules that depend on current state such as the session generation.
	check func(ctx context.Context, tx *sql.Tx) error
	// replayCheck runs inside the transaction before an earlier result is
	// returned, so a replay is refused once current state has moved on.
	replayCheck func(ctx context.Context, tx *sql.Tx, result string) error
	apply       func(ctx context.Context, tx *sql.Tx, now time.Time) (any, error)
}

func (s *Store) run(ctx context.Context, c Caller, cmd command, out any) error {
	// Caller policy comes first, on every call including replays, so a
	// receipt never answers a caller the current policy would refuse.
	if err := cmd.authorize(c); err != nil {
		return err
	}
	if err := checkKeyAndScope(cmd); err != nil {
		return err
	}
	if cmd.validate != nil {
		if err := cmd.validate(); err != nil {
			return err
		}
	}
	// JSON encoding replaces invalid UTF-8 with U+FFFD, which would give
	// distinct inputs the same digest. Refuse such text before hashing.
	if err := validateText(reflect.ValueOf(cmd.input)); err != nil {
		return err
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
		if cmd.replayCheck != nil {
			if err := cmd.replayCheck(ctx, tx, prevResult); err != nil {
				return err
			}
		}
		return decodeResult(prevResult, out)
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("read receipt: %w", err)
	}

	if cmd.check != nil {
		if err := cmd.check(ctx, tx); err != nil {
			return err
		}
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

func checkKeyAndScope(cmd command) error {
	if cmd.key == "" || len(cmd.key) > maxKeyLen || !utf8.ValidString(cmd.key) {
		return fmt.Errorf("%w: idempotency key must be 1-%d bytes of valid UTF-8", ErrInvalid, maxKeyLen)
	}
	if !utf8.ValidString(cmd.scope) {
		return fmt.Errorf("%w: scope is not valid UTF-8", ErrInvalid)
	}
	return nil
}

// receiptExists reports whether the caller already committed this command.
// Commands that must call out before their transaction (identity proof) use
// it so a retry is answered from the receipt instead of calling out again.
// Authorization and key checks come first, as in run.
func (s *Store) receiptExists(ctx context.Context, c Caller, cmd command) (bool, error) {
	if err := cmd.authorize(c); err != nil {
		return false, err
	}
	if err := checkKeyAndScope(cmd); err != nil {
		return false, err
	}
	var one int
	err := s.rdb.QueryRowContext(ctx,
		`SELECT 1 FROM receipts
		 WHERE caller_kind = ? AND caller_id = ? AND operation = ? AND scope = ? AND key = ?`,
		c.kind.String(), c.id, cmd.op, cmd.scope, cmd.key).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read receipt: %w", err)
	}
	return true, nil
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

// validateText refuses any string reachable from v that is not valid UTF-8.
func validateText(v reflect.Value) error {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return validateText(v.Elem())
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return fmt.Errorf("%w: text is not valid UTF-8", ErrInvalid)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if err := validateText(v.Field(i)); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if err := validateText(v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if err := validateText(iter.Key()); err != nil {
				return err
			}
			if err := validateText(iter.Value()); err != nil {
				return err
			}
		}
	}
	return nil
}
