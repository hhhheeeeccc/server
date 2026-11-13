package elasticsearch_indexing

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/shared/mlog"
	"github.com/mattermost/mattermost/server/v8/channels/app"
	"github.com/mattermost/mattermost/server/v8/enterprise/elasticsearch/common"
	"github.com/mattermost/mattermost/server/v8/platform/shared/filestore"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	"github.com/opensearch-project/opensearch-go/v4/opensearchutil"
)

type OpenSearchIndexer struct {
	server *app.Server
}

func (i *OpenSearchIndexer) MakeWorker() model.Worker {
	const workerName = "EnterpriseOpensearchIndexer"
	server := i.server
	logger := server.Jobs.Logger().With(mlog.String("worker_name", workerName))
	settings := server.Config().ElasticsearchSettings
	fileBackend := server.Platform().FileBackend()
	clientConfig, err := openSearchClientConfig(settings, fileBackend, logger)
	if err != nil {
		logger.Error("Unable to configure OpenSearch indexing client", mlog.Err(err))
		return nil
	}
	fence := &indexingFence{enabled: model.SafeDereference(server.Config().SqlSettings.DriverName) == model.DatabaseDriverPostgres}
	clientConfig.Client.Transport = &indexingTransport{base: clientConfig.Client.Transport, fence: fence}
	var client *opensearchapi.Client
	var bulkIndexer opensearchutil.BulkIndexer
	var results *bulkResults
	var session *indexingBulkSession
	return newCheckpointWorker(server, workerName, model.ElasticsearchSettingsOSBackend,
		indexingJobServer(server), logger, fileBackend, server.License,
		func() error {
			// Scope both callbacks and the client's discovery goroutines to this
			// processor. Closing a job must not retain its failures or resources.
			runResults := &bulkResults{}
			results = runResults
			session = newIndexingBulkSession(fence, time.Duration(*settings.RequestTimeoutSeconds)*time.Second)
			// OpenSearch drains final worker buffers serially. Allow one request
			// budget per worker, under one cumulative close deadline.
			session.closeTimeout *= time.Duration(common.NumIndexWorkers())
			configForJob := clientConfig
			configForJob.Client.Context = session.ctx
			configForJob.Client.DiscoverNodesOnStart = new(false)
			configForJob.Client.DiscoverNodesInterval = 0
			var err error
			client, err = opensearchapi.NewClient(configForJob)
			if err != nil {
				session.cancel(err)
				return err
			}
			if *settings.Sniff {
				session.discover(client.Client.DiscoverNodes, 30*time.Second, func(err error) { logger.Warn("OpenSearch node discovery failed", mlog.Err(err)) })
			}
			config := opensearchutil.BulkIndexerConfig{
				Client: client, Timeout: time.Duration(*settings.RequestTimeoutSeconds) * time.Second,
				NumWorkers: common.NumIndexWorkers(),
				// Keep native worker startup independent of cancellation: this
				// SDK launches workers before adding to its WaitGroup. A canceled
				// construction context can let Done precede Add. Flush/HTTP use
				// the session below; Close closes the queue and joins all workers.
				OnFlushStart: session.startFlush,
				OnFlushEnd:   session.endFlush,
				OnError: func(_ context.Context, err error) {
					// Partial bulk errors already have one result per rejected item.
					// In particular, a missing delete is not a transport failure.
					var partial *opensearchapi.PartialBulkError
					if !errors.As(err, &partial) {
						runResults.processorError.Store(true)
					}
					logger.Error("OpenSearch bulk indexing failed", mlog.Err(err))
				},
			}
			if *settings.Trace == "all" {
				config.DebugLogger = common.NewBulkIndexerLogger(logger, workerName)
			}
			bulkIndexer, err = opensearchutil.NewBulkIndexer(config)
			if err != nil {
				return errors.Join(err, session.close(func(context.Context) error { return nil }, client.Close))
			}
			return nil
		},
		func(indexName, action, documentID string, body io.ReadSeeker) error {
			runResults := results
			complete := runResults.addItem()
			err := session.add(func(ctx context.Context) error {
				return bulkIndexer.Add(ctx, opensearchutil.BulkIndexerItem{
					Index: indexName, Action: action, DocumentID: documentID, Body: body,
					OnSuccess: func(context.Context, opensearchutil.BulkIndexerItem, opensearchapi.BulkRespItem) {
						complete(false)
					},
					OnFailure: func(_ context.Context, item opensearchutil.BulkIndexerItem, response opensearchapi.BulkRespItem, err error) {
						if err == nil && item.Action == "delete" && response.Status == http.StatusNotFound {
							complete(false)
							return
						}
						complete(true)
						logger.Warn("OpenSearch document indexing failed", mlog.String("index", item.Index), mlog.String("doc_id", item.DocumentID), mlog.Int("status", response.Status), mlog.Err(err))
					},
				})
			})
			if err != nil {
				complete(true)
			}
			return err
		},
		func() (int64, error) {
			err := session.close(bulkIndexer.Close, client.Close)
			return results.failureCount(err, logger, "OpenSearch"), err
		}, func() *bulkResults { return results }, fence)
}

func openSearchClientConfig(settings model.ElasticsearchSettings, fileBackend filestore.FileBackend, logger mlog.LoggerIFace) (opensearchapi.Config, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: *settings.SkipTLSVerification} //nolint:gosec -- administrator setting
	transport.HTTP2 = &http.HTTP2Config{SendPingTimeout: 30 * time.Second}
	config := opensearchapi.Config{Client: opensearch.Config{
		Addresses: []string{*settings.ConnectionURL}, Username: *settings.Username, Password: *settings.Password,
		Transport: transport, DiscoverNodesOnStart: settings.Sniff,
		RequestTimeout: time.Duration(*settings.RequestTimeoutSeconds) * time.Second,
		RetryOnStatus:  []int{429, 502, 503, 504}, MaxRetries: 3,
		RetryBackoff:      func(attempt int) time.Duration { return time.Duration(attempt) * 100 * time.Millisecond },
		EnableDebugLogger: *settings.Trace == "all",
		Logger:            common.NewLogger("OpenSearch", logger, *settings.Trace == "all"),
	}}
	if *settings.Sniff {
		config.Client.DiscoverNodesInterval = 30 * time.Second
	}
	if *settings.CA != "" {
		ca, err := common.ReadFileSafely(fileBackend, *settings.CA)
		if err != nil {
			return config, fmt.Errorf("read OpenSearch CA: %w", err)
		}
		if err := setIndexingTransportCA(transport, ca); err != nil {
			return config, err
		}
	}
	if *settings.ClientCert != "" || *settings.ClientKey != "" {
		certPEM, err := common.ReadFileSafely(fileBackend, *settings.ClientCert)
		if err != nil {
			return config, fmt.Errorf("read OpenSearch client certificate: %w", err)
		}
		keyPEM, err := common.ReadFileSafely(fileBackend, *settings.ClientKey)
		if err != nil {
			return config, fmt.Errorf("read OpenSearch client key: %w", err)
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return config, fmt.Errorf("parse OpenSearch client certificate: %w", err)
		}
		transport.TLSClientConfig.Certificates = []tls.Certificate{cert}
	}
	return config, nil
}
