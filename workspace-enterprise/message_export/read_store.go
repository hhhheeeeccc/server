package message_export

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"
	"github.com/lib/pq"
	"github.com/mattermost/enterprise/message_export/shared"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/store"
	sq "github.com/mattermost/squirrel"
)

// Keep native interfaces available to older callers. Only this worker opts in
// to the private reader; no application store or pool is replaced.
type exportReadStore struct {
	shared.MessageExportStore
	reader *messageExportReader
}

var _ shared.ContextualMetadataStore = (*exportReadStore)(nil)

func (s *exportReadStore) ReadQuery(rctx request.CTX, run func(context.Context, *sql.DB) error) error {
	return s.reader.query(rctx.Context(), run)
}

func (s *exportReadStore) selectRows(rctx request.CTX, dest any, query string, args ...any) error {
	return s.ReadQuery(rctx, func(ctx context.Context, db *sql.DB) error {
		return sqlx.NewDb(db, "postgres").SelectContext(ctx, dest, query, args...)
	})
}

// Filters match SqlPostStore.AnalyticsPostCount, including equal-timestamp IDs
// and the optional upper bound. Reads use the primary alongside the job cursor.
func (s *exportReadStore) ExportPostCount(rctx request.CTX, options *model.PostCountOptions) (int64, error) {
	query := sq.StatementBuilder.PlaceholderFormat(sq.Dollar).Select("COUNT(*)").From("Posts p")
	if options.TeamId != "" {
		query = query.Join("Channels c ON c.Id=p.ChannelId").Where(sq.Eq{"c.TeamId": options.TeamId})
	}
	if options.UsersPostsOnly {
		query = query.Where(sq.And{sq.Eq{"p.Type": ""}, sq.Expr("p.UserId NOT IN (SELECT UserId FROM Bots)")})
	}
	if options.MustHaveFile {
		query = query.Where(sq.Or{sq.NotEq{"p.FileIds": "[]"}, sq.NotEq{"p.Filenames": "[]"}})
	}
	if options.MustHaveHashtag {
		query = query.Where(sq.NotEq{"p.Hashtags": ""})
	}
	if options.ExcludeDeleted {
		query = query.Where(sq.Eq{"p.DeleteAt": 0})
	}
	if options.ExcludeSystemPosts {
		query = query.Where("p.Type NOT LIKE 'system_%'")
	}
	if options.SinceUpdateAt > 0 {
		query = query.Where(sq.Or{sq.Gt{"p.UpdateAt": options.SinceUpdateAt},
			sq.And{sq.Eq{"p.UpdateAt": options.SinceUpdateAt}, sq.Gt{"p.Id": options.SincePostID}}})
	}
	if options.UntilUpdateAt > 0 {
		query = query.Where(sq.LtOrEq{"p.UpdateAt": options.UntilUpdateAt})
	}
	statement, args, err := query.ToSql()
	if err != nil {
		return 0, err
	}
	var count int64
	err = s.ReadQuery(rctx, func(ctx context.Context, db *sql.DB) error {
		return db.QueryRowContext(ctx, statement, args...).Scan(&count)
	})
	return count, err
}

func (s *exportReadStore) ExportActiveChannels(rctx request.CTX, start, end int64) ([]string, error) {
	ids := make([]string, 0)
	err := s.selectRows(rctx, &ids, `SELECT p.ChannelId FROM Posts p
        WHERE p.UpdateAt >= $1 AND p.UpdateAt <= $2 AND p.Type NOT LIKE 'system_%'
        UNION SELECT cmh.ChannelId FROM ChannelMemberHistory cmh
        WHERE (cmh.JoinTime >= $1 AND cmh.JoinTime <= $2)
           OR (cmh.LeaveTime >= $1 AND cmh.LeaveTime <= $2)`, start, end)
	return ids, err
}

func (s *exportReadStore) ExportChannels(rctx request.CTX, ids []string) ([]shared.ExportChannel, error) {
	channels := make([]shared.ExportChannel, 0)
	err := s.selectRows(rctx, &channels, `SELECT Id,TeamId,Name,DisplayName,Type FROM Channels
        WHERE Id=ANY($1) AND Type IN ('O','P','D','G')`, pq.Array(ids))
	if err == nil && len(channels) == 0 {
		err = store.NewErrNotFound("Channel", fmt.Sprintf("ids=%v", ids))
	}
	return channels, err
}

func (s *exportReadStore) ExportMembershipHistory(rctx request.CTX, start, end int64, ids []string) ([]*model.ChannelMemberHistoryResult, error) {
	histories := make([]*model.ChannelMemberHistoryResult, 0)
	// Preserve the native pre-history fallback, including NULL leave times
	// and the exact join/leave boundary comparisons.
	err := s.ReadQuery(rctx, func(ctx context.Context, db *sql.DB) error {
		var minimum sql.NullInt64
		if err := db.QueryRowContext(ctx, `SELECT MIN(JoinTime) FROM ChannelMemberHistory`).Scan(&minimum); err != nil {
			return err
		}
		connection := sqlx.NewDb(db, "postgres")
		if minimum.Valid && minimum.Int64 <= start {
			return connection.SelectContext(ctx, &histories, `SELECT cmh.ChannelId,cmh.UserId,cmh.JoinTime,cmh.LeaveTime,
                u.Email AS "Email",u.Username,(b.UserId IS NOT NULL) AS IsBot,u.DeleteAt AS UserDeleteAt
                FROM ChannelMemberHistory cmh JOIN Users u ON cmh.UserId=u.Id
                LEFT JOIN Bots b ON b.UserId=u.Id WHERE cmh.ChannelId=ANY($3)
                AND cmh.JoinTime <= $2 AND (cmh.LeaveTime IS NULL OR cmh.LeaveTime >= $1)
                ORDER BY cmh.JoinTime ASC`, start, end, pq.Array(ids))
		}
		if err := connection.SelectContext(ctx, &histories, `SELECT DISTINCT ch.ChannelId,ch.UserId,
            u.Email AS "Email",u.Username,(b.UserId IS NOT NULL) AS IsBot,u.DeleteAt AS UserDeleteAt
            FROM ChannelMembers ch JOIN Users u ON ch.UserId=u.Id LEFT JOIN Bots b ON b.UserId=u.Id
            WHERE ch.ChannelId=ANY($1)`, pq.Array(ids)); err != nil {
			return err
		}
		for _, history := range histories {
			history.JoinTime = start
			history.LeaveTime = model.NewPointer(end)
		}
		return nil
	})
	return histories, err
}

func (s *exportReadStore) FileInfo() shared.MEFileInfoStore { return exportFileInfoReader{s} }

type exportFileInfoReader struct{ source *exportReadStore }

func (r exportFileInfoReader) GetForPost(id string, _, includeDeleted, _ bool) ([]*model.FileInfo, error) {
	infos := make([]*model.FileInfo, 0)
	query := `SELECT Id,CreatorId,PostId,COALESCE(ChannelId,'') AS ChannelId,CreateAt,UpdateAt,DeleteAt,
        Path,ThumbnailPath,PreviewPath,Name,Extension,Size,MimeType,Width,Height,HasPreviewImage,MiniPreview,
        COALESCE(Content,'') AS Content,COALESCE(RemoteId,'') AS RemoteId,Archived FROM FileInfo WHERE PostId=$1`
	if !includeDeleted {
		query += ` AND DeleteAt=0`
	}
	query += ` ORDER BY CreateAt`
	err := r.source.selectRows(request.EmptyContext(nil).WithContext(r.source.reader.ctx), &infos, query, id)
	return infos, err
}

func (s *exportReadStore) Compliance() store.ComplianceStore {
	return exportComplianceReader{ComplianceStore: s.MessageExportStore.Compliance(), source: s}
}

type exportComplianceReader struct {
	store.ComplianceStore
	source *exportReadStore
}

func (r exportComplianceReader) MessageExport(rctx request.CTX, cursor model.MessageExportCursor, limit int) ([]*model.MessageExport, model.MessageExportCursor, error) {
	posts := make([]*model.MessageExport, 0)
	query := `SELECT p.Id AS PostId,p.CreateAt AS PostCreateAt,p.UpdateAt AS PostUpdateAt,
        p.DeleteAt AS PostDeleteAt,p.EditAt AS PostEditAt,p.Message AS PostMessage,p.Type AS PostType,
        p.Props AS PostProps,p.OriginalId AS PostOriginalId,p.RootId AS PostRootId,p.FileIds AS PostFileIds,
        t.Id AS TeamId,t.Name AS TeamName,t.DisplayName AS TeamDisplayName,c.Id AS ChannelId,c.Name AS ChannelName,
        c.Type AS ChannelType,u.Id AS UserId,u.Email AS UserEmail,u.Username,(b.UserId IS NOT NULL) AS IsBot,
        CASE WHEN c.Type='D' THEN 'Direct Message' WHEN c.Type='G' THEN 'Group Message' ELSE c.DisplayName END AS ChannelDisplayName
        FROM Posts p LEFT JOIN Channels c ON p.ChannelId=c.Id LEFT JOIN Teams t ON c.TeamId=t.Id
        LEFT JOIN Users u ON p.UserId=u.Id LEFT JOIN Bots b ON b.UserId=p.UserId
        WHERE (p.UpdateAt>$1 OR (p.UpdateAt=$1 AND p.Id>$2)) AND p.Type NOT LIKE 'system_%'`
	args := []any{cursor.LastPostUpdateAt, cursor.LastPostId}
	if cursor.UntilUpdateAt > 0 {
		query += ` AND p.UpdateAt <= $3`
		args = append(args, cursor.UntilUpdateAt)
	}
	query += fmt.Sprintf(` ORDER BY PostUpdateAt,PostId LIMIT $%d`, len(args)+1)
	args = append(args, limit)
	if err := r.source.selectRows(rctx, &posts, query, args...); err != nil {
		return nil, cursor, err
	}
	if len(posts) > 0 {
		last := posts[len(posts)-1]
		cursor.LastPostUpdateAt, cursor.LastPostId = *last.PostUpdateAt, *last.PostId
	}
	return posts, cursor, nil
}
