package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func ensureTLS(dir string) (tls.Certificate, []byte, error) {
	certPath, keyPath := filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem")
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err = os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
			return tls.Certificate{}, nil, err
		}
		stage, err := os.MkdirTemp(filepath.Dir(dir), ".coppy-tls-")
		if err != nil {
			return tls.Certificate{}, nil, err
		}
		defer os.RemoveAll(stage)
		if err = generateTLS(stage); err != nil {
			return tls.Certificate{}, nil, err
		}
		if err = os.Rename(stage, dir); err != nil {
			return tls.Certificate{}, nil, err
		}
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return cert, nil, fmt.Errorf("load TLS identity in %s: %w (existing identities are never overwritten)", dir, err)
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return cert, nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return cert, nil, fmt.Errorf("invalid public CA")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return cert, nil, err
	}
	intermediates := x509.NewCertPool()
	for _, der := range cert.Certificate[1:] {
		c, e := x509.ParseCertificate(der)
		if e != nil {
			return cert, nil, e
		}
		intermediates.AddCert(c)
	}
	_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates})
	return cert, ca, err
}
func generateTLS(dir string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial := func() *big.Int {
		n, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if e != nil {
			panic(e)
		}
		return n
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "Coppy local CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		return err
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	leaf := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "Coppy server"}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0), DNSNames: []string{"localhost", hostname}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ip, _, e := net.ParseCIDR(a.String()); e == nil {
			leaf.IPAddresses = append(leaf.IPAddresses, ip)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &serverKey.PublicKey, key)
	if err != nil {
		return err
	}
	for name, k := range map[string]*ecdsa.PrivateKey{"ca-key.pem": key, "server-key.pem": serverKey} {
		b, e := x509.MarshalPKCS8PrivateKey(k)
		if e != nil {
			return e
		}
		if e = os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b}), 0600); e != nil {
			return e
		}
	}
	for name, b := range map[string][]byte{"ca.pem": caDER, "server.pem": der} {
		if e := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b}), 0644); e != nil {
			return e
		}
	}
	return nil
}
func fingerprint(ca []byte) string {
	block, _ := pem.Decode(ca)
	if block == nil {
		return ""
	}
	h := sha256.Sum256(block.Bytes)
	s := strings.ToUpper(hex.EncodeToString(h[:]))
	parts := []string{}
	for i := 0; i < len(s); i += 2 {
		parts = append(parts, s[i:i+2])
	}
	return strings.Join(parts, ":")
}
