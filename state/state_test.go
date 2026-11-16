package state

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/masudur-rahman/talos-discovery/models"
)

// fakeClock is moved by hand so TTLs can be crossed without sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestState() (*State, *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	return New(WithClock(clock.Now)), clock
}

func TestUpdateCreatesAndMerges(t *testing.T) {
	s, _ := newTestState()

	if err := s.Update("c1", "a1", []byte("data"), [][]byte{[]byte("e1")}, time.Minute); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// No data: the stored data stays, the endpoint list grows.
	if err := s.Update("c1", "a1", nil, [][]byte{[]byte("e2"), []byte("e1")}, time.Minute); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got := s.List("c1")
	if len(got) != 1 {
		t.Fatalf("affiliates = %d, want 1", len(got))
	}
	if string(got[0].Data) != "data" {
		t.Errorf("data = %q, want kept", got[0].Data)
	}
	if len(got[0].Endpoints) != 2 {
		t.Errorf("endpoints = %q, want e1 and e2 once each", got[0].Endpoints)
	}
}

func TestListUnknownClusterDoesNotCreateIt(t *testing.T) {
	s, _ := newTestState()
	if got := s.List("nope"); len(got) != 0 {
		t.Errorf("List = %v, want empty", got)
	}
	s.Delete("nope", "a1")
	if st := s.Stats(); st.Clusters != 0 {
		t.Errorf("clusters = %d, want 0", st.Clusters)
	}
}

func TestEndpointsExpireIndependently(t *testing.T) {
	s, clock := newTestState()

	_ = s.Update("c1", "a1", []byte("d"), [][]byte{[]byte("old")}, time.Minute)
	clock.Advance(30 * time.Second)
	_ = s.Update("c1", "a1", nil, [][]byte{[]byte("new")}, time.Minute)
	clock.Advance(45 * time.Second)

	// "old" passed its TTL; reads hide it before GC runs.
	got := s.List("c1")
	if len(got) != 1 || len(got[0].Endpoints) != 1 || string(got[0].Endpoints[0]) != "new" {
		t.Fatalf("List = %+v, want only the new endpoint", got)
	}

	if res := s.GC(); res.Affiliates != 0 {
		t.Errorf("GC removed %d affiliates, want 0", res.Affiliates)
	}
	if st := s.Stats(); st.Endpoints != 1 {
		t.Errorf("endpoints = %d, want 1 after GC", st.Endpoints)
	}
}

func TestAffiliateExpiryRemovesClusterAndNotifies(t *testing.T) {
	s, clock := newTestState()
	_ = s.Update("c1", "a1", []byte("d"), nil, time.Minute)

	snap, sub := s.Watch("c1")
	if len(snap) != 1 {
		t.Fatalf("snapshot = %d, want 1", len(snap))
	}

	clock.Advance(2 * time.Minute)
	if res := s.GC(); res.Affiliates != 1 || res.Clusters != 0 {
		t.Fatalf("GC = %+v, want one affiliate and the cluster kept for its watcher", res)
	}

	n := <-sub.Notifications()
	if !n.Deleted || n.Affiliate.ID != "a1" {
		t.Errorf("notification = %+v, want deletion of a1", n)
	}

	sub.Close()
	if res := s.GC(); res.Clusters != 1 {
		t.Errorf("GC = %+v, want the idle cluster dropped", res)
	}
}

func TestWatchSeesUpdatesAndDeletes(t *testing.T) {
	s, _ := newTestState()
	_, sub := s.Watch("c1")
	defer sub.Close()

	_ = s.Update("c1", "a1", []byte("d1"), nil, time.Minute)
	// The same data again is not a change; watchers are not woken for it.
	_ = s.Update("c1", "a1", []byte("d1"), nil, time.Minute)
	_ = s.Update("c1", "a1", []byte("d2"), nil, time.Minute)
	s.Delete("c1", "a1")

	want := []string{"a1:d1", "a1:d2", "a1:deleted"}
	for _, w := range want {
		select {
		case n := <-sub.Notifications():
			got := n.Affiliate.ID + ":" + string(n.Affiliate.Data)
			if n.Deleted {
				got = n.Affiliate.ID + ":deleted"
			}
			if got != w {
				t.Errorf("notification = %s, want %s", got, w)
			}
		default:
			t.Fatalf("missing notification %s", w)
		}
	}
	select {
	case n := <-sub.Notifications():
		t.Errorf("unexpected notification %+v", n)
	default:
	}
}

func TestClusterAffiliateLimit(t *testing.T) {
	s, _ := newTestState()
	for i := range models.ClusterAffiliatesMax {
		if err := s.Update("c1", fmt.Sprintf("a%d", i), nil, nil, time.Minute); err != nil {
			t.Fatalf("Update %d: %v", i, err)
		}
	}
	err := s.Update("c1", "one-too-many", nil, nil, time.Minute)
	if !errors.Is(err, models.ErrTooManyAffiliates) {
		t.Errorf("err = %v, want ErrTooManyAffiliates", err)
	}
	// An existing affiliate can still refresh in a full cluster.
	if err := s.Update("c1", "a0", []byte("d"), nil, time.Minute); err != nil {
		t.Errorf("refresh in full cluster: %v", err)
	}
}

func TestEndpointLimitLeavesAffiliateUntouched(t *testing.T) {
	s, _ := newTestState()
	var eps [][]byte
	for i := range models.AffiliateEndpointsMax {
		eps = append(eps, fmt.Appendf(nil, "e%d", i))
	}
	if err := s.Update("c1", "a1", []byte("d1"), eps, time.Minute); err != nil {
		t.Fatalf("Update: %v", err)
	}

	err := s.Update("c1", "a1", []byte("d2"), [][]byte{[]byte("extra")}, time.Minute)
	if !errors.Is(err, models.ErrTooManyEndpoints) {
		t.Fatalf("err = %v, want ErrTooManyEndpoints", err)
	}
	got := s.List("c1")[0]
	if string(got.Data) != "d1" || len(got.Endpoints) != models.AffiliateEndpointsMax {
		t.Errorf("affiliate changed by a rejected update: data=%q endpoints=%d", got.Data, len(got.Endpoints))
	}
}

func TestSlowWatcherIsDropped(t *testing.T) {
	s, _ := newTestState()
	_, sub := s.Watch("c1")
	defer sub.Close()

	for i := range subscriptionBuffer + 1 {
		_ = s.Update("c1", "a1", fmt.Appendf(nil, "d%d", i), nil, time.Minute)
	}

	select {
	case err := <-sub.Err():
		if !errors.Is(err, models.ErrSubscriptionOverflow) {
			t.Errorf("err = %v, want overflow", err)
		}
	default:
		t.Fatal("overflowed subscription did not report an error")
	}
	if st := s.Stats(); st.Subscriptions != 0 {
		t.Errorf("subscriptions = %d, want the slow one detached", st.Subscriptions)
	}
}

func TestReturnedDataIsACopy(t *testing.T) {
	s, _ := newTestState()
	data := []byte("data")
	_ = s.Update("c1", "a1", data, [][]byte{[]byte("e1")}, time.Minute)
	data[0] = 'X'

	got := s.List("c1")[0]
	got.Data[1] = 'Y'
	if again := s.List("c1")[0]; string(again.Data) != "data" {
		t.Errorf("stored data = %q, want it isolated from callers", again.Data)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	s, clock := newTestState()
	_ = s.Update("c1", "a1", []byte("d1"), [][]byte{[]byte("e1")}, 10*time.Minute)
	_ = s.Update("c1", "a2", []byte("d2"), nil, time.Minute)
	_ = s.Update("c2", "b1", []byte("x"), [][]byte{[]byte("e")}, 10*time.Minute)

	path := filepath.Join(t.TempDir(), "state", "state.binpb")
	st, err := s.SaveSnapshot(path)
	if err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if st.Clusters != 2 || st.Affiliates != 3 || st.Endpoints != 2 || st.Bytes == 0 {
		t.Errorf("save stats = %+v", st)
	}

	// Lifetimes are stored as remaining TTL, so downtime does not age them:
	// a2 had a minute left when saved and still has a minute after restore.
	clock.Advance(5 * time.Minute)
	restored := New(WithClock(clock.Now))
	st, err = restored.LoadSnapshot(path)
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if st.Clusters != 2 || st.Affiliates != 3 || st.Endpoints != 2 {
		t.Errorf("load stats = %+v", st)
	}

	got := restored.List("c1")
	if len(got) != 2 || got[0].ID != "a1" || string(got[0].Endpoints[0]) != "e1" || string(got[1].Data) != "d2" {
		t.Errorf("restored c1 = %+v", got)
	}

	clock.Advance(90 * time.Second)
	if got := restored.List("c1"); len(got) != 1 {
		t.Errorf("after a2's TTL, c1 = %+v, want only a1", got)
	}
}

func TestLoadMissingSnapshot(t *testing.T) {
	s, _ := newTestState()
	if _, err := s.LoadSnapshot(filepath.Join(t.TempDir(), "none")); err != nil {
		t.Errorf("LoadSnapshot on missing file: %v", err)
	}
}

func TestReadSnapshotRejectsGarbage(t *testing.T) {
	s, _ := newTestState()
	if _, err := s.ReadSnapshot(bytes.NewReader([]byte("not gob"))); err == nil {
		t.Error("ReadSnapshot accepted garbage")
	}
}

// TestConcurrentTrafficWithGC is for -race: writers, readers, watchers and GC
// on the same clusters at once.
func TestConcurrentTrafficWithGC(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	stop := make(chan struct{})

	for w := range 4 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				cid := fmt.Sprintf("c%d", i%3)
				_ = s.Update(cid, fmt.Sprintf("a%d-%d", w, i%10), []byte("d"), [][]byte{[]byte("e")}, time.Millisecond)
				s.List(cid)
				if i%7 == 0 {
					s.Delete(cid, fmt.Sprintf("a%d-%d", w, i%10))
				}
			}
		})
	}
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, sub := s.Watch("c1")
			sub.Close()
			s.GC()
			s.Stats()
		}
	})

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}
