package app

import (
	"testing"
	"time"
)

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
