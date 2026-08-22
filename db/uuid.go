package db

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"sync"
	"time"
)

// NewID returns a UUIDv7: 48 bits of Unix milliseconds, a 12-bit counter,
// then 62 bits of randomness, in the canonical 8-4-4-4-12 hex form.
//
// Version 7 and not version 4 because the id is the primary key. Random ids
// insert uniformly across a B-tree, so every insert dirties a different page;
// time-ordered ids append at the right edge, where the pages are already in
// cache. It also means the id sorts by creation time, which removes the
// second index a "newest first" listing would otherwise need.
//
// The counter is the monotonic variant in RFC 9562 §6.2: without it two
// documents created in the same millisecond sort arbitrarily, and a loop that
// creates rows is exactly the case where that shows up.
func NewID() string {
	var b [16]byte
	rand.Read(b[:]) //nolint:errcheck // crypto/rand.Read cannot fail; it panics instead (Go 1.24+)

	ms, seq := tick()

	var t [8]byte
	binary.BigEndian.PutUint64(t[:], uint64(ms))
	copy(b[0:6], t[2:8])
	b[6] = 0x70 | byte(seq>>8&0x0f) // version 7 in the high nibble
	b[7] = byte(seq)
	b[8] = 0x80 | b[8]&0x3f // RFC 9562 variant

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

var clock struct {
	sync.Mutex
	millis int64
	seq    uint16
}

// tick returns the timestamp and counter for one id. Past 4096 ids in a
// single millisecond it borrows the next millisecond instead of wrapping —
// wrapping would emit an id that sorts before the one issued just before it,
// which is the single guarantee this function exists to provide.
func tick() (int64, uint16) {
	clock.Lock()
	defer clock.Unlock()
	now := time.Now().UnixMilli()
	switch {
	case now > clock.millis:
		clock.millis, clock.seq = now, 0
	case clock.seq >= 0x0fff:
		clock.millis, clock.seq = clock.millis+1, 0
	default:
		clock.seq++
	}
	return clock.millis, clock.seq
}
