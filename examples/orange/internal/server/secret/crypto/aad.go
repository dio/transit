// Package crypto provides the encryption primitives for the secret store:
// XChaCha20-Poly1305 authenticated encryption, HKDF-SHA256 key derivation,
// and length-prefix encoded AAD construction.
package crypto

import (
	"bytes"
	"encoding/binary"
)

// canonicalAAD returns a length-prefix–encoded concatenation of fields.
// Using uint32 length prefixes prevents field-boundary ambiguity:
// ("ab","c") and ("a","bc") produce different byte sequences.
func canonicalAAD(fields ...string) []byte {
	var buf bytes.Buffer
	for _, f := range fields {
		_ = binary.Write(&buf, binary.BigEndian, uint32(len(f)))
		buf.WriteString(f)
	}
	return buf.Bytes()
}

// SecretAAD binds a secret ciphertext to its full identity so that replaying
// the ciphertext under a different (realm, secretID, versionID, dekID,
// dekVersion) triple fails the Poly1305 tag check.
func SecretAAD(realm, secretID, versionID, dekID, dekVersion string) []byte {
	return canonicalAAD(realm, secretID, versionID, dekID, dekVersion)
}

// DEKWrapAAD binds a wrapped DEK to its key identity.
func DEKWrapAAD(realm, dekID, dekVersion, kekID, kekVersion string) []byte {
	return canonicalAAD(realm, dekID, dekVersion, kekID, kekVersion)
}

// ServiceKEKWrapAAD binds a wrapped SERVICE_KEK to its key identity.
func ServiceKEKWrapAAD(kekID, kekVersion, masterKeyID, masterKeyVersion string) []byte {
	return canonicalAAD("system", kekID, kekVersion, masterKeyID, masterKeyVersion)
}
