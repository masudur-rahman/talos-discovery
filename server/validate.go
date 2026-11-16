package server

import (
	"fmt"
	"time"

	"github.com/masudur-rahman/talos-discovery/models"
)

func validateClusterID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: cluster id is empty", models.ErrInvalid)
	case len(id) > models.ClusterIDMax:
		return fmt.Errorf("%w: cluster id is longer than %d bytes", models.ErrInvalid, models.ClusterIDMax)
	}
	return nil
}

func validateAffiliateID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: affiliate id is empty", models.ErrInvalid)
	case len(id) > models.AffiliateIDMax:
		return fmt.Errorf("%w: affiliate id is longer than %d bytes", models.ErrInvalid, models.AffiliateIDMax)
	}
	return nil
}

func validateAffiliateData(data []byte) error {
	if len(data) > models.AffiliateDataMax {
		return fmt.Errorf("%w: affiliate data is larger than %d bytes", models.ErrInvalid, models.AffiliateDataMax)
	}
	return nil
}

// validateEndpoints checks each endpoint on its own. How many an affiliate may
// hold in total is a state limit, since it depends on what is already stored.
func validateEndpoints(endpoints [][]byte) error {
	if len(endpoints) > models.AffiliateEndpointsMax {
		return fmt.Errorf("%w: more than %d endpoints in one update", models.ErrInvalid, models.AffiliateEndpointsMax)
	}
	for _, ep := range endpoints {
		if len(ep) > models.AffiliateEndpointMax {
			return fmt.Errorf("%w: endpoint is larger than %d bytes", models.ErrInvalid, models.AffiliateEndpointMax)
		}
	}
	return nil
}

// validateTTL only caps the TTL. A zero or missing one is accepted, as the
// public service accepts it: the entry is simply expired on arrival and never
// listed, so there is nothing to protect against.
func validateTTL(ttl time.Duration) error {
	if ttl > models.TTLMax {
		return fmt.Errorf("%w: ttl is longer than %s", models.ErrInvalid, models.TTLMax)
	}
	return nil
}
