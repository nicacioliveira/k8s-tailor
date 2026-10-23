package webhook

import (
	"crypto/tls"
	"log"
	"os"
	"sync"
	"time"
)

// CertReloader serves the webhook's TLS key pair from disk and re-reads it when
// the files change. The Secret volume is updated in place by the kubelet when
// cert-manager (or anyone) rotates the certificate; without a reload the engine
// would keep serving the old cert until restarted — and fail every admission
// handshake once it expires or the caBundle moves to the new CA.
type CertReloader struct {
	certFile, keyFile string
	minInterval       time.Duration // how often the files are stat'ed at most

	mu        sync.Mutex
	cert      *tls.Certificate
	modCrt    time.Time
	modKey    time.Time
	lastCheck time.Time
}

// NewCertReloader loads the pair once (error if unreadable/invalid).
func NewCertReloader(certFile, keyFile string) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile, minInterval: 10 * time.Second}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *CertReloader) load() error {
	ci, err := os.Stat(r.certFile)
	if err != nil {
		return err
	}
	ki, err := os.Stat(r.keyFile)
	if err != nil {
		return err
	}
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return err
	}
	r.cert, r.modCrt, r.modKey = &c, ci.ModTime(), ki.ModTime()
	return nil
}

// GetCertificate implements tls.Config.GetCertificate. A failed reload (e.g. a
// half-written pair mid-rotation) keeps serving the last good certificate.
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if now.Sub(r.lastCheck) >= r.minInterval {
		r.lastCheck = now
		ci, err1 := os.Stat(r.certFile)
		ki, err2 := os.Stat(r.keyFile)
		if err1 == nil && err2 == nil && (!ci.ModTime().Equal(r.modCrt) || !ki.ModTime().Equal(r.modKey)) {
			prev := r.cert
			if err := r.load(); err != nil {
				log.Printf("webhook cert reload failed (serving previous): %v", err)
				r.cert = prev
			} else {
				log.Printf("webhook cert reloaded from %s", r.certFile)
			}
		}
	}
	return r.cert, nil
}
