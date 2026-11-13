package compliance_service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
)

var errComplianceOwnershipLost = errors.New("compliance export lost its owning database session")
var dailyOwnerSlot = make(chan struct{}, 1)
var adhocOwnerSlots = make(chan struct{}, 2)

// A replacement backend cannot inherit an advisory lock. Refuse every second
// Connect, including database/sql's transparent ErrBadConn retry.
type complianceOwnerConnector struct {
	driver.Connector
	mu        sync.Mutex
	attempted bool
}

func (c *complianceOwnerConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.attempted {
		return nil, errComplianceOwnershipLost
	}
	c.attempted = true
	return c.Connector.Connect(ctx)
}

type complianceExecution struct {
	ctx       context.Context
	cancel    context.CancelCauseFunc
	db        *sql.DB
	dialer    *complianceDialer
	timeout   time.Duration
	file      *os.File
	directory string
	lost      atomic.Bool
	done      chan struct{}
	once      sync.Once
	slots     chan struct{}
	kind      string
	reportID  string
	ownedSQL  string
}

func (s *Service) acquireDailyExecution(ctx context.Context) (*complianceExecution, bool, error) {
	if !s.dailyEnabled() {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	select {
	case dailyOwnerSlot <- struct{}{}:
	default:
		return nil, false, nil
	}
	return s.acquireComplianceExecution(ctx, dailyOwnerSlot, model.ComplianceTypeDaily, "", ".daily-export.lock",
		`SELECT pg_try_advisory_lock(11283544,3)`, nil, dailyLockOwned)
}

func (s *Service) acquireAdhocExecution(ctx context.Context, id string) (*complianceExecution, bool, error) {
	if err := s.checkComplianceExportEnabled(); err != nil {
		return nil, false, err
	}
	if !model.IsValidId(id) {
		return nil, false, errors.New("valid saved adhoc report ID required")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	wait, cancel := context.WithTimeout(ctx, complianceOwnershipTimeout(s.app.Config().SqlSettings))
	defer cancel()
	select {
	case adhocOwnerSlots <- struct{}{}:
	case <-wait.Done():
		return nil, false, wait.Err()
	}
	// Separate the bigint advisory-lock namespace from daily's two-int lock.
	// A hash collision only serializes unrelated IDs; updates also require ID.
	sum := sha256.Sum256([]byte("workspace/compliance/adhoc/v1/" + id))
	key := binary.BigEndian.Uint64(sum[:8])
	owned := fmt.Sprintf(`EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory'
        AND pid=pg_backend_pid() AND classid=%d AND objid=%d AND objsubid=1 AND granted)`, uint32(key>>32), uint32(key))
	return s.acquireComplianceExecution(ctx, adhocOwnerSlots, model.ComplianceTypeAdhoc, id, ".adhoc-"+id+".lock",
		`SELECT pg_try_advisory_lock($1::bigint)`, []any{int64(key)}, owned)
}

func complianceOwnershipTimeout(settings model.SqlSettings) time.Duration {
	timeout := time.Duration(model.SafeDereference(settings.QueryTimeout)) * time.Second
	if timeout <= 0 || timeout > 3*time.Second {
		return 3 * time.Second
	}
	return timeout
}

// The caller already holds a process permit. Every exit releases it together
// with the permanent filesystem inode and non-reconnecting primary session.
func (s *Service) acquireComplianceExecution(ctx context.Context, slots chan struct{}, kind, id, lockFile, lockSQL string, lockArgs []any, ownedSQL string) (*complianceExecution, bool, error) {
	cfg := s.app.Config()
	timeout := complianceOwnershipTimeout(cfg.SqlSettings)
	ctx, cancel := context.WithCancelCause(ctx)
	e := &complianceExecution{ctx: ctx, cancel: cancel, timeout: timeout, slots: slots, kind: kind, reportID: id, ownedSQL: ownedSQL}
	success := false
	defer func() {
		if !success {
			e.Close()
		}
	}()
	directory := filepath.Join(model.SafeDereference(cfg.ComplianceSettings.Directory), "compliance")
	e.directory = directory
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, false, err
	}
	file, err := os.OpenFile(filepath.Join(directory, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	e.file = file
	locked, err := tryDailyFileLock(file)
	if err != nil || !locked {
		return nil, false, err
	}
	dsn, err := complianceSessionDSN(model.SafeDereference(cfg.SqlSettings.DataSource), "workspace_compliance_owner", timeout, false)
	if err != nil {
		return nil, false, err
	}
	connector, err := pq.NewConnector(dsn)
	if err != nil {
		return nil, false, errors.New("invalid compliance ownership database configuration")
	}
	lifetime, stop := context.WithCancel(context.Background())
	e.dialer = &complianceDialer{ctx: lifetime, cancel: stop, timeout: min(timeout, 5*time.Second)}
	connector.Dialer(e.dialer)
	e.db = sql.OpenDB(&complianceOwnerConnector{Connector: connector})
	e.db.SetMaxOpenConns(1)
	e.db.SetMaxIdleConns(1)
	err = e.query(ctx, func(ctx context.Context, db *sql.DB) error {
		return db.QueryRowContext(ctx, lockSQL, lockArgs...).Scan(&locked)
	})
	if err != nil || !locked {
		return nil, false, err
	}
	e.done = make(chan struct{})
	go e.monitor()
	success = true
	return e, true, nil
}

func (e *complianceExecution) Close() {
	e.once.Do(func() {
		e.lost.Store(true)
		e.cancel(context.Canceled)
		if e.dialer != nil {
			e.dialer.close()
		}
		if e.done != nil {
			<-e.done
		}
		if e.db != nil {
			_ = e.db.Close()
		}
		// Keep this inode permanently. Unlinking a lock file can let a new
		// process lock a different inode while a prior descriptor is held.
		if e.file != nil {
			_ = e.file.Close()
		}
		<-e.slots
	})
}

func (e *complianceExecution) query(parent context.Context, run func(context.Context, *sql.DB) error) error {
	if e.lost.Load() {
		return errComplianceOwnershipLost
	}
	ctx, cancel := context.WithTimeout(parent, e.timeout)
	defer cancel()
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { e.dialer.close(); close(finished) })
	err := run(ctx, e.db)
	if !stop() {
		<-finished
	}
	if ctx.Err() != nil {
		err = errors.Join(ctx.Err(), err)
	}
	if err != nil {
		err = errors.Join(errComplianceOwnershipLost, err)
		e.lost.Store(true)
		e.cancel(err)
		e.dialer.close()
	}
	return err
}

const dailyLockOwned = `EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory'
    AND pid=pg_backend_pid() AND classid=11283544 AND objid=3 AND objsubid=2 AND granted)`

func (e *complianceExecution) check() error {
	return e.query(e.ctx, func(ctx context.Context, db *sql.DB) error {
		var owned bool
		if err := db.QueryRowContext(ctx, `SELECT `+e.ownedSQL).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return errComplianceOwnershipLost
		}
		return nil
	})
}

func (e *complianceExecution) monitor() {
	defer close(e.done)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			if e.check() != nil {
				return
			}
		}
	}
}

func (e *complianceExecution) update(job *model.Compliance) error {
	if job.Type != e.kind || (e.reportID != "" && job.Id != e.reportID) {
		return errors.New("compliance job does not match its owner")
	}
	if appErr := job.IsValid(); appErr != nil {
		return appErr
	}
	return e.query(e.ctx, func(ctx context.Context, db *sql.DB) error {
		result, err := db.ExecContext(ctx, `UPDATE Compliances SET Status=$2,Count=$3
            WHERE Id=$1 AND Type=$4 AND Status IN ('created','running','failed') AND `+e.ownedSQL,
			job.Id, job.Status, job.Count, e.kind)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err == nil && count != 1 {
			return errComplianceOwnershipLost
		}
		return err
	})
}

// The shared file lock proves no other compatible exporter can still
// own these temporary files. Remove only this report's abandoned regular files.
func (e *complianceExecution) cleanTemporary(job *model.Compliance) error {
	if err := e.check(); err != nil {
		return err
	}
	directory, err := os.Open(e.directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	prefix := job.JobName() + "-"
	for {
		entries, err := directory.ReadDir(128)
		for _, entry := range entries {
			if cause := e.ctx.Err(); cause != nil {
				return cause
			}
			if strings.HasPrefix(entry.Name(), prefix) && strings.HasSuffix(entry.Name(), ".tmp") {
				info, err := entry.Info()
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					return err
				}
				if !info.Mode().IsRegular() {
					continue
				}
				if err := os.Remove(filepath.Join(e.directory, entry.Name())); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
