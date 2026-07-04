// Package eebuscert loads or creates SHIP-compatible EEBUS certificates and
// derives the SKI from them. It is shared by the EEBUS clients in this module.
package eebuscert

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"github.com/enbility/ship-go/cert"
)

// LoadOrCreate returns the TLS certificate at certPath/keyPath. If the files do
// not exist, it creates a new self-signed SHIP certificate with the given
// common name, persists it to those paths, and returns it. It also returns the
// SKI derived from the certificate's public key.
func LoadOrCreate(certPath, keyPath, commonName string) (tls.Certificate, string, error) {
	if fileExists(certPath) && fileExists(keyPath) {
		certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return tls.Certificate{}, "", err
		}
		ski, err := SKI(certificate)
		return certificate, ski, err
	}

	certificate, err := cert.CreateCertificate("Demo", "Demo", "DE", commonName)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := write(certPath, keyPath, certificate); err != nil {
		return tls.Certificate{}, "", err
	}
	ski, err := SKI(certificate)
	return certificate, ski, err
}

// SKI returns the Subject Key Identifier of the certificate's leaf.
func SKI(certificate tls.Certificate) (string, error) {
	if len(certificate.Certificate) == 0 {
		return "", fmt.Errorf("certificate contains no leaf certificate")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return "", err
	}
	return cert.SkiFromCertificate(leaf)
}

func write(certPath, keyPath string, certificate tls.Certificate) error {
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
		return err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
	privateKey, ok := certificate.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return fmt.Errorf("certificate private key is %T, expected *ecdsa.PrivateKey", certificate.PrivateKey)
	}
	keyBytes, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyPath, keyPEM, 0o600)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
