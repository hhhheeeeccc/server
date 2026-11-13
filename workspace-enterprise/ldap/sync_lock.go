package ldap

import "context"

const syncLockKey int64 = 6492261403

func (s *Service) acquireSyncLock(ctx context.Context) (func(), error) {
	return s.syncLocker.Acquire(ctx, syncLockKey)
}
