package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// A snapshot is every live affiliate of every cluster, written as protobuf in
// the layout of sidero.discovery.storage.StateSnapshot (api/storage in the
// upstream service). Using that layout means a snapshot taken by an upstream
// server can be loaded here and the other way round, which is what lets a
// fleet move between the two without every node starting from nothing.
//
// Lifetimes are stored as remaining TTL, not as an absolute time, so a server
// that was down for a while restores what it had instead of finding it all
// expired: the nodes would re-announce within one TTL anyway, and in the
// meantime they keep seeing each other. The absolute expiration is written
// too, because older readers only know that field.

// SnapshotStats describes one written or loaded snapshot.
type SnapshotStats struct {
	Clusters   int
	Affiliates int
	Endpoints  int
	Bytes      int
}

type clusterSnapshot struct {
	ID         string              `json:"id"`
	Affiliates []affiliateSnapshot `json:"affiliates,omitempty"`
}

type affiliateSnapshot struct {
	ID        string             `json:"id"`
	TTL       jsonDuration       `json:"ttl"`
	Data      []byte             `json:"data,omitempty"`
	Endpoints []endpointSnapshot `json:"endpoints,omitempty"`

	// expiration is only read, from snapshots that carry no TTL.
	expiration time.Time
}

type endpointSnapshot struct {
	TTL  jsonDuration `json:"ttl"`
	Data []byte       `json:"data"`

	expiration time.Time
}

// jsonDuration prints as protojson prints a Duration ("12.5s"), so a decoded
// snapshot reads the same as one decoded by the upstream tool.
type jsonDuration time.Duration

func (d jsonDuration) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatFloat(time.Duration(d).Seconds(), 'f', -1, 64) + "s")
}

// WriteSnapshot encodes every live affiliate to w. Watchers are not part of a
// snapshot: they are connections, and they reconnect on their own.
func (s *State) WriteSnapshot(w io.Writer) (SnapshotStats, error) {
	now := s.clock()
	var (
		clusters []clusterSnapshot
		st       SnapshotStats
	)

	for _, c := range s.snapshotClusters() {
		c.mu.Lock()
		cs := clusterSnapshot{ID: c.id}
		for _, a := range c.affiliates {
			if a.expired(now) {
				continue
			}
			as := affiliateSnapshot{ID: a.id, Data: bytes.Clone(a.data), TTL: jsonDuration(a.expiresAt.Sub(now))}
			for _, ep := range a.endpoints {
				if ep.expiresAt.After(now) {
					as.Endpoints = append(as.Endpoints, endpointSnapshot{Data: bytes.Clone(ep.data), TTL: jsonDuration(ep.expiresAt.Sub(now))})
				}
			}
			st.Endpoints += len(as.Endpoints)
			cs.Affiliates = append(cs.Affiliates, as)
		}
		c.mu.Unlock()

		if len(cs.Affiliates) > 0 {
			clusters = append(clusters, cs)
			st.Clusters++
			st.Affiliates += len(cs.Affiliates)
		}
	}

	n, err := w.Write(encodeSnapshot(clusters, now))
	st.Bytes = n
	if err != nil {
		return st, fmt.Errorf("write snapshot: %w", err)
	}
	return st, nil
}

// ReadSnapshot loads a snapshot into the store. It is meant for boot, before
// any client connects.
func (s *State) ReadSnapshot(r io.Reader) (SnapshotStats, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return SnapshotStats{}, fmt.Errorf("read snapshot: %w", err)
	}
	clusters, err := decodeSnapshot(raw)
	if err != nil {
		return SnapshotStats{}, err
	}

	now := s.clock()
	st := SnapshotStats{Bytes: len(raw)}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cs := range clusters {
		c := newCluster(cs.ID)
		for _, as := range cs.Affiliates {
			expiresAt := expiry(now, as.TTL, as.expiration)
			if !expiresAt.After(now) {
				continue
			}
			a := &affiliate{id: as.ID, data: as.Data, expiresAt: expiresAt}
			for _, es := range as.Endpoints {
				if epExpiresAt := expiry(now, es.TTL, es.expiration); epExpiresAt.After(now) {
					a.endpoints = append(a.endpoints, endpoint{data: es.Data, expiresAt: epExpiresAt})
				}
			}
			c.affiliates[a.id] = a
			st.Endpoints += len(a.endpoints)
		}
		if len(c.affiliates) > 0 {
			s.clusters[c.id] = c
			st.Clusters++
			st.Affiliates += len(c.affiliates)
		}
	}
	return st, nil
}

// expiry prefers the TTL; only a snapshot from a writer that predates it has
// nothing but the absolute expiration.
func expiry(now time.Time, ttl jsonDuration, expiration time.Time) time.Time {
	if ttl != 0 {
		return now.Add(time.Duration(ttl))
	}
	return expiration
}

// SaveSnapshot writes the snapshot to path atomically: a crash mid-write leaves
// the previous snapshot in place, never a truncated one.
func (s *State) SaveSnapshot(path string) (SnapshotStats, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return SnapshotStats{}, fmt.Errorf("create snapshot dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return SnapshotStats{}, fmt.Errorf("create snapshot temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	st, err := s.WriteSnapshot(tmp)
	if err != nil {
		_ = tmp.Close()
		return st, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return st, fmt.Errorf("sync snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return st, fmt.Errorf("close snapshot: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return st, fmt.Errorf("replace snapshot: %w", err)
	}
	return st, nil
}

// LoadSnapshot reads the snapshot at path. A missing file is the first boot,
// not an error.
func (s *State) LoadSnapshot(path string) (SnapshotStats, error) {
	f, err := os.Open(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		return SnapshotStats{}, nil
	}
	if err != nil {
		return SnapshotStats{}, fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()
	return s.ReadSnapshot(f)
}

// DecodeSnapshotJSON turns a snapshot file into indented JSON, for looking at
// what a server holds without starting it. Data and endpoints stay ciphertext,
// base64 encoded, as protojson would print them.
func DecodeSnapshotJSON(raw []byte) ([]byte, error) {
	clusters, err := decodeSnapshot(raw)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for i := range clusters {
		for j := range clusters[i].Affiliates {
			a := &clusters[i].Affiliates[j]
			a.TTL = jsonDuration(expiry(now, a.TTL, a.expiration).Sub(now))
			for k := range a.Endpoints {
				e := &a.Endpoints[k]
				e.TTL = jsonDuration(expiry(now, e.TTL, e.expiration).Sub(now))
			}
		}
	}
	return json.MarshalIndent(struct {
		Clusters []clusterSnapshot `json:"clusters"`
	}{clusters}, "", "  ")
}
