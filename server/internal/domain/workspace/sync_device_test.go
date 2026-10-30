package workspace

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

func TestSyncthingDeviceIDFromCertificateDER(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "syncthing"},
		NotBefore: time.Unix(1_700_000_000, 0), NotAfter: time.Unix(1_900_000_000, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}

	got, err := SyncthingDeviceIDFromCertificateDER(der)
	if err != nil {
		t.Fatal(err)
	}
	want := syncthingDeviceID(sha256.Sum256(der))
	if got != want {
		t.Fatalf("device ID from certificate = %q, want %q", got, want)
	}
	if again, err := SyncthingDeviceIDFromCertificateDER(der); err != nil || again != got {
		t.Fatalf("device ID is not stable: %q, %v; first %q", again, err, got)
	}

	const vector = "SGEJ4QP-U72C4QY-L53UPY6-DCIJXM4-BMNBW7W-S5ZYTCX-VFLBTD5-IEYQYAY"
	if actual := syncthingDeviceID(sha256.Sum256([]byte("syncthing-device-id-vector"))); actual != vector {
		t.Fatalf("Syncthing device ID algorithm vector = %q, want %q", actual, vector)
	}
}

func TestSyncthingDeviceIDFromCertificateDERRejectsInvalidCertificate(t *testing.T) {
	if got, err := SyncthingDeviceIDFromCertificateDER([]byte("not DER")); err == nil || got != "" {
		t.Fatalf("device ID from invalid certificate = %q, %v; want error", got, err)
	}
}
