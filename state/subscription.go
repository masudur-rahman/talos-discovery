package state

import (
	"sync"

	"github.com/masudur-rahman/talos-discovery/models"
)

// subscriptionBuffer is how many notifications a watcher may fall behind by.
// A whole cluster booting at once announces every node within seconds, so this
// is sized for that burst rather than for the steady trickle of TTL refreshes.
const subscriptionBuffer = 256

// Notification is one change to a cluster. When Deleted is set, only
// Affiliate.ID is meaningful.
type Notification struct {
	Affiliate Affiliate
	Deleted   bool
}

// Subscription delivers the changes of one cluster to one watcher.
//
// A watcher that stops draining is not allowed to stall the cluster: once its
// buffer is full it is dropped and Err fires. The client reconnects and starts
// again from a fresh snapshot, which is cheaper than buffering without bound.
type Subscription struct {
	ch      chan Notification
	errCh   chan error
	cluster *cluster
	once    sync.Once
}

func newSubscription(c *cluster) *Subscription {
	return &Subscription{
		ch:      make(chan Notification, subscriptionBuffer),
		errCh:   make(chan error, 1),
		cluster: c,
	}
}

// Notifications yields the changes after the snapshot Watch returned.
func (s *Subscription) Notifications() <-chan Notification {
	return s.ch
}

// Err fires once if the subscription is dropped for falling behind.
func (s *Subscription) Err() <-chan error {
	return s.errCh
}

// Close detaches the subscription from its cluster. It is safe to call more
// than once and after an overflow.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.cluster.mu.Lock()
		delete(s.cluster.subs, s)
		s.cluster.mu.Unlock()
	})
}

// notify is called with the cluster lock held.
func (s *Subscription) notify(n Notification) {
	select {
	case s.ch <- n:
	default:
		delete(s.cluster.subs, s)
		select {
		case s.errCh <- models.ErrSubscriptionOverflow:
		default:
		}
	}
}
