package global_relay_export

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

// The peer acknowledges setup, then withholds exactly one SMTP response.
// Reading until EOF verifies that the client closes its own socket, rather
// than relying on a server-side timeout to release the delivery worker.
func silentDeliveryPeer(t *testing.T, stage string) (*model.Config, <-chan struct{}, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	stop := make(chan struct{})
	done := make(chan error, 1)
	finished := make(chan struct{})
	reached := make(chan struct{})
	shutdown := sync.OnceFunc(func() { close(stop); _ = listener.Close() })
	t.Cleanup(func() {
		shutdown()
		select {
		case <-finished:
			require.NoError(t, <-done)
		case <-time.After(3 * time.Second):
			t.Error("SMTP fixture did not stop")
		}
	})
	t.Setenv("CI_INBUCKET_HOST", "127.0.0.1")
	t.Setenv("CI_INBUCKET_SMTP_PORT", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	go func() {
		defer close(finished)
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			<-stop
			_ = conn.Close()
		}()
		defer func() { <-closed }()
		reader := bufio.NewReader(conn)
		respond := func(text string) error { _, err := fmt.Fprint(conn, text+"\r\n"); return err }
		wait := func() {
			close(reached)
			_, _ = io.Copy(io.Discard, reader)
			done <- nil
		}
		if stage == "greeting" {
			wait()
			return
		}
		if err := respond("220 local SMTP ready"); err != nil {
			done <- err
			return
		}
		delivered := 0
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF && stage == "slow_success" && delivered == 2 {
					done <- nil
					return
				}
				done <- err
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO "), strings.HasPrefix(line, "HELO "):
				err = respond("250 localhost")
			case strings.HasPrefix(line, "MAIL FROM:"):
				if stage == "mail" {
					wait()
					return
				}
				err = respond("250 accepted")
			case strings.HasPrefix(line, "RCPT TO:"):
				if stage == "recipient" {
					wait()
					return
				}
				err = respond("250 accepted")
			case line == "DATA\r\n":
				if stage == "data" {
					wait()
					return
				}
				err = respond("354 send message")
				if err != nil {
					done <- err
					return
				}
				for {
					line, err = reader.ReadString('\n')
					if err != nil {
						done <- err
						return
					}
					if line == ".\r\n" {
						break
					}
				}
				if stage == "slow_success" {
					timer := time.NewTimer(650 * time.Millisecond)
					select {
					case <-timer.C:
					case <-stop:
						timer.Stop()
						done <- nil
						return
					}
					delivered++
					err = respond("250 queued")
				} else {
					wait() // Withhold the final queued acknowledgment.
					return
				}
			default:
				err = fmt.Errorf("unexpected SMTP fixture command %q", line)
			}
			if err != nil {
				done <- err
				return
			}
		}
	}()
	config := &model.Config{}
	config.SetDefaults()
	*config.MessageExportSettings.GlobalRelaySettings.CustomerType = "INBUCKET"
	*config.MessageExportSettings.GlobalRelaySettings.EmailAddress = "archive@example.test"
	return config, reached, shutdown
}

func TestDeliverCancellationClosesOwnedConnection(t *testing.T) {
	for _, stage := range []string{"greeting", "mail", "acknowledgment"} {
		t.Run(stage, func(t *testing.T) {
			config, reached, shutdown := silentDeliveryPeer(t, stage)
			*config.MessageExportSettings.GlobalRelaySettings.SMTPServerTimeout = 10
			archive := makeTestMailArchive(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- DeliverContext(ctx, archive, config) }()
			select {
			case <-reached:
			case err := <-result:
				t.Fatalf("delivery ended before cancellation target: %v", err)
			case <-time.After(3 * time.Second):
				shutdown()
				<-result
				t.Fatal("SMTP target stage not reached")
			}
			cancel()
			select {
			case err := <-result:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(500 * time.Millisecond):
				shutdown()
				<-result
				t.Fatal("canceled export retained its SMTP connection")
			}
		})
	}
}

func TestDeliverRenewsTimeoutPerMessage(t *testing.T) {
	config, _, _ := silentDeliveryPeer(t, "slow_success")
	*config.MessageExportSettings.GlobalRelaySettings.SMTPServerTimeout = 1
	archive := makeTestMailArchiveWithCount(t, 2)
	started := time.Now()
	require.NoError(t, Deliver(archive, config))
	require.Greater(t, time.Since(started), time.Second, "healthy batch may exceed the timeout for one message")
}

func TestDeliverBoundsSilentSMTPResponses(t *testing.T) {
	for _, stage := range []string{"mail", "recipient", "data", "acknowledgment"} {
		t.Run(stage, func(t *testing.T) {
			config, reached, shutdown := silentDeliveryPeer(t, stage)
			*config.MessageExportSettings.GlobalRelaySettings.SMTPServerTimeout = 1
			archive := makeTestMailArchive(t)
			result := make(chan error, 1)
			go func() { result <- Deliver(archive, config) }()
			select {
			case <-reached:
			case err := <-result:
				t.Fatalf("delivery ended before target SMTP response: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("SMTP target stage not reached")
			}
			select {
			case err := <-result:
				require.Error(t, err)
			case <-time.After(2500 * time.Millisecond):
				t.Error("delivery remained blocked after the configured SMTP timeout")
				shutdown()
				select {
				case <-result:
				case <-time.After(3 * time.Second):
					t.Fatal("delivery did not stop after fixture cleanup")
				}
			}
		})
	}
}
