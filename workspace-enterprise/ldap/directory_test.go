package ldap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"
	ldapclient "github.com/mattermost/ldap"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/stretchr/testify/require"
)

// A protocol-level fixture exercises the real LDAP client, including paged
// search, bind failures and TLS negotiation. It contains no production data.
type testDirectory struct {
	listener     net.Listener
	cert         tls.Certificate
	pem          []byte
	entries      []*ldapclient.Entry
	rangeReplies map[string]*ldapclient.Entry
	entriesMu    sync.RWMutex
	searches     atomic.Int32
	binds        atomic.Int32
	searchError  atomic.Uint32
	stall        atomic.Bool
}

func newTestDirectory(t *testing.T, security string) *testDirectory {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "LDAP fixture"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f := &testDirectory{listener: ln, cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), rangeReplies: map[string]*ldapclient.Entry{}}
	f.entries = []*ldapclient.Entry{
		ldapclient.NewEntry("cn=alpha,dc=example,dc=test", map[string][]string{"objectClass": {"groupOfNames"}, "cn": {"Alpha"}, "entryUUID": {"group-a"}}),
		ldapclient.NewEntry("cn=beta,dc=example,dc=test", map[string][]string{"objectClass": {"groupOfNames"}, "cn": {"Beta"}, "entryUUID": {"group-b"}}),
		ldapclient.NewEntry("cn=gamma,dc=example,dc=test", map[string][]string{"objectClass": {"groupOfNames"}, "cn": {"Gamma"}, "entryUUID": {"group-c"}}),
		ldapclient.NewEntry("uid=one,dc=example,dc=test", map[string][]string{"objectClass": {"person"}, "uid": {"one"}, "entryUUID": {"user-1"}, "mail": {"one@example.test"}}),
		ldapclient.NewEntry("uid=two,dc=example,dc=test", map[string][]string{"objectClass": {"person"}, "uid": {"two"}, "entryUUID": {"user-2"}}),
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	connections := []net.Conn{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections = append(connections, c)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				var conn net.Conn = c
				if security == "TLS" {
					conn = tls.Server(c, &tls.Config{Certificates: []tls.Certificate{f.cert}, MinVersion: tls.VersionTLS12})
				}
				f.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range connections {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return f
}

func (f *testDirectory) settings() model.LdapSettings {
	host, port, _ := net.SplitHostPort(f.listener.Addr().String())
	p, _ := strconv.Atoi(port)
	s := model.LdapSettings{}
	s.SetDefaults()
	s.Enable = new(true)
	s.LdapServer = &host
	s.LdapPort = &p
	s.BaseDN = new("dc=example,dc=test")
	s.BindUsername = new("cn=admin,dc=example,dc=test")
	s.BindPassword = new("fixture-password")
	s.GroupIdAttribute = new("entryUUID")
	s.GroupDisplayNameAttribute = new("cn")
	s.IdAttribute = new("entryUUID")
	s.UsernameAttribute = new("uid")
	s.LoginIdAttribute = new("uid")
	s.EmailAttribute = new("mail")
	s.UserFilter = new("(objectClass=person)")
	s.MaxPageSize = new(2)
	s.QueryTimeout = new(3)
	return s
}

func resultPacket(tag ber.Tag, code uint32) *ber.Packet {
	p := ber.Encode(ber.ClassApplication, ber.TypeConstructed, tag, nil, "")
	p.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, code, ""))
	p.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", ""))
	p.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", ""))
	return p
}

func sendPacket(c net.Conn, id int64, body *ber.Packet, control ldapclient.Control) {
	p := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
	p.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, ""))
	p.AppendChild(body)
	if control != nil {
		controls := ber.Encode(ber.ClassContext, ber.TypeConstructed, 0, nil, "")
		controls.AppendChild(control.Encode())
		p.AppendChild(controls)
	}
	_, _ = c.Write(p.Bytes())
}

func matches(entry *ldapclient.Entry, p *ber.Packet) bool {
	switch p.Tag {
	case 0:
		for _, child := range p.Children {
			if !matches(entry, child) {
				return false
			}
		}
		return true
	case 1:
		for _, child := range p.Children {
			if matches(entry, child) {
				return true
			}
		}
		return false
	case 2:
		return !matches(entry, p.Children[0])
	case 3:
		name := p.Children[0].Data.String()
		value := p.Children[1].Data.String()
		for _, v := range entry.GetAttributeValues(name) {
			if v == value || name == "objectClass" && strings.EqualFold(v, value) {
				return true
			}
		}
	case 7:
		return len(entry.GetAttributeValues(p.Data.String())) > 0
	}
	return false
}

func (f *testDirectory) serve(c net.Conn) {
	for {
		packet, err := ber.ReadPacket(c)
		if err != nil {
			return
		}
		id := packet.Children[0].Value.(int64)
		op := packet.Children[1]
		switch op.Tag {
		case ldapclient.ApplicationBindRequest:
			f.binds.Add(1)
			code := uint32(0)
			dn, password := op.Children[1].Data.String(), op.Children[2].Data.String()
			if !(dn == "cn=admin,dc=example,dc=test" && password == "fixture-password") &&
				!(dn == "uid=one,dc=example,dc=test" && password == "user-password") {
				code = ldapclient.LDAPResultInvalidCredentials
			}
			sendPacket(c, id, resultPacket(ldapclient.ApplicationBindResponse, code), nil)
		case ldapclient.ApplicationExtendedRequest:
			sendPacket(c, id, resultPacket(ldapclient.ApplicationExtendedResponse, 0), nil)
			secured := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{f.cert}, MinVersion: tls.VersionTLS12})
			if secured.Handshake() != nil {
				return
			}
			c = secured
		case ldapclient.ApplicationSearchRequest:
			f.searches.Add(1)
			if f.stall.Load() {
				var b [1]byte
				_, _ = c.Read(b[:])
				return
			}
			entries := []*ldapclient.Entry{}
			f.entriesMu.RLock()
			for _, e := range f.entries {
				if matches(e, op.Children[6]) {
					entries = append(entries, e)
				}
			}
			f.entriesMu.RUnlock()
			base := op.Children[0].Data.String()
			if base == "" {
				entries = []*ldapclient.Entry{ldapclient.NewEntry("", map[string][]string{"vendorName": {"Fixture LDAP"}, "vendorVersion": {"1.0"}})}
			}
			if op.Children[1].Value.(int64) == ldapclient.ScopeBaseObject && base != "" {
				entries = []*ldapclient.Entry{ldapclient.NewEntry(base, map[string][]string{"objectClass": {"domain"}})}
				if len(op.Children[7].Children) == 1 {
					requested := strings.ToLower(op.Children[7].Children[0].Data.String())
					f.entriesMu.RLock()
					if reply := f.rangeReplies[strings.ToLower(base)+"|"+requested]; reply != nil {
						entries = []*ldapclient.Entry{reply}
					}
					f.entriesMu.RUnlock()
				}
			}
			var responseControl ldapclient.Control
			if len(packet.Children) > 2 && len(packet.Children[2].Children) > 0 {
				control, _ := ldapclient.DecodeControl(packet.Children[2].Children[0])
				paging, ok := control.(*ldapclient.ControlPaging)
				if ok {
					start, _ := strconv.Atoi(string(paging.Cookie))
					end := min(start+int(paging.PagingSize), len(entries))
					next := ldapclient.NewControlPaging(0)
					if end < len(entries) {
						next.SetCookie([]byte(strconv.Itoa(end)))
					}
					entries = entries[start:end]
					responseControl = next
				}
			}
			for _, entry := range entries {
				body := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ldapclient.ApplicationSearchResultEntry, nil, "")
				body.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, entry.DN, ""))
				attrs := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
				for _, a := range entry.Attributes {
					attr := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
					attr.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, a.Name, ""))
					values := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "")
					for _, v := range a.Values {
						values.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, v, ""))
					}
					attr.AppendChild(values)
					attrs.AppendChild(attr)
				}
				body.AppendChild(attrs)
				sendPacket(c, id, body, nil)
			}
			sendPacket(c, id, resultPacket(ldapclient.ApplicationSearchResultDone, f.searchError.Load()), responseControl)
		case ldapclient.ApplicationUnbindRequest:
			return
		default:
			return
		}
	}
}

func TestDirectoryConnectionSecurityAndPaging(t *testing.T) {
	for _, security := range []string{"", "TLS", "STARTTLS"} {
		t.Run(security, func(t *testing.T) {
			f := newTestDirectory(t, security)
			settings := f.settings()
			settings.ConnectionSecurity = &security
			if security != "" {
				settings.PublicCertificateFile = new("fixture-ca.pem")
			}
			d, err := connect(request.TestContext(t), settings, func(string) ([]byte, error) { return f.pem, nil })
			require.NoError(t, err)
			defer d.close()
			entries, err := d.search(settings, *settings.BaseDN, groupFilter(settings), []string{"entryUUID", "cn"}, ldapclient.ScopeWholeSubtree)
			require.NoError(t, err)
			require.Len(t, entries, 3)
			require.EqualValues(t, 2, f.searches.Load())
			require.EqualValues(t, 1, f.binds.Load())
		})
	}
}

func TestDirectoryReadsADMembershipRanges(t *testing.T) {
	f := newTestDirectory(t, "")
	settings := f.settings()
	groupDN := "cn=beta,dc=example,dc=test"
	f.entriesMu.Lock()
	f.entries[1] = ldapclient.NewEntry(groupDN, map[string][]string{
		"objectClass": {"groupOfNames"}, "cn": {"Beta"}, "entryUUID": {"group-b"},
		"member;range=0-1": {"uid=one,dc=example,dc=test", "cn=alpha,dc=example,dc=test"},
	})
	f.rangeReplies[groupDN+"|member;range=2-*"] = ldapclient.NewEntry(groupDN, map[string][]string{
		"member;range=2-*": {"uid=two,dc=example,dc=test"},
	})
	f.entriesMu.Unlock()
	d, err := connect(request.TestContext(t), settings, nil)
	require.NoError(t, err)
	defer d.close()
	groups, err := d.search(settings, *settings.BaseDN, groupFilter(settings), []string{"entryUUID", "member"}, ldapclient.ScopeWholeSubtree)
	require.NoError(t, err)
	require.Nil(t, resolveRangedMembers(d, settings, groups[1]))
	require.Equal(t, []string{"uid=one,dc=example,dc=test", "cn=alpha,dc=example,dc=test", "uid=two,dc=example,dc=test"}, groups[1].GetAttributeValues("member"))

	f.entriesMu.Lock()
	f.rangeReplies[groupDN+"|member;range=2-*"] = ldapclient.NewEntry(groupDN, map[string][]string{
		"member;range=3-*": {"uid=two,dc=example,dc=test"},
	})
	f.entriesMu.Unlock()
	groups, err = d.search(settings, *settings.BaseDN, groupFilter(settings), []string{"entryUUID", "member"}, ldapclient.ScopeWholeSubtree)
	require.NoError(t, err)
	require.Error(t, resolveRangedMembers(d, settings, groups[1]), "a missing range must not be treated as a complete membership snapshot")
}

func TestDirectoryRejectsBadCredentialsCertificatesAndPartialResults(t *testing.T) {
	f := newTestDirectory(t, "")
	settings := f.settings()
	settings.BindPassword = new("wrong")
	_, err := connect(request.TestContext(t), settings, func(string) ([]byte, error) { return nil, nil })
	require.ErrorContains(t, err, "bind failed")
	require.NotContains(t, err.Error(), "wrong")
	settings.BindPassword = new("")
	_, err = connect(request.TestContext(t), settings, nil)
	require.ErrorContains(t, err, "both LDAP bind")
	secure := newTestDirectory(t, "TLS")
	settings = secure.settings()
	settings.ConnectionSecurity = new("TLS")
	_, err = connect(request.TestContext(t), settings, nil)
	require.Error(t, err, "untrusted certificate must not be accepted")
	settings = f.settings()
	d, err := connect(request.TestContext(t), settings, nil)
	require.NoError(t, err)
	defer d.close()
	f.searchError.Store(ldapclient.LDAPResultSizeLimitExceeded)
	entries, err := d.search(settings, *settings.BaseDN, defaultGroupFilter, []string{"cn"}, ldapclient.ScopeWholeSubtree)
	require.Error(t, err)
	require.Nil(t, entries)
}

func TestDirectoryCancellation(t *testing.T) {
	f := newTestDirectory(t, "")
	settings := f.settings()
	f.stall.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d, err := connect(request.TestContext(t).WithContext(ctx), settings, nil)
	require.NoError(t, err)
	defer d.close()
	done := make(chan error, 1)
	go func() {
		_, err := d.search(settings, *settings.BaseDN, defaultGroupFilter, []string{"cn"}, ldapclient.ScopeWholeSubtree)
		done <- err
	}()
	require.Eventually(t, func() bool { return f.searches.Load() > 0 }, time.Second, time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("canceled directory search did not stop")
	}
}

func TestUnsavedDiagnosticSettingsPreserveLiveConfigAndBindSecret(t *testing.T) {
	saved := model.LdapSettings{}
	saved.SetDefaults()
	saved.LdapServer = new("directory.example.test")
	saved.BindUsername = new("reader")
	saved.BindPassword = new("saved-secret")
	saved.UserFilter = new("(objectClass=person)")
	merged, appErr := mergeTestSettings(saved, model.LdapSettings{BindPassword: new(model.FakeSetting), UserFilter: new("")})
	require.Nil(t, appErr)
	require.Equal(t, "saved-secret", *merged.BindPassword)
	require.Empty(t, *merged.UserFilter)
	require.Equal(t, "(objectClass=person)", *saved.UserFilter)
	_, appErr = mergeTestSettings(saved, model.LdapSettings{LdapServer: new("different.example.test"), BindPassword: new(model.FakeSetting)})
	require.NotNil(t, appErr)
	require.Equal(t, "api.ldap.test.reenter_password", appErr.Id)
	require.Equal(t, "directory.example.test", *saved.LdapServer)
	require.Equal(t, "saved-secret", *saved.BindPassword)
	merged, appErr = mergeTestSettings(saved, model.LdapSettings{LdapServer: new("different.example.test"), BindPassword: new("new-secret")})
	require.Nil(t, appErr)
	require.Equal(t, "new-secret", *merged.BindPassword)
	require.Equal(t, "saved-secret", *saved.BindPassword)
}

func TestGroupIdentifiersAreEscapedAndPreserveCase(t *testing.T) {
	filter, err := groupIDFilter("entryUUID", "A*)(cn=*)")
	require.NoError(t, err)
	require.Equal(t, `(entryUUID=A\2a\29\28cn=\2a\29)`, filter)
	_, err = groupIDFilter("cn)(objectClass=*", "id")
	require.Error(t, err)
	entry := ldapclient.NewEntry("test", map[string][]string{"entryUUID": {"AbC"}, "objectGUID": {string([]byte{0x1e, 0xbc, 0xff, 0x1e, 0xa3, 0x4a, 0x9d, 0x4f, 0xa5, 0x08, 0xd4, 0xd8, 0x1f, 0x1f, 0x58, 0x73})}})
	require.Equal(t, "AbC", entryID(entry, "entryUUID"))
	require.Equal(t, "AbC", entryID(entry, "ENTRYuuid"))
	guid := entryID(entry, "objectGUID")
	require.Equal(t, guid, entryID(entry, "OBJECTguid"))
	require.Equal(t, `\1e\bc\ff\1e\a3\4a\9d\4f\a5\08\d4\d8\1f\1f\58\73`, guid)
	filter, err = groupIDFilter("objectGUID", guid)
	require.NoError(t, err)
	require.Equal(t, "(objectGUID="+guid+")", filter)
	_, err = groupIDFilter("objectGUID", guid+")(objectClass=*)")
	require.Error(t, err)
}
