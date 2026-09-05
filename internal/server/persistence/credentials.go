package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"qqtang/internal/accountauth"
)

func (store *PlayerStore) EnsureDefaultPassword(ctx context.Context, uin uint32) error {
	configured, err := store.HasPassword(ctx, uin)
	if err != nil {
		return err
	}
	if configured {
		return nil
	}
	verifier, err := accountauth.NewVerifier(accountauth.DefaultPassword)
	if err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// A GM reset may have completed after HasPassword. Bootstrap must never
	// replace that explicit password with the default.
	if err := insertCredential(ctx, tx, uin, verifier, false); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *PlayerStore) HasPassword(ctx context.Context, uin uint32) (bool, error) {
	if store == nil || store.db == nil || uin == 0 {
		return false, fmt.Errorf("password lookup requires a player store and non-zero UIN")
	}
	var present int
	err := store.db.QueryRowContext(ctx, `SELECT 1 FROM player_credentials WHERE uin = ?`, uin).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read password state for UIN %d: %w", uin, err)
	}
	return true, nil
}

func (store *PlayerStore) SetPassword(ctx context.Context, uin uint32, password string) error {
	if store == nil || store.db == nil || uin == 0 {
		return fmt.Errorf("password update requires a player store and non-zero UIN")
	}
	verifier, err := accountauth.NewVerifier(password)
	if err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin password update for UIN %d: %w", uin, err)
	}
	defer tx.Rollback()
	if err := insertCredential(ctx, tx, uin, verifier, true); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit password update for UIN %d: %w", uin, err)
	}
	return nil
}

// PasswordParameters exposes only the public PBKDF parameters required by the
// login helper. The stored verifier never leaves this package.
func (store *PlayerStore) PasswordParameters(ctx context.Context, uin uint32) (uint32, []byte, error) {
	verifier, err := store.loadPasswordVerifier(ctx, uin)
	if err != nil {
		return 0, nil, err
	}
	return verifier.Iterations, append([]byte(nil), verifier.Salt...), nil
}

func (store *PlayerStore) VerifyPasswordProof(ctx context.Context, uin uint32, nonce, proof []byte) (bool, error) {
	verifier, err := store.loadPasswordVerifier(ctx, uin)
	if err != nil {
		return false, err
	}
	return accountauth.VerifyProof(verifier, nonce, uin, proof), nil
}

func (store *PlayerStore) loadPasswordVerifier(ctx context.Context, uin uint32) (accountauth.Verifier, error) {
	if store == nil || store.db == nil || uin == 0 {
		return accountauth.Verifier{}, fmt.Errorf("password lookup requires a player store and non-zero UIN")
	}
	var verifier accountauth.Verifier
	if err := store.db.QueryRowContext(ctx, `SELECT iterations, salt, password_verifier FROM player_credentials WHERE uin = ?`, uin).
		Scan(&verifier.Iterations, &verifier.Salt, &verifier.StoredKey); err != nil {
		return accountauth.Verifier{}, fmt.Errorf("load password verifier for UIN %d: %w", uin, err)
	}
	if err := verifier.Validate(); err != nil {
		return accountauth.Verifier{}, fmt.Errorf("validate password verifier for UIN %d: %w", uin, err)
	}
	return verifier, nil
}

func insertCredential(ctx context.Context, tx *sql.Tx, uin uint32, verifier accountauth.Verifier, replace bool) error {
	if err := verifier.Validate(); err != nil {
		return err
	}
	conflict := "DO NOTHING"
	if replace {
		conflict = `DO UPDATE SET iterations = excluded.iterations, salt = excluded.salt,
			password_verifier = excluded.password_verifier, updated_utc = excluded.updated_utc`
	}
	statement := `INSERT INTO player_credentials(uin, iterations, salt, password_verifier, updated_utc)
		VALUES(?, ?, ?, ?, ?) ON CONFLICT(uin) ` + conflict
	if _, err := tx.ExecContext(ctx, statement, uin, verifier.Iterations, verifier.Salt, verifier.StoredKey, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("save password verifier for UIN %d: %w", uin, err)
	}
	return nil
}
