// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.enterprise for license information.

package global_relay_export

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/utils"
	"github.com/mattermost/mattermost/server/v8/channels/utils/testutils"
	"github.com/mattermost/mattermost/server/v8/platform/shared/mail"
)

const (
	GlobalRelayA9Server  = "mailarchivespool1.globalrelay.com"
	GlobalRelayA10Server = "feeds.globalrelay.com"
	GlobalRelayA9IP      = "208.81.212.70"
	GlobalRelayA10IP     = "208.81.213.24"

	defaultSMTPPort         = "25"
	defaultInbucketSMTPPort = "10025"
)

// A connection owns its cancellation registration and raw socket, including
// after STARTTLS replaces the SMTP client's transport with a TLS wrapper.
type smtpConnection struct {
	*smtp.Client
	socket net.Conn
	stop   func()
}

func (c *smtpConnection) Close() error {
	err := c.socket.Close()
	_ = c.Client.Close()
	c.stop()
	return err
}

func smtpDeadline(ctx context.Context, timeout time.Duration) time.Time {
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	return deadline
}

func connectToSMTPServer(ctx context.Context, config *model.Config) (*smtpConnection, error) {
	smtpServerName := ""
	smtpServerHost := ""
	smtpPort := defaultSMTPPort
	security := model.ConnSecurityStarttls
	auth := true
	if *config.MessageExportSettings.GlobalRelaySettings.CustomerType == "A10" {
		smtpServerName = GlobalRelayA10Server
		smtpServerHost = GlobalRelayA10IP
	} else if *config.MessageExportSettings.GlobalRelaySettings.CustomerType == "A9" {
		smtpServerName = GlobalRelayA9Server
		smtpServerHost = GlobalRelayA9IP
	} else if *config.MessageExportSettings.GlobalRelaySettings.CustomerType == "INBUCKET" {
		inbucketSMTPPort := os.Getenv("CI_INBUCKET_SMTP_PORT")
		if inbucketSMTPPort == "" {
			inbucketSMTPPort = defaultInbucketSMTPPort
		}
		inbucketHost := os.Getenv("CI_INBUCKET_HOST")
		if inbucketHost == "" {
			intPort, err := strconv.Atoi(inbucketSMTPPort)
			if err != nil {
				intPort = 0
			}
			inbucketHost = testutils.GetInterface(intPort)
		}
		smtpServerName = inbucketHost
		smtpServerHost = inbucketHost
		smtpPort = inbucketSMTPPort
		auth = false
	} else if *config.MessageExportSettings.GlobalRelaySettings.CustomerType == model.GlobalrelayCustomerTypeCustom {
		customSMTPPort := *config.MessageExportSettings.GlobalRelaySettings.CustomSMTPPort
		if customSMTPPort != "" {
			smtpPort = customSMTPPort
		}
		smtpServerName = *config.MessageExportSettings.GlobalRelaySettings.CustomSMTPServerName
		smtpServerHost = *config.MessageExportSettings.GlobalRelaySettings.CustomSMTPServerName
	}

	smtpConfig := &mail.SMTPConfig{
		ConnectionSecurity:                security,
		SkipServerCertificateVerification: false,
		Hostname:                          utils.GetHostnameFromSiteURL(*config.ServiceSettings.SiteURL),
		ServerName:                        smtpServerName,
		Server:                            smtpServerHost,
		Port:                              smtpPort,
		EnableSMTPAuth:                    auth,
		Username:                          *config.MessageExportSettings.GlobalRelaySettings.SMTPUsername,
		Password:                          *config.MessageExportSettings.GlobalRelaySettings.SMTPPassword,
		ServerTimeout:                     *config.MessageExportSettings.GlobalRelaySettings.SMTPServerTimeout,
	}
	dialer := &net.Dialer{Timeout: time.Duration(smtpConfig.ServerTimeout) * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(smtpConfig.Server, smtpConfig.Port))
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); _ = conn.Close() })
	stopAndJoin := func() {
		if !stop() {
			<-done
		}
	}
	ready := false
	defer func() {
		if !ready {
			_ = conn.Close()
			stopAndJoin()
		}
	}()
	if err := conn.SetDeadline(smtpDeadline(ctx, time.Duration(smtpConfig.ServerTimeout)*time.Second)); err != nil {
		return nil, err
	}
	// Keep the deadline active through greeting, EHLO, STARTTLS and AUTH.
	// The shared helper only applies its context to the initial greeting and
	// ignores STARTTLS errors, which could leave this delivery on plaintext.
	c, err := smtp.NewClient(conn, smtpConfig.ServerName+":"+smtpConfig.Port)
	if err != nil {
		return nil, err
	}
	if smtpConfig.Hostname != "" {
		if err := c.Hello(smtpConfig.Hostname); err != nil {
			return nil, err
		}
	}
	if *config.MessageExportSettings.GlobalRelaySettings.CustomerType != "INBUCKET" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return nil, errors.New("Global Relay SMTP requires STARTTLS")
		}
		if err := c.StartTLS(&tls.Config{ServerName: smtpConfig.ServerName}); err != nil {
			return nil, err
		}
		if _, encrypted := c.TLSConnectionState(); !encrypted {
			return nil, errors.New("Global Relay SMTP requires STARTTLS")
		}
		var authentication smtp.Auth = mail.LoginAuth(smtpConfig.Username, smtpConfig.Password, smtpConfig.ServerName+":"+smtpConfig.Port)
		if supported, methods := c.Extension("AUTH"); supported && strings.Contains(" "+methods+" ", " PLAIN ") {
			authentication = smtp.PlainAuth("", smtpConfig.Username, smtpConfig.Password, smtpConfig.ServerName+":"+smtpConfig.Port)
		}
		if err := c.Auth(authentication); err != nil {
			return nil, err
		}
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	ready = true
	return &smtpConnection{Client: c, socket: conn, stop: stopAndJoin}, nil
}
