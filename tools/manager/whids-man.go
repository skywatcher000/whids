package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/0xrawsec/golang-utils/log"
	"github.com/0xrawsec/whids/collector"
)

const (
	copyright = "WHIDS Copyright (C) 2017 RawSec SARL (@0xrawsec)"
	license   = `License Apache 2.0: This program comes with ABSOLUTELY NO WARRANTY.`
)

var (
	keygen     bool
	certgen    bool
	dumpConfig bool

	managerConf collector.ManagerConfig
	manager     *collector.Manager
	osSignals   = make(chan os.Signal)

	// Used for certificate generation
	defaultOrg          = "WHIDS Manager"
	defaultCertValidity = time.Hour * 24 * 365
)

/////////////////////////// generate_cert.go ///////////////////////////////////
func publicKey(priv interface{}) interface{} {
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		return &k.PublicKey
	case *ecdsa.PrivateKey:
		return &k.PublicKey
	default:
		return nil
	}
}

func pemBlockForKey(priv interface{}) *pem.Block {
	switch k := priv.(type) {
	case *rsa.PrivateKey:
		return &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}
	case *ecdsa.PrivateKey:
		b, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Unable to marshal ECDSA private key: %v", err)
			os.Exit(2)
		}
		return &pem.Block{Type: "EC PRIVATE KEY", Bytes: b}
	default:
		return nil
	}
}

func generateCert(hosts []string) error {
	if len(hosts) == 0 {
		return fmt.Errorf("Missing required --host parameter")
	}

	var priv interface{}
	var err error

	// generate RSA key
	priv, err = rsa.GenerateKey(rand.Reader, 4096)

	if err != nil {
		return fmt.Errorf("failed to generate private key: %s", err)
	}

	notBefore := time.Now()
	notAfter := notBefore.Add(defaultCertValidity)

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)

	if err != nil {
		return fmt.Errorf("failed to generate serial number: %s", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{defaultOrg},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, h)
		}
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, publicKey(priv), priv)

	if err != nil {
		return fmt.Errorf("Failed to create certificate: %s", err)
	}

	certOut, err := os.OpenFile("cert.pem", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to open cert.pem for writing: %s", err)
	}

	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	certOut.Close()

	log.Info("Written cert.pem")

	keyOut, err := os.OpenFile("key.pem", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)

	if err != nil {
		return fmt.Errorf("failed to open key.pem for writing: %s", err)
	}

	pem.Encode(keyOut, pemBlockForKey(priv))

	keyOut.Close()

	log.Info("Written key.pem")
	return nil
}

func printInfo(writer io.Writer) {
	fmt.Fprintf(writer, "Version: %s (commit: %s)\nCopyright: %s\nLicense: %s\n\n", version, commitID, copyright, license)
}

func main() {

	flag.BoolVar(&keygen, "key", keygen, "Generate a random client API key. Both client and manager configuration file will needs to be updated with it.")
	flag.BoolVar(&certgen, "certgen", certgen, "Generate a couple (key and cert) to be used for TLS connections."+
		"The certificate gets generated for the IP address specified in the configuration file.")
	flag.BoolVar(&dumpConfig, "dump-config", dumpConfig, "Dumps a skeleton of manager configuration")

	flag.Usage = func() {
		printInfo(os.Stderr)
		fmt.Fprintf(os.Stderr, "Usage: %s [OPTIONS] CONFIG_FILE\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}

	flag.Parse()

	config := flag.Arg(0)

	if keygen {
		key := collector.KeyGen(collector.DefaultKeySize)
		fmt.Printf("New API key: %s\n", key)
		fmt.Printf("Please manually update client and manager configuration file to make it effective\n")
		os.Exit(0)
	}

	if dumpConfig {
		b, err := json.MarshalIndent(collector.ManagerConfig{}, "", "    ")
		if err != nil {
			panic(err)
		}
		fmt.Println(string(b))
		os.Exit(0)
	}

	fd, err := os.Open(config)
	if err != nil {
		log.LogErrorAndExit(fmt.Errorf("Failed to open configuration file: %s", err))
	}

	b, err := ioutil.ReadAll(fd)
	if err != nil {
		log.LogErrorAndExit(fmt.Errorf("Failed to read configuration file: %s", err))
	}
	err = json.Unmarshal(b, &managerConf)
	if err != nil {
		log.LogErrorAndExit(fmt.Errorf("Failed to parse configuration data: %s", err))
	}
	// Closing configuration file
	fd.Close()

	if certgen {
		err = generateCert([]string{managerConf.Host})
		if err != nil {
			log.LogErrorAndExit(fmt.Errorf("Failed to generate key/cert pair: %s", err))
		}
		log.Infof("Certificate and key generated should be used for testing purposes only.")
		os.Exit(0)
	}

	manager, err = collector.NewManager(&managerConf)
	if err != nil {
		log.LogErrorAndExit(fmt.Errorf("Failed to create manager: %s", err))
	}

	// Registering signal handler for sig interrupt
	signal.Notify(osSignals, os.Interrupt)
	go func() {
		<-osSignals
		log.Infof("Received SIGINT, shutting the manager down properly")
		manager.Shutdown()
	}()
	// Running the manager
	manager.Run()
	manager.Wait()
}
