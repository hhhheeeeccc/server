package partial_search

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/public/shared/request"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/platform/services/searchengine"
)

const (
	queryTimeout        = 15 * time.Second
	optimizationTimeout = 2 * time.Minute
	maximumPageSize     = 100
)

var quotedTermsPattern = regexp.MustCompile(`"[^"]+"`)

const arabicSearchDiacritics = "ـًٌٍؘَؙُؚِّْٰٕٖٜٟۣ۪ۭؐؑؒؓؔؕؖؗٓٔٗ٘ٙٚٛٝٞۖۗۘۙۚۛۜ۟۠ۡۢۤۧۨ۫۬"

var arabicSearchLetterReplacer = strings.NewReplacer(
	"أ", "ا",
	"إ", "ا",
	"آ", "ا",
	"ٱ", "ا",
	"ؤ", "و",
	"ئ", "ي",
	"ء", "",
)

// Service supplies the partial PostgreSQL search used by the original
// Workspace project without changing Mattermost's SQL store.
type Service struct {
	app *app.App

	started atomic.Bool
	healthy atomic.Bool

	optimizationOnce sync.Once
}

var _ searchengine.SearchEngineInterface = (*Service)(nil)

// Register replaces only the search-engine slot. Mattermost's search layer
// still resolves the caller's channel memberships before invoking SearchPosts.
func Register(a *app.App) {
	// Keep Mattermost's Elasticsearch/OpenSearch implementation when the
	// administrator explicitly configured it before startup.
	if configuredSearchEngineEnabled(a.Config()) {
		a.Log().Info("Workspace partial search skipped because an external search engine is configured")
		return
	}

	service := &Service{app: a}
	a.SearchEngine().RegisterElasticsearchEngine(service)
	service.registerSearchRoute()

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	if appErr := service.Start(ctx); appErr != nil {
		a.Log().Error("Unable to start Workspace partial search", mlog.Err(appErr))
	}
}

func configuredSearchEngineEnabled(cfg *model.Config) bool {
	if cfg == nil {
		return false
	}
	settings := cfg.ElasticsearchSettings
	return model.SafeDereference(settings.EnableIndexing) ||
		model.SafeDereference(settings.EnableSearching) ||
		model.SafeDereference(settings.EnableAutocomplete)
}

func (s *Service) Start(ctx context.Context) *model.AppError {
	db := s.database()
	if db == nil {
		s.started.Store(false)
		s.healthy.Store(false)
		return searchError("PartialSearch.Start", fmt.Errorf("database is unavailable"))
	}
	if err := db.PingContext(ctx); err != nil {
		s.started.Store(false)
		s.healthy.Store(false)
		return searchError("PartialSearch.Start", err)
	}

	s.started.Store(true)
	s.healthy.Store(true)
	return nil
}

func (s *Service) Stop() *model.AppError {
	s.started.Store(false)
	s.healthy.Store(false)
	return nil
}

func (s *Service) HealthCheck(rctx request.CTX) *model.AppError {
	db := s.database()
	if db == nil {
		return searchError("PartialSearch.HealthCheck", fmt.Errorf("database is unavailable"))
	}
	if err := db.PingContext(rctx.Context()); err != nil {
		return searchError("PartialSearch.HealthCheck", err)
	}
	return nil
}

func (s *Service) GetFullVersion() string { return "1.0.0" }
func (s *Service) GetVersion() int        { return 1 }
func (s *Service) GetPlugins() []string   { return []string{} }
func (s *Service) GetName() string        { return "workspace_partial_search" }

func (s *Service) UpdateConfig(_ *model.Config) {}

func (s *Service) IsEnabled() bool { return true }

func (s *Service) IsActive() bool {
	return s.started.Load() && s.app.License() != nil
}

func (s *Service) IsHealthy() bool { return s.healthy.Load() }

func (s *Service) SetHealthy(healthy bool) { s.healthy.Store(healthy) }

func (s *Service) IsIndexingEnabled() bool       { return false }
func (s *Service) IsSearchEnabled() bool         { return s.IsActive() }
func (s *Service) IsAutocompletionEnabled() bool { return s.IsActive() }
func (s *Service) IsIndexingSync() bool          { return false }

func (s *Service) SearchPosts(channels model.ChannelList, searchParams []*model.SearchParams, page, perPage int) ([]string, model.PostSearchMatches, *model.AppError) {
	if !s.IsActive() || len(channels) == 0 || len(searchParams) == 0 {
		return []string{}, model.PostSearchMatches{}, nil
	}

	channelIDs := channelIDs(channels)
	if len(channelIDs) == 0 {
		return []string{}, model.PostSearchMatches{}, nil
	}

	s.ensureTrigramIndex()
	query, args := buildPostQuery(channelIDs, searchParams, page, perPage)
	ids, err := s.queryIDs(query, args...)
	if err != nil {
		return nil, nil, searchError("PartialSearch.SearchPosts", err)
	}
	return ids, model.PostSearchMatches{}, nil
}

func (s *Service) SearchFiles(channels model.ChannelList, searchParams []*model.SearchParams, page, perPage int) ([]string, *model.AppError) {
	if !s.IsActive() || len(channels) == 0 || len(searchParams) == 0 {
		return []string{}, nil
	}

	channelIDs := channelIDs(channels)
	if len(channelIDs) == 0 {
		return []string{}, nil
	}

	query, args := buildFileQuery(channelIDs, searchParams, page, perPage)
	ids, err := s.queryIDs(query, args...)
	if err != nil {
		return nil, searchError("PartialSearch.SearchFiles", err)
	}
	return ids, nil
}

func (s *Service) database() *sql.DB {
	if s.app == nil || s.app.Srv() == nil || s.app.Srv().Store() == nil {
		return nil
	}
	if db := s.app.Srv().Store().GetInternalReplicaDB(); db != nil {
		return db
	}
	return s.app.Srv().Store().GetInternalMasterDB()
}

func (s *Service) queryIDs(query string, args ...any) ([]string, error) {
	return s.queryIDsContext(context.Background(), query, args...)
}

func (s *Service) queryIDsContext(parent context.Context, query string, args ...any) ([]string, error) {
	db := s.database()
	if db == nil {
		return nil, fmt.Errorf("database is unavailable")
	}

	ctx, cancel := context.WithTimeout(parent, queryTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// The index is the same optimization used by the original Workspace server.
// ILIKE remains functional when the database role cannot install extensions;
// only the acceleration is skipped in that case.
func (s *Service) ensureTrigramIndex() {
	s.optimizationOnce.Do(func() {
		s.app.Srv().Go(func() {
			db := s.app.Srv().Store().GetInternalMasterDB()
			if db == nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), optimizationTimeout)
			defer cancel()
			if _, err := db.ExecContext(ctx, "CREATE EXTENSION IF NOT EXISTS pg_trgm"); err != nil {
				s.app.Log().Warn("Workspace partial search is running without pg_trgm", mlog.Err(err))
				return
			}
			if err := ensurePostTrigramIndex(ctx, db); err != nil {
				s.app.Log().Warn("Unable to create Workspace partial-search index", mlog.Err(err))
			}
		})
	})
}

func searchError(where string, err error) *model.AppError {
	return model.NewAppError(where, "workspace.partial_search.database_error", nil, err.Error(), http.StatusInternalServerError).Wrap(err)
}

type sqlQueryBuilder struct {
	args []any
}

func (b *sqlQueryBuilder) bind(value any) string {
	b.args = append(b.args, value)
	return fmt.Sprintf("$%d", len(b.args))
}

func (b *sqlQueryBuilder) in(column string, values []string, excluded bool) string {
	if len(values) == 0 {
		return ""
	}
	placeholders := make([]string, 0, len(values))
	for _, value := range values {
		placeholders = append(placeholders, b.bind(value))
	}
	op := " IN "
	if excluded {
		op = " NOT IN "
	}
	return column + op + "(" + strings.Join(placeholders, ", ") + ")"
}

func buildPostQuery(channelIDs []string, paramsList []*model.SearchParams, page, perPage int) (string, []any) {
	builder := &sqlQueryBuilder{}
	conditions := []string{
		"p.deleteat = 0",
		builder.in("p.channelid", channelIDs, false),
		"p.type NOT LIKE " + builder.bind(model.PostSystemMessagePrefix+"%"),
		"p.type <> " + builder.bind(model.PostTypeCard),
		"p.type <> " + builder.bind(model.PostTypeBurnOnRead),
	}

	groups := make([]string, 0, len(paramsList))
	for _, params := range paramsList {
		if params == nil {
			continue
		}
		groups = append(groups, "("+buildParamClause(builder, params, postColumns)+")")
	}
	if len(groups) > 0 {
		conditions = append(conditions, "("+strings.Join(groups, " OR ")+")")
	}

	limit, offset := pagination(page, perPage)
	query := "SELECT p.id FROM posts p WHERE " + strings.Join(nonEmpty(conditions), " AND ") +
		" ORDER BY p.createat DESC, p.id DESC LIMIT " + builder.bind(limit) +
		" OFFSET " + builder.bind(offset)
	return query, builder.args
}

func buildFileQuery(channelIDs []string, paramsList []*model.SearchParams, page, perPage int) (string, []any) {
	builder := &sqlQueryBuilder{}
	conditions := []string{
		"f.deleteat = 0",
		builder.in("f.channelid", channelIDs, false),
		"(f.creatorid = " + builder.bind(model.BookmarkFileOwner) + " OR f.postid <> '')",
		"NOT EXISTS (SELECT 1 FROM temporaryposts tp WHERE tp.postid = f.postid)",
	}

	groups := make([]string, 0, len(paramsList))
	for _, params := range paramsList {
		if params == nil {
			continue
		}
		groups = append(groups, "("+buildParamClause(builder, params, fileColumns)+")")
	}
	if len(groups) > 0 {
		conditions = append(conditions, "("+strings.Join(groups, " OR ")+")")
	}

	limit, offset := pagination(page, perPage)
	query := "SELECT f.id FROM fileinfo f WHERE " + strings.Join(nonEmpty(conditions), " AND ") +
		" ORDER BY f.createat DESC, f.id DESC LIMIT " + builder.bind(limit) +
		" OFFSET " + builder.bind(offset)
	return query, builder.args
}

type searchableColumns struct {
	message   string
	hashtags  string
	userID    string
	channelID string
	createAt  string
	extension string
}

var postColumns = searchableColumns{
	message:   "p.message",
	hashtags:  "p.hashtags",
	userID:    "p.userid",
	channelID: "p.channelid",
	createAt:  "p.createat",
}

var fileColumns = searchableColumns{
	message:   "(COALESCE(f.name, '') || ' ' || COALESCE(f.content, ''))",
	hashtags:  "(COALESCE(f.name, '') || ' ' || COALESCE(f.content, ''))",
	userID:    "f.creatorid",
	channelID: "f.channelid",
	createAt:  "f.createat",
	extension: "f.extension",
}

func buildParamClause(builder *sqlQueryBuilder, params *model.SearchParams, columns searchableColumns) string {
	conditions := make([]string, 0, 12)
	conditions = append(conditions,
		builder.in(columns.channelID, params.InChannels, false),
		builder.in(columns.channelID, params.ExcludedChannels, true),
		builder.in(columns.userID, params.FromUsers, false),
		builder.in(columns.userID, params.ExcludedUsers, true),
	)
	if columns.extension != "" {
		conditions = append(conditions,
			builder.in(columns.extension, params.Extensions, false),
			builder.in(columns.extension, params.ExcludedExtensions, true),
		)
	}

	appendDateConditions(builder, &conditions, columns.createAt, params)

	searchColumn := columns.message
	if params.IsHashtag {
		searchColumn = columns.hashtags
	}
	terms := extractTerms(params.Terms)
	if len(terms) > 0 {
		termConditions := make([]string, 0, len(terms))
		for _, term := range terms {
			termConditions = append(termConditions, likeCondition(builder, searchColumn, term, false))
		}
		operator := " AND "
		if params.OrTerms {
			operator = " OR "
		}
		conditions = append(conditions, "("+strings.Join(termConditions, operator)+")")
	}
	for _, term := range extractTerms(params.ExcludedTerms) {
		conditions = append(conditions, likeCondition(builder, searchColumn, term, true))
	}

	conditions = nonEmpty(conditions)
	if len(conditions) == 0 {
		return "TRUE"
	}
	return strings.Join(conditions, " AND ")
}

func appendDateConditions(builder *sqlQueryBuilder, conditions *[]string, column string, params *model.SearchParams) {
	if params.OnDate != "" {
		start, end := params.GetOnDateMillis()
		*conditions = append(*conditions, column+" BETWEEN "+builder.bind(start)+" AND "+builder.bind(end))
		return
	}
	if params.ExcludedDate != "" {
		start, end := params.GetExcludedDateMillis()
		*conditions = append(*conditions, column+" NOT BETWEEN "+builder.bind(start)+" AND "+builder.bind(end))
	}
	if params.AfterDate != "" {
		*conditions = append(*conditions, column+" >= "+builder.bind(params.GetAfterDateMillis()))
	}
	if params.BeforeDate != "" {
		*conditions = append(*conditions, column+" <= "+builder.bind(params.GetBeforeDateMillis()))
	}
	if params.ExcludedAfterDate != "" {
		*conditions = append(*conditions, column+" < "+builder.bind(params.GetExcludedAfterDateMillis()))
	}
	if params.ExcludedBeforeDate != "" {
		*conditions = append(*conditions, column+" > "+builder.bind(params.GetExcludedBeforeDateMillis()))
	}
}

func likeCondition(builder *sqlQueryBuilder, column, term string, excluded bool) string {
	operator := " ILIKE "
	if excluded {
		operator = " NOT ILIKE "
	}
	return column + operator + builder.bind("%"+escapeLike(term)+"%") + ` ESCAPE '\'`
}

func extractTerms(input string) []string {
	input = normalizeSpecialSearchCharacters(input)
	terms := make([]string, 0)
	for _, quoted := range quotedTermsPattern.FindAllString(input, -1) {
		term := strings.TrimSpace(strings.Trim(quoted, `"`))
		if term != "" {
			terms = append(terms, term)
		}
	}
	remaining := quotedTermsPattern.ReplaceAllString(input, " ")
	terms = append(terms, strings.Fields(remaining)...)
	return terms
}

func normalizeSpecialSearchCharacters(input string) string {
	return strings.NewReplacer(
		"<", " ",
		">", " ",
		"+", " ",
		"(", " ",
		")", " ",
		"~", " ",
		":", " ",
	).Replace(input)
}

func escapeLike(term string) string {
	term = strings.ReplaceAll(term, `\`, `\\`)
	term = strings.ReplaceAll(term, "%", `\%`)
	return strings.ReplaceAll(term, "_", `\_`)
}

func normalizeArabicForSearch(value string) string {
	value = arabicSearchLetterReplacer.Replace(value)
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(arabicSearchDiacritics, r) {
			return -1
		}
		return r
	}, value)
}

// normalizedArabicSQL mirrors normalizeArabicForSearch for trusted column
// expressions. The user-facing value remains unchanged in the database.
func normalizedArabicSQL(column string) string {
	return "translate(translate(COALESCE(" + column + ", ''), 'أإآٱؤئء', 'ااااوي'), '" + arabicSearchDiacritics + "', '')"
}

func channelIDs(channels model.ChannelList) []string {
	seen := make(map[string]struct{}, len(channels))
	ids := make([]string, 0, len(channels))
	for _, channel := range channels {
		if channel == nil || channel.Id == "" {
			continue
		}
		if _, ok := seen[channel.Id]; ok {
			continue
		}
		seen[channel.Id] = struct{}{}
		ids = append(ids, channel.Id)
	}
	return ids
}

func pagination(page, perPage int) (int, int) {
	if page < 0 {
		page = 0
	}
	if perPage <= 0 || perPage > maximumPageSize {
		perPage = maximumPageSize
	}
	return perPage, page * perPage
}

func buildUserQuery(teamID, channelID string, restrictedToChannels []string, term string, options *model.UserSearchOptions, inChannel bool) (string, []any) {
	builder := &sqlQueryBuilder{}
	conditions := make([]string, 0, 12)

	if !options.AllowInactive {
		conditions = append(conditions, "u.deleteat = 0")
	}
	if teamID != "" {
		conditions = append(conditions, "EXISTS (SELECT 1 FROM teammembers tm WHERE tm.userid = u.id AND tm.teamid = "+builder.bind(teamID)+" AND tm.deleteat = 0)")
	}
	if channelID != "" {
		membership := "EXISTS"
		if !inChannel {
			membership = "NOT EXISTS"
		}
		conditions = append(conditions, membership+" (SELECT 1 FROM channelmembers cm WHERE cm.userid = u.id AND cm.channelid = "+builder.bind(channelID)+")")
	}
	if restrictedToChannels != nil {
		conditions = append(conditions, "EXISTS (SELECT 1 FROM channelmembers rcm WHERE rcm.userid = u.id AND "+builder.in("rcm.channelid", restrictedToChannels, false)+")")
	}

	conditions = append(conditions, buildUserTermConditions(builder, term, options))
	conditions = append(conditions, buildUserRoleConditions(builder, teamID, channelID, restrictedToChannels, options))
	if options.GroupConstrained {
		if channelID != "" {
			conditions = append(conditions, `u.id IN (
				SELECT gm.userid
				FROM groupchannels gc
				JOIN usergroups ug ON ug.id = gc.groupid
				JOIN groupmembers gm ON gm.groupid = ug.id
				WHERE gc.channelid = `+builder.bind(channelID)+` AND gc.deleteat = 0 AND ug.deleteat = 0 AND gm.deleteat = 0
			)`)
		} else if teamID != "" {
			conditions = append(conditions, `u.id IN (
				SELECT gm.userid
				FROM groupteams gt
				JOIN usergroups ug ON ug.id = gt.groupid
				JOIN groupmembers gm ON gm.groupid = ug.id
				WHERE gt.teamid = `+builder.bind(teamID)+` AND gt.deleteat = 0 AND ug.deleteat = 0 AND gm.deleteat = 0
			)`)
		}
	}

	limit := options.Limit
	if limit <= 0 {
		limit = model.UserSearchDefaultLimit
	}
	if limit > model.UserSearchMaxLimit {
		limit = model.UserSearchMaxLimit
	}
	conditions = nonEmpty(conditions)
	if len(conditions) == 0 {
		conditions = append(conditions, "TRUE")
	}

	query := "SELECT u.id FROM users u WHERE " + strings.Join(conditions, " AND ") +
		" ORDER BY LOWER(u.username) ASC, u.id ASC LIMIT " + builder.bind(limit)
	return query, builder.args
}

func buildUserTermConditions(builder *sqlQueryBuilder, term string, options *model.UserSearchOptions) string {
	fields := []string{"u.username", "u.nickname"}
	if options.AllowFullNames {
		fields = append(fields, "u.firstname", "u.lastname")
	}
	if options.AllowEmails {
		fields = append(fields, "u.email")
	}

	termGroups := make([]string, 0)
	for _, rawTerm := range strings.Fields(normalizeArabicForSearch(term)) {
		cleanedTerm := strings.TrimLeft(rawTerm, "@")
		if cleanedTerm == "" {
			continue
		}
		matches := make([]string, 0, len(fields)+1)
		for _, field := range fields {
			matches = append(matches, normalizedArabicSQL(field)+" ILIKE "+builder.bind("%"+escapeLike(cleanedTerm)+"%")+` ESCAPE '\'`)
		}
		matches = append(matches, "u.id = "+builder.bind(cleanedTerm))
		termGroups = append(termGroups, "("+strings.Join(matches, " OR ")+")")
	}
	return strings.Join(termGroups, " AND ")
}

func buildUserRoleConditions(builder *sqlQueryBuilder, teamID, channelID string, restrictedToChannels []string, options *model.UserSearchOptions) string {
	conditions := make([]string, 0, 2)
	if options.Role != "" {
		conditions = append(conditions, roleTokenCondition(builder, "u.roles", options.Role))
	}

	multiRoles := make([]string, 0, len(options.Roles)+2)
	for _, role := range options.Roles {
		switch role {
		case model.SystemUserRoleId:
			multiRoles = append(multiRoles, "u.roles = "+builder.bind(role))
		case model.SystemGuestRoleId,
			model.SystemAdminRoleId,
			model.SystemUserManagerRoleId,
			model.SystemReadOnlyAdminRoleId,
			model.SystemManagerRoleId,
			model.SystemCustomGroupAdminRoleId,
			model.SharedChannelManagerRoleId:
			multiRoles = append(multiRoles, roleTokenCondition(builder, "u.roles", role))
		}
	}

	teamRoles := membershipRoleConditions(builder, "tm", options.TeamRoles, true)
	if len(teamRoles) > 0 && teamID != "" {
		multiRoles = append(multiRoles, "EXISTS (SELECT 1 FROM teammembers tm WHERE tm.userid = u.id AND tm.teamid = "+builder.bind(teamID)+" AND tm.deleteat = 0 AND ("+strings.Join(teamRoles, " OR ")+"))")
	}

	channelRoles := membershipRoleConditions(builder, "cm", options.ChannelRoles, false)
	if len(channelRoles) > 0 {
		channelFilter := ""
		if channelID != "" {
			channelFilter = " AND cm.channelid = " + builder.bind(channelID)
		} else if restrictedToChannels != nil && len(restrictedToChannels) > 0 {
			channelFilter = " AND " + builder.in("cm.channelid", restrictedToChannels, false)
		}
		multiRoles = append(multiRoles, "EXISTS (SELECT 1 FROM channelmembers cm WHERE cm.userid = u.id"+channelFilter+" AND ("+strings.Join(channelRoles, " OR ")+"))")
	}
	if len(multiRoles) > 0 {
		conditions = append(conditions, "("+strings.Join(multiRoles, " OR ")+")")
	}

	return strings.Join(conditions, " AND ")
}

func membershipRoleConditions(builder *sqlQueryBuilder, alias string, roles []string, team bool) []string {
	conditions := make([]string, 0, len(roles))
	for _, role := range roles {
		switch role {
		case model.TeamAdminRoleId, model.ChannelAdminRoleId:
			conditions = append(conditions, "COALESCE("+alias+".schemeadmin, false) AND NOT "+roleTokenCondition(builder, "u.roles", model.SystemAdminRoleId))
		case model.TeamUserRoleId, model.ChannelUserRoleId:
			conditions = append(conditions, "COALESCE("+alias+".schemeuser, false) AND NOT COALESCE("+alias+".schemeadmin, false) AND NOT "+roleTokenCondition(builder, "u.roles", model.SystemAdminRoleId))
		case model.TeamGuestRoleId:
			if team {
				conditions = append(conditions, "COALESCE("+alias+".schemeguest, false)")
			}
		case model.ChannelGuestRoleId:
			if !team {
				conditions = append(conditions, "COALESCE("+alias+".schemeguest, false)")
			}
		}
	}
	return conditions
}

func roleTokenCondition(builder *sqlQueryBuilder, column, role string) string {
	return "(' ' || COALESCE(" + column + ", '') || ' ') ILIKE " + builder.bind("% "+escapeLike(role)+" %") + ` ESCAPE '\'`
}

func nonEmpty(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func (s *Service) IndexPost(_ *model.Post, _, _ string) *model.AppError   { return nil }
func (s *Service) DeletePost(_ *model.Post) *model.AppError               { return nil }
func (s *Service) DeleteChannelPosts(request.CTX, string) *model.AppError { return nil }
func (s *Service) UpdatePostsChannelTypeByChannelId(request.CTX, string, string) *model.AppError {
	return nil
}
func (s *Service) BackfillPostsChannelType(request.CTX, []string, string) *model.AppError {
	return nil
}
func (s *Service) DeleteUserPosts(request.CTX, string) *model.AppError { return nil }
func (s *Service) IndexChannel(request.CTX, *model.Channel, []string, []string) *model.AppError {
	return nil
}
func (s *Service) SyncBulkIndexChannels(request.CTX, []*model.Channel, func(*model.Channel) ([]string, error), []string) *model.AppError {
	return nil
}
func (s *Service) SearchChannels(teamID, userID, term string, isGuest, includeDeleted bool) ([]string, *model.AppError) {
	if !s.IsActive() {
		return []string{}, nil
	}

	builder := &sqlQueryBuilder{}
	conditions := []string{
		"c.type IN (" + builder.bind(model.ChannelTypeOpen) + ", " + builder.bind(model.ChannelTypePrivate) + ")",
	}
	if teamID != "" {
		conditions = append(conditions, "c.teamid = "+builder.bind(teamID))
	} else {
		conditions = append(conditions, "EXISTS (SELECT 1 FROM teammembers tm WHERE tm.teamid = c.teamid AND tm.userid = "+builder.bind(userID)+" AND tm.deleteat = 0)")
	}
	if !includeDeleted {
		conditions = append(conditions, "c.deleteat = 0")
	}
	if isGuest {
		conditions = append(conditions, "c.type = "+builder.bind(model.ChannelTypeOpen))
	} else {
		conditions = append(conditions, "(c.type <> "+builder.bind(model.ChannelTypePrivate)+" OR EXISTS (SELECT 1 FROM channelmembers cm WHERE cm.channelid = c.id AND cm.userid = "+builder.bind(userID)+"))")
	}
	if normalizedTerm := strings.TrimSpace(normalizeArabicForSearch(term)); normalizedTerm != "" {
		likeTerm := "%" + escapeLike(normalizedTerm) + "%"
		fields := []string{"c.name", "c.displayname", "c.purpose", "c.header"}
		matches := make([]string, 0, len(fields))
		for _, field := range fields {
			matches = append(matches, normalizedArabicSQL(field)+" ILIKE "+builder.bind(likeTerm)+` ESCAPE '\'`)
		}
		conditions = append(conditions, "("+strings.Join(matches, " OR ")+")")
	}

	query := "SELECT c.id FROM channels c WHERE " + strings.Join(conditions, " AND ") +
		" ORDER BY c.displayname ASC, c.id ASC LIMIT " + builder.bind(model.ChannelSearchDefaultLimit)
	ids, err := s.queryIDs(query, builder.args...)
	if err != nil {
		return nil, searchError("PartialSearch.SearchChannels", err)
	}
	return ids, nil
}
func (s *Service) DeleteChannel(*model.Channel) *model.AppError { return nil }
func (s *Service) IndexUser(request.CTX, *model.User, []string, []string) *model.AppError {
	return nil
}
func (s *Service) SearchUsersInChannel(teamID, channelID string, restrictedToChannels []string, term string, options *model.UserSearchOptions) ([]string, []string, *model.AppError) {
	if !s.IsActive() || (restrictedToChannels != nil && len(restrictedToChannels) == 0) {
		return []string{}, []string{}, nil
	}
	if options == nil {
		options = &model.UserSearchOptions{}
	}

	// The caller already proved read access to channelID. Match Mattermost's
	// Elasticsearch behavior by keeping the in-channel half tied only to that
	// channel; team/view restrictions apply to the out-of-channel candidates.
	inChannelQuery, inChannelArgs := buildUserQuery("", channelID, nil, term, options, true)
	inChannel, err := s.queryIDs(inChannelQuery, inChannelArgs...)
	if err != nil {
		return nil, nil, searchError("PartialSearch.SearchUsersInChannel", err)
	}

	outOfChannelQuery, outOfChannelArgs := buildUserQuery(teamID, channelID, restrictedToChannels, term, options, false)
	outOfChannel, err := s.queryIDs(outOfChannelQuery, outOfChannelArgs...)
	if err != nil {
		return nil, nil, searchError("PartialSearch.SearchUsersInChannel", err)
	}
	return inChannel, outOfChannel, nil
}
func (s *Service) SearchUsersInTeam(teamID string, restrictedToChannels []string, term string, options *model.UserSearchOptions) ([]string, *model.AppError) {
	if !s.IsActive() || (restrictedToChannels != nil && len(restrictedToChannels) == 0) {
		return []string{}, nil
	}
	if options == nil {
		options = &model.UserSearchOptions{}
	}

	query, args := buildUserQuery(teamID, "", restrictedToChannels, term, options, true)
	ids, err := s.queryIDs(query, args...)
	if err != nil {
		return nil, searchError("PartialSearch.SearchUsersInTeam", err)
	}
	return ids, nil
}
func (s *Service) DeleteUser(*model.User) *model.AppError                     { return nil }
func (s *Service) IndexFile(*model.FileInfo, string) *model.AppError          { return nil }
func (s *Service) DeleteFile(string) *model.AppError                          { return nil }
func (s *Service) DeletePostFiles(request.CTX, string) *model.AppError        { return nil }
func (s *Service) DeleteUserFiles(request.CTX, string) *model.AppError        { return nil }
func (s *Service) DeleteFilesBatch(request.CTX, int64, int64) *model.AppError { return nil }
func (s *Service) TestConfig(rctx request.CTX, _ *model.Config) *model.AppError {
	return s.HealthCheck(rctx)
}
func (s *Service) PurgeIndexes(request.CTX) *model.AppError                          { return nil }
func (s *Service) PurgeIndexList(request.CTX, []string) *model.AppError              { return nil }
func (s *Service) RefreshIndexes(request.CTX) *model.AppError                        { return nil }
func (s *Service) DataRetentionDeleteIndexes(request.CTX, time.Time) *model.AppError { return nil }
