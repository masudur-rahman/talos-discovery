package state

import (
	"bytes"
	"slices"
	"time"

	"github.com/masudur-rahman/talos-discovery/models"
)

// Affiliate is a copy of one cluster member as the store holds it. Data and
// endpoints are ciphertext: the cluster encrypts them with its own key, so the
// server can store and relay them but never read them.
type Affiliate struct {
	ID        string
	Data      []byte
	Endpoints [][]byte
}

// endpoint expires on its own clock: a node keeps re-announcing the addresses
// it still has, and an address it stopped announcing ages out.
type endpoint struct {
	data      []byte
	expiresAt time.Time
}

type affiliate struct {
	id        string
	data      []byte
	expiresAt time.Time
	endpoints []endpoint
}

// update applies one AffiliateUpdate. Data is replaced when non-nil, endpoints
// are merged, and every lifetime touched is only ever extended. It reports
// whether watchers have anything new to see.
//
// The endpoint limit is checked before anything is written, so a rejected
// update leaves the affiliate exactly as it was.
func (a *affiliate) update(data []byte, endpoints [][]byte, expiresAt time.Time) (bool, error) {
	merged := slices.Clone(a.endpoints)
	changed := false

	for _, ep := range endpoints {
		i := slices.IndexFunc(merged, func(e endpoint) bool { return bytes.Equal(e.data, ep) })
		if i >= 0 {
			merged[i].expiresAt = later(merged[i].expiresAt, expiresAt)
			continue
		}
		merged = append(merged, endpoint{data: bytes.Clone(ep), expiresAt: expiresAt})
		changed = true
	}
	if len(merged) > models.AffiliateEndpointsMax {
		return false, models.ErrTooManyEndpoints
	}

	if data != nil && !bytes.Equal(a.data, data) {
		a.data = bytes.Clone(data)
		changed = true
	}
	a.endpoints = merged
	a.expiresAt = later(a.expiresAt, expiresAt)
	return changed, nil
}

// gc drops endpoints that aged out. remove is true when the affiliate itself
// expired; changed is true when it survives but lost endpoints.
func (a *affiliate) gc(now time.Time) (remove, changed bool) {
	if !a.expiresAt.After(now) {
		return true, false
	}

	kept := a.endpoints[:0]
	for _, ep := range a.endpoints {
		if ep.expiresAt.After(now) {
			kept = append(kept, ep)
		}
	}
	changed = len(kept) != len(a.endpoints)
	clear(a.endpoints[len(kept):])
	a.endpoints = kept
	return false, changed
}

func (a *affiliate) expired(now time.Time) bool {
	return !a.expiresAt.After(now)
}

// export copies the affiliate out, leaving out endpoints that expired since the
// last collection so a read never returns what GC is about to drop.
func (a *affiliate) export(now time.Time) Affiliate {
	out := Affiliate{
		ID:        a.id,
		Data:      bytes.Clone(a.data),
		Endpoints: make([][]byte, 0, len(a.endpoints)),
	}
	for _, ep := range a.endpoints {
		if ep.expiresAt.After(now) {
			out.Endpoints = append(out.Endpoints, bytes.Clone(ep.data))
		}
	}
	return out
}

func (a *affiliate) endpointCount() int {
	return len(a.endpoints)
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
