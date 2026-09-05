package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"math"
)

// availablePlayerIDTx treats the seed's ID as a preference. Account UINs are
// uint32, whereas the native room/member identity is a nonzero uint16, so a
// modulo conversion alone cannot allocate distinct persisted identities.
func availablePlayerIDTx(ctx context.Context, tx *sql.Tx, uin uint32, preferred uint16) (uint16, error) {
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(json_extract(profile_json, '$.player_id'), 0) FROM local_players WHERE uin <> ?`, uin)
	if err != nil {
		return 0, err
	}
	used := make(map[uint16]struct{})
	for rows.Next() {
		var id uint16
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		used[id] = struct{}{}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if _, taken := used[preferred]; preferred != 0 && !taken {
		return preferred, nil
	}
	for id := uint32(1); id <= math.MaxUint16; id++ {
		if _, taken := used[uint16(id)]; !taken {
			return uint16(id), nil
		}
	}
	return 0, fmt.Errorf("native uint16 player ID space is exhausted")
}

// ensureUniquePlayerID repairs an old modulo collision before the account is
// projected into a live session. Change only the identity field in the latest
// persisted record; this is not an inventory/profile snapshot replacement.
func (store *PlayerStore) ensureUniquePlayerID(ctx context.Context, uin uint32) (uint16, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	profile, err := readProfileRecordTx(ctx, tx, uin)
	if err != nil {
		return 0, err
	}
	id, err := availablePlayerIDTx(ctx, tx, uin, profile.PlayerID)
	if err != nil {
		return 0, err
	}
	if id == profile.PlayerID {
		return id, nil
	}
	profile.PlayerID = id
	if err := writeProfileRecordTx(ctx, tx, uin, profile); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}
