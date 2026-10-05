package api

import (
	"crypto/rand"
	"encoding/hex"
)

// newRequestID mints an id for a request that arrived without one.
//
// It is 16 random bytes rather than a counter, because ids from two nodes must
// not collide when their logs are read together, and a counter restarts at
// zero every time a process does. crypto/rand cannot realistically fail here;
// if it ever did, an empty id is preferable to refusing the request, since the
// id is for correlation and nothing depends on it being present.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
