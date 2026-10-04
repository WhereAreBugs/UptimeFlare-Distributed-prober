package spool

import (
	"light-prober/internal/protocol"
	"path/filepath"
	"testing"
)

func TestCertificateAndICMPMetadataSurviveRestartAndExactAck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	q, err := Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	days, rtt := 0.0, 0.0
	result := protocol.Result{MonitorID: "cert", Time: 1800000000, CertificateExpiresAt: 1800000001, CertificateDaysRemaining: &days, ICMPLatencyMS: &rtt, Stage: "tls", Code: "expiring"}
	if err = q.Append(result); err != nil {
		t.Fatal(err)
	}
	if err = q.Close(); err != nil {
		t.Fatal(err)
	}
	q, err = Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	entries, err := q.Peek(1)
	if err != nil || len(entries) != 1 {
		t.Fatal(err)
	}
	v := entries[0].Result
	if v.CertificateDaysRemaining == nil || *v.CertificateDaysRemaining != 0 || v.ICMPLatencyMS == nil || *v.ICMPLatencyMS != 0 || v.CertificateExpiresAt != result.CertificateExpiresAt {
		t.Fatalf("optional zero metadata lost %+v", v)
	}
	if err = q.Ack(entries); err != nil {
		t.Fatal(err)
	}
}
