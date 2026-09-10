package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testCertificate(t *testing.T, ip string, expired bool) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	if expired {
		until = time.Now().Add(-time.Hour)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test"}, NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: until, IPAddresses: []net.IP{net.ParseIP(ip)}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestTLSVerification(t *testing.T) {
	for _, tc := range []struct {
		name, ip                  string
		expired, trusted, success bool
	}{
		{"trusted", "127.0.0.1", false, true, true},
		{"untrusted", "127.0.0.1", false, false, false},
		{"wrong hostname", "192.0.2.1", false, true, false},
		{"expired", "127.0.0.1", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert, pemBytes := testCertificate(t, tc.ip, tc.expired)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
			server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
			server.StartTLS()
			defer server.Close()
			ca := ""
			if tc.trusted {
				ca = filepath.Join(t.TempDir(), "ca.pem")
				if err := os.WriteFile(ca, pemBytes, 0600); err != nil {
					t.Fatal(err)
				}
			}
			client, err := secureClient(ca)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			res, err := client.Get(server.URL)
			if res != nil {
				res.Body.Close()
			}
			if (err == nil) != tc.success {
				t.Fatalf("success=%v, error=%v", tc.success, err)
			}
		})
	}
}
func TestRejectRedirect(t *testing.T) {
	cert, pemBytes := testCertificate(t, "127.0.0.1", false)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://127.0.0.1:1/leak", 307) }))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	server.StartTLS()
	defer server.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pemBytes, 0600); err != nil {
		t.Fatal(err)
	}
	client, err := secureClient(ca)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	res, err := client.Get(server.URL)
	if res != nil {
		res.Body.Close()
	}
	if err == nil {
		t.Fatal("accepted redirect")
	}
}
func TestPeerURL(t *testing.T) {
	for input, want := range map[string]string{"192.168.1.20": "https://192.168.1.20:3737", "[::1]": "https://[::1]:3737", "https://localhost:8443/": "https://localhost:8443"} {
		got, err := peerURL(input)
		if err != nil || got != want {
			t.Fatalf("%s: got %s, %v", input, got, err)
		}
	}
	for _, input := range []string{"http://localhost:3737", "https://user:pass@localhost", "https://localhost/api", "https://localhost?secret=1", "https://localhost#fragment"} {
		if _, err := peerURL(input); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}
