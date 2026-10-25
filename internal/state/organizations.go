package state

import (
	"encoding/json"
	"fmt"
)

// Writers that opened the database before schema 10 still key accounts by the
// bare account UUID; rejecting those inserts aborts their mutation before it
// can delete the organization-scoped rows.
const organizationKeySchema = `CREATE TRIGGER IF NOT EXISTS accounts_require_organization_key BEFORE INSERT ON accounts
WHEN CASE WHEN json_valid(NEW.id_token) THEN json_extract(NEW.id_token, '$.organization_id') != ''
	AND NEW.account_id = json_extract(NEW.id_token, '$.account_id') END
BEGIN
	SELECT RAISE(ABORT, 'account key must include its organization');
END;`

func AccountKey(account, organization string) string {
	if account == "" || organization == "" {
		return account
	}
	return account + ":" + organization
}

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
			keys[id] = AccountKey(id, identity.OrganizationID)
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
	if _, err := tx.Exec(organizationKeySchema); err != nil {
		return err
	}
	if _, err := tx.Exec("PRAGMA user_version = 10"); err != nil {
		return err
	}
	return tx.Commit()
}
