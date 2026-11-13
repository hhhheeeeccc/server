package elasticsearch_indexing

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
	"github.com/mattermost/mattermost/server/public/model"
)

const indexingAttemptKey = "workspace_indexing_attempt_v1"
const indexingJobColumns = `Id,Type,Priority,CreateAt,StartAt,LastActivityAt,Status,Progress,Data`

var errIndexingOwnershipLost = errors.New("indexing execution no longer owns its database session and job attempt")

// database/sql retries ErrBadConn on a new connection. An advisory lock would
// be lost on that new backend, so an execution may dial exactly once.
type indexingConnector struct {
	connector driver.Connector
	mu        sync.Mutex
	attempted bool
}

func (c *indexingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.attempted {
		return nil, errIndexingOwnershipLost
	}
	c.attempted = true
	return c.connector.Connect(ctx)
}
func (c *indexingConnector) Driver() driver.Driver { return c.connector.Driver() }

type indexingExecution struct {
	db        *sql.DB
	dialer    *indexingDialer
	id, token string
	timeout   time.Duration
	lost      atomic.Bool
	held      bool
}

func newIndexingExecution(dsn string, timeout time.Duration) (*indexingExecution, error) {
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	dsn, err := indexingSessionDSN(dsn, "workspace_indexing_owner", false, timeout)
	if err != nil {
		return nil, err
	}
	connector, err := pq.NewConnector(dsn)
	if err != nil {
		return nil, errors.New("invalid indexing database configuration")
	}
	dialer := &indexingDialer{timeout: timeout}
	connector.Dialer(dialer)
	db := sql.OpenDB(&indexingConnector{connector: connector})
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return &indexingExecution{db: db, dialer: dialer, timeout: timeout}, nil
}
func (s *indexingExecution) Close() {
	s.lost.Store(true)
	s.dialer.close()
	_ = s.db.Close()
}
func (s *indexingExecution) context(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		s.lost.Store(true)
		s.dialer.close()
		close(finished)
	})
	return ctx, func() {
		if !stop() {
			<-finished
		}
		cancel()
	}
}

func indexingLock(id string) int64 {
	hash := sha256.Sum256([]byte("workspace-indexing-attempt-v1:" + id))
	return int64(binary.BigEndian.Uint64(hash[:8]))
}

func (s *indexingExecution) acquire(ctx context.Context, id string) (bool, error) {
	s.id = id
	ctx, cancel := s.context(ctx)
	defer cancel()
	var acquired bool
	err := s.db.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, indexingLock(id)).Scan(&acquired)
	if err != nil {
		s.lost.Store(true)
	}
	s.held = err == nil && acquired
	return acquired, err
}

type indexingScanner interface{ Scan(...any) error }

func scanIndexingJob(row indexingScanner) (*model.Job, error) {
	job := &model.Job{}
	var data []byte
	err := row.Scan(&job.Id, &job.Type, &job.Priority, &job.CreateAt, &job.StartAt, &job.LastActivityAt, &job.Status, &job.Progress, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &job.Data); err != nil {
			return nil, err
		}
	}
	return job, nil
}

func (s *indexingExecution) claim(id string) (*model.Job, error) {
	if s.lost.Load() || !s.held || id != s.id {
		return nil, errIndexingOwnershipLost
	}
	ctx, cancel := s.context(context.Background())
	defer cancel()
	token := model.NewId()
	job, err := scanIndexingJob(s.db.QueryRowContext(ctx, `UPDATE Jobs SET Status=$2,StartAt=$3,LastActivityAt=$3,
        Data=COALESCE(NULLIF(Data,'null'::jsonb),'{}'::jsonb)||jsonb_build_object($4::text,$5::text)
        WHERE Id=$1 AND Type=$6 AND Status=$7
        AND (Data IS NULL OR jsonb_typeof(Data) IN ('null','object')) RETURNING `+indexingJobColumns,
		id, model.JobStatusInProgress, model.GetMillis(), indexingAttemptKey, token, model.JobTypeElasticsearchPostIndexing, model.JobStatusPending))
	if err != nil {
		s.lost.Store(true)
	}
	if job != nil {
		s.token = token
	}
	return job, err
}

func (s *indexingExecution) update(job *model.Job, from string) (*model.Job, error) {
	if s.lost.Load() || !s.held || job.Id != s.id || s.token == "" || job.Data[indexingAttemptKey] != s.token {
		return nil, errIndexingOwnershipLost
	}
	data, err := json.Marshal(job.Data)
	if err != nil {
		return nil, err
	}
	ctx, cancel := s.context(context.Background())
	defer cancel()
	updated, err := scanIndexingJob(s.db.QueryRowContext(ctx, `UPDATE Jobs SET Status=$3,LastActivityAt=$4,Progress=$5,Data=$6::jsonb
        WHERE Id=$1 AND Status=$2 AND Type=$7 AND Data->>$8=$9 RETURNING `+indexingJobColumns,
		job.Id, from, job.Status, model.GetMillis(), job.Progress, string(data), model.JobTypeElasticsearchPostIndexing, indexingAttemptKey, s.token))
	if err != nil {
		s.lost.Store(true)
	}
	return updated, err
}

func (s *indexingExecution) check(ctx context.Context) error {
	if s.lost.Load() || !s.held || s.token == "" {
		return errIndexingOwnershipLost
	}
	ctx, cancel := s.context(ctx)
	defer cancel()
	var owns bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM Jobs WHERE Id=$1 AND Type=$2
        AND Status IN ($3,$4) AND Data->>$5=$6)`, s.id, model.JobTypeElasticsearchPostIndexing,
		model.JobStatusInProgress, model.JobStatusCancelRequested, indexingAttemptKey, s.token).Scan(&owns)
	if err != nil || !owns {
		s.lost.Store(true)
		if err != nil {
			return errors.Join(errIndexingOwnershipLost, err)
		}
		return errIndexingOwnershipLost
	}
	return nil
}

// Each HTTP bulk attempt, including retries, must check the owning session
// immediately before dispatch. A request already accepted by a remote node
// cannot be revoked by a PostgreSQL lock; this is not remote write fencing.
type indexingFence struct {
	enabled   bool
	execution atomic.Pointer[indexingExecution]
	work      atomic.Pointer[indexingWorkContext]
}
type indexingTransport struct {
	base  http.RoundTripper
	fence *indexingFence
}

func (t *indexingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	if t.fence.enabled && strings.HasSuffix(r.URL.Path, "/_bulk") {
		execution := t.fence.execution.Load()
		if execution == nil {
			return nil, errIndexingOwnershipLost
		}
		// A canceled HTTP request must not destroy the owning SQL session:
		// Stop still needs it for the checkpoint CAS. Its independent control
		// timeout bounds this check; cancellation is rechecked before dispatch.
		if err := execution.check(context.Background()); err != nil {
			return nil, err
		}
	}
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(r)
}
func (t *indexingTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
