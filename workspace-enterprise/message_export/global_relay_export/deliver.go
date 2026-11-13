// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.enterprise for license information.

package global_relay_export

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"net/smtp"
	"os"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

func Deliver(export *os.File, config *model.Config) error {
	return DeliverContext(context.Background(), export, config)
}

// DeliverContext bounds each SMTP message by the configured timeout and
// interrupts the owned socket when the export job is canceled.
func DeliverContext(ctx context.Context, export *os.File, config *model.Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := export.Stat()
	if err != nil {
		return fmt.Errorf("unable to get the information of the export temporary file: %w", err)
	}
	zipFile, err := zip.NewReader(export, info.Size())
	if err != nil {
		return fmt.Errorf("unable to open the export temporary file: %w", err)
	}

	to := *config.MessageExportSettings.GlobalRelaySettings.EmailAddress
	var conn *smtpConnection
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	connect := func() error {
		var err error
		conn, err = connectToSMTPServer(ctx, config)
		if err != nil {
			return fmt.Errorf("unable to connect to the smtp server: %w", errors.Join(err, ctx.Err()))
		}
		return nil
	}
	if err := connect(); err != nil {
		return err
	}

	mailsCount := 0
	for _, mail := range zipFile.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mailsCount == MaxEmailsPerConnection {
			conn.Close()
			conn = nil
			if err := connect(); err != nil {
				return err
			}
			mailsCount = 0
		}
		from, err := getFrom(mail)
		if err != nil {
			return err
		}
		// Renew per message, so a healthy 400-message connection is not cut
		// off merely because the whole batch outlives one message's timeout.
		if err := conn.socket.SetDeadline(smtpDeadline(ctx, time.Duration(*config.MessageExportSettings.GlobalRelaySettings.SMTPServerTimeout)*time.Second)); err != nil {
			return errors.Join(err, ctx.Err())
		}
		if err := deliverEmail(conn.Client, mail, from, to); err != nil {
			return errors.Join(err, ctx.Err())
		}

		mailsCount++
	}
	return ctx.Err()
}

func deliverEmail(c *smtp.Client, mailFile *zip.File, from string, to string) error {
	mailData, err := mailFile.Open()
	if err != nil {
		return fmt.Errorf("unable to get the an email from the temporary file: %w", err)
	}
	defer mailData.Close()

	err = c.Mail(from)
	if err != nil {
		return fmt.Errorf("unable to set the email From address: %w", err)
	}

	err = c.Rcpt(to)
	if err != nil {
		return fmt.Errorf("unable to set the email To address: %w", err)
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("unable to write the email message: %w", err)
	}

	_, err = io.Copy(w, mailData)
	if err != nil {
		return fmt.Errorf("unable to set the email message: %w", err)
	}
	err = w.Close()
	if err != nil {
		return fmt.Errorf("unable to deliver the email to Global Relay: %w", err)
	}
	return nil
}

func getFrom(mailFile *zip.File) (string, error) {
	mailData, err := mailFile.Open()
	if err != nil {
		return "", fmt.Errorf("unable to get the an email from the temporary file: %w", err)
	}
	defer mailData.Close()

	message, err := mail.ReadMessage(mailData)
	if err != nil {
		return "", fmt.Errorf("unable to read the email information: %w", err)
	}
	return message.Header.Get("From"), nil
}
