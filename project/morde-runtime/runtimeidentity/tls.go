// Package runtimeidentity derives the private runtime TLS identity from the
// per-Host runtime credential. No private key is persisted.
package runtimeidentity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"crypto/hkdf"
	"crypto/sha512"
)

const ServerName = "morde-runtime.internal"

var (
	ErrInvalidCredential = errors.New("invalid runtime credential")
	ErrInvalidHostID     = errors.New("invalid Host ID")
)

// ServerTLSConfig returns a TLS 1.3 server config. The leaf certificate and
// private key are derived deterministically for this credential and Host ID;
// rotating the credential for a new Sandbox generation rotates the identity.
func ServerTLSConfig(credential, hostID string) (*tls.Config, error) {
	_, caKey, caCert, err := deriveCA(credential, hostID)
	if err != nil {
		return nil, err
	}
	leafKey, err := deriveKey(credential, hostID, "server-leaf")
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	serial := serialFor(leafKey.Public().(ed25519.PublicKey))
	leafTemplate := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: ServerName},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		DNSNames:              []string{ServerName},
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	leafDER, err := x509.CreateCertificate(nil, leafTemplate, caCert, leafKey.Public(), caKey)
	if err != nil {
		return nil, errors.New("derive runtime server certificate")
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return nil, errors.New("parse runtime server certificate")
	}
	cert := tls.Certificate{
		Certificate: [][]byte{leafDER, caCert.Raw},
		PrivateKey:  leafKey,
		Leaf:        leaf,
	}
	return &tls.Config{ //nolint:gosec // TLS 1.3 and a pinned private CA are mandatory here.
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
	}, nil
}

// ClientTLSConfig returns a TLS 1.3 config trusting only the CA derived from
// this Host's current credential and pinning the one runtime DNS name.
func ClientTLSConfig(credential, hostID string) (*tls.Config, error) {
	_, _, caCert, err := deriveCA(credential, hostID)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
		RootCAs:    roots,
		ServerName: ServerName,
	}, nil
}

func deriveCA(credential, hostID string) (ed25519.PublicKey, ed25519.PrivateKey, *x509.Certificate, error) {
	key, err := deriveKey(credential, hostID, "ca")
	if err != nil {
		return nil, nil, nil, err
	}
	publicKey := key.Public().(ed25519.PublicKey)
	template := &x509.Certificate{
		SerialNumber:          serialFor(publicKey),
		Subject:               pkix.Name{CommonName: "Morde Runtime CA " + hostID},
		NotBefore:             time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}
	der, err := x509.CreateCertificate(nil, template, template, publicKey, key)
	if err != nil {
		return nil, nil, nil, errors.New("derive runtime CA certificate")
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, errors.New("parse runtime CA certificate")
	}
	return publicKey, key, cert, nil
}

func deriveKey(credential, hostID, purpose string) (ed25519.PrivateKey, error) {
	if !validHostID(hostID) {
		return nil, ErrInvalidHostID
	}
	secret, err := base64.RawURLEncoding.DecodeString(credential)
	if err != nil || len(secret) != ed25519.SeedSize || base64.RawURLEncoding.EncodeToString(secret) != credential {
		return nil, ErrInvalidCredential
	}
	prk, err := hkdf.Extract(sha512.New, secret, []byte("morde-runtime/tls/v1"))
	if err != nil {
		return nil, errors.New("derive runtime TLS identity")
	}
	seed, err := hkdf.Expand(sha512.New, prk, "ed25519/"+purpose+"/host/"+hostID, ed25519.SeedSize)
	if err != nil {
		return nil, errors.New("derive runtime TLS identity")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func serialFor(publicKey ed25519.PublicKey) *big.Int {
	digest := sha256.Sum256(publicKey)
	serial := new(big.Int).SetBytes(digest[:20])
	serial.SetBit(serial, 159, 0)
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	return serial
}

func validHostID(hostID string) bool {
	if len(hostID) != 8 || hostID[0] != 'h' {
		return false
	}
	for _, char := range hostID[1:] {
		if !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}

// ReadCredentialFile reads the fixed-format root-only credential without
// following symlinks. The process must run as root and the file must be root
// owned with no group or other permissions.
func ReadCredentialFile(path string) (string, error) {
	return readRootOnlyCredentialFile(path)
}

func ReadReplicaCredentialFile(path string) (string, error) {
	return readRootOnlyCredentialFile(path)
}

func readRootOnlyCredentialFile(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || os.Geteuid() != 0 {
		return "", ErrInvalidCredential
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o400 {
		return "", ErrInvalidCredential
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return "", ErrInvalidCredential
	}
	file, err := os.Open(path)
	if err != nil {
		return "", ErrInvalidCredential
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return "", ErrInvalidCredential
	}
	contents, err := io.ReadAll(io.LimitReader(file, 44))
	if err != nil || len(contents) != 43 {
		return "", ErrInvalidCredential
	}
	credential := string(contents)
	if _, err := deriveKey(credential, "habc1234", "validation"); err != nil {
		return "", ErrInvalidCredential
	}
	return credential, nil
}
