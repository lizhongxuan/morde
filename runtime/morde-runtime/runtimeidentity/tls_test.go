package runtimeidentity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDerivedTLSIdentityIsHostBoundAndTLS13(t *testing.T) {
	credential := testCredential()
	serverConfig, err := ServerTLSConfig(credential, "h1234567")
	if err != nil {
		t.Fatal(err)
	}
	clientConfig, err := ClientTLSConfig(credential, "h1234567")
	if err != nil {
		t.Fatal(err)
	}
	if serverConfig.MinVersion != tls.VersionTLS13 || serverConfig.MaxVersion != tls.VersionTLS13 || clientConfig.MinVersion != tls.VersionTLS13 || clientConfig.MaxVersion != tls.VersionTLS13 {
		t.Fatal("TLS configuration does not require TLS 1.3")
	}
	if clientConfig.InsecureSkipVerify || clientConfig.ServerName != ServerName {
		t.Fatal("client does not verify the pinned server name")
	}
	if len(serverConfig.Certificates) != 1 || serverConfig.Certificates[0].Leaf == nil {
		t.Fatal("server certificate missing parsed leaf")
	}
	leaf := serverConfig.Certificates[0].Leaf
	if !reflect.DeepEqual(leaf.DNSNames, []string{ServerName}) || len(leaf.IPAddresses) != 0 || leaf.IsCA {
		t.Fatalf("unexpected server SAN/certificate type: DNS=%v IP=%v IsCA=%v", leaf.DNSNames, leaf.IPAddresses, leaf.IsCA)
	}
	if len(serverConfig.Certificates[0].Certificate) != 2 {
		t.Fatal("self-signed CA was not sent with the server certificate")
	}
	ca, err := x509.ParseCertificate(serverConfig.Certificates[0].Certificate[1])
	if err != nil || !ca.IsCA || !ca.BasicConstraintsValid || ca.Subject.CommonName != "Morde Runtime CA h1234567" {
		t.Fatalf("invalid derived CA: cert=%v err=%v", ca, err)
	}
	if err := leaf.CheckSignatureFrom(ca); err != nil {
		t.Fatalf("server certificate is not signed by the pinned CA: %v", err)
	}

	otherHostConfig, err := ServerTLSConfig(credential, "h7654321")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(serverConfig.Certificates[0].Certificate[1], otherHostConfig.Certificates[0].Certificate[1]) {
		t.Fatal("derived CA is shared across Hosts")
	}
	otherCredentialConfig, err := ServerTLSConfig(testCredentialFromByte(0x42), "h1234567")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(serverConfig.Certificates[0].Certificate[1], otherCredentialConfig.Certificates[0].Certificate[1]) {
		t.Fatal("derived CA is shared across credentials")
	}

	// Stable CA/key derivation lets a restarted client pin the same identity.
	restartedServerConfig, err := ServerTLSConfig(credential, "h1234567")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(serverConfig.Certificates[0].Certificate[1], restartedServerConfig.Certificates[0].Certificate[1]) {
		t.Fatal("CA identity is not deterministic for the same Host credential")
	}
	if _, err := restartedServerConfig.Certificates[0].Leaf.Verify(x509.VerifyOptions{Roots: clientConfig.RootCAs, DNSName: ServerName}); err != nil {
		t.Fatalf("client pin does not trust restarted server certificate: %v", err)
	}
}

func TestTLSHandshakeAcceptsOnlyMatchingHostCredential(t *testing.T) {
	credential := testCredential()
	serverConfig, err := ServerTLSConfig(credential, "h1234567")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok\n"))
	}))
	server.TLS = serverConfig
	server.StartTLS()
	defer server.Close()

	validConfig, err := ClientTLSConfig(credential, "h1234567")
	if err != nil {
		t.Fatal(err)
	}
	validConfig.ServerName = ServerName // httptest uses a loopback URL; verify the certificate DNS SAN.
	validClient := newTLSClient(validConfig)
	response, err := validClient.Get(server.URL)
	if err != nil {
		t.Fatalf("matching credential/Host TLS request failed: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("valid TLS response status = %d", response.StatusCode)
	}

	wrongCredential, err := ClientTLSConfig(testCredentialFromByte(0x72), "h1234567")
	if err != nil {
		t.Fatal(err)
	}
	wrongCredential.ServerName = ServerName
	if _, err := newTLSClient(wrongCredential).Get(server.URL); err == nil {
		t.Fatal("TLS accepted a different Host credential")
	}

	wrongHost, err := ClientTLSConfig(credential, "h7654321")
	if err != nil {
		t.Fatal(err)
	}
	wrongHost.ServerName = ServerName
	if _, err := newTLSClient(wrongHost).Get(server.URL); err == nil {
		t.Fatal("TLS accepted a different Host identity")
	}

	wrongName := validConfig.Clone()
	wrongName.ServerName = "not-morde-runtime.internal"
	if _, err := newTLSClient(wrongName).Get(server.URL); err == nil {
		t.Fatal("TLS accepted a server certificate with the wrong DNS name")
	}
}

func TestIdentityRejectsMalformedInput(t *testing.T) {
	if _, err := ServerTLSConfig("short", "h1234567"); err == nil {
		t.Fatal("ServerTLSConfig accepted malformed credential")
	}
	if _, err := ClientTLSConfig(testCredential(), "invalid"); err == nil {
		t.Fatal("ClientTLSConfig accepted malformed Host ID")
	}
}

func TestReadCredentialFileRequiresRootOnlyFile(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to validate root-owned credential file")
	}
	path := filepath.Join(t.TempDir(), "credential")
	credential := testCredential()
	if err := os.WriteFile(path, []byte(credential), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	got, err := ReadCredentialFile(path)
	if err != nil || got != credential {
		t.Fatalf("ReadCredentialFile() = %q, %v", got, err)
	}
	got, err = ReadReplicaCredentialFile(path)
	if err != nil || got != credential {
		t.Fatalf("ReadReplicaCredentialFile() = %q, %v", got, err)
	}
	if err := os.Chmod(path, 0o440); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCredentialFile(path); err == nil {
		t.Fatal("ReadCredentialFile accepted group-readable credential")
	}
	if _, err := ReadReplicaCredentialFile(path); err == nil {
		t.Fatal("ReadReplicaCredentialFile accepted group-readable credential")
	}
}

func newTLSClient(config *tls.Config) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: config, Proxy: nil}}
}

func testCredential() string { return testCredentialFromByte(0x24) }

func testCredentialFromByte(value byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{value}, ed25519.SeedSize))
}
