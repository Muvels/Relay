// Package pin implements certificate fingerprint pinning shared by the
// server (which advertises its fingerprint) and the agent (which verifies
// it on join and pins the certificate thereafter).
package pin

import (
	"crypto/sha256"
	"encoding/hex"
)

// Len is the truncated fingerprint length in hex chars (128 bits).
const Len = 32

// Fingerprint of a DER-encoded certificate.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])[:Len]
}
