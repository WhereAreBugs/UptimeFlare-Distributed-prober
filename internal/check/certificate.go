package check

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/url"
	"time"

	"light-prober/internal/protocol"
)

func (c *Checker) checkCertificate(ctx context.Context, monitor protocol.Monitor, result protocol.Result) protocol.Result {
	u, err := url.Parse(monitor.Target)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return failed(result, "configuration", "target", "Certificate checks require an HTTPS URL without credentials")
	}
	threshold := protocol.DefaultCertificateExpiryDays
	if monitor.CertificateExpiryDays != nil {
		threshold = *monitor.CertificateExpiryDays
	}
	if threshold < 0 || threshold > 365 {
		return failed(result, "configuration", "certificate_threshold", "Certificate expiry threshold must be between 0 and 365 days")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	connection, err := c.dial(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return failureFor(result, "tcp", err)
	}
	defer connection.Close()
	config := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	if c.transport.TLSClientConfig != nil {
		config = c.transport.TLSClientConfig.Clone()
		config.ServerName, config.InsecureSkipVerify = u.Hostname(), false
	}
	secure := tls.Client(connection, config)
	if err := secure.HandshakeContext(ctx); err != nil {
		var verification *tls.CertificateVerificationError
		if errors.As(err, &verification) && len(verification.UnverifiedCertificates) > 0 {
			setCertificateExpiry(&result, verification.UnverifiedCertificates[0].NotAfter)
		}
		return failureFor(result, "tls", err)
	}
	certificates := secure.ConnectionState().PeerCertificates
	if len(certificates) == 0 {
		return failed(result, "tls", "certificate", "TLS peer did not provide a certificate")
	}
	setCertificateExpiry(&result, certificates[0].NotAfter)
	if !certificates[0].NotAfter.After(time.Now().Add(time.Duration(threshold) * 24 * time.Hour)) {
		return failed(result, "tls", "expiring", "TLS certificate expires within the configured warning window")
	}
	result.Up = true
	return result
}

func setCertificateExpiry(result *protocol.Result, expiry time.Time) {
	if expiry.Unix() <= 0 {
		return
	}
	remaining := time.Until(expiry).Hours() / 24
	result.CertificateExpiresAt, result.CertificateDaysRemaining = expiry.Unix(), &remaining
}
