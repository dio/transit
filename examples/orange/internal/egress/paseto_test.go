package egress_test

import (
	"testing"
	"time"

	"github.com/dio/transit/examples/orange/internal/egress"
)

// testToken is a real token issued against the test bundle. It uses slot 2.
const testToken = "sk-Cw0dlbxDR.AjhSN1ZhVG1PVAGeoDSzjXiXQRvtiPastF0Wu8SmxwgnmdS2p7ZwTtdftN9zI98irp5fNx7cOevVmg8va3pFM50W224AFXHp-Sg9sKNDOKCBDg"

// PEM keys extracted from testdata bundle (paseto-1.pub and paseto-2.pub).
const (
	testPaseto1PubPEM = `-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAyz9U034R50qBZyno12NfFkSFSuB8VD6NA30xg4gxVAE=
-----END PUBLIC KEY-----`

	testPaseto2PubPEM = `-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAdECf8a01GrSmU5RDo4pcF7ULj1kkwAL797AYrMAwZ6c=
-----END PUBLIC KEY-----`
)

func loadTestKeys(t *testing.T) (pub1, pub2 []byte) {
	t.Helper()
	p1, err := egress.ParseEd25519PublicKey(testPaseto1PubPEM)
	if err != nil {
		t.Fatalf("parse slot-1 public key: %v", err)
	}
	p2, err := egress.ParseEd25519PublicKey(testPaseto2PubPEM)
	if err != nil {
		t.Fatalf("parse slot-2 public key: %v", err)
	}
	return p1, p2
}

func TestVerifyToken(t *testing.T) {
	pub1, pub2 := loadTestKeys(t)

	claims, err := egress.VerifyToken(testToken, pub1, pub2)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}

	if claims.WorkspaceSlug != "Cw0dlbxDR" {
		t.Errorf("WorkspaceSlug = %q, want %q", claims.WorkspaceSlug, "Cw0dlbxDR")
	}
	if claims.Slot != 2 {
		t.Errorf("Slot = %d, want 2", claims.Slot)
	}
	if claims.KeyEntrySlug != "8R7VaTmOT" {
		t.Errorf("KeyEntrySlug = %q, want %q", claims.KeyEntrySlug, "8R7VaTmOT")
	}
	if len(claims.TID) != 8 {
		t.Errorf("TID length = %d, want 8", len(claims.TID))
	}
	if !claims.Exp.IsZero() {
		t.Errorf("Exp = %v, want zero (no expiry)", claims.Exp)
	}
	if claims.Expired() {
		t.Error("Expired() = true, want false for no-expiry token")
	}
}

func TestVerifyToken_BearerPrefix(t *testing.T) {
	pub1, pub2 := loadTestKeys(t)

	claims, err := egress.VerifyToken("Bearer "+testToken, pub1, pub2)
	if err != nil {
		t.Fatalf("VerifyToken with Bearer prefix: %v", err)
	}
	if claims.WorkspaceSlug != "Cw0dlbxDR" {
		t.Errorf("WorkspaceSlug = %q, want %q", claims.WorkspaceSlug, "Cw0dlbxDR")
	}
}

func TestVerifyToken_WrongSlot(t *testing.T) {
	pub1, _ := loadTestKeys(t)

	// Token uses slot 2; passing only pub1 (slot-2 key is nil) must fail.
	_, err := egress.VerifyToken(testToken, pub1, nil)
	if err == nil {
		t.Fatal("expected error when slot-2 key is nil, got nil")
	}
}

func TestVerifyToken_TamperedBody(t *testing.T) {
	pub1, pub2 := loadTestKeys(t)

	// Flip the last byte of the base64 body to invalidate the signature.
	tampered := testToken[:len(testToken)-1] + "A"
	_, err := egress.VerifyToken(tampered, pub1, pub2)
	if err == nil {
		t.Fatal("expected error for tampered token, got nil")
	}
}

func TestVerifyToken_BadPrefix(t *testing.T) {
	pub1, pub2 := loadTestKeys(t)

	_, err := egress.VerifyToken("not-a-token", pub1, pub2)
	if err == nil {
		t.Fatal("expected error for token without sk- prefix, got nil")
	}
}

func TestVerifyToken_MissingSeparator(t *testing.T) {
	pub1, pub2 := loadTestKeys(t)

	_, err := egress.VerifyToken("sk-noDotHere", pub1, pub2)
	if err == nil {
		t.Fatal("expected error for token without '.' separator, got nil")
	}
}

func TestTokenClaims_Expired(t *testing.T) {
	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)

	if c := (&egress.TokenClaims{Exp: past}); !c.Expired() {
		t.Error("past expiry: Expired() = false, want true")
	}
	if c := (&egress.TokenClaims{Exp: future}); c.Expired() {
		t.Error("future expiry: Expired() = true, want false")
	}
	if c := (&egress.TokenClaims{}); c.Expired() {
		t.Error("zero expiry: Expired() = true, want false (never expires)")
	}
}

func TestParseEd25519PublicKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		pem  string
	}{
		{"slot1", testPaseto1PubPEM},
		{"slot2", testPaseto2PubPEM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, err := egress.ParseEd25519PublicKey(tc.pem)
			if err != nil {
				t.Fatalf("ParseEd25519PublicKey: %v", err)
			}
			if len(key) != 32 {
				t.Errorf("key length = %d, want 32", len(key))
			}
		})
	}
}

func TestParseEd25519PublicKey_Invalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		pem  string
	}{
		{"empty", ""},
		{"garbage", "not-pem"},
		{"wrong type", "-----BEGIN CERTIFICATE-----\nYQ==\n-----END CERTIFICATE-----"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := egress.ParseEd25519PublicKey(tc.pem)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}
