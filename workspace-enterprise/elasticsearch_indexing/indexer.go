package elasticsearch_indexing

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esutil"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/channels/jobs"
	ejobs "github.com/mattermost/mattermost/server/v8/einterfaces/jobs"
	"github.com/mattermost/mattermost/server/v8/enterprise/elasticsearch/common"
	core "github.com/mattermost/mattermost/server/v8/enterprise/elasticsearch/elasticsearch"
)

// New uses Enterprise result accounting for OpenSearch and PostgreSQL/ES.
// PostgreSQL indexing omits posts whose channels no longer exist.
func New(server *app.Server) ejobs.IndexerJobInterface {
	if *server.Config().ElasticsearchSettings.Backend == model.ElasticsearchSettingsOSBackend {
		return &OpenSearchIndexer{server: server}
	}
	if server.Config().SqlSettings.DriverName != nil && *server.Config().SqlSettings.DriverName == model.DatabaseDriverPostgres {
		return &Indexer{server: server}
	}
	return &core.ElasticsearchIndexerInterfaceImpl{Server: server}
}

type Indexer struct {
	server *app.Server
}

func (i *Indexer) MakeWorker() model.Worker {
	const workerName = "EnterpriseElasticsearchIndexer"
	server := i.server
	logger := server.Jobs.Logger().With(mlog.String("worker_name", workerName))
	settings := server.Config().ElasticsearchSettings
	fileBackend := server.Platform().FileBackend()

	fence := &indexingFence{enabled: model.SafeDereference(server.Config().SqlSettings.DriverName) == model.DatabaseDriverPostgres}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: *settings.SkipTLSVerification} //nolint:gosec -- administrator setting
	clientConfig := elasticsearch.Config{
		Addresses:     []string{*settings.ConnectionURL},
		Username:      *settings.Username,
		Password:      *settings.Password,
		Transport:     &indexingTransport{base: transport, fence: fence},
		RetryOnStatus: []int{429, 502, 503, 504},
		MaxRetries:    3,
	}
	if *settings.CA != "" {
		ca, err := common.ReadFileSafely(fileBackend, *settings.CA)
		if err != nil {
			logger.Error("Unable to read Elasticsearch CA", mlog.Err(err))
			return nil
		}
		if err := setIndexingTransportCA(transport, ca); err != nil {
			logger.Error("Invalid Elasticsearch CA", mlog.Err(err))
			return nil
		}
	}
	if *settings.ClientCert != "" || *settings.ClientKey != "" {
		certPEM, certErr := common.ReadFileSafely(fileBackend, *settings.ClientCert)
		keyPEM, keyErr := common.ReadFileSafely(fileBackend, *settings.ClientKey)
		if certErr != nil || keyErr != nil {
			logger.Error("Unable to read Elasticsearch client certificate", mlog.Err(certErr), mlog.Err(keyErr))
			return nil
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			logger.Error("Invalid Elasticsearch client certificate", mlog.Err(err))
			return nil
		}
		transport.TLSClientConfig.Certificates = []tls.Certificate{cert}
	}
	jobServer := indexingJobServer(server)
	var client *elasticsearch.Client
	var bulkIndexer esutil.BulkIndexer
	var results *bulkResults
	var session *indexingBulkSession

	return newCheckpointWorker(server, workerName, model.ElasticsearchSettingsESBackend,
		jobServer, logger, fileBackend, server.License,
		func() error {
			// A worker serves multiple jobs. Its callbacks must retain the results
			// for this processor, never a counter shared with earlier jobs.
			runResults := &bulkResults{}
			results = runResults
			session = newIndexingBulkSession(fence, time.Duration(*settings.RequestTimeoutSeconds)*time.Second)
			var err error
			client, err = elasticsearch.NewClient(clientConfig)
			if err != nil {
				session.cancel(err)
				return err
			}
			if *settings.Sniff {
				discovery := client.Transport.(interface{ DiscoverNodesContext(context.Context) error })
				session.discover(discovery.DiscoverNodesContext, 0, func(err error) { logger.Warn("Elasticsearch node discovery failed", mlog.Err(err)) })
			}
			bulkIndexer, err = esutil.NewBulkIndexer(esutil.BulkIndexerConfig{
				Client:       client,
				Timeout:      time.Duration(*settings.RequestTimeoutSeconds) * time.Second,
				NumWorkers:   common.NumIndexWorkers(),
				OnFlushStart: session.startFlush,
				OnFlushEnd:   session.endFlush,
				OnError: func(_ context.Context, err error) {
					runResults.processorError.Store(true)
					logger.Error("Elasticsearch bulk indexing failed", mlog.Err(err))
				},
			})
			if err != nil {
				return errors.Join(err, session.close(func(context.Context) error { return nil }, func() error { return client.Close(context.Background()) }))
			}
			return err
		},
		func(indexName, action, documentID string, body io.ReadSeeker) error {
			runResults := results
			complete := runResults.addItem()
			err := session.add(func(ctx context.Context) error {
				return bulkIndexer.Add(ctx, esutil.BulkIndexerItem{
					Index: indexName, Action: action, DocumentID: documentID, Body: body,
					OnSuccess: func(context.Context, esutil.BulkIndexerItem, esutil.BulkIndexerResponseItem) {
						complete(false)
					},
					OnFailure: func(_ context.Context, item esutil.BulkIndexerItem, response esutil.BulkIndexerResponseItem, err error) {
						if err == nil && item.Action == "delete" && response.Status == http.StatusNotFound {
							complete(false)
							return
						}
						complete(true)
						logger.Warn("Elasticsearch document indexing failed", mlog.String("index", item.Index), mlog.String("doc_id", item.DocumentID), mlog.Int("status", response.Status), mlog.Err(err))
					},
				})
			})
			// Add can reject an item before queueing it, without OnFailure. The
			// shared worker logs that error and continues, so retain it here too.
			if err != nil {
				complete(true)
			}
			return err
		},
		func() (int64, error) {
			err := session.close(bulkIndexer.Close, func() error { return client.Close(context.Background()) })
			transport.CloseIdleConnections()
			return results.failureCount(err, logger, "Elasticsearch"), err
		}, func() *bulkResults { return results }, fence)
}

func indexingJobServer(server *app.Server) *jobs.JobServer {
	if server.Config().SqlSettings.DriverName == nil || *server.Config().SqlSettings.DriverName != model.DatabaseDriverPostgres {
		return server.Jobs
	}
	baseStore := server.Jobs.Store
	db := baseStore.GetInternalReplicaDB()
	if db == nil {
		db = baseStore.GetInternalMasterDB()
	}
	return jobs.NewJobServer(server.Platform(), &indexingStore{Store: baseStore, db: db}, server.GetMetrics(), server.Log(), nil)
}

// Install trust before wrapping the HTTP transport. Both SDKs reject CACert
// when their transport is our ownership-checking RoundTripper rather than a
// raw *http.Transport. Preserve their custom-CA pool semantics here.
func setIndexingTransportCA(transport *http.Transport, certificate []byte) error {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		return errors.New("unable to add indexing CA certificate")
	}
	transport.TLSClientConfig.RootCAs = roots
	return nil
}
