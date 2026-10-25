package app

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPoolKeepsOneAccountPerClaudeOrganization(t *testing.T) {
	store, err := openStateStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pool, err := loadPool(store)
	if err != nil {
		t.Fatal(err)
	}
	login := func(organization, name, access string) *Account {
		return accountFromClaudeCredentials(&claudeCredentials{
			AccessToken:      access,
			RefreshToken:     "refresh-" + access,
			AccountID:        "account",
			Email:            "user@example.com",
			OrganizationID:   organization,
			OrganizationName: name,
			LastRefresh:      time.Now(),
		})
	}
	for _, account := range []*Account{login("personal", "Personal", "first"), login("team", "Team", "second"), login("team", "Team", "third")} {
		if err := pool.add(account); err != nil {
			t.Fatal(err)
		}
	}
	if pool.count() != 2 {
		t.Fatalf("accounts = %d, want one per organization", pool.count())
	}
	if got := pool.find("account:personal").persisted().AccessToken; got != "first" {
		t.Fatalf("personal access token = %q, want first", got)
	}
	if got := pool.find("account:team").persisted().AccessToken; got != "third" {
		t.Fatalf("team access token = %q, want the latest login", got)
	}
	labels := accountLabels(pool.all(), label)
	if labels["account:personal"] != "user@example.com (Personal)" || labels["account:team"] != "user@example.com (Team)" {
		t.Fatalf("labels = %v", labels)
	}
	short := (dashboard{pool: pool}).shortNames()
	if short["account:personal"] != "user (Personal)" || short["account:team"] != "user (Team)" {
		t.Fatalf("short labels = %v", short)
	}
	if _, err := pool.resolve("user@example.com"); err == nil {
		t.Fatal("shared email resolved to a single organization")
	}
}

func TestAccountApplyPersistedComparesRefreshInstants(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name        string
		lastRefresh time.Time
		wantChanged bool
	}{
		{name: "identical", lastRefresh: now},
		{name: "database round trip", lastRefresh: time.Unix(0, now.UnixNano())},
		{name: "different time zone", lastRefresh: now.In(time.FixedZone("offset", 3600))},
		{name: "later refresh", lastRefresh: now.Add(time.Nanosecond), wantChanged: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := accountFromState(accountState{LastRefresh: now})
			next := account.persisted()
			next.LastRefresh = test.lastRefresh
			if got := account.applyPersisted(next); got != test.wantChanged {
				t.Fatalf("changed = %v, want %v", got, test.wantChanged)
			}
			if got := account.persisted().LastRefresh; !got.Equal(test.lastRefresh) {
				t.Fatalf("last refresh = %v, want %v", got, test.lastRefresh)
			}
		})
	}
}
