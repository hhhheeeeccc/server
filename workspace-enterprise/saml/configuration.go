package saml

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/mattermost/enterprise/internal/advisorylock"
	saml2 "github.com/mattermost/gosaml2"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/app/platform"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
	dsig "github.com/russellhaering/goxmldsig"
)

// Bound extra coordination connections independently of the native SQL pool.
// Each login uses one connection even when local conversion needs two keys.
const maxIdentityLockConnections = 16

type Service struct {
	app           *app.App
	schemaMu      sync.Mutex
	schemaReady   bool
	identityLocks *advisorylock.Locker
}

var _ einterfaces.SamlInterface = (*Service)(nil)

func New(a *app.App) einterfaces.SamlInterface {
	return &Service{app: a, identityLocks: advisorylock.NewWithCapacity(model.SafeDereference(a.Config().SqlSettings.DataSource), maxIdentityLockConnections)}
}

func samlError(id string, status int) *model.AppError {
	return model.NewAppError("SAML", id, nil, "", status)
}

func licensed(license *model.License) bool {
	return license != nil && license.Features != nil && model.SafeDereference(license.Features.SAML)
}

func (s *Service) provider(login bool) (*saml2.SAMLServiceProvider, model.SamlSettings, *model.AppError) {
	cfg := s.app.Config().SamlSettings
	cfg.SetDefaults()
	if !licensed(s.app.License()) || (login && !*cfg.Enable) {
		return nil, cfg, samlError("api.user.saml.not_available.app_error", http.StatusForbidden)
	}
	sp, err := configure(cfg, s.app.GetConfigFile)
	if err != nil {
		return nil, cfg, samlError("ent.saml.configuration.app_error", http.StatusBadRequest)
	}
	return sp, cfg, nil
}

func configure(cfg model.SamlSettings, readFile func(string) ([]byte, error)) (*saml2.SAMLServiceProvider, error) {
	cfg.SetDefaults()
	if !*cfg.Verify {
		return nil, errors.New("SAML signature verification is required")
	}
	if *cfg.EmailAttribute == "" || *cfg.UsernameAttribute == "" {
		return nil, errors.New("SAML email and username mappings are required by this server")
	}
	for _, filter := range []string{*cfg.AdminAttribute, *cfg.GuestAttribute} {
		if filter == "" {
			continue
		}
		name, value, ok := strings.Cut(filter, "=")
		if !ok || name == "" || value == "" || strings.Contains(value, "=") {
			return nil, errors.New("SAML role filters must be field=value")
		}
	}
	for _, endpoint := range []string{*cfg.IdpURL, *cfg.AssertionConsumerServiceURL} {
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return nil, errors.New("invalid SAML endpoint")
		}
	}
	if *cfg.IdpDescriptorURL == "" || *cfg.ServiceProviderIdentifier == "" {
		return nil, errors.New("SAML entity identifiers are required")
	}
	data, err := readFile(*cfg.IdpCertificateFile)
	if err != nil {
		return nil, errors.New("unable to read SAML IdP certificate")
	}
	certs := []*x509.Certificate{}
	for len(data) != 0 {
		block, rest := pem.Decode(data)
		if block == nil {
			break
		}
		data = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid SAML IdP certificate")
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, errors.New("SAML IdP certificate is missing")
	}
	sp := &saml2.SAMLServiceProvider{
		IdentityProviderSSOURL: *cfg.IdpURL, IdentityProviderSSOBinding: saml2.BindingHttpRedirect,
		IdentityProviderIssuer: *cfg.IdpDescriptorURL, ServiceProviderIssuer: *cfg.ServiceProviderIdentifier,
		AssertionConsumerServiceURL: *cfg.AssertionConsumerServiceURL, AudienceURI: *cfg.ServiceProviderIdentifier,
		IDPCertificateStore: &dsig.MemoryX509CertificateStore{Roots: certs},
		SignAuthnRequests:   *cfg.SignRequest, ValidateEncryptionCert: true,
		MaximumDecompressedBodySize: 2 * 1024 * 1024,
		ScopingIDPProviderId:        *cfg.ScopingIDPProviderId, ScopingIDPProviderName: *cfg.ScopingIDPName,
	}
	switch *cfg.SignatureAlgorithm {
	case model.SamlSettingsSignatureAlgorithmSha256:
		sp.SignAuthnRequestsAlgorithm = dsig.RSASHA256SignatureMethod
	case model.SamlSettingsSignatureAlgorithmSha512:
		sp.SignAuthnRequestsAlgorithm = dsig.RSASHA512SignatureMethod
	case model.SamlSettingsSignatureAlgorithmSha1:
		sp.SignAuthnRequestsAlgorithm = dsig.RSASHA1SignatureMethod
	default:
		return nil, errors.New("unsupported SAML signing algorithm")
	}
	switch *cfg.CanonicalAlgorithm {
	case model.SamlSettingsCanonicalAlgorithmC14n:
		sp.SignAuthnRequestsCanonicalizer = dsig.MakeC14N10RecCanonicalizer()
	case model.SamlSettingsCanonicalAlgorithmC14n11:
		sp.SignAuthnRequestsCanonicalizer = dsig.MakeC14N11Canonicalizer()
	default:
		return nil, errors.New("unsupported SAML canonicalization algorithm")
	}
	if *cfg.Encrypt || *cfg.SignRequest {
		public, err := readFile(*cfg.PublicCertificateFile)
		if err != nil {
			return nil, errors.New("unable to read SAML SP certificate")
		}
		private, err := readFile(*cfg.PrivateKeyFile)
		if err != nil {
			return nil, errors.New("unable to read SAML SP private key")
		}
		pair, err := tls.X509KeyPair(public, private)
		if err != nil {
			return nil, errors.New("invalid SAML SP certificate/key pair")
		}
		if *cfg.Encrypt {
			sp.SPKeyStore = dsig.TLSCertKeyStore(pair)
		}
		if *cfg.SignRequest {
			sp.SPSigningKeyStore = dsig.TLSCertKeyStore(pair)
		}
	}
	return sp, nil
}

func (s *Service) ConfigureSP(request.CTX) error {
	if !model.SafeDereference(s.app.Config().SamlSettings.Enable) {
		return nil
	}
	_, _, appErr := s.provider(false)
	if appErr != nil {
		return appErr
	}
	return nil
}

func (s *Service) GetMetadata(rctx request.CTX) (result string, resultErr *model.AppError) {
	defer func() { localizeError(rctx, resultErr) }()
	sp, _, appErr := s.provider(false)
	if appErr != nil {
		return "", appErr
	}
	metadata, err := sp.Metadata()
	if err != nil {
		return "", samlError("ent.saml.configuration.app_error", http.StatusBadRequest)
	}
	data, err := xml.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", samlError("ent.saml.configuration.app_error", http.StatusInternalServerError)
	}
	return xml.Header + string(data), nil
}

type Diagnostic struct{ platform *platform.PlatformService }

func NewDiagnostic(ps *platform.PlatformService) einterfaces.SamlDiagnosticInterface {
	return &Diagnostic{platform: ps}
}
func (d *Diagnostic) RunSupportPacketTest(_ request.CTX, cfg model.SamlSettings) error {
	if !licensed(d.platform.License()) {
		return errors.New("SAML requires an active licensed feature")
	}
	_, err := configure(cfg, d.platform.GetConfigFile)
	return err
}
