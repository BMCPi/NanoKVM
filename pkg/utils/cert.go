package utils

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"strings"
	"time"
)

func GenerateCert(log *slog.Logger) error {
	var (
		host     = "localhost"
		validFor = time.Hour * 24 * 365 * 10
		certFile = "/etc/kvm/server.crt"
		keyFile  = "/etc/kvm/server.key"
	)

	// Every address a client might reach this BMC on has to be in the SAN
	// set, not just loopback. A certificate naming only localhost fails
	// hostname verification for anyone on the network, which forces every
	// tool — browsers, gofish, inventory scanners — into an
	// insecure-skip-verify mode, and some refuse to proceed at all.
	dnsNames, ipAddress := certSubjectNames(host, log)

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Error("failed to generate RSA private key", slog.Any("err", err))
		return err
	}
	publicKey := &privateKey.PublicKey

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		log.Error("failed to generate serial number", slog.Any("err", err))
		return err
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: host,
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(validFor),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddress,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, publicKey, privateKey)
	if err != nil {
		log.Error("failed to create certificate", slog.Any("err", err))
		return err
	}

	// generate certificate
	certOut, err := os.Create(certFile)
	if err != nil {
		log.Error("failed to create certificate file", slog.String("path", certFile), slog.Any("err", err))
		return err
	}

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		log.Error("failed to encode certificate file", slog.String("path", certFile), slog.Any("err", err))
		return err
	}

	_ = certOut.Sync()
	_ = certOut.Close()
	log.Debug("certificate file generated", slog.String("path", certFile))

	// generate private key
	keyOut, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // 权限 0600
	if err != nil {
		log.Error("failed to create key file", slog.String("path", keyFile), slog.Any("err", err))
		return err
	}

	privateBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		log.Error("failed to marshal private key", slog.Any("err", err))
		return err
	}

	if err := pem.Encode(keyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: privateBytes}); err != nil {
		log.Error("failed to encode key file", slog.String("path", keyFile), slog.Any("err", err))
		return err
	}

	_ = keyOut.Sync()
	_ = keyOut.Close()
	log.Debug("key file generated", slog.String("path", keyFile))

	return nil
}

// certSubjectNames collects the DNS names and IP addresses the server
// certificate should cover: loopback, the device's hostname, and every
// non-loopback address currently configured on its interfaces.
//
// Addresses are a point-in-time snapshot. A BMC that later moves to a
// different DHCP lease will not be covered until the certificate is
// regenerated — the alternative, a cert valid for every possible address,
// does not exist. Link-local addresses are included deliberately: the
// RHI link to the managed host lives there.
func certSubjectNames(host string, log *slog.Logger) ([]string, []net.IP) {
	dnsNames := []string{host}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}

	if name, err := os.Hostname(); err == nil {
		if name = strings.TrimSpace(name); name != "" && name != host {
			dnsNames = append(dnsNames, name)
		}
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		log.Warn("cert: cannot enumerate interface addresses", slog.Any("err", err))
		return dnsNames, ips
	}
	for _, addr := range addrs {
		ipnet, ok := addr.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		ips = append(ips, ipnet.IP)
	}
	return dnsNames, ips
}
