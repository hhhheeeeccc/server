package cloud

import (
	"errors"
	"mime/multipart"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/einterfaces"
)

var ErrUnavailable = errors.New("Workspace Cloud service is not configured")

type Service struct{}

var _ einterfaces.CloudInterface = (*Service)(nil)

func New(*app.Server) einterfaces.CloudInterface { return &Service{} }

func (*Service) GetCloudProduct(string, string) (*model.Product, error)  { return nil, ErrUnavailable }
func (*Service) GetCloudProducts(string, bool) ([]*model.Product, error) { return nil, ErrUnavailable }
func (*Service) GetSelfHostedProducts(string) ([]*model.Product, error)  { return nil, ErrUnavailable }
func (*Service) GetCloudLimits(string) (*model.ProductLimits, error)     { return nil, ErrUnavailable }
func (*Service) GetCloudCustomer(string) (*model.CloudCustomer, error)   { return nil, ErrUnavailable }
func (*Service) UpdateCloudCustomer(string, *model.CloudCustomerInfo) (*model.CloudCustomer, error) {
	return nil, ErrUnavailable
}
func (*Service) UpdateCloudCustomerAddress(string, *model.Address) (*model.CloudCustomer, error) {
	return nil, ErrUnavailable
}
func (*Service) GetSubscription(string) (*model.Subscription, error) { return nil, ErrUnavailable }
func (*Service) GetInvoicesForSubscription(string) ([]*model.Invoice, error) {
	return nil, ErrUnavailable
}
func (*Service) GetInvoicePDF(string, string) ([]byte, string, error) { return nil, "", ErrUnavailable }
func (*Service) ChangeSubscription(string, string, *model.SubscriptionChange) (*model.Subscription, error) {
	return nil, ErrUnavailable
}
func (*Service) ValidateBusinessEmail(string, string) error { return ErrUnavailable }
func (*Service) InvalidateCaches() error                    { return ErrUnavailable }
func (*Service) CreateOrUpdateSubscriptionHistoryEvent(string, int) (*model.SubscriptionHistory, error) {
	return nil, ErrUnavailable
}
func (*Service) HandleLicenseChange() error      { return ErrUnavailable }
func (*Service) CheckCWSConnection(string) error { return ErrUnavailable }
func (*Service) ApplyIPFilters(string, *model.AllowedIPRanges) (*model.AllowedIPRanges, error) {
	return nil, ErrUnavailable
}
func (*Service) GetIPFilters(string) (*model.AllowedIPRanges, error)        { return nil, ErrUnavailable }
func (*Service) GetInstallation(string) (*model.Installation, error)        { return nil, ErrUnavailable }
func (*Service) RemoveAuditLoggingCert(string) error                        { return ErrUnavailable }
func (*Service) CreateAuditLoggingCert(string, *multipart.FileHeader) error { return ErrUnavailable }
