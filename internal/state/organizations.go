package state

import (
	"encoding/json"
	"fmt"
)

func (s *Store) migrateOrganizationAccountKeys() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT account_id, id_token FROM accounts`)
	if err != nil {
		return err
	}
	keys := map[string]string{}
	for rows.Next() {
		var id, token string
		if err := rows.Scan(&id, &token); err != nil {
			rows.Close()
			return err
		}
		var identity struct {
			AccountID      string `json:"account_id"`
			OrganizationID string `json:"organization_id"`
		}
		if json.Unmarshal([]byte(token), &identity) == nil && identity.AccountID == id && identity.OrganizationID != "" {
			keys[id] = id + ":" + identity.OrganizationID
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.Exec("PRAGMA defer_foreign_keys = ON"); err != nil {
		return err
	}
	for previous, next := range keys {
		for _, table := range []string{"accounts", "routes", "response_usage"} {
			if _, err := tx.Exec(`UPDATE `+table+` SET account_id = ? WHERE account_id = ?`, next, previous); err != nil {
				return fmt.Errorf("migrate %s organization account keys: %w", table, err)
			}
		}
	}
	if _, err := tx.Exec("PRAGMA user_version = 10"); err != nil {
		return err
	}
	return tx.Commit()
}
