package server

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certCheckInterval bounds how often the files are stat'ed. Handshakes are
// rare, nodes hold one connection each, so this is about not hitting the disk
// on a reconnect storm rather than about latency.
const certCheckInterval = 30 * time.Second

// CertReloader serves a certificate from files and picks up a renewed one
// without a restart. cert-manager and certbot both replace the files in place,
// and a restart would drop every node's Watch for nothing.
type CertReloader struct {
	certFile, keyFile string

	mu        sync.Mutex
	cert      *tls.Certificate
	modTime   time.Time
	checkedAt time.Time
}

// NewCertReloader loads the pair once and fails if it cannot.
func NewCertReloader(certFile, keyFile string) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// TLSConfig returns a server config backed by the reloader.
func (r *CertReloader) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: r.GetCertificate,
	}
}

// GetCertificate returns the current certificate, reloading it if either file
// changed. A failed reload keeps serving the old one: a half-written renewal
// must not take the service down.
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if time.Since(r.checkedAt) >= certCheckInterval {
		r.checkedAt = time.Now()
		if mt, err := r.latestModTime(); err == nil && mt.After(r.modTime) {
			if err := r.loadLocked(); err != nil {
				slog.Warn("certificate reload failed, keeping the old one", "error", err)
			} else {
				slog.Info("certificate reloaded", "cert", r.certFile)
			}
		}
	}
	return r.cert, nil
}

func (r *CertReloader) load() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadLocked()
}

func (r *CertReloader) loadLocked() error {
	mt, err := r.latestModTime()
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load certificate: %w", err)
	}
	r.cert = &cert
	r.modTime = mt
	r.checkedAt = time.Now()
	return nil
}

func (r *CertReloader) latestModTime() (time.Time, error) {
	var latest time.Time
	for _, f := range []string{r.certFile, r.keyFile} {
		fi, err := os.Stat(f)
		if err != nil {
			return time.Time{}, fmt.Errorf("stat %s: %w", f, err)
		}
		if fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest, nil
}
