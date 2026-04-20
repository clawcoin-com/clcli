// Package evm — minimal RLP encoder sufficient for signing legacy EIP-155 transactions.
package evm

import (
	"encoding/binary"
)

// rlpBytes encodes a byte string per RLP rules.
// - 0x00..0x7f: single byte as-is
// - 0..55 bytes: 0x80+len || payload
// - >=56 bytes: 0xb7+len_of_len || big-endian len || payload
func rlpBytes(b []byte) []byte {
	if len(b) == 1 && b[0] < 0x80 {
		return []byte{b[0]}
	}
	if len(b) <= 55 {
		return append([]byte{0x80 + byte(len(b))}, b...)
	}
	lenBytes := encodeLength(uint64(len(b)))
	return append(append([]byte{0xb7 + byte(len(lenBytes))}, lenBytes...), b...)
}

// rlpList wraps a concatenated list of already-RLP-encoded items.
func rlpList(items ...[]byte) []byte {
	var payload []byte
	for _, it := range items {
		payload = append(payload, it...)
	}
	if len(payload) <= 55 {
		return append([]byte{0xc0 + byte(len(payload))}, payload...)
	}
	lenBytes := encodeLength(uint64(len(payload)))
	return append(append([]byte{0xf7 + byte(len(lenBytes))}, lenBytes...), payload...)
}

// encodeLength returns the minimal big-endian representation of n.
func encodeLength(n uint64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], n)
	// strip leading zeros
	i := 0
	for i < 7 && buf[i] == 0 {
		i++
	}
	return buf[i:]
}

// trimLeadingZeros strips leading zero bytes (RLP expects canonical big-int encoding).
func trimLeadingZeros(b []byte) []byte {
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	return b
}
