package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const (
	defaultGatewayURL = "https://llmgateway.moutai.com.cn/v1"
	defaultImagePath  = "/workspace/docs/images/architecture.png"
)

type fixture struct {
	Tenant types.Tenant
	Model  types.Model
	Spec   modeladmission.Spec
}

type appTarget struct {
	Name    string
	BaseURL string
}

type callPlan struct {
	App     appTarget
	Fixture *fixture
	Prompt  string
}

type callResult struct {
	App          string `json:"app"`
	TenantID     uint64 `json:"tenant_id"`
	ModelID      string `json:"model_id"`
	HTTPStatus   int    `json:"http_status"`
	OK           bool   `json:"ok"`
	ElapsedMS    int64  `json:"elapsed_ms"`
	ServerMS     int64  `json:"server_elapsed_ms,omitempty"`
	AnswerRunes  int    `json:"answer_runes,omitempty"`
	Error        string `json:"error,omitempty"`
	ClientError  string `json:"client_error,omitempty"`
	CircuitOpen  bool   `json:"circuit_open"`
	ProviderFail bool   `json:"provider_failure"`
}

type debugEnvelope struct {
	Success bool `json:"success"`
	Data    struct {
		OK          bool            `json:"ok"`
		ElapsedMS   int64           `json:"elapsed_ms"`
		RawResponse json.RawMessage `json:"raw_response"`
		Error       string          `json:"error"`
	} `json:"data"`
}

type pressureReport struct {
	MaxTotal      int64            `json:"max_total"`
	MaxPerTenant  map[uint64]int64 `json:"max_per_tenant"`
	Samples       int              `json:"samples"`
	Drained       bool             `json:"drained"`
	DrainElapsedM int64            `json:"drain_elapsed_ms"`
}

type groupReport struct {
	Name       string         `json:"name"`
	StartedAt  time.Time      `json:"started_at"`
	ElapsedMS  int64          `json:"elapsed_ms"`
	Pressure   pressureReport `json:"pressure"`
	Calls      []callResult   `json:"calls"`
	Assertions []string       `json:"assertions"`
	Passed     bool           `json:"passed"`
}

type runReport struct {
	RunID        string                 `json:"run_id"`
	Scenario     string                 `json:"scenario"`
	StartedAt    time.Time              `json:"started_at"`
	FinishedAt   time.Time              `json:"finished_at"`
	GatewayURL   string                 `json:"gateway_url"`
	ModelName    string                 `json:"model_name"`
	RedisDB      int                    `json:"redis_db"`
	KeyPrefix    string                 `json:"key_prefix"`
	Domain       string                 `json:"domain"`
	Health       map[string]string      `json:"health,omitempty"`
	Groups       []groupReport          `json:"groups,omitempty"`
	Assertions   []string               `json:"assertions"`
	Observations map[string]interface{} `json:"observations,omitempty"`
	Passed       bool                   `json:"passed"`
}

type workflowDocumentReport struct {
	KnowledgeID          string              `json:"knowledge_id"`
	Filename             string              `json:"filename"`
	SubmittedVia         string              `json:"submitted_via"`
	QueueWorkflowState   string              `json:"queue_workflow_state"`
	QueueWorkflowStage   string              `json:"queue_workflow_stage"`
	QueueDiagnostic      map[string]string   `json:"queue_terminal_diagnostic"`
	ParseStatus          string              `json:"parse_status"`
	EnableStatus         string              `json:"enable_status"`
	SummaryStatus        string              `json:"summary_status"`
	EnrichmentStatus     string              `json:"enrichment_status"`
	WikiStatus           string              `json:"wiki_status"`
	PendingSubtasks      int                 `json:"pending_subtasks"`
	ErrorMessage         string              `json:"error_message,omitempty"`
	ObservedRootOwners   []string            `json:"observed_root_owners,omitempty"`
	StageExecutors       map[string][]string `json:"stage_executors"`
	ChunkTypes           map[string]int64    `json:"chunk_types"`
	EmbeddingRows        int64               `json:"embedding_rows"`
	CompletedElapsedMS   int64               `json:"completed_elapsed_ms"`
	FailedSpanNames      []string            `json:"failed_span_names,omitempty"`
	HistoricalFailures   []string            `json:"historical_failed_span_names,omitempty"`
	MultimodalSpanMillis []int64             `json:"multimodal_span_ms,omitempty"`
}

type workflowReport struct {
	KnowledgeBaseID   string                   `json:"knowledge_base_id"`
	EmbeddingModelID  string                   `json:"embedding_model_id"`
	DerivativeModelID string                   `json:"derivative_model_id"`
	VLMModelID        string                   `json:"vlm_model_id"`
	Documents         []workflowDocumentReport `json:"documents"`
	ExecutorInstances []string                 `json:"executor_instances"`
	Pressure          pressureReport           `json:"vlm_pressure"`
	CleanupCompleted  bool                     `json:"cleanup_completed"`
}

type redisKeys struct {
	Total   string
	Circuit string
	Probe   string
	Tenant  map[uint64]string
}

type runner struct {
	apps       []appTarget
	fixtures   []*fixture
	redis      *redis.Client
	keys       redisKeys
	keyPrefix  string
	image      []byte
	imageName  string
	httpClient *http.Client
	runID      string
}

func main() {
	scenario := flag.String("scenario", "normal", "normal|stability|workflow|fault-sequence|half-open|hold|single|cleanup")
	expect := flag.String("expect", "success", "single scenario expectation: success|failure|either")
	holdCount := flag.Int("hold-count", 2, "number of concurrent calls for hold")
	rounds := flag.Int("rounds", 10, "number of four-request cohorts for stability")
	output := flag.String("output", "", "optional JSON report path")
	flag.Parse()

	started := time.Now().UTC()
	r, err := newRunner()
	if err != nil {
		fatalReport(*scenario, started, *output, err)
	}
	defer r.redis.Close()

	report := runReport{
		RunID:        r.runID,
		Scenario:     *scenario,
		StartedAt:    started,
		GatewayURL:   defaultGatewayURL,
		ModelName:    r.fixtures[0].Model.Name,
		RedisDB:      r.redis.Options().DB,
		KeyPrefix:    r.keyPrefix,
		Domain:       r.fixtures[0].Spec.Domain,
		Health:       map[string]string{},
		Observations: map[string]interface{}{},
		Passed:       true,
	}

	switch *scenario {
	case "normal":
		err = r.runNormal(&report)
	case "stability":
		err = r.runStability(&report, *rounds)
	case "workflow":
		err = r.runWorkflow(&report)
	case "fault-sequence":
		err = r.runFaultSequence(&report)
	case "half-open":
		err = r.runHalfOpen(&report)
	case "hold":
		err = r.runHold(&report, *holdCount)
	case "single":
		err = r.runSingle(&report, *expect)
	case "cleanup":
		err = r.cleanupPrefix(context.Background())
		if err == nil {
			report.Assertions = append(report.Assertions, "isolated Redis prefix removed")
		}
	default:
		err = fmt.Errorf("unsupported scenario %q", *scenario)
	}
	if err != nil {
		report.Passed = false
		report.Assertions = append(report.Assertions, "FAILED: "+err.Error())
	}
	report.FinishedAt = time.Now().UTC()
	if writeErr := writeReport(report, *output); writeErr != nil {
		fmt.Fprintln(os.Stderr, writeErr)
		os.Exit(1)
	}
	if !report.Passed {
		os.Exit(1)
	}
}

func (r *runner) runStability(report *runReport, rounds int) error {
	if rounds < 1 {
		return errors.New("rounds must be positive")
	}
	if err := r.cleanupPrefix(context.Background()); err != nil {
		return err
	}
	for _, app := range r.apps {
		status, err := health(r.httpClient, app)
		if err != nil {
			return err
		}
		report.Health[app.Name] = status
	}
	failures := 0
	admissionFailures := 0
	for round := 0; round < rounds; round++ {
		plans := make([]callPlan, 0, 4)
		for tenantIndex, item := range r.fixtures {
			for replicaIndex, app := range r.apps {
				plans = append(plans, callPlan{
					App:     app,
					Fixture: item,
					Prompt: fmt.Sprintf(
						"稳定性测试 %s，轮次=%d，租户=%d，副本=%d。请用 80 到 120 个中文字符概括图片，直接回答。",
						r.runID,
						round+1,
						tenantIndex+1,
						replicaIndex+1,
					),
				})
			}
		}
		group := r.runGroup(
			context.Background(),
			fmt.Sprintf("four_slot_stability_round_%02d_of_%02d", round+1, rounds),
			plans,
		)
		if group.Pressure.MaxTotal != 4 {
			group.Passed = false
			group.Assertions = append(
				group.Assertions,
				fmt.Sprintf("FAILED: provider pressure=%d, want 4", group.Pressure.MaxTotal),
			)
		}
		for _, item := range r.fixtures {
			if got := group.Pressure.MaxPerTenant[item.Tenant.ID]; got != 2 {
				group.Passed = false
				group.Assertions = append(
					group.Assertions,
					fmt.Sprintf("FAILED: tenant %d pressure=%d, want 2", item.Tenant.ID, got),
				)
			}
		}
		for _, result := range group.Calls {
			if result.OK {
				continue
			}
			if strings.Contains(strings.ToLower(result.Error), "model admission") {
				admissionFailures++
			} else {
				failures++
			}
		}
		report.Groups = append(report.Groups, group)
	}
	report.Observations["requested_calls"] = rounds * 4
	report.Observations["provider_call_failures"] = failures
	report.Observations["admission_wait_failures"] = admissionFailures
	report.Assertions = append(
		report.Assertions,
		fmt.Sprintf("%d bounded calls were distributed across two full WeKnora replicas", rounds*4),
		"every cohort targeted the real production llmgateway model and filled global/per-tenant limits 4/2",
	)
	if admissionFailures != 0 {
		return fmt.Errorf("stability run observed %d admission failures", admissionFailures)
	}
	if failures != 0 {
		return fmt.Errorf("stability run observed %d provider failures", failures)
	}
	return nil
}

type workflowKnowledgeRow struct {
	ID                   string    `gorm:"column:id"`
	Title                string    `gorm:"column:title"`
	ParseStatus          string    `gorm:"column:parse_status"`
	EnableStatus         string    `gorm:"column:enable_status"`
	SummaryStatus        string    `gorm:"column:summary_status"`
	EnrichmentStatus     string    `gorm:"column:enrichment_status"`
	WikiStatus           string    `gorm:"column:wiki_status"`
	PendingSubtasksCount int       `gorm:"column:pending_subtasks_count"`
	ProcessingOwner      string    `gorm:"column:processing_owner"`
	ProcessingGeneration string    `gorm:"column:processing_generation"`
	ErrorMessage         string    `gorm:"column:error_message"`
	UpdatedAt            time.Time `gorm:"column:updated_at"`
}

type workflowSpanRow struct {
	ID          int64  `gorm:"column:id"`
	KnowledgeID string `gorm:"column:knowledge_id"`
	Attempt     int    `gorm:"column:attempt"`
	Name        string `gorm:"column:name"`
	Status      string `gorm:"column:status"`
	Metadata    []byte `gorm:"column:metadata"`
	DurationMS  int64  `gorm:"column:duration_ms"`
}

type workflowChunkCount struct {
	KnowledgeID string `gorm:"column:knowledge_id"`
	ChunkType   string `gorm:"column:chunk_type"`
	Count       int64  `gorm:"column:count"`
}

type workflowTerminalRow struct {
	KnowledgeID        string `gorm:"column:knowledge_id"`
	State              string `gorm:"column:state"`
	Stage              string `gorm:"column:stage"`
	TerminalDiagnostic []byte `gorm:"column:terminal_diagnostic"`
	CreatedAt          time.Time
}

type workflowUpload struct {
	KnowledgeID string
	Filename    string
	App         appTarget
	Err         error
}

func (r *runner) runWorkflow(report *runReport) (retErr error) {
	ctx := context.Background()
	if err := r.cleanupPrefix(ctx); err != nil {
		return err
	}
	for _, app := range r.apps {
		status, err := health(r.httpClient, app)
		if err != nil {
			return err
		}
		report.Health[app.Name] = status
	}

	db, err := openDB()
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	const (
		embeddingModelID  = "prod-qwen3-embedding-8b"
		summaryModelID    = "prod-qwen36-35b-chat"
		derivativeModelID = "prod-qwen36-35b-derivative"
	)
	for _, modelID := range []string{
		embeddingModelID,
		summaryModelID,
		derivativeModelID,
		r.fixtures[0].Model.ID,
	} {
		if err := validateWorkflowModel(db, r.fixtures[0].Tenant.ID, modelID); err != nil {
			return err
		}
	}

	workflow := workflowReport{
		EmbeddingModelID:  embeddingModelID,
		DerivativeModelID: derivativeModelID,
		VLMModelID:        r.fixtures[0].Model.ID,
	}
	var kbID string
	var pressureCancel context.CancelFunc
	var pressureCh chan pressureReport
	pressureCollected := false
	collectPressure := func() {
		if pressureCollected || pressureCh == nil {
			return
		}
		pressureCancel()
		workflow.Pressure = <-pressureCh
		pressureCollected = true
		workflow.Pressure.Drained = waitForAllZero(
			context.Background(), r.redis, r.keys, 12*time.Second,
		)
	}
	defer func() {
		collectPressure()
		if kbID != "" {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			cleanupErr := r.deleteWorkflowKB(
				cleanupCtx, r.apps[1], r.fixtures[0], db, kbID,
			)
			cancel()
			workflow.CleanupCompleted = cleanupErr == nil
			if cleanupErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("cleanup workflow KB: %w", cleanupErr))
			}
		}
		if cleanupErr := r.cleanupPrefix(context.Background()); cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
		}
		report.Observations["workflow"] = workflow
	}()

	kbID, err = r.createWorkflowKB(
		ctx,
		r.apps[0],
		r.fixtures[0],
		embeddingModelID,
		summaryModelID,
		derivativeModelID,
	)
	if err != nil {
		return err
	}
	workflow.KnowledgeBaseID = kbID

	imagePaths := []string{
		"/workspace/docs/images/architecture.png",
		"/workspace/docs/images/wiki-browser.png",
		"/workspace/docs/images/pipeline.png",
		"/workspace/docs/images/graph1.png",
	}
	images := make([][]byte, len(imagePaths))
	for i, imagePath := range imagePaths {
		images[i], err = os.ReadFile(imagePath)
		if err != nil {
			return fmt.Errorf("read workflow image %s: %w", imagePath, err)
		}
	}

	monitorCtx, cancelMonitor := context.WithCancel(context.Background())
	pressureCancel = cancelMonitor
	pressureCh = make(chan pressureReport, 1)
	go func() {
		pressureCh <- monitorPressure(monitorCtx, r.redis, r.keys)
	}()

	workflowStarted := time.Now()
	uploads := make([]workflowUpload, len(imagePaths))
	start := make(chan struct{})
	var uploadWG sync.WaitGroup
	for i := range imagePaths {
		uploadWG.Add(1)
		go func(index int) {
			defer uploadWG.Done()
			<-start
			app := r.apps[index%len(r.apps)]
			filename := fmt.Sprintf(
				"vlm-replica-e2e-%02d-%s",
				index+1,
				filepath.Base(imagePaths[index]),
			)
			knowledgeID, uploadErr := r.uploadWorkflowImage(
				ctx,
				app,
				r.fixtures[0],
				kbID,
				filename,
				images[index],
			)
			uploads[index] = workflowUpload{
				KnowledgeID: knowledgeID,
				Filename:    filename,
				App:         app,
				Err:         uploadErr,
			}
		}(i)
	}
	close(start)
	uploadWG.Wait()

	knowledgeIDs := make([]string, 0, len(uploads))
	submittedVia := make(map[string]string, len(uploads))
	for _, upload := range uploads {
		if upload.Err != nil {
			return fmt.Errorf("upload %s via %s: %w", upload.Filename, upload.App.Name, upload.Err)
		}
		if strings.TrimSpace(upload.KnowledgeID) == "" {
			return fmt.Errorf("upload %s returned an empty knowledge id", upload.Filename)
		}
		knowledgeIDs = append(knowledgeIDs, upload.KnowledgeID)
		submittedVia[upload.KnowledgeID] = upload.App.Name
	}

	rows, owners, completedElapsed, err := waitWorkflowDocuments(
		ctx,
		db,
		r.fixtures[0].Tenant.ID,
		kbID,
		knowledgeIDs,
		workflowStarted,
		12*time.Minute,
	)
	collectPressure()
	if err != nil {
		return err
	}

	terminals, err := waitWorkflowTerminals(
		ctx,
		db,
		r.fixtures[0].Tenant.ID,
		kbID,
		knowledgeIDs,
		30*time.Second,
	)
	if err != nil {
		return err
	}

	documents, executors, err := inspectWorkflowEvidence(
		db,
		rows,
		owners,
		completedElapsed,
		submittedVia,
		terminals,
	)
	workflow.Documents = documents
	workflow.ExecutorInstances = executors
	if err != nil {
		return err
	}

	tenantPressure := workflow.Pressure.MaxPerTenant[r.fixtures[0].Tenant.ID]
	if workflow.Pressure.MaxTotal != 2 || tenantPressure != 2 {
		return fmt.Errorf(
			"background VLM pressure total/tenant=%d/%d, want production tenant ceiling 2/2",
			workflow.Pressure.MaxTotal,
			tenantPressure,
		)
	}
	if !workflow.Pressure.Drained {
		return errors.New("background workflow left VLM admission leases behind")
	}
	if exists, existsErr := r.redis.Exists(ctx, r.keys.Circuit, r.keys.Probe).Result(); existsErr != nil {
		return existsErr
	} else if exists != 0 {
		return fmt.Errorf("successful workflow polluted VLM circuit/probe state: %d keys", exists)
	}
	expectedExecutors := []string{"vlm-replica-e2e-a", "vlm-replica-e2e-b"}
	if len(workflow.ExecutorInstances) != len(expectedExecutors) {
		return fmt.Errorf(
			"background executors=%v, want exactly %v",
			workflow.ExecutorInstances,
			expectedExecutors,
		)
	}
	for index, expected := range expectedExecutors {
		if workflow.ExecutorInstances[index] != expected {
			return fmt.Errorf(
				"background executors=%v, want exactly %v",
				workflow.ExecutorInstances,
				expectedExecutors,
			)
		}
	}

	report.Assertions = append(
		report.Assertions,
		"four distinct image uploads traversed the durable background document workflow",
		"VLM, embedding and derivative models all resolve directly to the production llmgateway",
		"both local WeKnora replicas executed background stages from one shared Redis queue",
		"every document reached completed with OCR, caption and persisted embedding evidence",
		"every durable queue workflow reached completed with an atomic done diagnostic before cleanup",
		"background VLM work respected the shared per-tenant production ceiling of 2 and drained every lease",
	)
	return nil
}

func validateWorkflowModel(db *gorm.DB, tenantID uint64, modelID string) error {
	var model types.Model
	if err := db.First(
		&model,
		"id = ? AND tenant_id = ? AND deleted_at IS NULL",
		modelID,
		tenantID,
	).Error; err != nil {
		return fmt.Errorf("load workflow model %s: %w", modelID, err)
	}
	if strings.TrimRight(model.Parameters.BaseURL, "/") != strings.TrimRight(defaultGatewayURL, "/") {
		return fmt.Errorf(
			"workflow model %s points to %s, want production gateway %s",
			modelID,
			model.Parameters.BaseURL,
			defaultGatewayURL,
		)
	}
	return nil
}

func (r *runner) createWorkflowKB(
	ctx context.Context,
	app appTarget,
	item *fixture,
	embeddingModelID string,
	summaryModelID string,
	derivativeModelID string,
) (string, error) {
	body := map[string]interface{}{
		"name":        "VLM-Replica-E2E-" + strings.ReplaceAll(r.runID, ".", "-"),
		"description": "Disposable local two-replica workflow test using the production llmgateway",
		"type":        types.KnowledgeBaseTypeDocument,
		"chunking_config": map[string]interface{}{
			"chunk_size":    2048,
			"chunk_overlap": 128,
			"strategy":      "auto",
			"token_limit":   1024,
		},
		"embedding_model_id":  embeddingModelID,
		"summary_model_id":    summaryModelID,
		"derivative_model_id": derivativeModelID,
		"image_processing_config": map[string]interface{}{
			"model_id": item.Model.ID,
		},
		"vlm_config": map[string]interface{}{
			"enabled":  true,
			"model_id": item.Model.ID,
		},
		"asr_config": map[string]interface{}{
			"enabled": false,
		},
		"question_generation_config": map[string]interface{}{
			"enabled":        false,
			"question_count": 0,
		},
		"extract_config": map[string]interface{}{
			"enabled": false,
		},
		"indexing_strategy": map[string]interface{}{
			"vector_enabled":  true,
			"keyword_enabled": true,
			"wiki_enabled":    false,
			"graph_enabled":   false,
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	data, err := r.requestAPI(
		ctx,
		app,
		item,
		http.MethodPost,
		"/api/v1/knowledge-bases",
		payload,
		"application/json",
	)
	if err != nil {
		return "", fmt.Errorf("create workflow knowledge base: %w", err)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		return "", fmt.Errorf("decode workflow knowledge base: %w", err)
	}
	if strings.TrimSpace(created.ID) == "" {
		return "", errors.New("create workflow knowledge base returned no id")
	}
	return created.ID, nil
}

func (r *runner) uploadWorkflowImage(
	ctx context.Context,
	app appTarget,
	item *fixture,
	kbID string,
	filename string,
	content []byte,
) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("fileName", filename); err != nil {
		return "", err
	}
	if err := writer.WriteField("enable_multimodel", "true"); err != nil {
		return "", err
	}
	metadata, _ := json.Marshal(map[string]string{
		"e2e_run_id": r.runID,
		"e2e_kind":   "vlm_replica_background_workflow",
	})
	if err := writer.WriteField("metadata", string(metadata)); err != nil {
		return "", err
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(content); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	data, err := r.requestAPI(
		ctx,
		app,
		item,
		http.MethodPost,
		"/api/v1/knowledge-bases/"+kbID+"/knowledge/file",
		body.Bytes(),
		writer.FormDataContentType(),
	)
	if err != nil {
		return "", err
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		return "", fmt.Errorf("decode uploaded knowledge: %w", err)
	}
	return strings.TrimSpace(created.ID), nil
}

func (r *runner) requestAPI(
	ctx context.Context,
	app appTarget,
	item *fixture,
	method string,
	path string,
	body []byte,
	contentType string,
) (json.RawMessage, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		method,
		strings.TrimRight(app.BaseURL, "/")+path,
		bodyReader,
	)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-API-Key", item.Tenant.APIKey)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := r.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if readErr != nil {
		return nil, readErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf(
			"%s %s status %d: %s",
			method,
			path,
			response.StatusCode,
			truncate(string(payload), 1000),
		)
	}
	if len(payload) == 0 {
		return nil, nil
	}
	var envelope struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode %s %s response: %w", method, path, err)
	}
	if !envelope.Success {
		return nil, fmt.Errorf(
			"%s %s failed: %s",
			method,
			path,
			truncate(envelope.Message, 500),
		)
	}
	return envelope.Data, nil
}

func waitWorkflowDocuments(
	ctx context.Context,
	db *gorm.DB,
	tenantID uint64,
	kbID string,
	knowledgeIDs []string,
	started time.Time,
	timeout time.Duration,
) (
	[]workflowKnowledgeRow,
	map[string]map[string]struct{},
	map[string]int64,
	error,
) {
	owners := make(map[string]map[string]struct{}, len(knowledgeIDs))
	completedElapsed := make(map[string]int64, len(knowledgeIDs))
	for _, knowledgeID := range knowledgeIDs {
		owners[knowledgeID] = make(map[string]struct{})
	}
	deadline := time.Now().Add(timeout)
	var lastRows []workflowKnowledgeRow
	for time.Now().Before(deadline) {
		var rows []workflowKnowledgeRow
		err := db.WithContext(ctx).
			Table("knowledges").
			Where(
				"tenant_id = ? AND knowledge_base_id = ? AND id IN ? AND deleted_at IS NULL",
				tenantID,
				kbID,
				knowledgeIDs,
			).
			Find(&rows).Error
		if err != nil {
			return nil, owners, completedElapsed, err
		}
		lastRows = rows
		allCompleted := len(rows) == len(knowledgeIDs)
		for _, row := range rows {
			if owner := strings.TrimSpace(row.ProcessingOwner); owner != "" {
				owners[row.ID][owner] = struct{}{}
			}
			switch strings.ToLower(strings.TrimSpace(row.ParseStatus)) {
			case types.ParseStatusFailed, types.ParseStatusCancelled, types.ParseStatusDeleting:
				return rows, owners, completedElapsed, fmt.Errorf(
					"workflow document %s became %s: %s",
					row.ID,
					row.ParseStatus,
					row.ErrorMessage,
				)
			case types.ParseStatusCompleted:
				if _, ok := completedElapsed[row.ID]; !ok {
					completedElapsed[row.ID] = time.Since(started).Milliseconds()
				}
			default:
				allCompleted = false
			}
		}
		if allCompleted {
			return rows, owners, completedElapsed, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	statuses := make([]string, 0, len(lastRows))
	for _, row := range lastRows {
		statuses = append(statuses, row.ID+"="+row.ParseStatus)
	}
	sort.Strings(statuses)
	return lastRows, owners, completedElapsed, fmt.Errorf(
		"workflow documents did not complete within %s: %s",
		timeout,
		strings.Join(statuses, ", "),
	)
}

func waitWorkflowTerminals(
	ctx context.Context,
	db *gorm.DB,
	tenantID uint64,
	kbID string,
	knowledgeIDs []string,
	timeout time.Duration,
) (map[string]workflowTerminalRow, error) {
	deadline := time.Now().Add(timeout)
	lastStates := make(map[string]string, len(knowledgeIDs))
	for time.Now().Before(deadline) {
		var rows []workflowTerminalRow
		if err := db.WithContext(ctx).
			Table("custom_document_queue_workflows").
			Select("knowledge_id, state, stage, terminal_diagnostic, created_at").
			Where(
				"tenant_id = ? AND knowledge_base_id = ? AND knowledge_id IN ?",
				tenantID,
				kbID,
				knowledgeIDs,
			).
			Order("created_at DESC").
			Scan(&rows).Error; err != nil {
			return nil, err
		}

		latest := make(map[string]workflowTerminalRow, len(knowledgeIDs))
		allCompleted := true
		for _, row := range rows {
			if _, exists := latest[row.KnowledgeID]; exists {
				continue
			}
			latest[row.KnowledgeID] = row
			lastStates[row.KnowledgeID] = row.State + "/" + row.Stage
			switch strings.ToLower(strings.TrimSpace(row.State)) {
			case "failed", "cancelled", "superseded":
				return latest, fmt.Errorf(
					"durable workflow %s became %s/%s before cleanup",
					row.KnowledgeID,
					row.State,
					row.Stage,
				)
			case "completed":
				if !strings.EqualFold(strings.TrimSpace(row.Stage), "completed") {
					return latest, fmt.Errorf(
						"durable workflow %s completed with stage %s",
						row.KnowledgeID,
						row.Stage,
					)
				}
				diagnostic, err := decodeWorkflowTerminalDiagnostic(row)
				if err != nil {
					return latest, err
				}
				if diagnostic["source"] != "workflow" ||
					diagnostic["status"] != types.SpanStatusDone ||
					diagnostic["error_code"] != "" ||
					diagnostic["error_message"] != "" {
					return latest, fmt.Errorf(
						"durable workflow %s has unexpected terminal diagnostic %v",
						row.KnowledgeID,
						diagnostic,
					)
				}
			default:
				allCompleted = false
			}
		}
		if len(latest) != len(knowledgeIDs) {
			allCompleted = false
		}
		if allCompleted {
			return latest, nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	statuses := make([]string, 0, len(knowledgeIDs))
	for _, knowledgeID := range knowledgeIDs {
		status := lastStates[knowledgeID]
		if status == "" {
			status = "missing"
		}
		statuses = append(statuses, knowledgeID+"="+status)
	}
	sort.Strings(statuses)
	return nil, fmt.Errorf(
		"durable workflows did not complete within %s: %s",
		timeout,
		strings.Join(statuses, ", "),
	)
}

func decodeWorkflowTerminalDiagnostic(row workflowTerminalRow) (map[string]string, error) {
	var diagnostic map[string]string
	if len(row.TerminalDiagnostic) == 0 {
		return nil, fmt.Errorf(
			"durable workflow %s has no terminal diagnostic",
			row.KnowledgeID,
		)
	}
	if err := json.Unmarshal(row.TerminalDiagnostic, &diagnostic); err != nil {
		return nil, fmt.Errorf(
			"decode durable workflow %s terminal diagnostic: %w",
			row.KnowledgeID,
			err,
		)
	}
	return diagnostic, nil
}

func classifyWorkflowSpanFailures(
	rows []workflowSpanRow,
) (map[string][]string, map[string][]string) {
	historical := make(map[string][]string)
	latestAttempts := make(map[string]int)
	latestByName := make(map[string]map[string]workflowSpanRow)
	for _, row := range rows {
		if strings.EqualFold(row.Status, types.SpanStatusFailed) {
			historical[row.KnowledgeID] = append(historical[row.KnowledgeID], row.Name)
		}
		if _, exists := latestByName[row.KnowledgeID]; !exists ||
			row.Attempt > latestAttempts[row.KnowledgeID] {
			latestAttempts[row.KnowledgeID] = row.Attempt
			latestByName[row.KnowledgeID] = make(map[string]workflowSpanRow)
		}
		if row.Attempt != latestAttempts[row.KnowledgeID] {
			continue
		}
		previous, exists := latestByName[row.KnowledgeID][row.Name]
		if !exists || row.ID >= previous.ID {
			// Durable retries deliberately preserve the failed row and open
			// another row with the same name. The greatest ID in the latest
			// attempt is the authoritative current result.
			latestByName[row.KnowledgeID][row.Name] = row
		}
	}

	latestFailures := make(map[string][]string)
	for knowledgeID, spans := range latestByName {
		for name, row := range spans {
			if strings.EqualFold(row.Status, types.SpanStatusFailed) {
				latestFailures[knowledgeID] = append(latestFailures[knowledgeID], name)
			}
		}
		latestFailures[knowledgeID] = uniqueSorted(latestFailures[knowledgeID])
	}
	for knowledgeID := range historical {
		historical[knowledgeID] = uniqueSorted(historical[knowledgeID])
	}
	return historical, latestFailures
}

func inspectWorkflowEvidence(
	db *gorm.DB,
	rows []workflowKnowledgeRow,
	owners map[string]map[string]struct{},
	completedElapsed map[string]int64,
	submittedVia map[string]string,
	terminals map[string]workflowTerminalRow,
) ([]workflowDocumentReport, []string, error) {
	knowledgeIDs := make([]string, 0, len(rows))
	rowByID := make(map[string]workflowKnowledgeRow, len(rows))
	for _, row := range rows {
		knowledgeIDs = append(knowledgeIDs, row.ID)
		rowByID[row.ID] = row
	}

	var spanRows []workflowSpanRow
	if err := db.Table("knowledge_processing_spans").
		Select("id, knowledge_id, attempt, name, status, metadata, COALESCE(duration_ms, 0) AS duration_ms").
		Where("knowledge_id IN ?", knowledgeIDs).
		Order("knowledge_id, attempt, id").
		Scan(&spanRows).Error; err != nil {
		return nil, nil, err
	}
	var chunkCounts []workflowChunkCount
	if err := db.Table("chunks").
		Select("knowledge_id, chunk_type, COUNT(*) AS count").
		Where("knowledge_id IN ? AND deleted_at IS NULL", knowledgeIDs).
		Group("knowledge_id, chunk_type").
		Scan(&chunkCounts).Error; err != nil {
		return nil, nil, err
	}

	chunksByKnowledge := make(map[string]map[string]int64, len(rows))
	for _, count := range chunkCounts {
		if chunksByKnowledge[count.KnowledgeID] == nil {
			chunksByKnowledge[count.KnowledgeID] = make(map[string]int64)
		}
		chunksByKnowledge[count.KnowledgeID][count.ChunkType] = count.Count
	}
	executorSets := make(map[string]map[string]struct{}, len(rows))
	historicalFailedSpans, latestFailedSpans := classifyWorkflowSpanFailures(spanRows)
	multimodalMillis := make(map[string][]int64, len(rows))
	globalExecutors := make(map[string]struct{})
	for _, span := range spanRows {
		var metadata map[string]interface{}
		if len(span.Metadata) > 0 {
			_ = json.Unmarshal(span.Metadata, &metadata)
		}
		executor, _ := metadata["executor_instance_id"].(string)
		executor = strings.TrimSpace(executor)
		if executor != "" {
			if executorSets[span.KnowledgeID] == nil {
				executorSets[span.KnowledgeID] = make(map[string]struct{})
			}
			executorSets[span.KnowledgeID][span.Name+"\x00"+executor] = struct{}{}
			globalExecutors[executor] = struct{}{}
		}
		if strings.Contains(strings.ToLower(span.Name), "multimodal") && span.DurationMS > 0 {
			multimodalMillis[span.KnowledgeID] = append(
				multimodalMillis[span.KnowledgeID],
				span.DurationMS,
			)
		}
	}

	documents := make([]workflowDocumentReport, 0, len(rows))
	var evidenceErrors []error
	for _, knowledgeID := range knowledgeIDs {
		row := rowByID[knowledgeID]
		stageExecutors := make(map[string][]string)
		hasMultimodalExecutor := false
		for composite := range executorSets[knowledgeID] {
			parts := strings.SplitN(composite, "\x00", 2)
			if len(parts) != 2 {
				continue
			}
			stageExecutors[parts[0]] = append(stageExecutors[parts[0]], parts[1])
			if strings.Contains(strings.ToLower(parts[0]), "multimodal") {
				hasMultimodalExecutor = true
			}
		}
		for name := range stageExecutors {
			sort.Strings(stageExecutors[name])
		}
		ownerList := setKeys(owners[knowledgeID])
		historicalFailures := historicalFailedSpans[knowledgeID]
		failed := latestFailedSpans[knowledgeID]
		chunkTypes := chunksByKnowledge[knowledgeID]
		if chunkTypes == nil {
			chunkTypes = map[string]int64{}
		}
		var embeddingRows int64
		if err := db.Table("embeddings").
			Where("knowledge_id = ?", knowledgeID).
			Count(&embeddingRows).Error; err != nil {
			return nil, nil, err
		}
		terminal := terminals[knowledgeID]
		terminalDiagnostic, err := decodeWorkflowTerminalDiagnostic(terminal)
		if err != nil {
			return nil, nil, err
		}
		document := workflowDocumentReport{
			KnowledgeID:          knowledgeID,
			Filename:             row.Title,
			SubmittedVia:         submittedVia[knowledgeID],
			QueueWorkflowState:   terminal.State,
			QueueWorkflowStage:   terminal.Stage,
			QueueDiagnostic:      terminalDiagnostic,
			ParseStatus:          row.ParseStatus,
			EnableStatus:         row.EnableStatus,
			SummaryStatus:        row.SummaryStatus,
			EnrichmentStatus:     row.EnrichmentStatus,
			WikiStatus:           row.WikiStatus,
			PendingSubtasks:      row.PendingSubtasksCount,
			ErrorMessage:         row.ErrorMessage,
			ObservedRootOwners:   ownerList,
			StageExecutors:       stageExecutors,
			ChunkTypes:           chunkTypes,
			EmbeddingRows:        embeddingRows,
			CompletedElapsedMS:   completedElapsed[knowledgeID],
			FailedSpanNames:      failed,
			HistoricalFailures:   historicalFailures,
			MultimodalSpanMillis: multimodalMillis[knowledgeID],
		}
		documents = append(documents, document)

		if !strings.EqualFold(row.ParseStatus, types.ParseStatusCompleted) ||
			!strings.EqualFold(row.EnableStatus, "enabled") ||
			row.PendingSubtasksCount != 0 {
			evidenceErrors = append(evidenceErrors, fmt.Errorf(
				"%s terminal state parse=%s enable=%s pending=%d",
				knowledgeID,
				row.ParseStatus,
				row.EnableStatus,
				row.PendingSubtasksCount,
			))
		}
		if !strings.EqualFold(row.SummaryStatus, types.SummaryStatusCompleted) {
			evidenceErrors = append(evidenceErrors, fmt.Errorf(
				"%s summary status=%s",
				knowledgeID,
				row.SummaryStatus,
			))
		}
		if len(failed) != 0 {
			evidenceErrors = append(evidenceErrors, fmt.Errorf(
				"%s failed spans=%v",
				knowledgeID,
				failed,
			))
		}
		if chunkTypes[string(types.ChunkTypeImageOCR)] == 0 ||
			chunkTypes[string(types.ChunkTypeImageCaption)] == 0 {
			evidenceErrors = append(evidenceErrors, fmt.Errorf(
				"%s missing image OCR/caption chunks: %v",
				knowledgeID,
				chunkTypes,
			))
		}
		if embeddingRows == 0 {
			evidenceErrors = append(evidenceErrors, fmt.Errorf(
				"%s has no persisted embeddings",
				knowledgeID,
			))
		}
		if !hasMultimodalExecutor {
			evidenceErrors = append(evidenceErrors, fmt.Errorf(
				"%s has no executor-tagged multimodal span",
				knowledgeID,
			))
		}
	}
	sort.Slice(documents, func(i, j int) bool {
		return documents[i].Filename < documents[j].Filename
	})
	executors := setKeys(globalExecutors)
	if len(evidenceErrors) > 0 {
		return documents, executors, errors.Join(evidenceErrors...)
	}
	return documents, executors, nil
}

func (r *runner) deleteWorkflowKB(
	ctx context.Context,
	app appTarget,
	item *fixture,
	db *gorm.DB,
	kbID string,
) error {
	if _, err := r.requestAPI(
		ctx,
		app,
		item,
		http.MethodDelete,
		"/api/v1/knowledge-bases/"+kbID,
		nil,
		"",
	); err != nil {
		return err
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		var liveKB, liveKnowledge, liveChunks, embeddings, pendingOps int64
		if err := db.Table("knowledge_bases").
			Where("id = ? AND deleted_at IS NULL", kbID).
			Count(&liveKB).Error; err != nil {
			return err
		}
		if err := db.Table("knowledges").
			Where("knowledge_base_id = ? AND deleted_at IS NULL", kbID).
			Count(&liveKnowledge).Error; err != nil {
			return err
		}
		if err := db.Table("chunks").
			Where("knowledge_base_id = ? AND deleted_at IS NULL", kbID).
			Count(&liveChunks).Error; err != nil {
			return err
		}
		if err := db.Table("embeddings").
			Where("knowledge_base_id = ?", kbID).
			Count(&embeddings).Error; err != nil {
			return err
		}
		if err := db.Table("task_pending_ops").
			Where("task_type = ? AND scope_id = ?", types.TypeKBDelete, kbID).
			Count(&pendingOps).Error; err != nil {
			return err
		}
		if liveKB == 0 && liveKnowledge == 0 && liveChunks == 0 &&
			embeddings == 0 && pendingOps == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf(
				"cleanup did not drain: kb=%d knowledge=%d chunks=%d embeddings=%d pending_ops=%d: %w",
				liveKB,
				liveKnowledge,
				liveChunks,
				embeddings,
				pendingOps,
				ctx.Err(),
			)
		case <-ticker.C:
		}
	}
}

func setKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func uniqueSorted(values []string) []string {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return setKeys(set)
}

func newRunner() (*runner, error) {
	apps := []appTarget{
		{Name: "app-a", BaseURL: envString("VLM_E2E_APP_A", "http://weknora-vlm-replica-e2e-a:8080")},
		{Name: "app-b", BaseURL: envString("VLM_E2E_APP_B", "http://weknora-vlm-replica-e2e-b:8080")},
	}
	db, err := openDB()
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	defer sqlDB.Close()

	modelIDs := []struct {
		tenantID uint64
		modelID  string
	}{
		{tenantID: envUint64("VLM_E2E_TENANT_A", 10000), modelID: envString("VLM_E2E_MODEL_A", "prod-qwen3-vl-32b-vlm")},
		{tenantID: envUint64("VLM_E2E_TENANT_B", 10002), modelID: envString("VLM_E2E_MODEL_B", "c6b2c22e-ac2e-5b40-a035-3f46f5c31abc")},
	}
	fixtures := make([]*fixture, 0, len(modelIDs))
	for _, item := range modelIDs {
		var tenant types.Tenant
		if err := db.First(&tenant, "id = ? AND deleted_at IS NULL", item.tenantID).Error; err != nil {
			return nil, fmt.Errorf("load tenant %d: %w", item.tenantID, err)
		}
		var model types.Model
		if err := db.First(
			&model,
			"id = ? AND tenant_id = ? AND deleted_at IS NULL",
			item.modelID,
			item.tenantID,
		).Error; err != nil {
			return nil, fmt.Errorf("load model %s for tenant %d: %w", item.modelID, item.tenantID, err)
		}
		if strings.TrimRight(model.Parameters.BaseURL, "/") != strings.TrimRight(defaultGatewayURL, "/") {
			return nil, fmt.Errorf(
				"model %s points to %s, want production gateway %s",
				model.ID,
				model.Parameters.BaseURL,
				defaultGatewayURL,
			)
		}
		if !strings.EqualFold(strings.TrimSpace(model.Name), "Qwen3-VL-32B") {
			return nil, fmt.Errorf("model %s is %q, want Qwen3-VL-32B", model.ID, model.Name)
		}
		if strings.TrimSpace(tenant.APIKey) == "" {
			return nil, fmt.Errorf("tenant %d API key is empty", tenant.ID)
		}
		spec := modeladmission.SpecForModel(modeladmission.KindVLM, &model, "")
		fixtures = append(fixtures, &fixture{Tenant: tenant, Model: model, Spec: spec})
	}
	if fixtures[0].Spec.Domain != fixtures[1].Spec.Domain {
		return nil, fmt.Errorf(
			"production model configs do not share one provider domain: %s != %s",
			fixtures[0].Spec.Domain,
			fixtures[1].Spec.Domain,
		)
	}

	redisDB := envInt("VLM_E2E_REDIS_DB", 8)
	redisClient := redis.NewClient(&redis.Options{
		Addr:       envString("REDIS_ADDR", "redis:6379"),
		Username:   os.Getenv("REDIS_USERNAME"),
		Password:   os.Getenv("REDIS_PASSWORD"),
		DB:         redisDB,
		MaxRetries: -1,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := redisClient.Ping(ctx).Err(); err != nil {
		redisClient.Close()
		return nil, fmt.Errorf("ping Redis DB %d: %w", redisDB, err)
	}

	keyPrefix := envString("VLM_E2E_KEY_PREFIX", "weknora:e2e:vlm-replica:")
	base := keyPrefix + "{" + fixtures[0].Spec.Domain + "}"
	keys := redisKeys{
		Total:   base + ":total",
		Circuit: base + ":circuit",
		Probe:   base + ":circuit-probe",
		Tenant: map[uint64]string{
			fixtures[0].Tenant.ID: base + ":tenant:" + strconv.FormatUint(fixtures[0].Tenant.ID, 10),
			fixtures[1].Tenant.ID: base + ":tenant:" + strconv.FormatUint(fixtures[1].Tenant.ID, 10),
		},
	}

	imagePath := envString("VLM_E2E_IMAGE", defaultImagePath)
	image, err := os.ReadFile(imagePath)
	if err != nil {
		redisClient.Close()
		return nil, fmt.Errorf("read image %s: %w", imagePath, err)
	}
	return &runner{
		apps:       apps,
		fixtures:   fixtures,
		redis:      redisClient,
		keys:       keys,
		keyPrefix:  keyPrefix,
		image:      image,
		imageName:  filepath.Base(imagePath),
		httpClient: &http.Client{Timeout: 8 * time.Minute},
		runID:      envString("VLM_E2E_RUN_ID", time.Now().UTC().Format("20060102T150405.000000000Z")),
	}, nil
}

func openDB() (*gorm.DB, error) {
	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable TimeZone=Asia/Shanghai",
		envString("DB_HOST", "postgres"),
		envString("DB_PORT", "5432"),
		envString("DB_USER", "postgres"),
		os.Getenv("DB_PASSWORD"),
		envString("DB_NAME", "WeKnora"),
	)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("connect PostgreSQL: %w", err)
	}
	return db, nil
}

func (r *runner) runNormal(report *runReport) error {
	ctx := context.Background()
	if err := r.cleanupPrefix(ctx); err != nil {
		return err
	}
	for _, app := range r.apps {
		status, err := health(r.httpClient, app)
		if err != nil {
			return err
		}
		report.Health[app.Name] = status
	}
	report.Assertions = append(
		report.Assertions,
		"two independent WeKnora HTTP services are healthy",
		"both tenant model rows resolve to one production llmgateway/Qwen3-VL-32B circuit domain",
	)

	baselinePrompt := fmt.Sprintf(
		"测试标记 %s。请用 80 到 120 个中文字符概括这张架构图，直接给出结论。",
		r.runID,
	)
	baselinePlans := []callPlan{
		{App: r.apps[0], Fixture: r.fixtures[0], Prompt: baselinePrompt + " 请求来自副本A。"},
		{App: r.apps[1], Fixture: r.fixtures[0], Prompt: baselinePrompt + " 请求来自副本B。"},
	}
	baseline := r.runGroup(ctx, "real_gateway_baseline_both_replicas", baselinePlans)
	report.Groups = append(report.Groups, baseline)
	if !baseline.Passed {
		return errors.New("baseline calls did not succeed on both replicas")
	}

	loadPrompt := fmt.Sprintf(
		"测试标记 %s。请详细识别图中的组件、连线和数据流，回答至少 300 个中文字符；持续正常输出，不要只给一句总结。",
		r.runID,
	)
	// The debug endpoint is intentionally classified as interactive and has a
	// production 30-second admission wait. Actual image:multimodal work is
	// classified by AsynqMiddleware as background and waits on its durable task
	// context. Submit bounded cohorts that fill every production slot without
	// manufacturing interactive-wait failures unrelated to the provider.
	gatewayFailures := 0
	admissionFailures := 0
	for round := 0; round < 4; round++ {
		plans := []callPlan{
			{
				App:     r.apps[0],
				Fixture: r.fixtures[0],
				Prompt:  fmt.Sprintf("%s 同租户轮次=%d，副本=A。", loadPrompt, round+1),
			},
			{
				App:     r.apps[1],
				Fixture: r.fixtures[0],
				Prompt:  fmt.Sprintf("%s 同租户轮次=%d，副本=B。", loadPrompt, round+1),
			},
		}
		group := r.runGroup(ctx, fmt.Sprintf("same_tenant_round_%d_of_4", round+1), plans)
		group.Assertions = append(group.Assertions,
			"the cohort fills the shared per-tenant ceiling of 2 without creating artificial interactive waiters",
		)
		tenantMax := group.Pressure.MaxPerTenant[r.fixtures[0].Tenant.ID]
		if tenantMax != 2 || group.Pressure.MaxTotal != 2 {
			group.Passed = false
			group.Assertions = append(
				group.Assertions,
				fmt.Sprintf("FAILED: observed total/tenant pressure %d/%d, want 2/2", group.Pressure.MaxTotal, tenantMax),
			)
		}
		for _, result := range group.Calls {
			if !result.OK {
				if strings.Contains(strings.ToLower(result.Error), "model admission") {
					admissionFailures++
				} else {
					gatewayFailures++
				}
			}
		}
		report.Groups = append(report.Groups, group)
	}

	for round := 0; round < 3; round++ {
		plans := make([]callPlan, 0, 4)
		for tenantIndex, item := range r.fixtures {
			for replicaIndex, app := range r.apps {
				plans = append(plans, callPlan{
					App:     app,
					Fixture: item,
					Prompt: fmt.Sprintf(
						"%s 跨租户轮次=%d，租户序号=%d，副本序号=%d。",
						loadPrompt,
						round+1,
						tenantIndex+1,
						replicaIndex+1,
					),
				})
			}
		}
		group := r.runGroup(ctx, fmt.Sprintf("two_tenants_round_%d_of_3", round+1), plans)
		group.Assertions = append(group.Assertions,
			"the cohort fills the shared provider ceiling of 4 and each tenant ceiling of 2",
		)
		if group.Pressure.MaxTotal != 4 {
			group.Passed = false
			group.Assertions = append(
				group.Assertions,
				fmt.Sprintf("FAILED: observed provider pressure %d, want 4", group.Pressure.MaxTotal),
			)
		}
		for _, item := range r.fixtures {
			if got := group.Pressure.MaxPerTenant[item.Tenant.ID]; got != 2 {
				group.Passed = false
				group.Assertions = append(
					group.Assertions,
					fmt.Sprintf("FAILED: tenant %d pressure=%d, want 2", item.Tenant.ID, got),
				)
			}
		}
		for _, result := range group.Calls {
			if !result.OK {
				if strings.Contains(strings.ToLower(result.Error), "model admission") {
					admissionFailures++
				} else {
					gatewayFailures++
				}
			}
		}
		report.Groups = append(report.Groups, group)
	}
	report.Observations["provider_call_failures"] = gatewayFailures
	report.Observations["admission_wait_failures"] = admissionFailures

	cancelReport, err := r.runCancellation(ctx)
	report.Groups = append(report.Groups, cancelReport)
	if err != nil {
		return err
	}
	report.Assertions = append(report.Assertions,
		"all successful calls used the real production llmgateway model configuration",
		"client cancellation released the distributed lease without opening the provider circuit",
	)
	if admissionFailures != 0 {
		return fmt.Errorf("observed %d unexpected admission failures in bounded cohorts", admissionFailures)
	}
	if gatewayFailures != 0 {
		return fmt.Errorf("real llmgateway produced %d failures during bounded cohorts", gatewayFailures)
	}
	return nil
}

func (r *runner) runFaultSequence(report *runReport) error {
	if err := r.cleanupPrefix(context.Background()); err != nil {
		return err
	}
	report.Assertions = append(report.Assertions,
		"fault is local DNS blackholing only; saved URL/model/credential and circuit domain remain production-identical",
	)
	results := make([]callResult, 0, 4)
	for i := 0; i < 3; i++ {
		plan := callPlan{
			App:     r.apps[i%len(r.apps)],
			Fixture: r.fixtures[0],
			Prompt:  fmt.Sprintf("VLM circuit fault %s request %d", r.runID, i+1),
		}
		results = append(results, r.callVLM(context.Background(), plan))
	}
	rejectedStarted := time.Now()
	rejected := r.callVLM(context.Background(), callPlan{
		App:     r.apps[1],
		Fixture: r.fixtures[0],
		Prompt:  "this request must be rejected by the shared open circuit",
	})
	rejected.ElapsedMS = time.Since(rejectedStarted).Milliseconds()
	results = append(results, rejected)
	group := groupReport{
		Name:       "three_cross_replica_failures_then_fast_reject",
		StartedAt:  report.StartedAt,
		Calls:      results,
		Passed:     true,
		Assertions: []string{"three real call attempts are followed by one pre-call shared-circuit rejection"},
	}
	for i := 0; i < 3; i++ {
		if results[i].OK || results[i].CircuitOpen {
			group.Passed = false
			group.Assertions = append(
				group.Assertions,
				fmt.Sprintf("FAILED: call %d was not a provider-call failure", i+1),
			)
		}
	}
	if rejected.OK || !rejected.CircuitOpen || rejected.ElapsedMS > 1500 {
		group.Passed = false
		group.Assertions = append(group.Assertions, "FAILED: fourth call was not a fast circuit-open rejection")
	}
	exists, err := r.redis.Exists(context.Background(), r.keys.Circuit).Result()
	if err != nil {
		return err
	}
	if exists != 1 {
		group.Passed = false
		group.Assertions = append(group.Assertions, "FAILED: shared Redis circuit state is missing")
	}
	group.ElapsedMS = time.Since(report.StartedAt).Milliseconds()
	report.Groups = append(report.Groups, group)
	report.Observations["circuit_key_exists"] = exists == 1
	if !group.Passed {
		return errors.New("shared circuit did not open after the configured three failures")
	}
	return nil
}

func (r *runner) runHalfOpen(report *runReport) error {
	exists, err := r.redis.Exists(context.Background(), r.keys.Circuit).Result()
	if err != nil {
		return err
	}
	if exists != 1 {
		return errors.New("no preserved open circuit found; run fault-sequence first")
	}
	prompt := fmt.Sprintf(
		"Half-open marker %s。请详细说明图片架构，至少输出 500 个中文字符，以便探针保持足够长时间。",
		r.runID,
	)
	plans := []callPlan{
		{App: r.apps[0], Fixture: r.fixtures[0], Prompt: prompt + " candidate=A"},
		{App: r.apps[1], Fixture: r.fixtures[0], Prompt: prompt + " candidate=B"},
	}
	group := r.runGroup(context.Background(), "two_replicas_compete_for_one_half_open_probe", plans)
	successes := 0
	rejections := 0
	for _, result := range group.Calls {
		if result.OK {
			successes++
		}
		if result.CircuitOpen {
			rejections++
		}
	}
	group.Passed = true
	group.Assertions = []string{
		"exactly one replica may call the recovered real gateway while the other is rejected",
	}
	if successes != 1 || rejections != 1 {
		group.Passed = false
		group.Assertions = append(
			group.Assertions,
			fmt.Sprintf("FAILED: successes=%d circuit_rejections=%d, want 1/1", successes, rejections),
		)
	}
	if !group.Pressure.Drained {
		group.Passed = false
		group.Assertions = append(group.Assertions, "FAILED: half-open admission leases did not drain")
	}
	report.Groups = append(report.Groups, group)
	if !group.Passed {
		return errors.New("half-open probe was not unique across replicas")
	}

	post := r.callVLM(context.Background(), callPlan{
		App:     r.apps[1],
		Fixture: r.fixtures[0],
		Prompt:  fmt.Sprintf("post-recovery %s，请用一句话描述图片。", r.runID),
	})
	postGroup := groupReport{
		Name:       "post_probe_circuit_closed",
		StartedAt:  time.Now().UTC(),
		Calls:      []callResult{post},
		Passed:     post.OK,
		Assertions: []string{"a successful half-open probe closes the shared circuit for the other replica"},
	}
	report.Groups = append(report.Groups, postGroup)
	if !post.OK {
		return errors.New("circuit did not close after the successful half-open probe")
	}
	if exists, err := r.redis.Exists(context.Background(), r.keys.Circuit, r.keys.Probe).Result(); err != nil {
		return err
	} else if exists != 0 {
		return fmt.Errorf("circuit/probe keys remain after recovery: %d", exists)
	}
	report.Assertions = append(report.Assertions,
		"one cross-process half-open probe reached the real gateway and cleared shared circuit state",
	)
	return r.cleanupPrefix(context.Background())
}

func (r *runner) runHold(report *runReport, count int) error {
	if count < 1 {
		return errors.New("hold-count must be positive")
	}
	if err := r.cleanupPrefix(context.Background()); err != nil {
		return err
	}
	plans := make([]callPlan, count)
	for i := range plans {
		plans[i] = callPlan{
			App:     r.apps[0],
			Fixture: r.fixtures[0],
			Prompt: fmt.Sprintf(
				"Hard-kill marker %s/%d。请极其详细地逐项解释图片，目标至少 4000 个中文字符。",
				r.runID,
				i+1,
			),
		}
	}
	group := r.runGroup(context.Background(), "hold_calls_for_external_hard_kill", plans)
	group.Assertions = append(group.Assertions,
		"this scenario expects the orchestration process to hard-kill app-a after both Redis leases appear",
	)
	report.Groups = append(report.Groups, group)
	// A hard kill intentionally makes the HTTP calls fail. The host-side
	// orchestration validates lease fencing and later recovery.
	return nil
}

func (r *runner) runSingle(report *runReport, expect string) error {
	result := r.callVLM(context.Background(), callPlan{
		App:     r.apps[1],
		Fixture: r.fixtures[0],
		Prompt:  fmt.Sprintf("single recovery check %s，请用一句话描述图片。", r.runID),
	})
	passed := expect == "either" ||
		(expect == "success" && result.OK) ||
		(expect == "failure" && !result.OK)
	group := groupReport{
		Name:       "single_request",
		StartedAt:  report.StartedAt,
		ElapsedMS:  result.ElapsedMS,
		Calls:      []callResult{result},
		Passed:     passed,
		Assertions: []string{"expected result: " + expect},
	}
	report.Groups = append(report.Groups, group)
	if !passed {
		return fmt.Errorf("single request expectation %q not met: ok=%v error=%s", expect, result.OK, result.Error)
	}
	return nil
}

func (r *runner) runCancellation(ctx context.Context) (groupReport, error) {
	started := time.Now().UTC()
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	resultCh := make(chan callResult, 1)
	plan := callPlan{
		App:     r.apps[0],
		Fixture: r.fixtures[0],
		Prompt: fmt.Sprintf(
			"Cancellation marker %s。请输出至少 3000 个中文字符，逐项详细解释全部组件和连线。",
			r.runID,
		),
	}
	go func() {
		resultCh <- r.callVLM(callCtx, plan)
	}()

	acquired := waitForZCardAtLeast(ctx, r.redis, r.keys.Tenant[plan.Fixture.Tenant.ID], 1, 10*time.Second)
	if !acquired {
		cancel()
		result := <-resultCh
		return groupReport{
			Name:      "client_cancel_releases_cross_instance_lease",
			StartedAt: started,
			Calls:     []callResult{result},
			Passed:    false,
		}, errors.New("cancellation call never acquired a distributed lease")
	}
	time.Sleep(750 * time.Millisecond)
	cancel()
	result := <-resultCh
	drainStarted := time.Now()
	drained := waitForAllZero(ctx, r.redis, r.keys, 6*time.Second)
	circuitExists, circuitErr := r.redis.Exists(ctx, r.keys.Circuit, r.keys.Probe).Result()
	group := groupReport{
		Name:      "client_cancel_releases_cross_instance_lease",
		StartedAt: started,
		ElapsedMS: time.Since(started).Milliseconds(),
		Calls:     []callResult{result},
		Pressure: pressureReport{
			Drained:       drained,
			DrainElapsedM: time.Since(drainStarted).Milliseconds(),
		},
		Passed: true,
		Assertions: []string{
			"request was cancelled after Redis admission was visibly acquired",
			"lease drains promptly and cancellation does not count as provider failure",
		},
	}
	if result.OK || result.ClientError == "" {
		group.Passed = false
		group.Assertions = append(group.Assertions, "FAILED: request completed instead of observing client cancellation")
	}
	if !drained {
		group.Passed = false
		group.Assertions = append(group.Assertions, "FAILED: admission lease did not drain after cancellation")
	}
	if circuitErr != nil {
		group.Passed = false
		group.Assertions = append(group.Assertions, "FAILED: "+circuitErr.Error())
	} else if circuitExists != 0 {
		group.Passed = false
		group.Assertions = append(group.Assertions, "FAILED: cancellation polluted circuit state")
	}
	if !group.Passed {
		return group, errors.New("client cancellation propagation assertion failed")
	}
	return group, nil
}

func (r *runner) runGroup(ctx context.Context, name string, plans []callPlan) groupReport {
	started := time.Now().UTC()
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	pressureCh := make(chan pressureReport, 1)
	go func() {
		pressureCh <- monitorPressure(monitorCtx, r.redis, r.keys)
	}()

	start := make(chan struct{})
	results := make([]callResult, len(plans))
	var wg sync.WaitGroup
	for i := range plans {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			results[index] = r.callVLM(ctx, plans[index])
		}(i)
	}
	close(start)
	wg.Wait()
	stopMonitor()
	pressure := <-pressureCh
	drainStarted := time.Now()
	pressure.Drained = waitForAllZero(context.Background(), r.redis, r.keys, 8*time.Second)
	pressure.DrainElapsedM = time.Since(drainStarted).Milliseconds()

	group := groupReport{
		Name:      name,
		StartedAt: started,
		ElapsedMS: time.Since(started).Milliseconds(),
		Pressure:  pressure,
		Calls:     results,
		Passed:    true,
	}
	for index, result := range results {
		if !result.OK {
			group.Passed = false
			group.Assertions = append(
				group.Assertions,
				fmt.Sprintf("FAILED: call %d via %s: %s%s", index+1, result.App, result.Error, result.ClientError),
			)
		}
	}
	if pressure.MaxTotal > 4 {
		group.Passed = false
		group.Assertions = append(group.Assertions, "FAILED: global Redis admission pressure exceeded 4")
	}
	if !pressure.Drained {
		group.Passed = false
		group.Assertions = append(group.Assertions, "FAILED: Redis admission did not drain after group")
	}
	if group.Passed {
		group.Assertions = append(group.Assertions, "all calls succeeded and all distributed leases drained")
	}
	return group
}

func (r *runner) callVLM(ctx context.Context, plan callPlan) callResult {
	started := time.Now()
	result := callResult{
		App:      plan.App.Name,
		TenantID: plan.Fixture.Tenant.ID,
		ModelID:  plan.Fixture.Model.ID,
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("input", plan.Prompt)
	part, err := writer.CreateFormFile("file", r.imageName)
	if err == nil {
		_, err = part.Write(r.image)
	}
	closeErr := writer.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		result.ClientError = err.Error()
		result.ElapsedMS = time.Since(started).Milliseconds()
		return result
	}
	url := strings.TrimRight(plan.App.BaseURL, "/") +
		"/api/v1/models/" + plan.Fixture.Model.ID + "/debug"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		result.ClientError = err.Error()
		result.ElapsedMS = time.Since(started).Milliseconds()
		return result
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-API-Key", plan.Fixture.Tenant.APIKey)
	response, err := r.httpClient.Do(request)
	if err != nil {
		result.ClientError = err.Error()
		result.ElapsedMS = time.Since(started).Milliseconds()
		return result
	}
	defer response.Body.Close()
	result.HTTPStatus = response.StatusCode
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	result.ElapsedMS = time.Since(started).Milliseconds()
	if readErr != nil {
		result.ClientError = readErr.Error()
		return result
	}
	var envelope debugEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		result.ClientError = fmt.Sprintf("decode response: %v; body=%s", err, truncate(string(payload), 500))
		return result
	}
	result.OK = response.StatusCode == http.StatusOK && envelope.Success && envelope.Data.OK
	result.ServerMS = envelope.Data.ElapsedMS
	result.Error = envelope.Data.Error
	lower := strings.ToLower(result.Error)
	result.CircuitOpen = strings.Contains(lower, "circuit open") ||
		strings.Contains(lower, "熔断")
	result.ProviderFail = strings.Contains(lower, "provider unavailable") ||
		strings.Contains(lower, "provider temporarily unavailable") ||
		strings.Contains(lower, "first_token_timeout") ||
		strings.Contains(lower, "stream_idle_timeout") ||
		strings.Contains(lower, "stream_truncated") ||
		strings.Contains(lower, "connection refused") ||
		strings.Contains(lower, "no route to host") ||
		strings.Contains(lower, "i/o timeout")
	if len(envelope.Data.RawResponse) > 0 && string(envelope.Data.RawResponse) != "null" {
		var answer string
		if json.Unmarshal(envelope.Data.RawResponse, &answer) == nil {
			result.AnswerRunes = utf8.RuneCountInString(answer)
		}
	}
	return result
}

func monitorPressure(ctx context.Context, client *redis.Client, keys redisKeys) pressureReport {
	report := pressureReport{MaxPerTenant: make(map[uint64]int64)}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	sample := func() {
		report.Samples++
		if count, err := client.ZCard(context.Background(), keys.Total).Result(); err == nil && count > report.MaxTotal {
			report.MaxTotal = count
		}
		for tenantID, key := range keys.Tenant {
			if count, err := client.ZCard(context.Background(), key).Result(); err == nil &&
				count > report.MaxPerTenant[tenantID] {
				report.MaxPerTenant[tenantID] = count
			}
		}
	}
	sample()
	for {
		select {
		case <-ctx.Done():
			sample()
			return report
		case <-ticker.C:
			sample()
		}
	}
}

func waitForAllZero(ctx context.Context, client *redis.Client, keys redisKeys, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		total, err := client.ZCard(ctx, keys.Total).Result()
		if err == nil && total == 0 {
			allZero := true
			for _, key := range keys.Tenant {
				count, tenantErr := client.ZCard(ctx, key).Result()
				if tenantErr != nil || count != 0 {
					allZero = false
					break
				}
			}
			if allZero {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func waitForZCardAtLeast(
	ctx context.Context,
	client *redis.Client,
	key string,
	want int64,
	timeout time.Duration,
) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		count, err := client.ZCard(ctx, key).Result()
		if err == nil && count >= want {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func (r *runner) cleanupPrefix(ctx context.Context) error {
	var cursor uint64
	pattern := r.keyPrefix + "*"
	for {
		keys, next, err := r.redis.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return fmt.Errorf("scan isolated Redis prefix: %w", err)
		}
		if len(keys) > 0 {
			if err := r.redis.Del(ctx, keys...).Err(); err != nil {
				return fmt.Errorf("delete isolated Redis keys: %w", err)
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

func health(client *http.Client, app appTarget) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		strings.TrimRight(app.BaseURL, "/")+"/health",
		nil,
	)
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("%s health: %w", app.Name, err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s health status %d: %s", app.Name, response.StatusCode, payload)
	}
	return strings.TrimSpace(string(payload)), nil
}

func writeReport(report runReport, output string) error {
	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(payload))
	if strings.TrimSpace(output) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	return os.WriteFile(output, append(payload, '\n'), 0o600)
}

func fatalReport(scenario string, started time.Time, output string, cause error) {
	report := runReport{
		RunID:      time.Now().UTC().Format("20060102T150405.000000000Z"),
		Scenario:   scenario,
		StartedAt:  started,
		FinishedAt: time.Now().UTC(),
		Passed:     false,
		Assertions: []string{"FAILED: " + cause.Error()},
	}
	_ = writeReport(report, output)
	os.Exit(1)
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envUint64(name string, fallback uint64) uint64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
}
