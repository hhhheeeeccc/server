package message_export

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
)

const messageExportAttemptKey = "workspace_message_export_attempt_v1"
const messageExportJobColumns = `Id,Type,Priority,CreateAt,StartAt,LastActivityAt,Status,Progress,Data`

var errMessageExportAttemptLost = errors.New("message export attempt no longer owns this job")

type messageExportJobScanner interface{ Scan(...any) error }

func scanMessageExportJob(row messageExportJobScanner) (*model.Job, error) {
	job := &model.Job{}
	var data []byte
	err := row.Scan(&job.Id, &job.Type, &job.Priority, &job.CreateAt, &job.StartAt,
		&job.LastActivityAt, &job.Status, &job.Progress, &data)
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

// This session acquires the lock before claiming a job and never reconnects.
// The random token distinguishes successive claims of the same job ID.
type messageExportOwner struct {
	*messageExportExecution
	id, token string
	ownedSQL  string
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
}

func (w *MessageExportWorker) openClaimSession(ctx context.Context) (*messageExportExecution, error) {
	cfg := w.jobServer.Config()
	return newMessageExportExecution(ctx, model.SafeDereference(cfg.SqlSettings.DataSource),
		time.Duration(model.SafeDereference(cfg.SqlSettings.QueryTimeout))*time.Second)
}

func (w *MessageExportWorker) acquireJobOwner(ctx context.Context, id string) (*messageExportOwner, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	session, err := w.openClaimSession(ctx)
	if err != nil {
		cancel()
		return nil, false, err
	}
	sum := sha256.Sum256([]byte("workspace/message-export/claim/v1/" + id))
	key := binary.BigEndian.Uint64(sum[:8])
	owner := &messageExportOwner{messageExportExecution: session, id: id, cancel: cancel,
		ownedSQL: fmt.Sprintf(`EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory'
        AND pid=pg_backend_pid() AND classid=%d AND objid=%d AND objsubid=1 AND granted)`, uint32(key>>32), uint32(key))}
	query, stop := session.queryContext()
	var locked bool
	err = session.db.QueryRowContext(query, `SELECT pg_try_advisory_lock($1::bigint)`, int64(key)).Scan(&locked)
	stop()
	if err != nil || !locked {
		owner.Close()
		return nil, false, err
	}
	return owner, true, nil
}

func (o *messageExportOwner) Close() {
	o.closeOnce.Do(func() {
		o.cancel()
		o.messageExportExecution.Close()
		if o.done != nil {
			<-o.done
		}
	})
}

func (o *messageExportOwner) claim() (*model.Job, error) {
	ctx, stop := o.queryContext()
	defer stop()
	job, err := scanMessageExportJob(o.db.QueryRowContext(ctx, `UPDATE Jobs SET Status=$2,
        StartAt=$3,LastActivityAt=$3,Data=COALESCE(NULLIF(Data,'null'::jsonb),'{}'::jsonb)
        || jsonb_build_object($4::text,$5::text)
        WHERE Id=$1 AND Type=$6 AND Status=$7
        AND (Data IS NULL OR jsonb_typeof(Data) IN ('null','object')) AND `+o.ownedSQL+` RETURNING `+messageExportJobColumns,
		o.id, model.JobStatusInProgress, model.GetMillis(), messageExportAttemptKey, model.NewId(),
		model.JobTypeMessageExport, model.JobStatusPending))
	if job != nil {
		o.token = job.Data[messageExportAttemptKey]
	}
	return job, err
}

func (o *messageExportOwner) update(job *model.Job, status string, progress int64) (*model.Job, error) {
	if o.ctx.Err() != nil || o.token == "" || job.Id != o.id || job.Data[messageExportAttemptKey] != o.token {
		return nil, errMessageExportAttemptLost
	}
	data, err := json.Marshal(job.Data)
	if err != nil {
		return nil, err
	}
	ctx, stop := o.queryContext()
	defer stop()
	updated, err := scanMessageExportJob(o.db.QueryRowContext(ctx, `UPDATE Jobs SET
        Status=$3,LastActivityAt=$4,Progress=$5,Data=$6::jsonb WHERE Id=$1 AND Status=$2
        AND Type=$7 AND Data->>$8=$9 AND `+o.ownedSQL+` RETURNING `+messageExportJobColumns,
		o.id, model.JobStatusInProgress, status, model.GetMillis(), progress, string(data),
		model.JobTypeMessageExport, messageExportAttemptKey, o.token))
	if err != nil || updated == nil {
		o.cancel()
		if err == nil {
			err = errMessageExportAttemptLost
		}
	}
	return updated, err
}

func (o *messageExportOwner) monitor(w *MessageExportWorker) {
	defer close(o.done)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-o.ctx.Done():
			return
		case <-ticker.C:
			if !w.IsEnabled(w.jobServer.Config()) {
				o.cancel()
				return
			}
			ctx, stop := o.queryContext()
			var current bool
			err := o.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM Jobs WHERE Id=$1
                AND Type=$2 AND Status=$3 AND Data->>$4=$5) AND `+o.ownedSQL,
				o.id, model.JobTypeMessageExport, model.JobStatusInProgress, messageExportAttemptKey, o.token).Scan(&current)
			stop()
			if err != nil || !current {
				o.cancel()
				return
			}
		}
	}
}

func (w *MessageExportWorker) updateOwnedJob(job *model.Job, status string, progress int64) error {
	if w.execution == nil {
		return errMessageExportAttemptLost
	}
	updated, err := w.execution.update(job, status, progress)
	if err != nil {
		return err
	}
	*job = *updated
	return w.publishJob(job)
}

func (w *MessageExportWorker) publishJob(job *model.Job) error {
	if w.server == nil {
		return nil
	}
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	message := model.NewWebSocketEvent(model.WebsocketEventJobUpdated, "", "", "", nil, "")
	message.Add("job", string(data))
	message.GetBroadcast().ContainsSensitiveData = true
	message.GetBroadcast().RequiredPermissions = []string{model.PermissionReadComplianceExportJob.Id}
	w.server.Platform().Publish(message)
	return nil
}

// Cleanup may run after cancellation destroyed the owning socket. Only change
// status, preserving the last committed cursor, and require this exact token.
func (w *MessageExportWorker) transitionClaim(job *model.Job, from, to string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session, err := w.openClaimSession(ctx)
	if err != nil {
		return false, err
	}
	defer session.Close()
	query, stop := session.queryContext()
	defer stop()
	updated, err := scanMessageExportJob(session.db.QueryRowContext(query, `UPDATE Jobs SET
        Status=$3,LastActivityAt=$4 WHERE Id=$1 AND Status=$2 AND Type=$5 AND Data->>$6=$7
        RETURNING `+messageExportJobColumns, job.Id, from, to, model.GetMillis(),
		model.JobTypeMessageExport, messageExportAttemptKey, job.Data[messageExportAttemptKey]))
	if err != nil || updated == nil {
		return false, err
	}
	*job = *updated
	return true, w.publishJob(updated)
}

func (w *MessageExportWorker) recoverInterruptedJobs() {
	if w.jobServer == nil || w.context == nil || !w.IsEnabled(w.jobServer.Config()) {
		return
	}
	if err := w.recoverClaims(); err != nil && w.context.Err() == nil {
		w.logger.Warn("Unable to recover interrupted message export jobs", mlog.Err(err))
	}
}

func (w *MessageExportWorker) recoverClaims() error {
	if !w.IsEnabled(w.jobServer.Config()) {
		return nil
	}
	ctx, cancel := context.WithTimeout(w.context, 10*time.Second)
	defer cancel()
	session, err := w.openClaimSession(ctx)
	if err != nil {
		return err
	}
	defer session.Close()
	query, stop := session.queryContext()
	rows, err := session.db.QueryContext(query, `SELECT `+messageExportJobColumns+` FROM Jobs
        WHERE Type=$1 AND Status IN ($2,$3) AND Data->>$4 ~ '^[a-z0-9]{26}$'
        AND (CreateAt,Id)>($5,$6) ORDER BY CreateAt,Id LIMIT 100`, model.JobTypeMessageExport,
		model.JobStatusInProgress, model.JobStatusCancelRequested, messageExportAttemptKey, w.recoveryAt, w.recoveryID)
	if err != nil {
		stop()
		return err
	}
	var candidates []*model.Job
	for rows.Next() {
		job, err := scanMessageExportJob(rows)
		if err != nil {
			_ = rows.Close()
			stop()
			return err
		}
		candidates = append(candidates, job)
	}
	err = errors.Join(rows.Err(), rows.Close())
	stop()
	if err != nil {
		return err
	}
	// Rotate past live owners so they cannot starve later candidates forever.
	w.recoveryAt, w.recoveryID = 0, ""
	if len(candidates) == 100 {
		last := candidates[len(candidates)-1]
		w.recoveryAt, w.recoveryID = last.CreateAt, last.Id
	}
	for _, job := range candidates {
		if ctx.Err() != nil || !w.IsEnabled(w.jobServer.Config()) {
			return ctx.Err()
		}
		owner, acquired, err := w.acquireJobOwner(ctx, job.Id)
		if err != nil {
			return err
		}
		if !acquired {
			continue
		}
		to := model.JobStatusPending
		if job.Status == model.JobStatusCancelRequested {
			to = model.JobStatusCanceled
		}
		query, stop := owner.queryContext()
		updated, err := scanMessageExportJob(owner.db.QueryRowContext(query, `UPDATE Jobs SET
            Status=$3,LastActivityAt=$4 WHERE Id=$1 AND Status=$2 AND Type=$5 AND Data->>$6=$7
            AND `+owner.ownedSQL+` RETURNING `+messageExportJobColumns, job.Id, job.Status, to,
			model.GetMillis(), model.JobTypeMessageExport, messageExportAttemptKey, job.Data[messageExportAttemptKey]))
		stop()
		owner.Close()
		if err != nil {
			return err
		}
		if updated != nil {
			if err := w.publishJob(updated); err != nil {
				return err
			}
		}
	}
	return nil
}
