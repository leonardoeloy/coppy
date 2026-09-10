package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func peerURL(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("peer must be an HTTPS server IP[:port] or origin URL; plaintext HTTP is not supported")
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), "3737")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// Set by the Go build command for server-specific downloads. Public CA only.
var embeddedCA string

func secureClient(caPath string) (*http.Client, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if caPath != "" || embeddedCA != "" {
		var pem []byte
		var err error
		if caPath != "" {
			pem, err = os.ReadFile(caPath)
		} else {
			pem, err = base64.StdEncoding.DecodeString(embeddedCA)
		}
		if err != nil {
			return nil, err
		}
		// Explicit --ca restricts trust to this supplied CA rather than adding it to
		// every system root. Standard certificate, hostname and expiry checks remain.
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA file contains no certificates")
		}
		config.RootCAs = roots
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = config
	return &http.Client{Transport: transport, Timeout: 30 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		// Sync never needs redirects; rejecting them prevents downgrade or sending
		// file bytes to a different server, including one signed by the same CA.
		return fmt.Errorf("server redirects are not supported")
	}}, nil
}
