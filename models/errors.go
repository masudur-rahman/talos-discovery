package models

import "errors"

var (
	// ErrInvalid is returned for a request the server rejects with InvalidArgument.
	ErrInvalid = errors.New("invalid request")
	// ErrTooManyAffiliates is returned when a cluster is full.
	ErrTooManyAffiliates = errors.New("too many affiliates in the cluster")
	// ErrTooManyEndpoints is returned when an affiliate's endpoint list is full.
	ErrTooManyEndpoints = errors.New("too many endpoints for the affiliate")
	// ErrSubscriptionOverflow is returned to a watcher that fell so far behind
	// its buffer filled. The client reconnects and gets a fresh snapshot.
	ErrSubscriptionOverflow = errors.New("watch subscription overflowed")
)
