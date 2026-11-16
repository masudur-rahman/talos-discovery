// Package models holds the limits and errors shared by the state store and the
// gRPC server.
package models

import "time"

// Request limits. Talos stays well inside every one of them; they exist so a
// misbehaving or hostile client cannot grow the in-memory state without bound.
const (
	// ClusterIDMax is the longest cluster id accepted, in bytes.
	ClusterIDMax = 256
	// AffiliateIDMax is the longest affiliate id accepted, in bytes.
	AffiliateIDMax = 256
	// AffiliateDataMax is the largest encrypted affiliate payload accepted.
	AffiliateDataMax = 2048
	// AffiliateEndpointMax is the largest single encrypted endpoint accepted.
	AffiliateEndpointMax = 32
	// TTLMax is the longest lifetime a client may ask for.
	TTLMax = 30 * time.Minute
)

// State limits.
const (
	// ClusterAffiliatesMax caps the affiliates (nodes) of one cluster.
	ClusterAffiliatesMax = 1024
	// AffiliateEndpointsMax caps the endpoints one affiliate may hold.
	AffiliateEndpointsMax = 64
)
