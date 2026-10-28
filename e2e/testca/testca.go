// Package testca is the end-to-end runs' throwaway certificate authority:
// a CA, server certificates it signs, and their SPKI pins. The b4 harness
// (e2e/realaimem) and the pilot simulation (e2e/pilotsim) share it. It is
// for tests only: the keys live as long as a run.
package testca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"time"
)

// CA is a throwaway certificate authority.
type CA struct {
	Cert *x509.Certificate
	DER  []byte
	key  *ecdsa.PrivateKey
}

// New makes a CA named cn, valid from an hour ago for validity.
func New(cn string, validity time.Duration) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(validity),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, DER: der, key: key}, nil
}

// Leaf is a server certificate the CA signed, with its key and the pin of
// its public key.
type Leaf struct {
	CertDER []byte
	KeyDER  []byte // SEC 1 ("EC PRIVATE KEY")
	Pin     string // sha256-BASE64 of the public key
}

// Leaf issues a server certificate named name for the DNS names and IP
// addresses given, valid from an hour ago for validity.
func (ca *CA) Leaf(name string, dns []string, ips []net.IP, validity time.Duration) (Leaf, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Leaf{}, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(validity),
		DNSNames: dns, IPAddresses: ips,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.key)
	if err != nil {
		return Leaf{}, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Leaf{}, err
	}
	pin, err := Pin(&key.PublicKey)
	if err != nil {
		return Leaf{}, err
	}
	return Leaf{CertDER: der, KeyDER: kder, Pin: pin}, nil
}

// Pin is the SPKI pin of a public key: sha256- and the base64 of the
// SHA-256 of its PKIX encoding.
func Pin(pub any) (string, error) {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(spki)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:]), nil
}

// WritePEM writes der as one PEM block of type typ.
func WritePEM(path, typ string, der []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode)
}
