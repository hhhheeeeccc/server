package ldap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	ldapclient "github.com/mattermost/ldap"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
)

const defaultGroupFilter = "(|(objectClass=group)(objectClass=groupOfNames)(objectClass=groupOfUniqueNames))"

var attributeName = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9-]*|[0-9]+(\.[0-9]+)+)(;[A-Za-z0-9-]+)*$`)
var rangedMemberRequest = regexp.MustCompile(`(?i)^(member|uniqueMember|memberUid);range=[0-9]+-\*$`)
var escapedGUID = regexp.MustCompile(`^(\\[0-9a-fA-F]{2}){16}$`)

type directory struct {
	conn   *ldapclient.Conn
	cancel context.CancelFunc
	stop   func() bool
}

func (d *directory) close() {
	d.stop()
	d.cancel()
	d.conn.Close()
}

// Each operation owns its connection, so binding and cancellation cannot affect
// a different request. The deadline bounds the whole operation, including pages.
func connect(rctx request.CTX, settings model.LdapSettings, readFile func(string) ([]byte, error)) (*directory, error) {
	settings.SetDefaults()
	host := strings.TrimSpace(*settings.LdapServer)
	if host == "" || strings.ContainsAny(host, "/?#@") || *settings.LdapPort < 1 || *settings.LdapPort > 65535 {
		return nil, fmt.Errorf("invalid LDAP server or port")
	}
	timeout := time.Duration(*settings.QueryTimeout) * time.Second
	if *settings.QueryTimeout <= 0 || timeout/time.Second != time.Duration(*settings.QueryTimeout) {
		return nil, fmt.Errorf("invalid LDAP query timeout")
	}
	security := strings.ToUpper(*settings.ConnectionSecurity)
	if security != "" && security != "TLS" && security != "STARTTLS" {
		return nil, fmt.Errorf("invalid LDAP connection security")
	}
	ctx, cancel := context.WithTimeout(rctx.Context(), timeout)
	address := net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(*settings.LdapPort))
	var tlsConfig *tls.Config
	var err error
	if security != "" {
		tlsConfig, err = directoryTLS(settings, readFile)
		if err != nil {
			cancel()
			return nil, err
		}
	}
	var socket net.Conn
	if security == "TLS" {
		socket, err = (&tls.Dialer{Config: tlsConfig}).DialContext(ctx, "tcp", address)
	} else {
		socket, err = (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}
	if err != nil {
		cancel()
		return nil, fmt.Errorf("connect to LDAP: %w", err)
	}
	deadline, _ := ctx.Deadline()
	if err = socket.SetDeadline(deadline); err != nil {
		socket.Close()
		cancel()
		return nil, err
	}
	d := &directory{conn: ldapclient.NewConn(socket, security == "TLS"), cancel: cancel}
	d.stop = context.AfterFunc(ctx, func() { _ = socket.Close() })
	d.conn.Start()
	d.conn.SetTimeout(timeout)
	if security == "STARTTLS" {
		if err = d.conn.StartTLS(tlsConfig); err != nil {
			d.close()
			return nil, fmt.Errorf("LDAP StartTLS: %w", err)
		}
	}
	// A named bind with an empty password may be accepted as unauthenticated
	// by some directories. Do not report it as a successful credential test.
	if *settings.BindUsername != "" || *settings.BindPassword != "" {
		if *settings.BindUsername == "" || *settings.BindPassword == "" {
			d.close()
			return nil, fmt.Errorf("both LDAP bind username and password are required")
		}
		if err = d.conn.Bind(*settings.BindUsername, *settings.BindPassword); err != nil {
			d.close()
			// Do not include server diagnostics which could echo credentials.
			return nil, fmt.Errorf("LDAP bind failed")
		}
	}
	return d, nil
}

func directoryTLS(settings model.LdapSettings, readFile func(string) ([]byte, error)) (*tls.Config, error) {
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         strings.Trim(*settings.LdapServer, "[]"),
		InsecureSkipVerify: *settings.SkipCertificateVerification, // explicit administrator setting
	}
	public, private := *settings.PublicCertificateFile, *settings.PrivateKeyFile
	if public == "" && private == "" {
		return cfg, nil
	}
	if public == "" {
		return nil, fmt.Errorf("LDAP private key requires a public certificate")
	}
	pem, err := readFile(public)
	if err != nil {
		return nil, fmt.Errorf("read LDAP public certificate: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("invalid LDAP public certificate")
	}
	cfg.RootCAs = roots
	if private != "" {
		key, err := readFile(private)
		if err != nil {
			return nil, fmt.Errorf("read LDAP private key: %w", err)
		}
		cert, err := tls.X509KeyPair(pem, key)
		if err != nil {
			return nil, fmt.Errorf("invalid LDAP client certificate/key pair")
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

func (d *directory) search(settings model.LdapSettings, base, filter string, attributes []string, scope int) ([]*ldapclient.Entry, error) {
	if filter == "" {
		filter = "(objectClass=*)"
	}
	if _, err := ldapclient.CompileFilter(filter); err != nil {
		return nil, fmt.Errorf("invalid LDAP search filter")
	}
	for _, name := range attributes {
		if !attributeName.MatchString(name) && !rangedMemberRequest.MatchString(name) {
			return nil, fmt.Errorf("invalid LDAP attribute name")
		}
	}
	if *settings.MaxPageSize < 0 || uint64(*settings.MaxPageSize) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("invalid LDAP page size")
	}
	req := ldapclient.NewSearchRequest(base, scope, ldapclient.NeverDerefAliases, 0, *settings.QueryTimeout, false, filter, attributes, nil)
	var result *ldapclient.SearchResult
	var err error
	if *settings.MaxPageSize > 0 && scope != ldapclient.ScopeBaseObject {
		result, err = d.conn.SearchWithPaging(req, uint32(*settings.MaxPageSize))
	} else {
		result, err = d.conn.Search(req)
	}
	if err != nil {
		return nil, err // Never return a truncated, apparently successful directory.
	}
	return result.Entries, nil
}

func groupFilter(settings model.LdapSettings) string {
	if *settings.GroupFilter != "" {
		return *settings.GroupFilter
	}
	return defaultGroupFilter
}

// Mattermost documents objectGUID as escaped hexadecimal bytes, not the
// Microsoft display UUID byte order. Keep text IDs verbatim and case-sensitive.
func entryID(entry *ldapclient.Entry, name string) string {
	if strings.EqualFold(name, "objectGUID") || strings.EqualFold(name, "objectGUID;binary") {
		var id strings.Builder
		for _, b := range []byte(attributeValue(entry, name)) {
			fmt.Fprintf(&id, `\%02x`, b)
		}
		return id.String()
	}
	return attributeValue(entry, name)
}

// Attribute names are case-insensitive in LDAP (RFC 4512), even when the
// directory returns a different spelling from the configured mapping.
func attributeValue(entry *ldapclient.Entry, name string) string {
	values := attributeValues(entry, name)
	if len(values) > 0 {
		return values[0]
	}
	return ""
}

func attributeValues(entry *ldapclient.Entry, name string) []string {
	for _, attribute := range entry.Attributes {
		if strings.EqualFold(attribute.Name, name) {
			return attribute.Values
		}
	}
	return nil
}

func groupIDFilter(name, id string) (string, error) {
	if !attributeName.MatchString(name) || id == "" {
		return "", fmt.Errorf("invalid LDAP group identifier")
	}
	if strings.EqualFold(name, "objectGUID") || strings.EqualFold(name, "objectGUID;binary") {
		if !escapedGUID.MatchString(id) {
			return "", fmt.Errorf("invalid LDAP objectGUID")
		}
		return "(" + name + "=" + strings.ToLower(id) + ")", nil
	}
	return "(" + name + "=" + ldapclient.EscapeFilter(id) + ")", nil
}
