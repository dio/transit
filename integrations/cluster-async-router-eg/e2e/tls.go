package e2e

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// tlsBundle is a self-signed CA and the leaf cert+key it signed.
type tlsBundle struct {
	CAPEM, CertPEM, KeyPEM []byte
}

// genCA returns a fresh P-256 CA suitable for signing leaf certs.
func genCA() ([]byte, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cluster-async-router-eg-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key, nil
}

// genLeaf returns a leaf cert+key signed by caPEM/caKey, valid for dnsName.
func genLeaf(dnsName string, caPEM []byte, caKey *ecdsa.PrivateKey) (tlsBundle, error) {
	caBlock, _ := pem.Decode(caPEM)
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return tlsBundle{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tlsBundle{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		return tlsBundle{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: dnsName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{dnsName},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return tlsBundle{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tlsBundle{}, err
	}
	return tlsBundle{
		CAPEM:   caPEM,
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// caSecretManifest renders a generic Secret holding the CA bundle under the
// key ca.crt. The EnvoyProxy template mounts it via items[].path=ca.pem.
func caSecretManifest(name, namespace string, caPEM []byte) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
stringData:
  ca.crt: |
%s
`, name, namespace, indent(string(caPEM), "    "))
}

// tlsSecretManifest renders a kubernetes.io/tls Secret. Standard tls.crt /
// tls.key keys; the demo binary's upstream-tls mode mounts both at /etc/tls.
func tlsSecretManifest(name, namespace string, b tlsBundle) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: kubernetes.io/tls
stringData:
  tls.crt: |
%s
  tls.key: |
%s
`, name, namespace, indent(string(b.CertPEM), "    "), indent(string(b.KeyPEM), "    "))
}

func indent(s, prefix string) string {
	out := ""
	for i, line := range splitLines(s) {
		if i > 0 {
			out += "\n"
		}
		out += prefix + line
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
