package webhook

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSelfSigned(t *testing.T, dir, cn string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{cn}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	// Write to temp names then rename, like the kubelet's atomic secret update.
	crt := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	k := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	for name, b := range map[string][]byte{"tls.crt": crt, "tls.key": k} {
		tmp := filepath.Join(dir, "."+name)
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

// cert-manager renews the webhook cert in place (the Secret volume updates
// under the running pod). The served certificate must follow without a restart.
func TestCertReloaderPicksUpRotation(t *testing.T) {
	dir := t.TempDir()
	writeSelfSigned(t, dir, "first")
	r, err := NewCertReloader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	r.minInterval = 0
	cn := func() string {
		c, err := r.GetCertificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		x, _ := x509.ParseCertificate(c.Certificate[0])
		return x.Subject.CommonName
	}
	if got := cn(); got != "first" {
		t.Fatalf("cn = %q", got)
	}
	time.Sleep(10 * time.Millisecond) // distinct mtime
	writeSelfSigned(t, dir, "second")
	if got := cn(); got != "second" {
		t.Fatalf("after rotation cn = %q, want second", got)
	}
	// A broken write keeps serving the last good pair.
	_ = os.WriteFile(filepath.Join(dir, "tls.crt"), []byte("garbage"), 0o600)
	if got := cn(); got != "second" {
		t.Fatalf("after bad write cn = %q, want last good", got)
	}
}

func TestCertReloaderMissingFile(t *testing.T) {
	if _, err := NewCertReloader("/nonexistent/tls.crt", "/nonexistent/tls.key"); err == nil {
		t.Fatal("want error for missing cert")
	}
}
