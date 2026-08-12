package alloc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrBadJoinToken means the token is unknown, spent, or past its life.
//
// ONE ERROR FOR ALL THREE, deliberately. Telling a caller which of them applies
// tells somebody guessing tokens whether they guessed a real one, which is the
// only feedback that makes guessing worth doing.
var ErrBadJoinToken = errors.New("alloc: that join token is not usable")

// JoinToken is a short-lived credential that lets a machine ASK to enroll.
//
// It admits nothing on its own: a request still waits for an operator to compare
// fingerprints. What it buys is that a stranger who can reach the port cannot
// fill the pending list, or take a name before the machine that should have it.
type JoinToken struct {
	Note      string
	Uses      int
	CreatedAt string
	ExpiresAt string
}

// NewJoinToken mints one and returns the secret, which is shown exactly once.
//
// BASE32 WITHOUT PADDING, because this is read off one terminal and typed into
// another: no case sensitivity to lose, no `+` or `/` to mangle in a shell, and
// no `=` for somebody to trim.
func (a *Allocator) NewJoinToken(ctx context.Context, ttl time.Duration, uses int, note string) (string, error) {
	if ttl <= 0 {
		return "", errors.New("alloc: a join token needs a positive lifetime")
	}

	if uses <= 0 {
		return "", errors.New("alloc: a join token needs at least one use")
	}

	var raw [20]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("alloc: generate a join token: %w", err)
	}

	token := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:]))
	now := a.now().UTC()

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO join_tokens (token_sha256, note, uses_remaining, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?)`,
			hashJoinToken(token), note, uses, ts(now), ts(now.Add(ttl)))
		if err != nil {
			return fmt.Errorf("alloc: record a join token: %w", err)
		}

		return nil
	})

	return token, err
}

// SpendJoinToken checks a token and consumes one use.
//
// CHECK AND DECREMENT IN ONE STATEMENT, so two machines racing on a single-use
// token cannot both be admitted: the UPDATE matches only while a use remains, and
// whichever commits second changes no rows.
func (a *Allocator) SpendJoinToken(ctx context.Context, token string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		return a.spendJoinTokenTx(ctx, tx, token)
	})
}

// spendJoinTokenTx is the check-and-decrement, inside a caller's transaction so
// it can be made atomic with the request it authorises.
func (a *Allocator) spendJoinTokenTx(ctx context.Context, tx *sql.Tx, token string) error {
	if strings.TrimSpace(token) == "" {
		return ErrBadJoinToken
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE join_tokens SET uses_remaining = uses_remaining - 1
		  WHERE token_sha256 = ? AND uses_remaining > 0 AND expires_at > ?`,
		hashJoinToken(token), ts(a.now().UTC()))
	if err != nil {
		return fmt.Errorf("alloc: spend a join token: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("alloc: spend a join token: %w", err)
	}

	if n == 0 {
		return ErrBadJoinToken
	}

	return nil
}

// JoinTokens lists what is outstanding, without the secrets.
func (a *Allocator) JoinTokens(ctx context.Context) ([]JoinToken, error) {
	var out []JoinToken

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		out = nil

		rows, err := tx.QueryContext(ctx,
			`SELECT note, uses_remaining, created_at, expires_at FROM join_tokens
			  ORDER BY created_at DESC`)
		if err != nil {
			return fmt.Errorf("alloc: list join tokens: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var t JoinToken
			if err := rows.Scan(&t.Note, &t.Uses, &t.CreatedAt, &t.ExpiresAt); err != nil {
				return fmt.Errorf("alloc: scan a join token: %w", err)
			}

			out = append(out, t)
		}

		return rows.Err()
	})

	return out, err
}

// storedJoinTokenKeys is what the table actually holds, for the test that says
// the secret is not among it.
func (a *Allocator) storedJoinTokenKeys(ctx context.Context) ([]string, error) {
	var out []string

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		out = nil

		rows, err := tx.QueryContext(ctx, `SELECT token_sha256 FROM join_tokens`)
		if err != nil {
			return fmt.Errorf("alloc: read join tokens: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				return fmt.Errorf("alloc: scan a join token key: %w", err)
			}

			out = append(out, k)
		}

		return rows.Err()
	})

	return out, err
}

// hashJoinToken is what the ledger stores instead of the token.
func hashJoinToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(strings.ToLower(token))))

	return hex.EncodeToString(sum[:])
}
