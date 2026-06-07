package egress

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"strings"
	"time"
)

// tokenEpoch is the base for the 4-byte exp field in token payloads (2024-01-01 UTC).
// uint32 seconds from this epoch overflows in ~2160.
var tokenEpoch = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix()

const (
	sigSize       = 64
	shapeAPayload = 18 // slot(1) + ent(9) + tid(8)
	shapeBPayload = 22 // slot(1) + ent(9) + tid(8) + exp(4)
	slotHasExpBit = uint8(1 << 7)
)

// TokenClaims holds the decoded fields from a successfully verified PASETO token.
type TokenClaims struct {
	WorkspaceSlug string    // extracted from the sk-<slug>. prefix
	Slot          int       // keypair slot (1 or 2)
	KeyEntrySlug  string    // key_entries.slug identifying the routing config
	TID           []byte    // 8-byte token ID (first 8 bytes of UUIDv7); serves as jti
	Exp           time.Time // zero value means never expires
}

// Expired reports whether the token is past its expiry. Tokens with zero Exp never expire.
func (c *TokenClaims) Expired() bool {
	return !c.Exp.IsZero() && time.Now().UTC().After(c.Exp)
}

// VerifyToken verifies a PASETO Bearer token using the provided Ed25519 public keys.
// raw may be a full Authorization header value ("Bearer sk-...") or a bare token ("sk-...").
// pubKey1 and pubKey2 correspond to keypair slots 1 and 2; pass nil for an absent slot.
// Returns the decoded claims on success, or an error if the token is malformed or
// the signature is invalid.
func VerifyToken(raw string, pubKey1, pubKey2 ed25519.PublicKey) (*TokenClaims, error) {
	token := strings.TrimPrefix(strings.TrimSpace(raw), "Bearer ")
	token = strings.TrimSpace(token)

	if !strings.HasPrefix(token, "sk-") {
		n := len(token)
		if n > 16 {
			n = 16
		}
		return nil, fmt.Errorf("token must start with sk- (got %q)", token[:n])
	}
	after := token[len("sk-"):]

	// "." separates workspace slug from base64url body and never appears in base64url,
	// so this split is unambiguous even when the slug contains "-".
	dot := strings.IndexByte(after, '.')
	if dot < 0 {
		return nil, fmt.Errorf("malformed token: missing '.' separator after workspace slug")
	}
	wsSlug := after[:dot]

	body, err := base64.RawURLEncoding.DecodeString(after[dot+1:])
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}

	var payloadLen int
	switch len(body) {
	case shapeAPayload + sigSize:
		payloadLen = shapeAPayload
	case shapeBPayload + sigSize:
		payloadLen = shapeBPayload
	default:
		return nil, fmt.Errorf("unexpected decoded length %d (want %d or %d)", len(body), shapeAPayload+sigSize, shapeBPayload+sigSize)
	}

	payload := body[:payloadLen]
	sig := body[payloadLen:]

	slotByte := payload[0]
	hasExp := slotByte&slotHasExpBit != 0
	slot := int(slotByte & 0x7F)

	var pub ed25519.PublicKey
	switch slot {
	case 1:
		pub = pubKey1
	case 2:
		pub = pubKey2
	default:
		return nil, fmt.Errorf("unknown slot %d", slot)
	}
	if pub == nil {
		return nil, fmt.Errorf("slot %d public key not available", slot)
	}

	if !verifyV4Public(pub, payload, sig) {
		return nil, fmt.Errorf("signature invalid")
	}

	claims := &TokenClaims{
		WorkspaceSlug: wsSlug,
		Slot:          slot,
		KeyEntrySlug:  strings.TrimRight(string(payload[1:10]), "\x00"),
		TID:           append([]byte(nil), payload[10:18]...),
	}
	if hasExp {
		expSec := int64(binary.LittleEndian.Uint32(payload[18:22])) + tokenEpoch
		claims.Exp = time.Unix(expSec, 0).UTC()
	}
	return claims, nil
}

// ParseEd25519PublicKey decodes a PEM-encoded PKIX Ed25519 public key, as stored
// in egress bundle files (paseto-1.pub, paseto-2.pub).
func ParseEd25519PublicKey(pemStr string) (ed25519.PublicKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKIX: %w", err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("expected ed25519.PublicKey, got %T", key)
	}
	return pub, nil
}

// verifyV4Public verifies an Ed25519 PASETO v4.public signature.
// msg = PAE("v4.public.", payload, "")
func verifyV4Public(pub ed25519.PublicKey, payload, sig []byte) bool {
	return ed25519.Verify(pub, pae([]byte("v4.public."), payload, []byte("")), sig)
}

// pae implements PASETO Pre-Authentication Encoding (PAE).
func pae(pieces ...[]byte) []byte {
	tmp := make([]byte, 8)
	var out []byte
	binary.LittleEndian.PutUint64(tmp, uint64(len(pieces)))
	out = append(out, tmp...)
	for _, p := range pieces {
		chunk := make([]byte, 8)
		binary.LittleEndian.PutUint64(chunk, uint64(len(p)))
		out = append(out, chunk...)
		out = append(out, p...)
	}
	return out
}
