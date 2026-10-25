package state

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMigrateOrganizationAccountKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf("PRAGMA application_id = %d; PRAGMA user_version = 9;", ApplicationID) + currentSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO accounts
		(account_id, id_token, access_token, refresh_token, paused, routing_mode, last_refresh_ns, last_used_at_ns, reauth) VALUES
		('a', '{"provider":"claude","account_id":"a","organization_id":"org"}', 'access-a', 'refresh-a', 1, 'priority', 11, 12, ''),
		('b', '{"provider":"claude","account_id":"b","organization_id":""}', 'access-b', 'refresh-b', 0, 'normal', 21, 22, ''),
		('c', 'x.jwt.x', 'access-c', 'refresh-c', 0, 'normal', 31, 32, '');
		INSERT INTO routes VALUES ('thread-a', 'a', 41), ('thread-b', 'b', 42), ('removed-owner', 'removed', 43);
		INSERT INTO response_usage VALUES (1, NULL, 51, 'model', 'default', 1, 0, 0, 1, 0, 'a'), (2, NULL, 52, 'model', 'default', 1, 0, 0, 1, 0, 'b');`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	accounts, err := store.ReadAccounts()
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	if want := []string{"a:org", "b", "c"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("account ids = %v, want %v", ids, want)
	}
	if !accounts[0].Paused || accounts[0].RoutingMode != "priority" || accounts[0].AccessToken != "access-a" {
		t.Fatalf("rekeyed account state changed: %+v", accounts[0])
	}
	routes := map[string]string{}
	rows, err := store.db.Query(`SELECT key, account_id FROM routes`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key, account string
		if err := rows.Scan(&key, &account); err != nil {
			t.Fatal(err)
		}
		routes[key] = account
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"thread-a": "a:org", "thread-b": "b", "removed-owner": "removed"}; !reflect.DeepEqual(routes, want) {
		t.Fatalf("routes = %v, want %v", routes, want)
	}
	var usageA, usageB string
	if err := store.db.QueryRow(`SELECT (SELECT account_id FROM response_usage WHERE id = 1), (SELECT account_id FROM response_usage WHERE id = 2)`).Scan(&usageA, &usageB); err != nil || usageA != "a:org" || usageB != "b" {
		t.Fatalf("usage attribution = %q, %q, error = %v", usageA, usageB, err)
	}
	var violations, version int
	if err := store.db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
		t.Fatalf("foreign-key violations = %d, error = %v", violations, err)
	}
	if err := store.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("version = %d, error = %v", version, err)
	}
}
