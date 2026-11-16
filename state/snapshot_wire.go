package state

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// The encoding is written by hand with protowire rather than generated: it is
// four small messages, and generating them would put protoc in the build for
// no other reason. Field numbers follow sidero.discovery.storage exactly; a
// change here breaks compatibility with upstream snapshots.
//
//	StateSnapshot     { repeated ClusterSnapshot clusters = 1; }
//	ClusterSnapshot   { string id = 1; repeated AffiliateSnapshot affiliates = 2; }
//	AffiliateSnapshot { string id = 1; Timestamp expiration = 2; bytes data = 3;
//	                    repeated EndpointSnapshot endpoints = 4; Duration ttl = 5; }
//	EndpointSnapshot  { Timestamp expiration = 1; bytes data = 2; Duration ttl = 3; }
//	Timestamp, Duration { int64 seconds = 1; int32 nanos = 2; }

func encodeSnapshot(clusters []clusterSnapshot, now time.Time) []byte {
	var b []byte
	for _, c := range clusters {
		b = appendMessage(b, 1, encodeCluster(c, now))
	}
	return b
}

func encodeCluster(c clusterSnapshot, now time.Time) []byte {
	var b []byte
	b = appendString(b, 1, c.ID)
	for _, a := range c.Affiliates {
		b = appendMessage(b, 2, encodeAffiliate(a, now))
	}
	return b
}

func encodeAffiliate(a affiliateSnapshot, now time.Time) []byte {
	var b []byte
	b = appendString(b, 1, a.ID)
	b = appendMessage(b, 2, encodeSecondsNanos(now.Add(time.Duration(a.TTL)).UnixNano()))
	if len(a.Data) > 0 {
		b = protowire.AppendTag(b, 3, protowire.BytesType)
		b = protowire.AppendBytes(b, a.Data)
	}
	for _, e := range a.Endpoints {
		b = appendMessage(b, 4, encodeEndpoint(e, now))
	}
	b = appendMessage(b, 5, encodeSecondsNanos(int64(a.TTL)))
	return b
}

func encodeEndpoint(e endpointSnapshot, now time.Time) []byte {
	var b []byte
	b = appendMessage(b, 1, encodeSecondsNanos(now.Add(time.Duration(e.TTL)).UnixNano()))
	b = protowire.AppendTag(b, 2, protowire.BytesType)
	b = protowire.AppendBytes(b, e.Data)
	b = appendMessage(b, 3, encodeSecondsNanos(int64(e.TTL)))
	return b
}

// encodeSecondsNanos writes a Timestamp or a Duration, which share a layout.
func encodeSecondsNanos(nanos int64) []byte {
	var b []byte
	if secs := nanos / int64(time.Second); secs != 0 {
		b = protowire.AppendTag(b, 1, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(secs))
	}
	if rem := nanos % int64(time.Second); rem != 0 {
		b = protowire.AppendTag(b, 2, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(rem))
	}
	return b
}

func appendMessage(b []byte, num protowire.Number, msg []byte) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendBytes(b, msg)
}

func appendString(b []byte, num protowire.Number, s string) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)
	return protowire.AppendString(b, s)
}

func decodeSnapshot(b []byte) ([]clusterSnapshot, error) {
	var clusters []clusterSnapshot
	err := eachField(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if num != 1 || typ != protowire.BytesType {
			return nil
		}
		c, err := decodeCluster(v)
		if err != nil {
			return err
		}
		clusters = append(clusters, c)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	return clusters, nil
}

func decodeCluster(b []byte) (clusterSnapshot, error) {
	var c clusterSnapshot
	err := eachField(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if typ != protowire.BytesType {
			return nil
		}
		switch num {
		case 1:
			c.ID = string(v)
		case 2:
			a, err := decodeAffiliate(v)
			if err != nil {
				return err
			}
			c.Affiliates = append(c.Affiliates, a)
		}
		return nil
	})
	return c, err
}

func decodeAffiliate(b []byte) (affiliateSnapshot, error) {
	var a affiliateSnapshot
	err := eachField(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if typ != protowire.BytesType {
			return nil
		}
		switch num {
		case 1:
			a.ID = string(v)
		case 2:
			nanos, err := decodeSecondsNanos(v)
			if err != nil {
				return err
			}
			a.expiration = time.Unix(0, nanos)
		case 3:
			a.Data = append([]byte(nil), v...)
		case 4:
			e, err := decodeEndpoint(v)
			if err != nil {
				return err
			}
			a.Endpoints = append(a.Endpoints, e)
		case 5:
			nanos, err := decodeSecondsNanos(v)
			if err != nil {
				return err
			}
			a.TTL = jsonDuration(nanos)
		}
		return nil
	})
	return a, err
}

func decodeEndpoint(b []byte) (endpointSnapshot, error) {
	var e endpointSnapshot
	err := eachField(b, func(num protowire.Number, typ protowire.Type, v []byte, _ uint64) error {
		if typ != protowire.BytesType {
			return nil
		}
		switch num {
		case 1:
			nanos, err := decodeSecondsNanos(v)
			if err != nil {
				return err
			}
			e.expiration = time.Unix(0, nanos)
		case 2:
			e.Data = append([]byte(nil), v...)
		case 3:
			nanos, err := decodeSecondsNanos(v)
			if err != nil {
				return err
			}
			e.TTL = jsonDuration(nanos)
		}
		return nil
	})
	return e, err
}

func decodeSecondsNanos(b []byte) (int64, error) {
	var secs, nanos int64
	err := eachField(b, func(num protowire.Number, typ protowire.Type, _ []byte, n uint64) error {
		if typ != protowire.VarintType {
			return nil
		}
		switch num {
		case 1:
			secs = int64(n)
		case 2:
			nanos = int64(int32(n)) //nolint:gosec // int32 field on the wire
		}
		return nil
	})
	return secs*int64(time.Second) + nanos, err
}

// eachField walks one message's fields. Bytes fields arrive in v, varints in n;
// unknown fields are skipped, so a newer writer's additions do not break this
// reader.
func eachField(b []byte, f func(num protowire.Number, typ protowire.Type, v []byte, n uint64) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]

		var (
			v     []byte
			value uint64
		)
		switch typ {
		case protowire.BytesType:
			v, n = protowire.ConsumeBytes(b)
		case protowire.VarintType:
			value, n = protowire.ConsumeVarint(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]

		if err := f(num, typ, v, value); err != nil {
			return err
		}
	}
	return nil
}
