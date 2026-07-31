package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/custom/modules/derivativequeue"
	"github.com/Tencent/WeKnora/internal/custom/modules/documentqueue"
	"github.com/Tencent/WeKnora/internal/custom/modules/documentsplit"
	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/custom/modules/pipelineobs"
	"github.com/Tencent/WeKnora/internal/custom/modules/runtimeprofile"
	"github.com/Tencent/WeKnora/internal/custom/modules/taskdefer"
	"github.com/Tencent/WeKnora/internal/custom/modules/terminalrepair"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/middleware/asynqdl"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"go.uber.org/dig"
)

type AsynqTaskParams struct {
	dig.In

	Servers              *AsynqServers
	KnowledgeService     interfaces.KnowledgeService
	KnowledgeBaseService interfaces.KnowledgeBaseService
	TagService           interfaces.KnowledgeTagService
	DataSourceService    interfaces.DataSourceService
	ChunkExtractor       interfaces.TaskHandler `name:"chunkExtractor"`
	DataTableSummary     interfaces.TaskHandler `name:"dataTableSummary"`
	ImageMultimodal      interfaces.TaskHandler `name:"imageMultimodal"`
	KnowledgePostProcess interfaces.TaskHandler `name:"knowledgePostProcess"`
	WikiIngest           interfaces.TaskHandler `name:"wikiIngest"`
	DeadLetterRepo       interfaces.TaskDeadLetterRepository
	TaskEnqueuer         interfaces.TaskEnqueuer
	SpanTracker          service.SpanTracker
	DocumentQueue        *documentqueue.Coordinator
	DerivativeWorker     *derivativequeue.Worker
	Cleaner              interfaces.ResourceCleaner
}

// defaultRedisOpTimeout is the previous hard-coded read timeout. The 100ms
// floor was tight enough to cause spurious i/o timeout errors during bursty
// workloads (large batch uploads, multimodal counter DECRs under load), so we
// raise the default to 500ms while still allowing operators to tune via env.
const defaultRedisOpTimeoutMs = 500

// readRedisOpTimeoutMs reads WEKNORA_REDIS_OP_TIMEOUT_MS, falling back to
// defaultRedisOpTimeoutMs on missing/invalid input. Kept as a separate helper
// so both ReadTimeout and WriteTimeout share the same source of truth.
func readRedisOpTimeoutMs() int {
	if v := strings.TrimSpace(os.Getenv("WEKNORA_REDIS_OP_TIMEOUT_MS")); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			return parsed
		}
	}
	return defaultRedisOpTimeoutMs
}

func getAsynqRedisClientOpt() *asynq.RedisClientOpt {
	db := 0
	if dbStr := os.Getenv("REDIS_DB"); dbStr != "" {
		if parsed, err := strconv.Atoi(dbStr); err == nil {
			db = parsed
		}
	}
	timeoutMs := readRedisOpTimeoutMs()
	opt := &asynq.RedisClientOpt{
		Addr:        os.Getenv("REDIS_ADDR"),
		Username:    os.Getenv("REDIS_USERNAME"),
		Password:    os.Getenv("REDIS_PASSWORD"),
		ReadTimeout: time.Duration(timeoutMs) * time.Millisecond,
		// Writes are typically more sensitive to congestion than reads
		// (RESP pipelining, BRPOPLPUSH on Asynq dequeue), so we keep
		// WriteTimeout slightly larger to absorb head-of-line stalls.
		WriteTimeout: time.Duration(timeoutMs*2) * time.Millisecond,
		DB:           db,
	}
	return opt
}

func NewAsyncqClient() (*asynq.Client, error) {
	opt := getAsynqRedisClientOpt()
	client := asynq.NewClient(opt)
	err := client.Ping()
	if err != nil {
		return nil, err
	}
	return client, nil
}

// wikiIngestRetryDelay is a fixed, short polling interval for wiki ingest
// lock conflicts. A live owner renews the 60-second lease every 20 seconds,
// so a contender may need many polls before it can take over. Lock conflicts
// are therefore classified as non-failures below and do not consume retry
// budget while they wait.
const wikiIngestRetryDelay = 15 * time.Second
const documentOwnershipConflictRetryDelay = 2 * time.Second

// asynqIsFailureFunc distinguishes healthy serialization conflicts from actual
// task failures. Asynq still moves non-failure errors to its delayed retry set
// using RetryDelayFunc, but does not increment Retried. This preserves
// low-latency ownership handoff without exhausting MaxRetry or creating
// misleading task_dead_letters rows. All Redis, database, model, and payload
// errors remain ordinary failures. Raw provider transport/rate-limit/outage
// errors are budget-free for legacy handlers; long-running durable image/Wiki
// handlers explicitly mark real remote calls so they spend their bounded
// business budget while preserving Retry-After. Pre-call circuit rejects
// remain budget-free.
func asynqIsFailureFunc(err error) bool {
	return err != nil &&
		!errors.Is(err, service.ErrWikiIngestConcurrent) &&
		!errors.Is(err, documentqueue.ErrAlreadyLeased) &&
		!errors.Is(err, documentqueue.ErrInstanceCapacity) &&
		!errors.Is(err, documentqueue.ErrInstanceFenced) &&
		!errors.Is(err, taskdefer.ErrOriginalTaskLive) &&
		!modeladmission.IsModelWorkDeferred(err) &&
		!errors.Is(err, context.Canceled)
}

// asynqRetryDelayFunc customizes per-task retry backoff.
//
// Default asynq backoff is exponential (≈10s, 40s, 90s, 2.5m, ...), which
// is appropriate for transient errors like remote HTTP failures. For Wiki
// lock conflicts it creates an unnecessary tail after either a live owner
// finishes or a crashed owner's 60-second lease expires. A fixed poll keeps
// handoff latency bounded; duplicate document deliveries use a tighter poll so
// they can take over promptly if the current owner disappears. The IsFailure
// hook makes both forms of serialization wait budget-free.
func asynqRetryDelayFunc(n int, e error, t *asynq.Task) time.Duration {
	if errors.Is(e, documentqueue.ErrAlreadyLeased) ||
		errors.Is(e, documentqueue.ErrInstanceCapacity) ||
		errors.Is(e, documentqueue.ErrInstanceFenced) {
		return documentOwnershipConflictRetryDelay
	}
	if errors.Is(e, service.ErrWikiIngestConcurrent) {
		return wikiIngestRetryDelay
	}
	if errors.Is(e, context.Canceled) {
		return documentOwnershipConflictRetryDelay
	}
	if delay, ok := taskdefer.RetryDelay(e); ok {
		return delay
	}
	if retryAfter, ok := modeladmission.ModelRetryAfter(e); ok {
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		return modeladmission.SpreadProviderRetry(retryAfter, providerRetryIdentity(t))
	}
	return asynq.DefaultRetryDelayFunc(n, e, t)
}

func providerRetryIdentity(t *asynq.Task) string {
	if t == nil {
		return ""
	}
	// The NUL separator prevents a task type suffix from being confused with a
	// payload prefix. Asynq payloads are JSON in these queues.
	return t.Type() + "\x00" + string(t.Payload())
}

// Background fanout workers use a private operational setting so changing the
// Coordinator-owned document capacity cannot accidentally create hundreds of
// summary/graph workers. VLM images use a separate server below.
const defaultBackgroundTaskConcurrency = 32
const defaultMultimodalTaskConcurrency = 2
const defaultControlTaskConcurrency = 2
const defaultWikiMapTaskConcurrency = 4

type AsynqServers struct {
	Normal     *asynq.Server // background and legacy queues
	Multimodal *asynq.Server // slow VLM image lane
	WikiMap    *asynq.Server // bounded document-local Wiki Map lane
	Control    *asynq.Server // lifecycle/repair control plane
	Document   *asynq.Server
	Part       *asynq.Server
	Profile    runtimeprofile.Profile
}

type documentWorkflowRouter interface {
	ForwardLegacyRoot(context.Context, *asynq.Task) error
	Process(context.Context, *asynq.Task, asynq.HandlerFunc) error
}

type asynqServerLifecycle interface {
	Start(asynq.Handler) error
	Shutdown()
}

type documentQueueReadiness interface {
	MarkReady(context.Context) error
}

func startAsynqServers(
	normal asynqServerLifecycle,
	multimodal asynqServerLifecycle,
	wikiMap asynqServerLifecycle,
	control asynqServerLifecycle,
	document asynqServerLifecycle,
	part asynqServerLifecycle,
	handler asynq.Handler,
	readiness documentQueueReadiness,
) error {
	if normal == nil || multimodal == nil || wikiMap == nil || control == nil || document == nil || part == nil {
		return errors.New("background, multimodal, Wiki Map, control, document and document-part asynq servers are required")
	}
	if err := normal.Start(handler); err != nil {
		return fmt.Errorf("start background asynq server: %w", err)
	}
	if err := multimodal.Start(handler); err != nil {
		normal.Shutdown()
		return fmt.Errorf("start multimodal asynq server: %w", err)
	}
	if err := wikiMap.Start(handler); err != nil {
		multimodal.Shutdown()
		normal.Shutdown()
		return fmt.Errorf("start Wiki Map asynq server: %w", err)
	}
	if err := control.Start(handler); err != nil {
		wikiMap.Shutdown()
		multimodal.Shutdown()
		normal.Shutdown()
		return fmt.Errorf("start control asynq server: %w", err)
	}
	if err := document.Start(handler); err != nil {
		control.Shutdown()
		wikiMap.Shutdown()
		multimodal.Shutdown()
		normal.Shutdown()
		return fmt.Errorf("start document workflow asynq server: %w", err)
	}
	if err := part.Start(handler); err != nil {
		document.Shutdown()
		control.Shutdown()
		wikiMap.Shutdown()
		multimodal.Shutdown()
		normal.Shutdown()
		return fmt.Errorf("start document part asynq server: %w", err)
	}
	if readiness == nil {
		part.Shutdown()
		document.Shutdown()
		control.Shutdown()
		wikiMap.Shutdown()
		multimodal.Shutdown()
		normal.Shutdown()
		return errors.New("mark document queue ready: coordinator is unavailable")
	}
	if err := readiness.MarkReady(context.Background()); err != nil {
		part.Shutdown()
		document.Shutdown()
		control.Shutdown()
		wikiMap.Shutdown()
		multimodal.Shutdown()
		normal.Shutdown()
		return fmt.Errorf("mark document queue ready: %w", err)
	}
	return nil
}

func routeDocumentRootTask(
	ctx context.Context,
	queueName string,
	queueKnown bool,
	task *asynq.Task,
	coordinator documentWorkflowRouter,
	delegate asynq.HandlerFunc,
) error {
	if !queueKnown || queueName != types.QueueDocument {
		if coordinator == nil {
			return errors.New("document queue coordinator is unavailable for legacy root forwarding")
		}
		return coordinator.ForwardLegacyRoot(ctx, task)
	}
	if coordinator == nil {
		return delegate(ctx, task)
	}
	return coordinator.Process(ctx, task, delegate)
}

func documentRootHandler(
	coordinator documentWorkflowRouter,
	delegate asynq.HandlerFunc,
) asynq.HandlerFunc {
	return func(ctx context.Context, task *asynq.Task) error {
		queueName, queueKnown := asynq.GetQueueName(ctx)
		err := routeDocumentRootTask(ctx, queueName, queueKnown, task, coordinator, delegate)
		if isDocumentQueueControlError(err) {
			// These are epoch/ownership control outcomes, not business failures.
			// ACK the obsolete Redis copy; the PostgreSQL outbox remains the source
			// of truth and republishes queued work. Letting the error reach the
			// dead-letter middleware at retry=max would incorrectly fail a document
			// that another boot is actively processing.
			return nil
		}
		return err
	}
}

func isDocumentQueueControlError(err error) bool {
	return errors.Is(err, documentqueue.ErrAlreadyLeased) ||
		errors.Is(err, documentqueue.ErrFairnessDeferred) ||
		errors.Is(err, documentqueue.ErrInstanceFenced) ||
		errors.Is(err, documentqueue.ErrLeaseLost)
}

func documentServerConcurrency(coordinator *documentqueue.Coordinator) int {
	if coordinator == nil {
		panic("document queue coordinator is required to configure the document server")
	}
	return coordinator.Capacity()
}

func NewAsynqServers(
	coordinator *documentqueue.Coordinator,
	splitManager *documentsplit.Manager,
	profile runtimeprofile.Profile,
) *AsynqServers {
	opt := getAsynqRedisClientOpt()
	concurrency := documentServerConcurrency(coordinator)
	backgroundConcurrency := defaultBackgroundTaskConcurrency
	if raw := strings.TrimSpace(os.Getenv("WEKNORA_ASYNQ_TASK_CONCURRENCY")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			backgroundConcurrency = parsed
		}
	}
	log.Printf("asynq background server starting with concurrency=%d redis_op_timeout=%dms",
		backgroundConcurrency, readRedisOpTimeoutMs())
	normalQueues := map[string]int{
		types.QueueDefault:       3,
		types.QueueLow:           1,
		types.QueueGraph:         1,
		types.QueueQuestion:      1,
		types.QueueDerivative:    1,
		types.QueueDocumentHeavy: 1,
	}
	switch profile.Role {
	case runtimeprofile.RoleParseWorker:
		normalQueues = map[string]int{
			types.QueueDefault:       3,
			types.QueueLow:           1,
			types.QueueDocumentHeavy: 1,
		}
	case runtimeprofile.RoleDerivativeWorker:
		normalQueues = map[string]int{
			types.QueueGraph:      1,
			types.QueueQuestion:   1,
			types.QueueDerivative: 1,
		}
	}
	normal := asynq.NewServer(
		opt,
		asynq.Config{
			Concurrency:     backgroundConcurrency,
			Queues:          normalQueues,
			RetryDelayFunc:  asynqRetryDelayFunc,
			IsFailure:       asynqIsFailureFunc,
			ShutdownTimeout: 30 * time.Second,
		},
	)
	multimodalConcurrency := defaultMultimodalTaskConcurrency
	if raw := strings.TrimSpace(os.Getenv("WEKNORA_MULTIMODAL_TASK_CONCURRENCY")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			multimodalConcurrency = parsed
		}
	}
	log.Printf("asynq multimodal server starting with per-instance concurrency=%d", multimodalConcurrency)
	multimodal := asynq.NewServer(
		opt,
		asynq.Config{
			Concurrency: multimodalConcurrency,
			Queues: map[string]int{
				types.QueueMultimodal: 1,
			},
			RetryDelayFunc:  asynqRetryDelayFunc,
			IsFailure:       asynqIsFailureFunc,
			ShutdownTimeout: 30 * time.Second,
		},
	)
	wikiMapConcurrency := defaultWikiMapTaskConcurrency
	if raw := strings.TrimSpace(os.Getenv("WEKNORA_WIKI_MAP_TASK_CONCURRENCY")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			wikiMapConcurrency = parsed
		}
	}
	log.Printf("asynq Wiki Map server starting with per-instance concurrency=%d", wikiMapConcurrency)
	wikiMap := asynq.NewServer(
		opt,
		asynq.Config{
			Concurrency: wikiMapConcurrency,
			Queues: map[string]int{
				types.QueueWikiMap:     3,
				types.QueueWikiControl: 1,
			},
			RetryDelayFunc:  asynqRetryDelayFunc,
			IsFailure:       asynqIsFailureFunc,
			ShutdownTimeout: 30 * time.Second,
		},
	)
	// Lifecycle operations must remain available even when every background
	// worker is occupied by long model calls. Reparse admission and terminal
	// repair are short control-plane operations; isolating two workers per
	// replica gives them bounded start latency without consuming a document
	// parsing slot.
	log.Printf("asynq control server starting with concurrency=%d redis_op_timeout=%dms",
		defaultControlTaskConcurrency, readRedisOpTimeoutMs())
	control := asynq.NewServer(
		opt,
		asynq.Config{
			Concurrency: defaultControlTaskConcurrency,
			Queues: map[string]int{
				types.QueueCritical: 1,
			},
			RetryDelayFunc:  asynqRetryDelayFunc,
			IsFailure:       asynqIsFailureFunc,
			ShutdownTimeout: 30 * time.Second,
		},
	)
	log.Printf("asynq document workflow server starting with per-instance concurrency=%d redis_op_timeout=%dms",
		concurrency, readRedisOpTimeoutMs())
	document := asynq.NewServer(
		opt,
		asynq.Config{
			Concurrency: concurrency,
			Queues: map[string]int{
				types.QueueDocument: 1,
			},
			RetryDelayFunc:  asynqRetryDelayFunc,
			IsFailure:       asynqIsFailureFunc,
			ShutdownTimeout: 30 * time.Second,
		},
	)
	partConcurrency := splitManager.Config().PartConcurrency
	log.Printf("asynq document part server starting with concurrency=%d", partConcurrency)
	part := asynq.NewServer(
		opt,
		asynq.Config{
			Concurrency: partConcurrency,
			Queues: map[string]int{
				documentsplit.QueuePart: 1,
			},
			RetryDelayFunc:  asynqRetryDelayFunc,
			IsFailure:       asynqIsFailureFunc,
			ShutdownTimeout: 30 * time.Second,
		},
	)
	return &AsynqServers{
		Normal: normal, Multimodal: multimodal, WikiMap: wikiMap,
		Control: control, Document: document, Part: part, Profile: profile,
	}
}

func startRoleAsynqServers(
	servers *AsynqServers,
	handler asynq.Handler,
	readiness documentQueueReadiness,
) error {
	if servers == nil {
		return errors.New("asynq servers are unavailable")
	}
	if servers.Profile.Role == runtimeprofile.RoleDevAll {
		return startAsynqServers(
			servers.Normal,
			servers.Multimodal,
			servers.WikiMap,
			servers.Control,
			servers.Document,
			servers.Part,
			handler,
			readiness,
		)
	}

	selected := make([]struct {
		name   string
		server asynqServerLifecycle
	}, 0, 4)
	switch servers.Profile.Role {
	case runtimeprofile.RoleParseWorker:
		selected = append(selected,
			struct {
				name   string
				server asynqServerLifecycle
			}{"parse-background", servers.Normal},
			struct {
				name   string
				server asynqServerLifecycle
			}{"parse-multimodal", servers.Multimodal},
			struct {
				name   string
				server asynqServerLifecycle
			}{"parse-document", servers.Document},
			struct {
				name   string
				server asynqServerLifecycle
			}{"parse-part", servers.Part},
		)
	case runtimeprofile.RoleDerivativeWorker:
		selected = append(selected, struct {
			name   string
			server asynqServerLifecycle
		}{"derivative", servers.Normal})
	case runtimeprofile.RoleWikiWorker:
		selected = append(selected, struct {
			name   string
			server asynqServerLifecycle
		}{"wiki", servers.WikiMap})
	case runtimeprofile.RoleMaintenance:
		selected = append(selected, struct {
			name   string
			server asynqServerLifecycle
		}{"maintenance-control", servers.Control})
	default:
		return fmt.Errorf("runtime role %s is not a worker role", servers.Profile.Role)
	}
	started := make([]asynqServerLifecycle, 0, len(selected))
	for _, item := range selected {
		if item.server == nil {
			for index := len(started) - 1; index >= 0; index-- {
				started[index].Shutdown()
			}
			return fmt.Errorf("%s asynq server is unavailable", item.name)
		}
		if err := item.server.Start(handler); err != nil {
			for index := len(started) - 1; index >= 0; index-- {
				started[index].Shutdown()
			}
			return fmt.Errorf("start %s asynq server: %w", item.name, err)
		}
		started = append(started, item.server)
	}
	if servers.Profile.RunsParseWorker() {
		if readiness == nil {
			return errors.New("mark document queue ready: coordinator is unavailable")
		}
		if err := readiness.MarkReady(context.Background()); err != nil {
			for index := len(started) - 1; index >= 0; index-- {
				started[index].Shutdown()
			}
			return fmt.Errorf("mark document queue ready: %w", err)
		}
	}
	return nil
}

func RunAsynqServer(params AsynqTaskParams) (*asynq.ServeMux, error) {
	// Create a new mux and register all handlers
	mux := asynq.NewServeMux()

	// Install the dead-letter middleware FIRST so it sees the raw error
	// returned by the handler, before any other middleware that might
	// transform it. The middleware records one task_dead_letters row per
	// task that exhausts its retry budget — operators can then SQL-query
	// failures by task type, scope, or tenant without scraping logs.
	// Best-effort: a DB failure is logged and swallowed; the original task
	// error always propagates upstream to asynq for retry/archival.
	//
	// The callback flips Knowledge.parse_status to "failed" the moment a
	// document-related task exhausts its retry budget. Without this hook,
	// a permanently-failing task left its parent knowledge stranded in
	// "processing" until housekeeping cron caught it minutes later — the
	// UI signal users actually see.
	terminalRepairer := terminalrepair.New(
		params.KnowledgeService.GetRepository(), params.TaskEnqueuer, params.SpanTracker,
	)
	terminalRepairer.SetKnowledgeMoveRepairer(params.KnowledgeService)
	knowledgeFailer := newDeadLetterKnowledgeFailer(
		params.KnowledgeService, params.TaskEnqueuer, terminalRepairer,
	)
	mux.Use(asynqdl.MiddlewareWithCallback(params.DeadLetterRepo, knowledgeFailer))
	mux.Use(pipelineobs.AsynqExecutionMiddleware(
		params.DocumentQueue.InstanceID(),
		params.DocumentQueue.BootID(),
	))

	// Install Langfuse middleware BEFORE handler registration so every task
	// type is automatically wrapped. When Langfuse is disabled the middleware
	// is a pass-through; when enabled it resumes the upstream HTTP trace (if
	// the payload carries one) or opens a standalone trace, then wraps the
	// handler execution in a SPAN so all child generations (embedding / VLM /
	// chat / rerank / ASR) nest correctly in the Langfuse UI.
	mux.Use(langfuse.AsynqMiddleware())
	mux.Use(modeladmission.AsynqMiddleware())
	// Asynq archives before consulting IsFailure when Retried == MaxRetry.
	// Persist a control-plane resume before ACKing a generation-scoped model
	// task at that boundary.
	mux.Use(taskdefer.Middleware(params.TaskEnqueuer))

	// Register extract handlers - router will dispatch to appropriate handler
	if params.Servers == nil || params.Servers.Profile.Role != runtimeprofile.RoleDerivativeWorker {
		mux.HandleFunc(types.TypeChunkExtract, params.ChunkExtractor.Handle)
	}
	mux.HandleFunc(types.TypeDataTableSummary, params.DataTableSummary.Handle)

	// Register document processing handler
	documentHandler := documentRootHandler(params.DocumentQueue, params.KnowledgeService.ProcessDocument)
	manualHandler := documentRootHandler(params.DocumentQueue, params.KnowledgeService.ProcessManualUpdate)
	mux.HandleFunc(types.TypeDocumentProcess, documentHandler)
	mux.HandleFunc(documentsplit.TypePartProcess, params.KnowledgeService.ProcessDocumentSplitPart)
	mux.HandleFunc(documentsplit.TypeFinalize, params.KnowledgeService.ProcessDocumentSplitFinalize)

	// Register manual knowledge processing handler (cleanup + re-indexing)
	mux.HandleFunc(types.TypeManualProcess, manualHandler)

	// Register FAQ import handler (includes dry run mode)
	mux.HandleFunc(types.TypeFAQImport, params.KnowledgeService.ProcessFAQImport)

	// Register question generation handler
	if params.Servers == nil || params.Servers.Profile.Role != runtimeprofile.RoleDerivativeWorker {
		mux.HandleFunc(types.TypeQuestionGeneration, params.KnowledgeService.ProcessQuestionGeneration)
	}

	// Register summary generation handler
	if params.Servers == nil || params.Servers.Profile.Role != runtimeprofile.RoleDerivativeWorker {
		mux.HandleFunc(types.TypeSummaryGeneration, params.KnowledgeService.ProcessSummaryGeneration)
	}
	if params.DerivativeWorker != nil {
		mux.HandleFunc(derivativequeue.TypeWake, params.DerivativeWorker.Handle)
	}

	// Register KB clone handler
	mux.HandleFunc(types.TypeKBClone, params.KnowledgeService.ProcessKBClone)

	// Register knowledge move handler
	mux.HandleFunc(types.TypeKnowledgeMove, params.KnowledgeService.ProcessKnowledgeMove)

	// Register knowledge list delete handler
	mux.HandleFunc(types.TypeKnowledgeListDelete, params.KnowledgeService.ProcessKnowledgeListDelete)

	// Register knowledge list reparse handler
	mux.HandleFunc(types.TypeKnowledgeListReparse, params.KnowledgeService.ProcessKnowledgeListReparse)

	// Register index delete handler
	mux.HandleFunc(types.TypeIndexDelete, params.TagService.ProcessIndexDelete)

	// Register KB delete handler
	mux.HandleFunc(types.TypeKBDelete, params.KnowledgeBaseService.ProcessKBDelete)

	// Register image multimodal handler
	mux.HandleFunc(types.TypeImageMultimodal, params.ImageMultimodal.Handle)

	// Register knowledge post process handler
	mux.HandleFunc(types.TypeKnowledgePostProcess, params.KnowledgePostProcess.Handle)

	// Register data source sync handler
	mux.HandleFunc(types.TypeDataSourceSync, params.DataSourceService.ProcessSync)

	// Register wiki ingest handler
	mux.HandleFunc(types.TypeWikiIngest, params.WikiIngest.Handle)

	// Terminal repair has its own high-retry, stable-ID task. It never reruns
	// the exhausted business operation; it only persists the exact
	// generation/item terminal transition.
	mux.HandleFunc(types.TypeKnowledgeTerminalRepair, terminalRepairer.Handle)
	mux.HandleFunc(taskdefer.TypeResume, taskdefer.NewHandler(params.TaskEnqueuer).Handle)

	if params.Servers == nil {
		return nil, errors.New("could not run asynq servers: server set is required")
	}
	if params.DocumentQueue == nil {
		return nil, errors.New("could not run asynq servers: document queue coordinator is required")
	}
	if err := startRoleAsynqServers(params.Servers, mux, params.DocumentQueue); err != nil {
		return nil, fmt.Errorf("could not run asynq servers: %w", err)
	}
	if params.Cleaner != nil && params.Servers != nil {
		params.Cleaner.RegisterWithName("AsynqServers", func() error {
			if params.DocumentQueue != nil {
				params.DocumentQueue.MarkDraining()
			}
			// Stop root admission first while background workers continue to
			// drain derivatives needed by already-active document workflows.
			if params.Servers.Document != nil {
				params.Servers.Document.Shutdown()
			}
			if params.Servers.Part != nil {
				params.Servers.Part.Shutdown()
			}
			if params.Servers.WikiMap != nil {
				params.Servers.WikiMap.Shutdown()
			}
			if params.Servers.Multimodal != nil {
				params.Servers.Multimodal.Shutdown()
			}
			if params.Servers.Normal != nil {
				params.Servers.Normal.Shutdown()
			}
			if params.Servers.Control != nil {
				params.Servers.Control.Shutdown()
			}
			return nil
		})
	}
	return mux, nil
}

// deadLetterKnowledgePayload extracts the complete processing identity from
// document-related Asynq payloads. A knowledge ID by itself is not enough:
// an exhausted task can be delivered after the document was reparsed, moved,
// or completed by a newer processing generation.
type deadLetterKnowledgePayload struct {
	TenantID             uint64 `json:"tenant_id,omitempty"`
	KnowledgeID          string `json:"knowledge_id,omitempty"`
	KnowledgeBaseID      string `json:"knowledge_base_id,omitempty"`
	ProcessingGeneration string `json:"processing_generation,omitempty"`
	ProcessingOwner      string `json:"processing_owner,omitempty"`
	// Attempt threads through DocumentProcess / ManualProcess /
	// KnowledgePostProcess payloads (added when span tracking shipped)
	// — extracted here so the dead-letter callback can also close the
	// matching root span as failed. Older in-flight payloads without
	// this field decode as 0 and the tracker call no-ops.
	Attempt int `json:"attempt,omitempty"`
}

func (p deadLetterKnowledgePayload) processingGeneration() string {
	return strings.TrimSpace(p.ProcessingGeneration)
}

// deadLetterKnowledgePostProcessGenerationRepository is intentionally narrower
// than KnowledgeRepository so the callback cannot accidentally fall back to an
// unfenced ID-only update. The production repository implements this extension;
// focused router tests can provide a small in-memory state machine.
type deadLetterKnowledgePostProcessGenerationRepository interface {
	CompletePostProcessDeadLetterGeneration(
		ctx context.Context,
		tenantID uint64,
		id string,
		knowledgeBaseID string,
		expectedGeneration string,
	) (bool, error)
}

// deadLetterDocumentProcessingGenerationRepository is the stricter core-task
// fence. In addition to tenant/KB/generation it requires the exact task owner;
// the repository also requires processed_at IS NULL so a delayed Document or
// Manual dead letter cannot overwrite a successfully committed parse.
type deadLetterDocumentProcessingGenerationRepository interface {
	FailDocumentProcessingGeneration(
		ctx context.Context,
		tenantID uint64,
		id string,
		knowledgeBaseID string,
		expectedGeneration string,
		expectedOwner string,
		values map[string]interface{},
	) (bool, error)
}

// taskTypesAffectingKnowledgeStatus enumerates the asynq task types whose
// dead-letter event should terminally repair the parent Knowledge. Only task
// types that own a lifecycle transition are listed here:
//
//   - TypeDocumentProcess: the entry point of the parsing pipeline.
//   - TypeImageMultimodal: a single image hitting dead-letter would have
//     been counted by isFinalAsynqAttempt (see image_multimodal.go), so
//     the parent might still complete via remaining images. We DO NOT mark
//     the parent failed for this case — finalize-on-last-attempt already
//     ensures progress.
//   - TypeKnowledgePostProcess: core chunks/indexes are already committed;
//     exhaustion degrades optional enrichment and completes the document.
//   - TypeManualProcess: same shape as DocumentProcess for re-indexing.
//
// Question/Summary generation are NOT included: they own exactly-once slots in
// finalizing and their terminal handlers drain those slots themselves.
var taskTypesAffectingKnowledgeStatus = map[string]struct{}{
	types.TypeDocumentProcess:      {},
	types.TypeKnowledgePostProcess: {},
	types.TypeManualProcess:        {},
}

type deadLetterKnowledgeListDeletePayload struct {
	KnowledgeIDs []string `json:"knowledge_ids,omitempty"`
}

// newDeadLetterKnowledgeFailer returns the callback wired into the asynq
// dead-letter middleware. When a document-related task exhausts its retry
// budget, this callback performs the task-type-specific terminal repair so the
// UI surfaces a core failure or a completed document with enrichment degraded
// instead of a perpetual spinner.
//
// All work is best-effort: incomplete processing identity and DB errors are
// logged and swallowed. The dead-letter record is the source of truth — this
// is purely a UX shortcut so users don't wait for the housekeeping cron's next
// sweep. A status update is never attempted without the complete fence.
func newDeadLetterKnowledgeFailer(
	ks interfaces.KnowledgeService,
	enqueuer interfaces.TaskEnqueuer,
	repairer *terminalrepair.Service,
) asynqdl.OnDeadLetter {
	if ks == nil {
		return nil
	}
	repo := ks.GetRepository()
	if repo == nil {
		return nil
	}
	return func(ctx context.Context, t *asynq.Task, taskErr error) error {
		if t == nil {
			return nil
		}
		if t.Type() == types.TypeKnowledgeListDelete {
			markKnowledgeListDeleteFailed(ctx, repo, t, taskErr)
			return nil
		}
		// A repair task that itself exhausts its unusually high budget is kept
		// in task_dead_letters for operator action. Do not recursively spawn an
		// unbounded chain of repair tasks.
		if t.Type() == types.TypeKnowledgeTerminalRepair || repairer == nil {
			return nil
		}
		if err := repairer.Repair(ctx, t, taskErr); err != nil {
			if enqueueErr := terminalrepair.Enqueue(enqueuer, t, taskErr); enqueueErr != nil {
				return errors.Join(err, enqueueErr)
			}
			logger.Warnf(ctx,
				"dead-letter callback: direct terminal repair failed for %s; persisted dedicated repair task: %v",
				t.Type(), err)
		}
		return nil
	}
}

// newDeadLetterKnowledgeStatusFailer contains the document-status part of the
// callback behind a narrow repository contract. It fails closed whenever the
// payload lacks a complete tenant/KB/generation identity, or when the row has
// already left an eligible non-terminal lifecycle state.
func newDeadLetterKnowledgeStatusFailer(
	documentRepo deadLetterDocumentProcessingGenerationRepository,
	postProcessRepo deadLetterKnowledgePostProcessGenerationRepository,
	tracker service.SpanTracker,
) func(context.Context, *asynq.Task, error) {
	return func(ctx context.Context, t *asynq.Task, taskErr error) {
		if t == nil {
			return
		}
		if _, ok := taskTypesAffectingKnowledgeStatus[t.Type()]; !ok {
			return
		}
		var probe deadLetterKnowledgePayload
		if err := json.Unmarshal(t.Payload(), &probe); err != nil {
			logger.Warnf(ctx, "dead-letter callback: invalid %s payload; recorded dead letter without mutating knowledge: %v", t.Type(), err)
			return
		}
		generation := probe.processingGeneration()
		if probe.TenantID == 0 || strings.TrimSpace(probe.KnowledgeID) == "" ||
			strings.TrimSpace(probe.KnowledgeBaseID) == "" || generation == "" {
			logger.Warnf(ctx,
				"dead-letter callback: incomplete fenced identity for %s task (knowledge=%s); recorded dead letter without mutating knowledge",
				t.Type(), probe.KnowledgeID)
			return
		}
		taskErrText := "unknown task error"
		if taskErr != nil {
			taskErrText = taskErr.Error()
		}
		errMsg := "task " + t.Type() + " exhausted retries: " + taskErrText
		// 8KB is the same cap the dead-letter row uses for last_error.
		if len(errMsg) > 8192 {
			errMsg = errMsg[:8192]
		}
		documentFailureValues := map[string]interface{}{
			"parse_status":           types.ParseStatusFailed,
			"error_message":          errMsg,
			"pending_subtasks_count": 0,
			"processing_owner":       "",
			"processing_fanout":      nil,
		}
		var (
			swapped bool
			err     error
		)
		switch t.Type() {
		case types.TypeDocumentProcess, types.TypeManualProcess:
			owner := strings.TrimSpace(probe.ProcessingOwner)
			if owner == "" {
				logger.Warnf(ctx,
					"dead-letter callback: incomplete owner fence for %s task (knowledge=%s); recorded dead letter without mutating knowledge",
					t.Type(), probe.KnowledgeID)
				return
			}
			if documentRepo == nil {
				logger.Warnf(ctx,
					"dead-letter callback: document owner/generation fence unavailable for %s task (knowledge=%s); recorded dead letter without mutating knowledge",
					t.Type(), probe.KnowledgeID)
				return
			}
			// Pending and Processing ownership, plus processed_at IS NULL, are
			// enforced atomically by this dead-letter-only repository method.
			swapped, err = documentRepo.FailDocumentProcessingGeneration(
				ctx,
				probe.TenantID,
				probe.KnowledgeID,
				probe.KnowledgeBaseID,
				generation,
				owner,
				documentFailureValues,
			)
		case types.TypeKnowledgePostProcess:
			if postProcessRepo == nil {
				logger.Warnf(ctx,
					"dead-letter callback: post-process generation fence unavailable for %s task (knowledge=%s); recorded dead letter without mutating knowledge",
					t.Type(), probe.KnowledgeID)
				return
			}
			// PostProcess runs only after the core commit has consumed the owner.
			// Its descendants are optional enrichment, so retry exhaustion must
			// preserve the usable document instead of relabelling it as a parse
			// failure. The repository additionally requires processed_at IS NOT
			// NULL and an exact tenant/KB/generation/non-terminal identity.
			swapped, err = postProcessRepo.CompletePostProcessDeadLetterGeneration(
				ctx,
				probe.TenantID,
				probe.KnowledgeID,
				probe.KnowledgeBaseID,
				generation,
			)
		}
		if err != nil {
			logger.Warnf(ctx, "dead-letter callback: failed to repair knowledge %s after %s exhaustion: %v",
				probe.KnowledgeID, t.Type(), err)
			return
		}
		if !swapped {
			logger.Infof(ctx,
				"dead-letter callback: skipped stale %s task for knowledge %s because its identity, generation, or lifecycle changed",
				t.Type(), probe.KnowledgeID)
			return
		}
		// Close the matching root span so the timeline stops showing
		// "进行中" after dead-letter exhaustion. Best-effort: nil
		// tracker / missing attempt / missing root all no-op cleanly.
		if tracker != nil && probe.Attempt > 0 {
			if t.Type() == types.TypeKnowledgePostProcess {
				tracker.FinalizeAttempt(ctx, probe.KnowledgeID, probe.Attempt,
					types.SpanStatusDone, types.JSONMap{
						"enrichment_degraded": true,
						"postprocess_error":   errMsg,
					}, "", "")
			} else {
				tracker.FinalizeAttempt(ctx, probe.KnowledgeID, probe.Attempt,
					types.SpanStatusFailed, nil, "TASK_TIMEOUT", errMsg)
			}
		}
		if t.Type() == types.TypeKnowledgePostProcess {
			logger.Warnf(ctx,
				"dead-letter callback: completed knowledge %s with enrichment degraded after postprocess retries exhausted",
				probe.KnowledgeID)
		} else {
			logger.Infof(ctx, "dead-letter callback: marked knowledge %s as failed (task=%s)", probe.KnowledgeID, t.Type())
		}
	}
}

func markKnowledgeListDeleteFailed(
	ctx context.Context,
	repo interfaces.KnowledgeRepository,
	t *asynq.Task,
	taskErr error,
) {
	var payload deadLetterKnowledgeListDeletePayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil || len(payload.KnowledgeIDs) == 0 {
		return
	}
	errMsg := "delete task exhausted retries: " + taskErr.Error()
	if len(errMsg) > 8192 {
		errMsg = errMsg[:8192]
	}
	for _, knowledgeID := range payload.KnowledgeIDs {
		if knowledgeID == "" {
			continue
		}
		// Keep the durable deleting intent. The custom delete-recovery scanner
		// republishes a fresh task after Asynq exhausts this task's retry budget;
		// changing the row to failed here would permanently strand partially
		// cleaned storage and Wiki retract work.
		updated, err := repo.UpdateActiveDeletingKnowledgeColumns(ctx, knowledgeID, map[string]interface{}{
			"error_message": errMsg,
		})
		if err != nil {
			logger.Warnf(ctx, "dead-letter callback: failed to mark delete failure for knowledge %s: %v", knowledgeID, err)
			continue
		}
		if !updated {
			logger.Infof(ctx, "dead-letter callback: skipped marking knowledge %s after delete task exhaustion because it is no longer active deleting", knowledgeID)
			continue
		}
		logger.Infof(ctx, "dead-letter callback: retained durable deleting intent for knowledge %s after task retries exhausted", knowledgeID)
	}
}
