package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/matteomarolt/relay/relayd/internal/pin"
)

// Relay's transport trust model is T3-style pinning, not PKI: the server
// self-generates one long-lived cert; `relay connect` embeds its
// fingerprint in the join string (token@host:port#fp); agents verify the
// fingerprint on first join, store the cert, and pin it thereafter.
// Hostname/SAN validation is deliberately irrelevant.

const FingerprintLen = pin.Len

// EnsureCert loads or creates the server certificate. Returns the cert and
// its pin fingerprint.
func EnsureCert(dataDir string) (tls.Certificate, string, error) {
	certPath := filepath.Join(dataDir, "tls-cert.pem")
	keyPath := filepath.Join(dataDir, "tls-key.pem")

	if _, err := os.Stat(certPath); err == nil {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return tls.Certificate{}, "", err
		}
		return cert, Fingerprint(cert.Certificate[0]), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return tls.Certificate{}, "", err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	hostname, _ := os.Hostname()
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "relayd"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost", hostname},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template,
		&key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	// Key first: the loader gates on the cert file, so cert-present must
	// imply key-present even across a crash between the writes.
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	return cert, Fingerprint(der), nil
}

// Fingerprint of a DER certificate, truncated for join strings.
func Fingerprint(der []byte) string { return pin.Fingerprint(der) }
