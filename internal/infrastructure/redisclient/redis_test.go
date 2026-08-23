package redisclient

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LDouble/campus-academic/internal/core/bootstrap"
)

func TestOptionsCarriesACLAndTLSConfiguration(t *testing.T) {
	root := t.TempDir()
	writeTestCA(t, filepath.Join(root, "ca.pem"))
	config := bootstrap.RedisConfig{
		Address:      "provider-redis.internal:6380",
		Username:     "provider",
		Password:     "secret",
		DB:           2,
		TLS:          true,
		TLSFilesRoot: root,
		CAFile:       "ca.pem",
		ServerName:   "provider-redis.internal",
	}

	options, err := Options(config)
	if err != nil {
		t.Fatalf("Options() error = %v", err)
	}
	if options.Addr != config.Address || options.Username != config.Username ||
		options.Password != config.Password || options.DB != config.DB {
		t.Fatalf("Options() = %+v", options)
	}
	if options.TLSConfig == nil || options.TLSConfig.ServerName != config.ServerName ||
		options.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS config = %+v", options.TLSConfig)
	}

	asynqOptions, err := AsynqOptions(config)
	if err != nil {
		t.Fatalf("AsynqOptions() error = %v", err)
	}
	if asynqOptions.Addr != config.Address || asynqOptions.Username != config.Username ||
		asynqOptions.Password != config.Password || asynqOptions.DB != config.DB ||
		asynqOptions.TLSConfig == nil || asynqOptions.TLSConfig.ServerName != config.ServerName {
		t.Fatalf("AsynqOptions() = %+v", asynqOptions)
	}
}

func TestTLSConfigRequiresConfinedValidCA(t *testing.T) {
	config := bootstrap.RedisConfig{TLS: true, TLSFilesRoot: "relative", CAFile: "ca.pem", ServerName: "redis.internal"}
	if _, err := TLSConfig(config); err == nil {
		t.Fatal("TLSConfig() accepted relative root")
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ca.pem"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write invalid CA: %v", err)
	}
	config.TLSFilesRoot = root
	if _, err := TLSConfig(config); err == nil {
		t.Fatal("TLSConfig() accepted invalid CA")
	}
}

func writeTestCA(t *testing.T, path string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "redis-test-ca"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write CA: %v", err)
	}
}
