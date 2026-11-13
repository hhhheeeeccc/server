package license

import "github.com/mattermost/mattermost/server/public/model"

// Service satisfies Mattermost's license extension point locally. Signature
// validation is supplied by this package's Validator and embedded public key.
// It never creates an automatic or fallback license, so custom features stay
// locked until a valid signed license is uploaded.
type Service struct{}

func New() *Service {
	return &Service{}
}

func (*Service) CanStartTrial() (bool, error) {
	return false, nil
}

func (*Service) GetPrevTrial() (*model.License, error) {
	return nil, nil
}

func (*Service) NewMattermostEntryLicense(string) *model.License {
	return nil
}
