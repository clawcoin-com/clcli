package keystore

import (
	"encoding/hex"
	"fmt"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// PersonalSign signs a message using EIP-191 personal_sign format.
// The message is prefixed with "\x19Ethereum Signed Message:\n<len>" then keccak256'd.
// Returns a 65-byte hex-encoded signature (r || s || v) with v as 27/28, 0x-prefixed.
func PersonalSign(privBytes, message []byte) (string, error) {
	if len(privBytes) != 32 {
		return "", fmt.Errorf("private key must be 32 bytes")
	}
	priv := secp256k1.PrivKeyFromBytes(privBytes)
	prefixed := append(
		[]byte(fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(message))),
		message...,
	)
	hash := Keccak256(prefixed)

	sig := ecdsa.SignCompact(priv, hash, false) // 65 bytes: [v, r, s]
	// SignCompact returns [recID+27, r, s]; we need [r, s, v-27] for Ethereum convention.
	r := sig[1:33]
	s := sig[33:65]
	v := sig[0] // already 27 or 28
	out := make([]byte, 65)
	copy(out[0:32], r)
	copy(out[32:64], s)
	out[64] = v - 27 // Ethereum EIP-191 v = 0/1; the server-side recoverSigner handles <27 case
	return "0x" + hex.EncodeToString(out), nil
}
