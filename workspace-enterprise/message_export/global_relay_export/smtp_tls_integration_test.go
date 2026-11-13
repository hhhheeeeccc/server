package global_relay_export

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

type smtpDelivery struct {
	body     string
	identity string
	err      error
}

func TestDeliverAuthenticatesAfterSTARTTLS(t *testing.T) {
	certificate, rootPEM := localSMTPCertificate(t)
	rootPath := filepath.Join(t.TempDir(), "smtp-root.pem")
	require.NoError(t, os.WriteFile(rootPath, rootPEM, 0600))
	t.Setenv("SSL_CERT_FILE", rootPath)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	delivery := make(chan smtpDelivery, 1)
	go serveTLSSMTP(listener, certificate, delivery)

	config := &model.Config{}
	config.SetDefaults()
	settings := config.MessageExportSettings.GlobalRelaySettings
	*settings.CustomerType = model.GlobalrelayCustomerTypeCustom
	*settings.CustomSMTPServerName = "localhost"
	*settings.CustomSMTPPort = strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	*settings.SMTPUsername = "archive-user"
	*settings.SMTPPassword = "archive-secret"
	*settings.EmailAddress = "archive@example.test"
	deliveryErr := Deliver(makeTestMailArchive(t), config)
	if deliveryErr != nil {
		select {
		case result := <-delivery:
			t.Logf("SMTP test server error: %v", result.err)
		case <-time.After(time.Second):
			t.Log("SMTP test server did not exit after client error")
		}
	}
	require.NoError(t, deliveryErr)
	select {
	case result := <-delivery:
		require.NoError(t, result.err)
		require.Equal(t, "\x00archive-user\x00archive-secret", result.identity)
		require.Contains(t, result.body, "workspace global relay SMTP marker")
	case <-time.After(5 * time.Second):
		t.Fatal("TLS SMTP server did not receive the message")
	}
}

func localSMTPCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	now := time.Now()
	root := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, rootKey.Public(), rootKey)
	require.NoError(t, err)
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	server := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		DNSNames:     []string{"localhost"},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, server, root, serverKey.Public(), rootKey)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{serverDER, rootDER}, PrivateKey: serverKey}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
}

func serveTLSSMTP(listener net.Listener, certificate tls.Certificate, delivered chan<- smtpDelivery) {
	result := smtpDelivery{}
	defer func() { delivered <- result }()
	connection, err := listener.Accept()
	if err != nil {
		result.err = err
		return
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(connection)
	writer := bufio.NewWriter(connection)
	respond := func(text string) error {
		if _, err := writer.WriteString(text + "\r\n"); err != nil {
			return err
		}
		return writer.Flush()
	}
	if result.err = respond("220 local SMTP ready"); result.err != nil {
		return
	}
	encrypted := false
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			if result.body == "" {
				result.err = readErr
			}
			return
		}
		command := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(command, "EHLO "), strings.HasPrefix(command, "HELO "):
			if encrypted {
				result.err = respond("250-localhost\r\n250 AUTH PLAIN")
			} else {
				result.err = respond("250-localhost\r\n250-STARTTLS\r\n250 AUTH PLAIN")
			}
		case command == "STARTTLS":
			if result.err = respond("220 begin TLS"); result.err != nil {
				return
			}
			secured := tls.Server(connection, &tls.Config{Certificates: []tls.Certificate{certificate}})
			if result.err = secured.Handshake(); result.err != nil {
				return
			}
			connection = secured
			reader = bufio.NewReader(connection)
			writer = bufio.NewWriter(connection)
			encrypted = true
		case strings.HasPrefix(command, "AUTH PLAIN "):
			if !encrypted {
				result.err = fmt.Errorf("credentials sent before TLS")
				return
			}
			identity, decodeErr := base64.StdEncoding.DecodeString(strings.TrimPrefix(command, "AUTH PLAIN "))
			if decodeErr != nil {
				result.err = decodeErr
				return
			}
			result.identity = string(identity)
			result.err = respond("235 authenticated")
		case strings.HasPrefix(command, "MAIL FROM:"), strings.HasPrefix(command, "RCPT TO:"):
			if !encrypted {
				result.err = fmt.Errorf("mail sent before TLS")
				return
			}
			result.err = respond("250 accepted")
		case command == "DATA":
			if result.err = respond("354 send message"); result.err != nil {
				return
			}
			var body strings.Builder
			for {
				dataLine, dataErr := reader.ReadString('\n')
				if dataErr != nil {
					result.err = dataErr
					return
				}
				if dataLine == ".\r\n" {
					break
				}
				body.WriteString(dataLine)
			}
			result.body = body.String()
			result.err = respond("250 queued")
		case command == "QUIT":
			result.err = respond("221 bye")
			return
		default:
			result.err = fmt.Errorf("unexpected SMTP command %q", command)
			return
		}
		if result.err != nil {
			return
		}
	}
}
