package rpc

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/LDouble/campus-academic/internal/core/bootstrap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// ClientDialOptions builds transport credentials for the private provider channel.
func ClientDialOptions(config bootstrap.ProviderConfig) ([]grpc.DialOption, error) {
	return clientDialOptions(
		"academic provider",
		config.Insecure,
		config.TLSFilesRoot,
		config.CAFile,
		config.ClientCertFile,
		config.ClientKeyFile,
		config.ServerName,
	)
}

func clientDialOptions(
	serviceName string,
	insecureTransport bool,
	tlsFilesRoot,
	caFile,
	clientCertFile,
	clientKeyFile,
	serverName string,
) ([]grpc.DialOption, error) {
	if insecureTransport {
		return []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithChainUnaryInterceptor(requestIDClientInterceptor),
		}, nil
	}
	ca, certificate, err := loadTLSMaterial(
		serviceName,
		tlsFilesRoot,
		caFile,
		clientCertFile,
		clientKeyFile,
	)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      ca,
		Certificates: []tls.Certificate{certificate},
		ServerName:   serverName,
	}
	return []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithChainUnaryInterceptor(requestIDClientInterceptor),
	}, nil
}

// ServerOptions builds mutual-TLS credentials for the private provider listener.
func ServerOptions(config bootstrap.ProviderConfig) ([]grpc.ServerOption, error) {
	return serverOptions(
		"academic provider",
		config.Insecure,
		config.TLSFilesRoot,
		config.CAFile,
		config.ServerCertFile,
		config.ServerKeyFile,
	)
}

// AnalyticsServerOptions builds mutual-TLS credentials for the Analytics
// listener while keeping the transport implementation shared.
func AnalyticsServerOptions(config bootstrap.AnalyticsConfig) ([]grpc.ServerOption, error) {
	return serverOptions(
		"academic analytics",
		config.Insecure,
		config.TLSFilesRoot,
		config.CAFile,
		config.ServerCertFile,
		config.ServerKeyFile,
	)
}

func serverOptions(
	serviceName string,
	insecureTransport bool,
	tlsFilesRoot,
	caFile,
	serverCertFile,
	serverKeyFile string,
) ([]grpc.ServerOption, error) {
	if insecureTransport {
		return []grpc.ServerOption{grpc.ChainUnaryInterceptor(requestIDServerInterceptor)}, nil
	}
	ca, certificate, err := loadTLSMaterial(
		serviceName,
		tlsFilesRoot,
		caFile,
		serverCertFile,
		serverKeyFile,
	)
	if err != nil {
		return nil, err
	}
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca,
		Certificates: []tls.Certificate{certificate},
	}
	return []grpc.ServerOption{
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		grpc.ChainUnaryInterceptor(requestIDServerInterceptor),
	}, nil
}

func loadTLSMaterial(
	serviceName string,
	rootPath string,
	caFile string,
	certFile string,
	keyFile string,
) (*x509.CertPool, tls.Certificate, error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("open %s TLS root: %w", serviceName, err)
	}
	defer func() { _ = root.Close() }()
	caData, err := root.ReadFile(caFile)
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("read %s CA: %w", serviceName, err)
	}
	certData, err := root.ReadFile(certFile)
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("read %s certificate: %w", serviceName, err)
	}
	keyData, err := root.ReadFile(keyFile)
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("read %s key: %w", serviceName, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caData) {
		return nil, tls.Certificate{}, fmt.Errorf("parse %s CA", serviceName)
	}
	certificate, err := tls.X509KeyPair(certData, keyData)
	if err != nil {
		return nil, tls.Certificate{}, fmt.Errorf("parse %s key pair: %w", serviceName, err)
	}
	return pool, certificate, nil
}
