package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-sdk-go/service/s3/s3manager/s3manageriface"
	"github.com/cactus/go-statsd-client/statsd"
	"github.com/twitchscience/aws_utils/logger"
	"github.com/twitchscience/rs_ingester/blueprint"
	"github.com/twitchscience/rs_ingester/control"
	"github.com/twitchscience/rs_ingester/migrator"
	"github.com/twitchscience/rs_ingester/versions"

	"github.com/twitchscience/rs_ingester/backend"
	"github.com/twitchscience/rs_ingester/healthcheck"
	"github.com/twitchscience/rs_ingester/lib"
	"github.com/twitchscience/rs_ingester/loadclient"
	"github.com/twitchscience/rs_ingester/metadata"
)

const (
	healthCheckPoolSize = 1
)

var (
	poolSize             int
	statsPrefix          string
	manifestBucket       string
	rsURL                string
	rollbarToken         string
	rollbarEnvironment   string
	blueprintHost        string
	pgConfig             metadata.PGConfig
	loadAgeSeconds       int
	workerGroup          sync.WaitGroup
	waitProcessorPeriod  time.Duration
	migratorPollPeriod   time.Duration
	offpeakStartHour     int
	offpeakDurationHours int
)

type loadWorker struct {
	MetadataBackend metadata.Backend
	Loader          loadclient.Loader
}

func (i *loadWorker) Work(stats statsd.Statter) {

	c := i.MetadataBackend.LoadReady()
	for load := range c {
		logger.WithField("loadUUID", load.UUID).
			WithField("numFiles", len(load.Loads)).
			WithField("table", load.TableName).
			Info("Loading manifest into table")
		err := i.Loader.LoadManifest(load)
		if err != nil {
			if err.Retryable() {
				i.MetadataBackend.LoadError(load.UUID, err.Error())
			}
			logger.WithError(err).WithField("retryable", err.Retryable()).
				WithField("loadUUID", load.UUID).Error("Error loading files into table.")

			statsdErr := stats.Inc("manifest_load.failures", 1, 1.0)
			if statsdErr != nil {
				logger.WithError(statsdErr).Printf("Error sending manifest_load.failures message to statsd")
			}

			continue
		}
		logger.WithField("loadUUID", load.UUID).
			WithField("table", load.TableName).Info("Loaded manifest into table")
		i.MetadataBackend.LoadDone(load.UUID)

		statsdErr := stats.Inc("manifest_load.count", 1, 1.0)
		if statsdErr != nil {
			logger.WithError(statsdErr).Printf("Error sending manifest_load.count message to statsd")
		}
		for _, tsv := range load.Loads {
			statsdEvent := fmt.Sprintf("tsv_files.%s.loaded", tsv.TableName)
			statsdErr = stats.Inc(statsdEvent, 1, 1.0)
			if statsdErr != nil {
				logger.WithError(statsdErr).Printf("Error sending %s message to statsd", statsdEvent)
			}
		}
	}
	workerGroup.Done()
}

func startWorkers(s3Uploader s3manageriface.UploaderAPI, b metadata.Backend, stats statsd.Statter, aceBackend backend.Backend) ([]loadWorker, error) {
	workers := make([]loadWorker, poolSize)
	for i := 0; i < poolSize; i++ {
		loadclient, err := loadclient.NewRSLoader(s3Uploader, aceBackend, manifestBucket, stats)
		if err != nil {
			return workers, err
		}
		workers[i] = loadWorker{MetadataBackend: b, Loader: loadclient}
		workerGroup.Add(1)
		index := i
		logger.Go(func() {
			workers[index].Work(stats)
		})
	}
	return workers, nil
}

func init() {
	flag.DurationVar(&migratorPollPeriod, "migratorPollPeriod", time.Minute, "the period betwen each poll the migrator does of ingesterdb for new versions to migrate to")
	flag.DurationVar(&waitProcessorPeriod, "waitProcessorPeriod", time.Minute*3, "the period we wait for processor to process all old version TSVs")
	flag.StringVar(&statsPrefix, "statsPrefix", "ingester", "the prefix to statsd")
	flag.StringVar(&pgConfig.DatabaseURL, "databaseURL", "", "Postgres-scheme url for the RDS instance")
	flag.StringVar(&manifestBucket, "manifestBucket", "", "S3 bucket for manifests.")
	flag.IntVar(&pgConfig.MaxConnections, "maxDBConnections", 5, "Number of database connections to open")
	flag.IntVar(&pgConfig.LoadCountTrigger, "loadCountTrigger", 5, "Number of queued tsvs before a load into redshift is triggered")
	flag.IntVar(&loadAgeSeconds, "loadAgeSeconds", 1800, "Max age of tsvs in queue before a load into redshift is triggered")
	flag.IntVar(&poolSize, "n_workers", 5, "Number of load workers and therefore redshift connections. Set to 0 to turn off ingests (COPYs).")
	flag.StringVar(&blueprintHost, "blueprint_host", "", "Host name (and optionally :port) for communicating with blueprint")
	flag.StringVar(&rsURL, "rsURL", "", "URL for Redshift")
	flag.StringVar(&rollbarToken, "rollbarToken", "", "Rollbar post_server_item token")
	flag.StringVar(&rollbarEnvironment, "rollbarEnvironment", "", "Rollbar environment")
	flag.IntVar(&offpeakStartHour, "offpeakStartHour", 3, "Hour that offpeak period starts and migrations can happen, in UTC")
	flag.IntVar(&offpeakDurationHours, "offpeakDurationHours", 8, "Duration of the offpeak migration period, in hours")
}

func main() {
	flag.Parse()
	pgConfig.LoadAgeTrigger = time.Second * time.Duration(loadAgeSeconds)

	stats, err := lib.InitStats(statsPrefix)
	if err != nil {
		logger.WithError(err).Fatal("Failed to setup statter")
	}

	logger.InitWithRollbar("info", rollbarToken, rollbarEnvironment)
	logger.CaptureDefault()
	logger.Info("starting")
	defer logger.LogPanic()

	session := session.New()
	s3Uploader := s3manager.NewUploader(session)
	aceBackend, err := backend.BuildRedshiftBackend(session.Config.Credentials, poolSize+healthCheckPoolSize, rsURL)
	if err != nil {
		logger.WithError(err).Fatal("Failed to setup redshift connection")
	}

	rsConnection, err := loadclient.NewRSLoader(s3Uploader, aceBackend, manifestBucket, stats)
	if err != nil {
		logger.WithError(err).Fatal("Failed to setup Redshift loading client for postgres")
	}

	initVersions, err := aceBackend.TableVersions()
	if err != nil {
		logger.WithError(err).Fatal("Failed initialization of table version cache")
	}
	tableVersions := versions.New(initVersions)

	var metaBackend metadata.Backend

	if poolSize > 0 {
		metaBackend, err = metadata.NewPostgresLoader(&pgConfig, rsConnection, tableVersions)
		if err != nil {
			logger.WithError(err).Fatal("Failed to setup postgres backend")
		}

		_, err = startWorkers(s3Uploader, metaBackend, stats, aceBackend)
		if err != nil {
			logger.WithError(err).Fatal("Failed to start workers")
		}
	}

	metaReader, err := metadata.NewPostgresReader(&pgConfig, tableVersions)
	if err != nil {
		logger.WithError(err).Fatal("Failed to setup postgres reader")
	}

	blueprintClient := blueprint.New(blueprintHost)
	versionIncrement := make(chan migrator.VersionIncrement)
	migrator := migrator.New(aceBackend, metaReader, blueprintClient, tableVersions, migratorPollPeriod,
		waitProcessorPeriod, offpeakStartHour, offpeakDurationHours, versionIncrement)

	hcBackend := healthcheck.NewBackend(rsConnection, metaReader)
	hcHandler := healthcheck.NewHandler(hcBackend)

	serveMux := http.NewServeMux()
	serveMux.Handle("/health", healthcheck.NewHealthRouter(hcHandler))

	controlBackend := control.NewControlBackend(metaReader, tableVersions, versionIncrement)
	controlHandler := control.NewControlHandler(controlBackend, stats)

	serveMux.Handle("/control/", control.NewControlRouter(controlHandler))

	logger.Go(func() {
		logger.WithError(http.ListenAndServe(net.JoinHostPort("localhost", "8080"), serveMux)).
			Fatal("Serving health and control failed")
	})

	logger.Go(func() {
		logger.WithError(http.ListenAndServe(":6060", nil)).
			Error("Serving pprof failed")
	})

	wait := make(chan struct{})
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT)
	logger.Info("Loader is set up")
	logger.Go(func() {
		<-sigc
		logger.Info("Sigint received -- shutting down")
		migrator.Close()
		if metaBackend != nil {
			metaBackend.Close()
		}
		// Cause flush
		err = stats.Close()
		if err != nil {
			logger.WithError(err).Error("Error closing statter")
		}
		workerGroup.Wait()
		logger.Info("Exiting main cleanly.")
		logger.Wait()
		close(wait)
	})
	<-wait
}
