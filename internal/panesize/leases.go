package panesize

import "time"

type Lease struct {
	Columns int
	// Rows is zero when the client leases only the width: the pane keeps
	// its own height, which is never written with a different value.
	Rows      int
	ExpiresAt time.Time
}

// Leases holds one pane's leases by client.
type Leases map[string]Lease

func ValidateLease(clientID, paneID string, columns, rows int) error {
	if clientID == "" || paneID == "" {
		return ErrInvalidLease
	}
	if columns < MinColumns || columns > MaxColumns {
		return ErrInvalidColumns
	}
	if rows != 0 && (rows < MinRows || rows > MaxRows) {
		return ErrInvalidRows
	}
	return nil
}

// Narrowest returns the smallest leased width and the shortest non-zero leased
// height. Rows is zero when no lease asks for a height.
func (l Leases) Narrowest() (columns, rows int) {
	for _, lease := range l {
		if columns == 0 || lease.Columns < columns {
			columns = lease.Columns
		}
		if lease.Rows > 0 && (rows == 0 || lease.Rows < rows) {
			rows = lease.Rows
		}
	}
	return columns, rows
}

func (l Leases) ActiveAt(now time.Time) Leases {
	active := make(Leases, len(l))
	for clientID, lease := range l {
		if lease.ExpiresAt.After(now) {
			active[clientID] = lease
		}
	}
	return active
}

// RemoveExpired deletes the leases that ended at or before now and reports the
// latest of their expiry times.
func (l Leases) RemoveExpired(now time.Time) (lastExpiry time.Time, removed bool) {
	for clientID, lease := range l {
		if lease.ExpiresAt.After(now) {
			continue
		}
		delete(l, clientID)
		removed = true
		if lease.ExpiresAt.After(lastExpiry) {
			lastExpiry = lease.ExpiresAt
		}
	}
	return lastExpiry, removed
}

// Shorten ends clientID's lease no later than lapse and returns the lease as
// stored. A client without a lease gets the zero Lease.
func (l Leases) Shorten(clientID string, lapse time.Time) Lease {
	lease, ok := l[clientID]
	if !ok {
		return Lease{}
	}
	if lease.ExpiresAt.After(lapse) {
		lease.ExpiresAt = lapse
		l[clientID] = lease
	}
	return lease
}
