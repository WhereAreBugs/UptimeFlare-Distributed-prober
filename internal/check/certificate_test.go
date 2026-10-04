package check

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"light-prober/internal/protocol"
)

func certificateServer(t *testing.T, expires time.Time) (*httptest.Server, *x509.CertPool, *atomic.Int32) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-365 * 24 * time.Hour), NotAfter: expires, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"}, BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	requests := &atomic.Int32{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(200) }))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, pool, requests
}

func TestCertificateExpiryVerificationAndNoHTTPRequest(t *testing.T) {
	for _, tc := range []struct {
		name      string
		days      int
		threshold *int
		trust     bool
		code      string
	}{
		{"healthy", 30, nil, true, ""}, {"warning", 3, nil, true, "expiring"}, {"warning-disabled", 3, new(int), true, ""}, {"expired", -1, nil, true, "certificate"}, {"untrusted", 30, nil, false, "certificate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expiry := time.Now().Add(time.Duration(tc.days) * 24 * time.Hour).Truncate(time.Second)
			server, pool, requests := certificateServer(t, expiry)
			checker := New()
			defer checker.Close()
			if tc.trust {
				checker.transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
			}
			result := checker.Check(context.Background(), protocol.Monitor{ID: "cert", Method: "SSL_CERT", Target: server.URL + "/secret/path", CertificateExpiryDays: tc.threshold})
			if tc.code == "" {
				if !result.Up {
					t.Fatalf("SSL failed: %+v", result)
				}
			} else {
				assertFailure(t, result, "tls", tc.code)
			}
			if result.CertificateExpiresAt != expiry.Unix() || result.CertificateDaysRemaining == nil {
				t.Fatalf("missing expiry metadata %+v", result)
			}
			if requests.Load() != 0 {
				t.Fatal("certificate check sent an HTTP request")
			}
		})
	}
}

func TestCertificateTimeoutAndConfiguration(t *testing.T) {
	checker := New()
	defer checker.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	closed := make(chan struct{})
	defer close(closed)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		<-closed
	}()
	result := checker.Check(context.Background(), protocol.Monitor{Method: "SSL_CERT", Target: "https://" + listener.Addr().String(), Timeout: 30})
	assertFailure(t, result, "tls", "timeout")
	for _, target := range []string{"http://localhost", "example.com", "https://user:SECRET@localhost"} {
		assertFailure(t, checker.Check(context.Background(), protocol.Monitor{Method: "SSL_CERT", Target: target}), "configuration", "target")
	}
	threshold := 366
	assertFailure(t, checker.Check(context.Background(), protocol.Monitor{Method: "SSL_CERT", Target: "https://localhost", CertificateExpiryDays: &threshold}), "configuration", "certificate_threshold")
}
