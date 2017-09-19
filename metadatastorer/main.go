/*
MetadataStorer fetches pointers to processed TSV files and stores
them into a Postgres instance.
*/
package main

import (
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/aws/aws-sdk-go/service/sqs/sqsiface"
	"github.com/twitchscience/aws_utils/listener"
	"github.com/twitchscience/aws_utils/logger"
	"github.com/twitchscience/rs_ingester/blueprint"
	"github.com/twitchscience/rs_ingester/metadata"
	"github.com/twitchscience/rs_ingester/monitoring"
	"github.com/twitchscience/scoop_protocol/scoop_protocol"
)

var (
	pgConfig                  metadata.PGConfig
	sqsPollWait               time.Duration
	sqsQueueName              string
	statsPrefix               string
	listenerCount             int
	rollbarToken              string
	rollbarEnvironment        string
	bpConfigsBucket           string
	bpMetadataConfigsKey      string
	bpMetadataReloadFrequency time.Duration
	bpMetadataRetryDelay      time.Duration
)

type rdsPipeHandler struct {
	MetadataStorer   metadata.Storer
	Signer           scoop_protocol.ScoopSigner
	Statter          monitoring.SafeStatter
	BpMetadataLoader *blueprint.MetadataLoader
	Tables           map[string]bool
}

func init() {
	flag.StringVar(&pgConfig.DatabaseURL, "databaseURL", "", "Postgres-scheme url for the RDS instance")
	flag.StringVar(&statsPrefix, "statsPrefix", "metadatastorer", "the prefix to statsd")
	flag.IntVar(&pgConfig.MaxConnections, "maxDBConnections", 5, "Max number of database connections to open")
	flag.DurationVar(&sqsPollWait, "sqsPollWait", time.Second*30, "Number of seconds to wait between polling SQS")
	flag.StringVar(&sqsQueueName, "sqsQueueName", "", "Name of sqs queue to list for events on")
	flag.IntVar(&listenerCount, "listenerCount", 1, "Number of sqs listeners to run")
	flag.StringVar(&rollbarToken, "rollbarToken", "", "Rollbar post_server_item token")
	flag.StringVar(&rollbarEnvironment, "rollbarEnvironment", "", "Rollbar environment")
	flag.StringVar(&bpConfigsBucket, "bpConfigsBucket", "", "The S3 bucket name where Blueprint configs are stored")
	flag.StringVar(&bpMetadataConfigsKey, "bpMetadataConfigsKey", "", "The file name of the Blueprint event metadata configs on S3")
	flag.DurationVar(&bpMetadataReloadFrequency, "bpMetadataReloadFrequency", 5*time.Minute, "How often to load Blueprint event metadata from S3")
	flag.DurationVar(&bpMetadataRetryDelay, "bpMetadataRetryDelay", 2*time.Second, "How long to sleep if there's an error loading Blueprint event metadata from S3")
}

func main() {
	flag.Parse()

	logger.InitWithRollbar("info", rollbarToken, rollbarEnvironment)
	defer logger.LogPanic()

	stats, err := monitoring.InitStats(statsPrefix)
	if err != nil {
		logger.WithError(err).Fatal("Error initializing stats")
	}

	logger.Go(func() {
		logger.WithError(http.ListenAndServe(":7767", http.DefaultServeMux)).
			Error("Serving pprof failed")
	})

	postgresBackend, err := metadata.NewPostgresStorer(&pgConfig)
	if err != nil {
		logger.WithError(err).Fatal("Error initializing PostgresStorer")
	}
	session, err := session.NewSession()
	if err != nil {
		logger.WithError(err).Fatal("Failed to setup aws session")
	}

	s3 := s3.New(session)
	fetcher := blueprint.NewFetcher(bpConfigsBucket, bpMetadataConfigsKey, s3)
	bpMetadataLoader, err := blueprint.NewMetadataLoader(fetcher, bpMetadataReloadFrequency, bpMetadataRetryDelay, stats)
	if err != nil {
		logger.WithError(err).Error("Failed to setup new Blueprint metadata loader")
	}
	logger.Go(bpMetadataLoader.Crank)

	// In cases we get a temporary influx of traffic, want to be resilient.
	sqs := sqs.New(session, aws.NewConfig().WithMaxRetries(10))

	// Make a deduplication filter for the SQSListeners
	filter := listener.NewDedupSQSFilter(1000, time.Hour)

	listeners := make([]*listener.SQSListener, listenerCount)
	for i := 0; i < listenerCount; i++ {
		listeners[i] = startWorker(sqs, sqsQueueName, stats, postgresBackend, filter, bpMetadataLoader)
	}

	wait := make(chan struct{})

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT)
	logger.Go(func() {
		<-sigc
		logger.Info("Sigint received -- shutting down")
		bpMetadataLoader.Close()
		// Cause flush
		var wg sync.WaitGroup
		wg.Add(listenerCount)
		for i := 0; i < listenerCount; i++ {
			index := i
			logger.Go(func() {
				defer wg.Done()
				listeners[index].Close()
			})
		}
		wg.Wait()
		logger.Info("Exiting main cleanly.")
		logger.Wait()
		close(wait)
	})

	<-wait
}

func startWorker(sqs sqsiface.SQSAPI, queue string, stats monitoring.SafeStatter, b metadata.Storer, f listener.SQSFilter, metadataLoader *blueprint.MetadataLoader) *listener.SQSListener {
	tables, err := b.ListDistinctTables()
	if err != nil {
		logger.WithError(err).Error("Error listing distinct tables from tsv")
	}

	var tablesMap = make(map[string]bool)
	for _, table := range tables {
		tablesMap[table] = true
	}

	ret := listener.BuildSQSListener(
		&rdsPipeHandler{
			MetadataStorer:   b,
			Signer:           scoop_protocol.GetScoopSigner(),
			Statter:          stats,
			Tables:           tablesMap,
			BpMetadataLoader: metadataLoader,
		},
		sqsPollWait,
		sqs,
		f)
	logger.Go(func() { ret.Listen(queue) })
	return ret
}

func (i *rdsPipeHandler) Handle(msg *sqs.Message) error {
	logger.WithField("body", msg.Body).WithField("messageID", msg.MessageId).Info("Received message")

	req, err := i.Signer.GetRowCopyRequest(strings.NewReader(aws.StringValue(msg.Body)))
	if err != nil {
		return err
	}

	load := metadata.Load(*req)

	if _, found := i.Tables[load.TableName]; !found {
		i.BpMetadataLoader.ForceReload()
	}

	if !i.BpMetadataLoader.TableExists(load.TableName) {
		err = fmt.Errorf("No metadata found for table %s after force refresh", load.TableName)
		logger.WithError(err).Error("Error retrieving target datastores")
		return err
	}

	i.Tables[load.TableName] = true

	if !i.BpMetadataLoader.LoadIntoAce(load.TableName) {
		i.Statter.SafeInc(fmt.Sprintf("tsv_files.%s.skipped.ace", load.TableName), 1, 1.0)
		i.Statter.SafeInc("tsv_files.total.skipped.ace", 1, 1.0)
		return nil
	}

	// Call i.GetTables() (distinguish between read table and get tables)
	// If we don't know about the table, refresh the metadata and add it to tables
	// If metadata read fails, rollbar error
	// If we still don't know about the table... error
	// Add to list of tables we know about
	// If not load into ace, return nil

	eventPattern := "tsv_files.%s.received"
	i.Statter.SafeInc(fmt.Sprintf(eventPattern, load.TableName), 1, 1.0)
	i.Statter.SafeInc(fmt.Sprintf(eventPattern, "total"), 1, 1.0)

	err = i.MetadataStorer.InsertLoad(&load)
	if err != nil {
		return err
	}

	eventPattern = "tsv_files.%s.queued"
	i.Statter.SafeInc(fmt.Sprintf(eventPattern, load.TableName), 1, 1.0)
	i.Statter.SafeInc(fmt.Sprintf(eventPattern, "total"), 1, 1.0)

	return nil
}
