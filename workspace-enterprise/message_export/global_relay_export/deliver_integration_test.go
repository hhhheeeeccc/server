package global_relay_export

import (
	"archive/zip"
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestDeliverToLocalSMTP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	t.Setenv("CI_INBUCKET_HOST", "127.0.0.1")
	t.Setenv("CI_INBUCKET_SMTP_PORT", port)

	delivered := make(chan string, 1)
	serverErrors := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErrors <- acceptErr
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(connection)
		writer := bufio.NewWriter(connection)
		respond := func(code string) error {
			if _, writeErr := writer.WriteString(code + "\r\n"); writeErr != nil {
				return writeErr
			}
			return writer.Flush()
		}
		if respond("220 local SMTP ready") != nil {
			return
		}
		messageAccepted := false
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				if readErr == io.EOF && messageAccepted {
					return
				}
				serverErrors <- readErr
				return
			}
			var responseErr error
			switch {
			case strings.HasPrefix(line, "EHLO "), strings.HasPrefix(line, "HELO "):
				responseErr = respond("250 localhost")
			case strings.HasPrefix(line, "STARTTLS"):
				responseErr = respond("454 TLS unavailable in local test")
			case strings.HasPrefix(line, "MAIL FROM:"), strings.HasPrefix(line, "RCPT TO:"):
				responseErr = respond("250 accepted")
			case strings.HasPrefix(line, "DATA"):
				if responseErr = respond("354 send message"); responseErr != nil {
					break
				}
				var body strings.Builder
				for {
					dataLine, dataErr := reader.ReadString('\n')
					if dataErr != nil {
						serverErrors <- dataErr
						return
					}
					if dataLine == ".\r\n" {
						break
					}
					body.WriteString(dataLine)
				}
				responseErr = respond("250 queued")
				if responseErr == nil {
					messageAccepted = true
					delivered <- body.String()
				}
			case strings.HasPrefix(line, "QUIT"):
				_ = respond("221 bye")
				return
			default:
				serverErrors <- fmt.Errorf("unexpected SMTP command %q", strings.TrimSpace(line))
				return
			}
			if responseErr != nil {
				serverErrors <- responseErr
				return
			}
		}
	}()

	exportFile := makeTestMailArchive(t)
	config := &model.Config{}
	config.SetDefaults()
	*config.MessageExportSettings.GlobalRelaySettings.CustomerType = "INBUCKET"
	*config.MessageExportSettings.GlobalRelaySettings.EmailAddress = "archive@example.test"
	deliveryErr := Deliver(exportFile, config)
	require.NoError(t, deliveryErr)
	select {
	case body := <-delivered:
		require.Contains(t, body, "workspace global relay SMTP marker")
	case serverErr := <-serverErrors:
		require.NoError(t, serverErr)
	case <-time.After(5 * time.Second):
		t.Fatal("local SMTP server did not receive the message")
	}
}

func TestDeliverRejectsPlaintextSMTP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	commands := make(chan string, 8)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(connection)
		writer := bufio.NewWriter(connection)
		_, _ = writer.WriteString("220 local SMTP ready\r\n")
		_ = writer.Flush()
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			command := strings.TrimSpace(line)
			commands <- command
			if strings.HasPrefix(command, "EHLO ") || strings.HasPrefix(command, "HELO ") {
				_, _ = writer.WriteString("250 localhost\r\n")
			} else if command == "STARTTLS" {
				_, _ = writer.WriteString("454 TLS unavailable\r\n")
			} else {
				_, _ = writer.WriteString("250 accepted\r\n")
			}
			_ = writer.Flush()
		}
	}()

	config := &model.Config{}
	config.SetDefaults()
	settings := config.MessageExportSettings.GlobalRelaySettings
	*settings.CustomerType = model.GlobalrelayCustomerTypeCustom
	*settings.CustomSMTPServerName = "127.0.0.1"
	*settings.CustomSMTPPort = strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	*settings.SMTPUsername = "archive-user"
	*settings.SMTPPassword = "archive-secret"
	*settings.EmailAddress = "archive@example.test"
	err = Deliver(makeTestMailArchive(t), config)
	require.ErrorContains(t, err, "STARTTLS")
	select {
	case <-serverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("SMTP test server did not close")
	}
	close(commands)
	for command := range commands {
		require.NotContains(t, command, "MAIL FROM:")
		require.NotContains(t, command, "AUTH ")
	}
}

func TestDeliverGreetingUsesGlobalRelayTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	t.Setenv("CI_INBUCKET_HOST", "127.0.0.1")
	t.Setenv("CI_INBUCKET_SMTP_PORT", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.Copy(io.Discard, connection)
	}()

	config := &model.Config{}
	config.SetDefaults()
	*config.MessageExportSettings.GlobalRelaySettings.CustomerType = "INBUCKET"
	*config.MessageExportSettings.GlobalRelaySettings.EmailAddress = "archive@example.test"
	*config.MessageExportSettings.GlobalRelaySettings.SMTPServerTimeout = 1
	*config.EmailSettings.SMTPServerTimeout = 10
	started := time.Now()
	err = Deliver(makeTestMailArchive(t), config)
	require.Error(t, err)
	require.Less(t, time.Since(started), 4*time.Second)
	select {
	case <-serverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out SMTP connection remained open")
	}
}

func TestDeliverClosesRotatedConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	t.Setenv("CI_INBUCKET_HOST", "127.0.0.1")
	t.Setenv("CI_INBUCKET_SMTP_PORT", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	type connectionResult struct {
		mails int
		err   error
	}
	results := make(chan connectionResult, 2)
	for range 2 {
		go func() {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				results <- connectionResult{err: acceptErr}
				return
			}
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
			reader := bufio.NewReader(connection)
			writer := bufio.NewWriter(connection)
			respond := func(response string) error {
				_, writeErr := writer.WriteString(response + "\r\n")
				if writeErr != nil {
					return writeErr
				}
				return writer.Flush()
			}
			result := connectionResult{}
			if err := respond("220 local SMTP ready"); err != nil {
				result.err = err
				results <- result
				return
			}
			for {
				line, readErr := reader.ReadString('\n')
				if readErr == io.EOF {
					results <- result
					return
				}
				if readErr != nil {
					result.err = readErr
					results <- result
					return
				}
				var response string
				switch {
				case strings.HasPrefix(line, "EHLO "), strings.HasPrefix(line, "HELO "):
					response = "250 localhost"
				case strings.HasPrefix(line, "MAIL FROM:"), strings.HasPrefix(line, "RCPT TO:"):
					response = "250 accepted"
				case strings.HasPrefix(line, "DATA"):
					response = "354 send message"
				case line == ".\r\n":
					result.mails++
					response = "250 queued"
				case strings.HasPrefix(line, "QUIT"):
					_ = respond("221 bye")
					results <- result
					return
				default:
					continue // Message data.
				}
				if err := respond(response); err != nil {
					result.err = err
					results <- result
					return
				}
			}
		}()
	}

	archive := makeTestMailArchiveWithCount(t, MaxEmailsPerConnection+1)
	config := &model.Config{}
	config.SetDefaults()
	*config.MessageExportSettings.GlobalRelaySettings.CustomerType = "INBUCKET"
	*config.MessageExportSettings.GlobalRelaySettings.EmailAddress = "archive@example.test"
	require.NoError(t, Deliver(archive, config))
	counts := []int{}
	for range 2 {
		select {
		case result := <-results:
			require.NoError(t, result.err)
			counts = append(counts, result.mails)
		case <-time.After(5 * time.Second):
			t.Fatal("SMTP connection remained open after delivery")
		}
	}
	require.ElementsMatch(t, []int{MaxEmailsPerConnection, 1}, counts)
}

func makeTestMailArchive(t *testing.T) *os.File {
	return makeTestMailArchiveWithCount(t, 1)
}

func makeTestMailArchiveWithCount(t *testing.T, count int) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "global-relay-*.zip")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	archive := zip.NewWriter(file)
	for i := range count {
		message, createErr := archive.Create(fmt.Sprintf("message-%d.eml", i))
		require.NoError(t, createErr)
		_, err = fmt.Fprint(message, "From: sender@example.test\r\nTo: archive@example.test\r\nSubject: Test\r\n\r\nworkspace global relay SMTP marker\r\n")
		require.NoError(t, err)
	}
	require.NoError(t, archive.Close())
	_, err = file.Seek(0, 0)
	require.NoError(t, err)
	return file
}
