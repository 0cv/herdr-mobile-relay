package panesize

import (
	"testing"
	"time"
)

func TestLeasesNarrowestIgnoresExpiredLeases(t *testing.T) {
	now := time.Unix(1000, 0)
	leases := Leases{
		"wide":    {Columns: 120, Rows: 0, ExpiresAt: now.Add(time.Minute)},
		"tall":    {Columns: 100, Rows: 50, ExpiresAt: now.Add(time.Minute)},
		"short":   {Columns: 90, Rows: 30, ExpiresAt: now.Add(time.Minute)},
		"expired": {Columns: 40, Rows: 10, ExpiresAt: now},
	}
	columns, rows := leases.ActiveAt(now).Narrowest()
	if columns != 90 || rows != 30 {
		t.Fatalf("narrowest = %dx%d, want 90x30", columns, rows)
	}
	columns, rows = Leases{"wide": leases["wide"]}.Narrowest()
	if columns != 120 || rows != 0 {
		t.Fatalf("width-only narrowest = %dx%d, want 120x0", columns, rows)
	}
}

func TestLeasesRemoveExpiredReportsLatestExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	leases := Leases{
		"old":    {Columns: 80, ExpiresAt: now.Add(-time.Minute)},
		"recent": {Columns: 80, ExpiresAt: now.Add(-time.Second)},
		"live":   {Columns: 80, ExpiresAt: now.Add(time.Second)},
	}
	lastExpiry, removed := leases.RemoveExpired(now)
	if !removed || !lastExpiry.Equal(now.Add(-time.Second)) || len(leases) != 1 {
		t.Fatalf("RemoveExpired = %v %v, leases %v", lastExpiry, removed, leases)
	}
	if _, removed := leases.RemoveExpired(now); removed {
		t.Fatal("second RemoveExpired removed a live lease")
	}
}

func TestLeasesShortenOnlyBringsExpiryForward(t *testing.T) {
	now := time.Unix(1000, 0)
	leases := Leases{
		"long":  {Columns: 80, ExpiresAt: now.Add(time.Minute)},
		"brief": {Columns: 80, ExpiresAt: now.Add(time.Second)},
	}
	if got := leases.Shorten("long", now.Add(10*time.Second)); !got.ExpiresAt.Equal(now.Add(10*time.Second)) || leases["long"] != got {
		t.Fatalf("long lease = %v", got)
	}
	if got := leases.Shorten("brief", now.Add(10*time.Second)); !got.ExpiresAt.Equal(now.Add(time.Second)) {
		t.Fatalf("brief lease = %v", got)
	}
	if got := leases.Shorten("missing", now); got != (Lease{}) || len(leases) != 2 {
		t.Fatalf("missing lease = %v, leases %v", got, leases)
	}
}

func TestValidateLeaseRejectsBadArguments(t *testing.T) {
	cases := []struct {
		client, pane  string
		columns, rows int
		want          error
	}{
		{"", "pane", 80, 0, ErrInvalidLease},
		{"client", "", 80, 0, ErrInvalidLease},
		{"client", "pane", MinColumns - 1, 0, ErrInvalidColumns},
		{"client", "pane", MaxColumns + 1, 0, ErrInvalidColumns},
		{"client", "pane", 80, MinRows - 1, ErrInvalidRows},
		{"client", "pane", 80, MaxRows + 1, ErrInvalidRows},
		{"client", "pane", 80, 0, nil},
		{"client", "pane", MaxColumns, MaxRows, nil},
	}
	for _, tc := range cases {
		if got := ValidateLease(tc.client, tc.pane, tc.columns, tc.rows); got != tc.want {
			t.Errorf("ValidateLease(%q, %q, %d, %d) = %v, want %v", tc.client, tc.pane, tc.columns, tc.rows, got, tc.want)
		}
	}
}
