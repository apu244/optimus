package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/api/option"

	"cloud.google.com/go/storage"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_logrus "github.com/grpc-ecosystem/go-grpc-middleware/logging/logrus"
	grpctags "github.com/grpc-ecosystem/go-grpc-middleware/tags"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/jinzhu/gorm"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	v1 "github.com/odpf/optimus/api/handler/v1"
	v1handler "github.com/odpf/optimus/api/handler/v1"
	pb "github.com/odpf/optimus/api/proto/v1"
	"github.com/odpf/optimus/core/logger"
	"github.com/odpf/optimus/core/progress"
	_ "github.com/odpf/optimus/ext/hook"
	"github.com/odpf/optimus/ext/scheduler/airflow"
	_ "github.com/odpf/optimus/ext/task"
	"github.com/odpf/optimus/instance"
	"github.com/odpf/optimus/job"
	"github.com/odpf/optimus/models"
	"github.com/odpf/optimus/resources"
	"github.com/odpf/optimus/store"
	"github.com/odpf/optimus/store/gcs"
	"github.com/odpf/optimus/store/postgres"
)

var (
	// Version of the service
	// overridden by the build system. see "Makefile"
	Version = "0.1"

	// AppName is used to prefix Version
	AppName = "optimus"

	//listen for sigterm
	termChan = make(chan os.Signal, 1)

	shutdownWait = 30 * time.Second
)

// Config for the service
var Config = struct {
	ServerPort    string
	ServerHost    string
	LogLevel      string
	DBHost        string
	DBUser        string
	DBPassword    string
	DBName        string
	DBSSLMode     string
	MaxIdleDBConn string
	MaxOpenDBConn string
	IngressHost   string
	AppKey        string
}{
	ServerPort:    "9100",
	ServerHost:    "0.0.0.0",
	LogLevel:      "DEBUG",
	MaxIdleDBConn: "5",
	MaxOpenDBConn: "10",
	DBSSLMode:     "disable",
	DBPassword:    "-",
}

func lookupEnvOrString(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// cfg defines an input parameter to the service
type cfg struct {
	Env, Cmd, Desc string
}

// cfgRules define how input parameters map to local
// configuration variables
var cfgRules = map[*string]cfg{
	&Config.ServerPort: {
		Env:  "SERVER_PORT",
		Cmd:  "server-port",
		Desc: "port to listen on",
	},
	&Config.ServerHost: {
		Env:  "SERVER_HOST",
		Cmd:  "server-host",
		Desc: "the network interface to listen on",
	},
	&Config.LogLevel: {
		Env:  "LOG_LEVEL",
		Cmd:  "log-level",
		Desc: "log level - DEBUG, INFO, WARNING, ERROR, FATAL",
	},
	&Config.DBHost: {
		Env:  "DB_HOST",
		Cmd:  "db-host",
		Desc: "database host to connect to database used by jazz",
	},
	&Config.DBUser: {
		Env:  "DB_USER",
		Cmd:  "db-user",
		Desc: "database user to connect to database used by jazz",
	},
	&Config.DBPassword: {
		Env:  "DB_PASSWORD",
		Cmd:  "db-password",
		Desc: "database password to connect to database used by jazz",
	},
	&Config.DBName: {
		Env:  "DB_NAME",
		Cmd:  "db-name",
		Desc: "database name to connect to database used by jazz",
	},
	&Config.DBSSLMode: {
		Env:  "DB_SSL_MODE",
		Cmd:  "db-ssl-mode",
		Desc: "database sslmode to connect to database used by jazz (require, disable)",
	},
	&Config.MaxIdleDBConn: {
		Env:  "MAX_IDLE_DB_CONN",
		Cmd:  "max-idle-db-conn",
		Desc: "maximum allowed idle DB connections",
	},
	&Config.IngressHost: {
		Env:  "INGRESS_HOST",
		Cmd:  "ingress-host",
		Desc: "service ingress host for jobs to communicate back to optimus",
	},
	&Config.AppKey: {
		Env:  "APP_KEY",
		Cmd:  "app-key",
		Desc: "random 32 character hash used for encrypting secrets",
	},
}

func validateConfig() error {
	var errs []string
	for v, cfg := range cfgRules {
		if strings.TrimSpace(*v) == "" {
			errs = append(
				errs,
				fmt.Sprintf(
					"missing required parameter: -%s (can also be set using %s environment variable)",
					cfg.Cmd,
					cfg.Env,
				),
			)
		}
		if *v == "-" { // "- is used for empty arguments"
			*v = ""
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	return nil
}

// jobSpecRepoFactory stores raw specifications
type jobSpecRepoFactory struct {
	db *gorm.DB
}

func (fac *jobSpecRepoFactory) New(proj models.ProjectSpec) store.JobSpecRepository {
	return postgres.NewJobRepository(fac.db, proj, postgres.NewAdapter(models.TaskRegistry, models.HookRegistry))
}

// jobRepoFactory stores compiled specifications that will be consumed by a
// scheduler
type jobRepoFactory struct {
	objWriterFac objectWriterFactory
	schd         models.SchedulerUnit
}

func (fac *jobRepoFactory) New(ctx context.Context, proj models.ProjectSpec) (store.JobRepository, error) {
	storagePath, ok := proj.Config[models.ProjectStoragePathKey]
	if !ok {
		return nil, errors.Errorf("%s not configured for project %s", models.ProjectStoragePathKey, proj.Name)
	}
	storageSecret, ok := proj.Secret.GetByName(models.ProjectSecretStorageKey)
	if !ok {
		return nil, errors.Errorf("%s secret not configured for project %s", models.ProjectSecretStorageKey, proj.Name)
	}

	p, err := url.Parse(storagePath)
	if err != nil {
		return nil, err
	}
	switch p.Scheme {
	case "gs":
		storageClient, err := storage.NewClient(ctx, option.WithCredentialsJSON([]byte(storageSecret)))
		if err != nil {
			return nil, errors.Wrap(err, "error creating google storage client")
		}
		return gcs.NewJobRepository(p.Hostname(), filepath.Join(p.Path, fac.schd.GetJobsDir()), fac.schd.GetJobsExtension(), storageClient), nil
	}
	return nil, errors.Errorf("unsupported storage config %s in %s of project %s", storagePath, models.ProjectStoragePathKey, proj.Name)
}

type projectRepoFactory struct {
	db   *gorm.DB
	hash models.ApplicationKey
}

func (fac *projectRepoFactory) New() store.ProjectRepository {
	return postgres.NewProjectRepository(fac.db, fac.hash)
}

type projectSecretRepoFactory struct {
	db   *gorm.DB
	hash models.ApplicationKey
}

func (fac *projectSecretRepoFactory) New(spec models.ProjectSpec) store.ProjectSecretRepository {
	return postgres.NewSecretRepository(fac.db, spec, fac.hash)
}

type instanceRepoFactory struct {
	db *gorm.DB
}

func (fac *instanceRepoFactory) New(spec models.JobSpec) store.InstanceSpecRepository {
	return postgres.NewInstanceRepository(fac.db, spec, postgres.NewAdapter(models.TaskRegistry, models.HookRegistry))
}

type objectWriterFactory struct {
}

func (o *objectWriterFactory) New(ctx context.Context, writerPath, writerSecret string) (store.ObjectWriter, error) {
	p, err := url.Parse(writerPath)
	if err != nil {
		return nil, err
	}

	switch p.Scheme {
	case "gs":
		gcsClient, err := storage.NewClient(ctx, option.WithCredentialsJSON([]byte(writerSecret)))
		if err != nil {
			return nil, errors.Wrap(err, "error creating google storage client")
		}
		return &gcs.GcsObjectWriter{
			Client: gcsClient,
		}, nil
	}
	return nil, errors.Errorf("unsupported storage config %s", writerPath)
}

type pipelineLogObserver struct {
	log logrus.FieldLogger
}

func (obs *pipelineLogObserver) Notify(evt progress.Event) {
	obs.log.Info(evt)
}

func jobSpecAssetDump() func(jobSpec models.JobSpec, scheduledAt time.Time) (map[string]string, error) {
	engine := instance.NewGoEngine()
	return func(jobSpec models.JobSpec, scheduledAt time.Time) (map[string]string, error) {
		return instance.DumpAssets(jobSpec, scheduledAt, engine)
	}
}

func init() {
	for v, cfg := range cfgRules {
		flag.StringVar(v, cfg.Cmd, lookupEnvOrString(cfg.Env, *v), cfg.Desc)
	}
	flag.Parse()
}

func main() {

	log := logrus.New()
	log.SetOutput(os.Stdout)
	logger.Init(Config.LogLevel)

	mainLog := log.WithField("reporter", "main")
	mainLog.Infof("starting optimus %s", Version)

	err := validateConfig()
	if err != nil {
		mainLog.Fatalf("configuration error:\n%v", err)
	}

	progressObs := &pipelineLogObserver{
		log: log.WithField("reporter", "pipeline"),
	}

	// setup db
	maxIdleConnection, _ := strconv.Atoi(Config.MaxIdleDBConn)
	maxOpenConnection, _ := strconv.Atoi(Config.MaxOpenDBConn)
	databaseURL := fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=%s", Config.DBUser, url.QueryEscape(Config.DBPassword), Config.DBHost, Config.DBName, Config.DBSSLMode)
	if err := postgres.Migrate(databaseURL); err != nil {
		panic(err)
	}
	dbConn, err := postgres.Connect(databaseURL, maxIdleConnection, maxOpenConnection)
	if err != nil {
		panic(err)
	}

	// init default scheduler, should be configurable by user configs later
	models.Scheduler = airflow.NewScheduler(
		resources.FileSystem,
		&objectWriterFactory{},
		&http.Client{},
	)

	appHash, err := models.NewApplicationSecret(Config.AppKey)
	if err != nil {
		panic(err)
	}

	// registered project store repository factory, its a wrapper over a storage
	// interface
	projectRepoFac := &projectRepoFactory{
		db:   dbConn,
		hash: appHash,
	}
	registeredProjects, err := projectRepoFac.New().GetAll()
	if err != nil {
		panic(err)
	}
	// bootstrap scheduler for registered projects
	for _, proj := range registeredProjects {
		func() {
			bootstrapCtx, cancel := context.WithTimeout(context.Background(), time.Second*10)
			defer cancel()

			logger.I("bootstrapping project ", proj.Name)
			if err := models.Scheduler.Bootstrap(bootstrapCtx, proj); err != nil {
				// Major ERROR, but we can't make this fatal
				// other projects might be working fine though
				logger.E(err)
			}
			logger.I("bootstrapped project ", proj.Name)
		}()
	}

	projectSecretRepoFac := &projectSecretRepoFactory{
		db:   dbConn,
		hash: appHash,
	}

	// registered job store repository factory
	jobSpecRepoFac := &jobSpecRepoFactory{
		db: dbConn,
	}
	jobCompiler := job.NewCompiler(resources.FileSystem, models.Scheduler.GetTemplatePath(), Config.IngressHost)
	dependencyResolver := job.NewDependencyResolver(
		jobSpecAssetDump(),
	)
	priorityResolver := job.NewPriorityResolver()

	// Logrus entry is used, allowing pre-definition of certain fields by the user.
	logrusEntry := logrus.NewEntry(log)
	// Shared options for the logger, with a custom gRPC code to log level function.
	opts := []grpc_logrus.Option{
		grpc_logrus.WithLevels(grpc_logrus.DefaultCodeToLevel),
	}
	// Make sure that log statements internal to gRPC library are logged using the logrus Logger as well.
	grpc_logrus.ReplaceGrpcLogger(logrusEntry)

	serverPort, err := strconv.Atoi(Config.ServerPort)
	if err != nil {
		panic("invalid server port")
	}
	grpcAddr := fmt.Sprintf("%s:%d", Config.ServerHost, serverPort)
	grpcOpts := []grpc.ServerOption{
		grpc_middleware.WithUnaryServerChain(
			grpctags.UnaryServerInterceptor(grpctags.WithFieldExtractor(grpctags.CodeGenRequestFieldExtractor)),
			grpc_logrus.UnaryServerInterceptor(logrusEntry, opts...),
		),
	}
	grpcServer := grpc.NewServer(grpcOpts...)
	reflection.Register(grpcServer)

	// runtime service instance over gprc
	pb.RegisterRuntimeServiceServer(grpcServer, v1handler.NewRuntimeServiceServer(
		Version,
		job.NewService(
			jobSpecRepoFac,
			&jobRepoFactory{
				schd: models.Scheduler,
			},
			jobCompiler,
			dependencyResolver,
			priorityResolver,
		),
		projectRepoFac,
		projectSecretRepoFac,
		v1.NewAdapter(models.TaskRegistry, models.HookRegistry),
		progressObs,
		instance.NewService(
			&instanceRepoFactory{
				db: dbConn,
			},
			time.Now().UTC,
		),
		models.Scheduler,
	))

	timeoutGrpcDialCtx, grpcDialCancel := context.WithTimeout(context.Background(), time.Second*5)
	defer grpcDialCancel()

	// prepare http proxy
	gwmux := runtime.NewServeMux(
		runtime.WithErrorHandler(runtime.DefaultHTTPErrorHandler),
	)
	// gRPC dialup options to proxy http connections
	grpcConn, err := grpc.DialContext(timeoutGrpcDialCtx, grpcAddr, []grpc.DialOption{
		grpc.WithInsecure(),
	}...)
	if err != nil {
		panic(fmt.Errorf("Fail to dial: %v", err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := pb.RegisterRuntimeServiceHandler(ctx, gwmux, grpcConn); err != nil {
		panic(err)
	}

	// base router
	baseMux := http.NewServeMux()
	baseMux.HandleFunc("/ping", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "pong")
	}))
	baseMux.Handle("/", gwmux)

	srv := &http.Server{
		Handler:      grpcHandlerFunc(grpcServer, baseMux),
		Addr:         grpcAddr,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// run our server in a goroutine so that it doesn't block.
	go func() {
		mainLog.Infoln("starting listening at ", grpcAddr)
		if err := srv.ListenAndServe(); err != nil {
			if err != http.ErrServerClosed {
				mainLog.Fatalf("server error: %v\n", err)
			}
		}
	}()

	// We'll accept graceful shutdowns when quit via SIGINT (Ctrl+C)
	signal.Notify(termChan, os.Interrupt)
	signal.Notify(termChan, os.Kill)
	signal.Notify(termChan, syscall.SIGTERM)

	// Block until we receive our signal.
	<-termChan
	mainLog.Info("termination request received")

	// Create a deadline to wait for server
	ctxProxy, cancelProxy := context.WithTimeout(context.Background(), shutdownWait)
	defer cancelProxy()

	// Doesn't block if no connections, but will otherwise wait
	// until the timeout deadline.
	if err := srv.Shutdown(ctxProxy); err != nil {
		mainLog.Warn(err)
	}
	grpcServer.GracefulStop()

	mainLog.Info("bye")
}

// grpcHandlerFunc routes http1 calls to baseMux and http2 with grpc header to grpcServer.
// Using a single port for proxying both http1 & 2 protocols will degrade http performance
// but for our usecase the convenience per performance tradeoff is better suited
// if in future, this does become a bottleneck(which I highly doubt), we can break the service
// into two ports, default port for grpc and default+1 for grpc-gateway proxy.
// We can also use something like a connection multiplexer
// https://github.com/soheilhy/cmux to achieve the same.
func grpcHandlerFunc(grpcServer *grpc.Server, otherHandler http.Handler) http.Handler {
	return h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.Contains(r.Header.Get("Content-Type"), "application/grpc") {
			grpcServer.ServeHTTP(w, r)
		} else {
			otherHandler.ServeHTTP(w, r)
		}
	}), &http2.Server{})
}
