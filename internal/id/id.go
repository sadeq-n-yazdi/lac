// Package id generates the opaque, time-ordered identifiers LAC uses for its records.
package id

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"time"
)

// encoding is lowercase base32 without padding: URL-safe, case-insensitive and shell-friendly.
var encoding = base32.NewEncoding("0123456789abcdefghijklmnopqrstuv").WithPadding(base32.NoPadding)

// now is a seam so tests can control the clock.
var now = time.Now

// randomBytes is the number of random bytes appended after the timestamp. Eighty bits make a
// collision within a single millisecond effectively impossible for a single-machine coordinator.
const randomBytes = 10

// New returns a new identifier prefixed with kind, for example "agent_0h2n8k...".
//
// The body encodes a millisecond timestamp followed by random bytes, so identifiers created in
// different milliseconds sort by creation time as plain strings. That keeps database indexes
// compact and log output readable. Identifiers created within the same millisecond are unique but
// unordered relative to each other; nothing in LAC relies on ordering at that resolution.
func New(kind string) string {
	var buffer [6 + randomBytes]byte

	milliseconds := uint64(now().UTC().UnixMilli())
	var timestamp [8]byte
	binary.BigEndian.PutUint64(timestamp[:], milliseconds)
	copy(buffer[:6], timestamp[2:]) // 48 bits of milliseconds lasts until the year 10889

	// rand.Read from crypto/rand never returns an error; it terminates the process if the OS source fails.
	rand.Read(buffer[6:])

	return fmt.Sprintf("%s_%s", kind, encoding.EncodeToString(buffer[:]))
}
