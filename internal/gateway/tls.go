package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cipherlake/internal/config"
	"go.uber.org/zap"
)

type TLSManager struct {
	mu          sync.RWMutex
	config      *config.TLSConfig
	cert        *tls.Certificate
	certModTime time.Time
	keyModTime  time.Time
	server      *http.Server
	redirectSrv *http.Server
	stopCh      chan struct{}
	wg          sync.WaitGroup
	startOnce   sync.Once
	stopOnce    sync.Once
}

func NewTLSManager(cfg *config.TLSConfig) (*TLSManager, error) {
	tm := &TLSManager{
		config: cfg,
		stopCh: make(chan struct{}),
	}

	if cfg.Enabled {
		if cfg.AutoCert {
			if err := tm.generateSelfSignedCert(); err != nil {
				return nil, fmt.Errorf("failed to generate self-signed cert: %w", err)
			}
		} else {
			if cfg.CertFile == "" || cfg.KeyFile == "" {
				return nil, fmt.Errorf("tls.enabled but cert_file and key_file are not set")
			}
			if err := tm.loadCertificate(); err != nil {
				return nil, fmt.Errorf("failed to load TLS certificate: %w", err)
			}
		}
	}

	return tm, nil
}

func (tm *TLSManager) loadCertificate() error {
	cert, err := tls.LoadX509KeyPair(tm.config.CertFile, tm.config.KeyFile)
	if err != nil {
		return fmt.Errorf("failed to load key pair: %w", err)
	}

	certInfo, err := tm.validateCertificate(&cert)
	if err != nil {
		return err
	}

	tm.mu.Lock()
	tm.cert = &cert
	tm.mu.Unlock()

	if certInfo != nil {
		ipStrs := make([]string, len(certInfo.IPAddresses))
		for i, ip := range certInfo.IPAddresses {
			ipStrs[i] = ip.String()
		}
		zap.L().Info("TLS certificate loaded",
			zap.Strings("dns_names", certInfo.DNSNames),
			zap.Strings("ip_addresses", ipStrs),
			zap.Time("not_before", certInfo.NotBefore),
			zap.Time("not_after", certInfo.NotAfter),
		)

		if time.Until(certInfo.NotAfter) < 30*24*time.Hour {
			zap.L().Warn("TLS certificate expires soon",
				zap.Time("not_after", certInfo.NotAfter),
				zap.Duration("remaining", time.Until(certInfo.NotAfter)),
			)
		}
	}

	certMod, keyMod := tm.getFileModTimes()
	tm.certModTime = certMod
	tm.keyModTime = keyMod

	return nil
}

func (tm *TLSManager) validateCertificate(cert *tls.Certificate) (*x509.Certificate, error) {
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("no certificates in key pair")
	}

	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse certificate: %w", err)
	}

	now := time.Now()
	if now.After(x509Cert.NotAfter) {
		return nil, fmt.Errorf("certificate expired on %s", x509Cert.NotAfter.Format(time.RFC3339))
	}
	if now.Before(x509Cert.NotBefore) {
		return nil, fmt.Errorf("certificate not valid until %s", x509Cert.NotBefore.Format(time.RFC3339))
	}

	switch x509Cert.PublicKeyAlgorithm {
	case x509.RSA:
		if pub, ok := x509Cert.PublicKey.(*rsa.PublicKey); ok {
			if pub.N.BitLen() < 2048 {
				return nil, fmt.Errorf("RSA key size %d is below minimum 2048 bits", pub.N.BitLen())
			}
		}
	case x509.ECDSA:
	case x509.Ed25519:
	default:
		return nil, fmt.Errorf("unsupported public key algorithm: %v", x509Cert.PublicKeyAlgorithm)
	}

	if x509Cert.SignatureAlgorithm == x509.UnknownSignatureAlgorithm {
		return nil, fmt.Errorf("unsupported signature algorithm")
	}

	return x509Cert, nil
}

func (tm *TLSManager) getFileModTimes() (certMod, keyMod time.Time) {
	if info, err := os.Stat(tm.config.CertFile); err == nil {
		certMod = info.ModTime()
	}
	if info, err := os.Stat(tm.config.KeyFile); err == nil {
		keyMod = info.ModTime()
	}
	return
}

func (tm *TLSManager) ReloadCertificate() error {
	if tm.config.AutoCert {
		return nil
	}

	certMod, keyMod := tm.getFileModTimes()
	if certMod.Equal(tm.certModTime) && keyMod.Equal(tm.keyModTime) {
		return nil
	}

	zap.L().Info("TLS certificate file changed, reloading...")

	if err := tm.loadCertificate(); err != nil {
		zap.L().Error("Failed to reload TLS certificate", zap.Error(err))
		return err
	}

	zap.L().Info("TLS certificate reloaded successfully")
	return nil
}

func (tm *TLSManager) StartCertWatcher() {
	if tm.config.AutoCert || tm.config.CertFile == "" {
		return
	}

	tm.startOnce.Do(func() {
		tm.wg.Add(1)
		go func() {
			defer tm.wg.Done()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					if err := tm.ReloadCertificate(); err != nil {
						zap.L().Error("Certificate reload failed", zap.Error(err))
					}
				case <-tm.stopCh:
					return
				}
			}
		}()

		zap.L().Info("TLS certificate watcher started", zap.String("cert_file", tm.config.CertFile))
	})
}

func (tm *TLSManager) Stop() {
	tm.stopOnce.Do(func() {
		close(tm.stopCh)
	})

	if tm.redirectSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tm.redirectSrv.Shutdown(ctx)
	}

	tm.wg.Wait()
}

func (tm *TLSManager) GetTLSConfig() *tls.Config {
	if !tm.config.Enabled {
		return nil
	}

	return &tls.Config{
		GetCertificate: tm.getCertificate,
		MinVersion:     tls.VersionTLS12,
		MaxVersion:     tls.VersionTLS13,
		CipherSuites: []uint16{
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		},
		CurvePreferences: []tls.CurveID{
			tls.X25519,
			tls.CurveP256,
			tls.CurveP384,
		},
		PreferServerCipherSuites: true,
		SessionTicketsDisabled:   false,
		SessionTicketKey:         [32]byte{},
		ClientAuth:               tls.NoClientCert,
		NextProtos:               []string{"h2", "http/1.1"},
	}
}

func (tm *TLSManager) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	if tm.cert == nil {
		return nil, fmt.Errorf("no TLS certificate available")
	}

	return tm.cert, nil
}

func (tm *TLSManager) generateSelfSignedCert() error {
	host := tm.config.AutoCertHost
	if host == "" {
		host = "localhost"
	}

	zap.L().Info("Generating self-signed TLS certificate", zap.String("host", host))

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("failed to generate private key: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return fmt.Errorf("failed to generate serial number: %w", err)
	}

	keyUsage := x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"CipherLake S3 Gateway"},
			CommonName:   host,
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              keyUsage,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	hosts := strings.Split(host, ",")
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, h)
		}
	}

	template.IPAddresses = append(template.IPAddresses,
		net.ParseIP("127.0.0.1"),
		net.ParseIP("::1"),
	)
	template.DNSNames = append(template.DNSNames, "localhost")

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("failed to create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	privBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privBytes})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("failed to load generated key pair: %w", err)
	}

	tm.mu.Lock()
	tm.cert = &cert
	tm.mu.Unlock()

	certDir := filepath.Dir(tm.config.CertFile)
	if certDir != "" && certDir != "." {
		os.MkdirAll(certDir, 0700)
	}
	keyDir := filepath.Dir(tm.config.KeyFile)
	if keyDir != "" && keyDir != "." {
		os.MkdirAll(keyDir, 0700)
	}

	if tm.config.CertFile != "" {
		if err := os.WriteFile(tm.config.CertFile, certPEM, 0644); err != nil {
			zap.L().Warn("Failed to write cert file", zap.Error(err))
		}
	}
	if tm.config.KeyFile != "" {
		if err := os.WriteFile(tm.config.KeyFile, keyPEM, 0600); err != nil {
			zap.L().Warn("Failed to write key file", zap.Error(err))
		}
	}

	ipStrs := make([]string, len(template.IPAddresses))
	for i, ip := range template.IPAddresses {
		ipStrs[i] = ip.String()
	}
	zap.L().Info("Self-signed TLS certificate generated",
		zap.Strings("dns_names", template.DNSNames),
		zap.Strings("ip_addresses", ipStrs),
		zap.Time("expires", template.NotAfter),
	)

	return nil
}

func (tm *TLSManager) StartHTTPRedirect(httpsAddr string) error {
	if !tm.config.Enabled {
		return nil
	}

	redirectAddr := tm.config.RedirectAddr
	if redirectAddr == "" {
		redirectAddr = ":80"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		target := "https://" + tm.redirectHost(r.Host) + r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})

	tm.redirectSrv = &http.Server{
		Addr:         redirectAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		zap.L().Info("Starting HTTP→HTTPS redirect server", zap.String("addr", redirectAddr))
		if err := tm.redirectSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			zap.L().Error("HTTP redirect server failed", zap.Error(err))
		}
	}()

	return nil
}

func (tm *TLSManager) redirectHost(host string) string {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		return host
	}
	return h + ":443"
}

func HSTSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
		next.ServeHTTP(w, r)
	})
}

func SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}
