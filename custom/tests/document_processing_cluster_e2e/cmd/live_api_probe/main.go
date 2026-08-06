package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type knowledgeItem struct {
	ID               string `json:"id"`
	ParseStatus      string `json:"parse_status"`
	SummaryStatus    string `json:"summary_status"`
	EnrichmentStatus string `json:"enrichment_status"`
	WikiStatus       string `json:"wiki_status"`
}

type listResponse struct {
	Success  bool            `json:"success"`
	Data     []knowledgeItem `json:"data"`
	Total    int             `json:"total"`
	Page     int             `json:"page"`
	PageSize int             `json:"page_size"`
}

type statusResult struct {
	Status           string   `json:"status"`
	Total            int      `json:"total"`
	Returned         int      `json:"returned"`
	RawParseStatuses []string `json:"raw_parse_statuses"`
}

type modelProbeResponse struct {
	Success bool `json:"success"`
	Data    struct {
		OK           bool           `json:"ok"`
		ElapsedMS    int64          `json:"elapsed_ms"`
		Observations map[string]any `json:"observations"`
		RawResponse  struct {
			Text string `json:"text"`
		} `json:"raw_response"`
		Error string `json:"error"`
	} `json:"data"`
}

type modelProbeResult struct {
	ModelID      string         `json:"model_id"`
	OK           bool           `json:"ok"`
	ElapsedMS    int64          `json:"elapsed_ms"`
	Observations map[string]any `json:"observations,omitempty"`
	Text         string         `json:"text,omitempty"`
	Error        string         `json:"error,omitempty"`
}

type modelAPIItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
	Source      string `json:"source"`
	Description string `json:"description"`
	Parameters  struct {
		BaseURL     string            `json:"base_url"`
		Provider    string            `json:"provider"`
		ExtraConfig map[string]string `json:"extra_config"`
	} `json:"parameters"`
}

type modelListAPIResponse struct {
	Success bool           `json:"success"`
	Data    []modelAPIItem `json:"data"`
}

type modelItemAPIResponse struct {
	Success bool         `json:"success"`
	Data    modelAPIItem `json:"data"`
}

type knowledgeBaseAPIResponse struct {
	Success bool                `json:"success"`
	Data    types.KnowledgeBase `json:"data"`
}

type multiStringFlag []string

func (values *multiStringFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *multiStringFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("file path cannot be empty")
	}
	*values = append(*values, value)
	return nil
}

func requiredEnv(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func loadTenantAPIKey(ctx context.Context, tenantID uint64) (string, error) {
	host, err := requiredEnv("DB_HOST")
	if err != nil {
		return "", err
	}
	port, err := requiredEnv("DB_PORT")
	if err != nil {
		return "", err
	}
	user, err := requiredEnv("DB_USER")
	if err != nil {
		return "", err
	}
	password, err := requiredEnv("DB_PASSWORD")
	if err != nil {
		return "", err
	}
	name, err := requiredEnv("DB_NAME")
	if err != nil {
		return "", err
	}
	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		host, port, user, password, name,
	)
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		return "", fmt.Errorf("open database: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return "", fmt.Errorf("get sql database: %w", err)
	}
	defer sqlDB.Close()

	var tenant types.Tenant
	if err := db.WithContext(ctx).First(&tenant, tenantID).Error; err != nil {
		return "", fmt.Errorf("load tenant %d: %w", tenantID, err)
	}
	if strings.TrimSpace(tenant.APIKey) == "" || strings.HasPrefix(tenant.APIKey, "enc:") {
		return "", errors.New("tenant API key was not decrypted")
	}
	return tenant.APIKey, nil
}

func requestJSON(
	ctx context.Context,
	client *http.Client,
	method, endpoint, apiKey string,
	payload any,
	result any,
) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf(
			"%s %s returned HTTP %d: %s",
			method,
			endpoint,
			response.StatusCode,
			strings.TrimSpace(string(responseBody)),
		)
	}
	if result == nil || len(responseBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(responseBody, result); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, endpoint, err)
	}
	return nil
}

func ensureOmniASRModel(
	ctx context.Context,
	client *http.Client,
	baseURL, apiKey, upstreamAPIKey, upstreamBaseURL, modelName string,
) (modelAPIItem, error) {
	modelEndpoint := strings.TrimRight(baseURL, "/") + "/api/v1/models"
	var listed modelListAPIResponse
	if err := requestJSON(
		ctx, client, http.MethodGet, modelEndpoint, apiKey, nil, &listed,
	); err != nil {
		return modelAPIItem{}, err
	}
	if !listed.Success {
		return modelAPIItem{}, errors.New("model list returned success=false")
	}

	var current *modelAPIItem
	for index := range listed.Data {
		item := &listed.Data[index]
		if item.Type == string(types.ModelTypeASR) &&
			strings.TrimSpace(item.Name) == strings.TrimSpace(modelName) {
			current = item
			break
		}
	}
	parameters := map[string]any{
		"base_url": upstreamBaseURL,
		"provider": "generic",
		"extra_config": map[string]string{
			"asr_transport":  "openai_chat_audio",
			"asr_chat_path":  "/chat/completions",
			"asr_max_tokens": "4096",
		},
	}
	requestBody := map[string]any{
		"name":         modelName,
		"display_name": "Qwen2.5-Omni-7B（音频转写）",
		"type":         string(types.ModelTypeASR),
		"source":       string(types.ModelSourceRemote),
		"description":  "OpenAI 兼容多模态对话接口的音频转写适配",
		"parameters":   parameters,
	}
	var saved modelItemAPIResponse
	if current == nil {
		parameters["api_key"] = upstreamAPIKey
		if err := requestJSON(
			ctx, client, http.MethodPost, modelEndpoint, apiKey, requestBody, &saved,
		); err != nil {
			return modelAPIItem{}, err
		}
	} else {
		endpoint := modelEndpoint + "/" + url.PathEscape(current.ID)
		if err := requestJSON(
			ctx, client, http.MethodPut, endpoint, apiKey, requestBody, &saved,
		); err != nil {
			return modelAPIItem{}, err
		}
	}
	if !saved.Success || strings.TrimSpace(saved.Data.ID) == "" {
		return modelAPIItem{}, errors.New("model save returned no model identity")
	}

	credentialEndpoint := modelEndpoint + "/" + url.PathEscape(saved.Data.ID) + "/credentials"
	var credentialResponse map[string]any
	if err := requestJSON(
		ctx,
		client,
		http.MethodPut,
		credentialEndpoint,
		apiKey,
		map[string]string{"api_key": upstreamAPIKey},
		&credentialResponse,
	); err != nil {
		return modelAPIItem{}, err
	}
	return saved.Data, nil
}

func enableKnowledgeBaseASR(
	ctx context.Context,
	client *http.Client,
	baseURL, apiKey, knowledgeBaseID, modelID string,
) (*types.KnowledgeBase, error) {
	kbEndpoint := strings.TrimRight(baseURL, "/") +
		"/api/v1/knowledge-bases/" + url.PathEscape(knowledgeBaseID)
	var current knowledgeBaseAPIResponse
	if err := requestJSON(
		ctx, client, http.MethodGet, kbEndpoint, apiKey, nil, &current,
	); err != nil {
		return nil, err
	}
	if !current.Success || strings.TrimSpace(current.Data.ID) == "" {
		return nil, errors.New("knowledge base lookup returned no data")
	}
	kb := &current.Data
	provider := kb.GetStorageProvider()
	if strings.TrimSpace(provider) == "" {
		provider = "local"
	}
	nodeExtract := map[string]any{"enabled": false}
	if kb.ExtractConfig != nil {
		nodeExtract = map[string]any{
			"enabled":   kb.ExtractConfig.Enabled,
			"text":      kb.ExtractConfig.Text,
			"tags":      kb.ExtractConfig.Tags,
			"nodes":     kb.ExtractConfig.Nodes,
			"relations": kb.ExtractConfig.Relations,
		}
	}
	questionGeneration := map[string]any{"enabled": false, "questionCount": 3}
	if kb.QuestionGenerationConfig != nil {
		questionGeneration = map[string]any{
			"enabled":       kb.QuestionGenerationConfig.Enabled,
			"questionCount": kb.QuestionGenerationConfig.QuestionCount,
		}
	}
	update := map[string]any{
		"llmModelId":       kb.SummaryModelID,
		"embeddingModelId": kb.EmbeddingModelID,
		"vlm_config":       kb.VLMConfig,
		"asr_config": map[string]any{
			"enabled": true, "model_id": modelID, "language": "zh",
		},
		"documentSplitting": map[string]any{
			"chunkSize":         kb.ChunkingConfig.ChunkSize,
			"chunkOverlap":      kb.ChunkingConfig.ChunkOverlap,
			"separators":        kb.ChunkingConfig.Separators,
			"parserEngineRules": kb.ChunkingConfig.ParserEngineRules,
			"enableParentChild": kb.ChunkingConfig.EnableParentChild,
			"parentChunkSize":   kb.ChunkingConfig.ParentChunkSize,
			"childChunkSize":    kb.ChunkingConfig.ChildChunkSize,
			"strategy":          kb.ChunkingConfig.Strategy,
			"tokenLimit":        kb.ChunkingConfig.TokenLimit,
			"languages":         kb.ChunkingConfig.Languages,
		},
		"multimodal":         map[string]any{"enabled": kb.VLMConfig.Enabled},
		"storageProvider":    provider,
		"nodeExtract":        nodeExtract,
		"questionGeneration": questionGeneration,
	}
	configEndpoint := strings.TrimRight(baseURL, "/") +
		"/api/v1/initialization/config/" + url.PathEscape(knowledgeBaseID)
	var updateResponse map[string]any
	if err := requestJSON(
		ctx, client, http.MethodPut, configEndpoint, apiKey, update, &updateResponse,
	); err != nil {
		return nil, err
	}

	var verified knowledgeBaseAPIResponse
	if err := requestJSON(
		ctx, client, http.MethodGet, kbEndpoint, apiKey, nil, &verified,
	); err != nil {
		return nil, err
	}
	if !verified.Data.ASRConfig.Enabled || verified.Data.ASRConfig.ModelID != modelID {
		return nil, errors.New("knowledge base ASR configuration did not persist")
	}
	return &verified.Data, nil
}

func requestStatus(
	ctx context.Context,
	client *http.Client,
	baseURL, apiKey, knowledgeBaseID, status string,
) (statusResult, error) {
	query := url.Values{
		"page":            {"1"},
		"page_size":       {"100"},
		"workflow_status": {status},
	}
	endpoint := strings.TrimRight(baseURL, "/") +
		"/api/v1/knowledge-bases/" + url.PathEscape(knowledgeBaseID) +
		"/knowledge?" + query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return statusResult{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	response, err := client.Do(request)
	if err != nil {
		return statusResult{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return statusResult{}, err
	}
	if response.StatusCode != http.StatusOK {
		return statusResult{}, fmt.Errorf(
			"workflow_status=%s returned HTTP %d: %s",
			status, response.StatusCode, strings.TrimSpace(string(body)),
		)
	}
	var payload listResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return statusResult{}, fmt.Errorf("decode workflow_status=%s: %w", status, err)
	}
	if !payload.Success {
		return statusResult{}, fmt.Errorf("workflow_status=%s returned success=false", status)
	}
	if payload.Total <= 0 || len(payload.Data) == 0 {
		return statusResult{}, fmt.Errorf(
			"workflow_status=%s unexpectedly returned no real documents",
			status,
		)
	}

	rawSet := make(map[string]struct{})
	for _, item := range payload.Data {
		if got := classify(item); got != status {
			return statusResult{}, fmt.Errorf(
				"workflow_status=%s returned item %s classified as %s "+
					"(parse=%s summary=%s enrichment=%s wiki=%s)",
				status, item.ID, got, item.ParseStatus, item.SummaryStatus,
				item.EnrichmentStatus, item.WikiStatus,
			)
		}
		rawSet[item.ParseStatus] = struct{}{}
	}
	raw := make([]string, 0, len(rawSet))
	for value := range rawSet {
		raw = append(raw, value)
	}
	return statusResult{
		Status:           status,
		Total:            payload.Total,
		Returned:         len(payload.Data),
		RawParseStatuses: raw,
	}, nil
}

func classify(item knowledgeItem) string {
	parse := strings.ToLower(strings.TrimSpace(item.ParseStatus))
	switch parse {
	case "pending":
		return "pending"
	case "processing", "finalizing":
		return "processing"
	case "failed":
		return "failed"
	case "cancelled", "draft":
		return parse
	case "completed":
		statuses := []string{
			strings.ToLower(strings.TrimSpace(item.SummaryStatus)),
			strings.ToLower(strings.TrimSpace(item.EnrichmentStatus)),
			strings.ToLower(strings.TrimSpace(item.WikiStatus)),
		}
		for _, status := range statuses {
			if status == "pending" || status == "processing" {
				return "processing"
			}
		}
		for _, status := range statuses {
			if status == "failed" || status == "degraded" {
				return "failed"
			}
		}
		for _, status := range statuses {
			switch status {
			case "", "none", "completed", "done", "skipped":
			default:
				return "unknown"
			}
		}
		return "completed"
	default:
		return "unknown"
	}
}

func probeModel(
	ctx context.Context,
	client *http.Client,
	baseURL, apiKey, modelID, input, filePath string,
) (modelProbeResult, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return modelProbeResult{}, fmt.Errorf("open %s probe file: %w", modelID, err)
	}
	defer file.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fields := map[string]string{
		"input":     input,
		"documents": "[]",
		"options":   `{"max_tokens":64,"temperature":0}`,
	}
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			return modelProbeResult{}, err
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set(
		"Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename="%s"`, filepath.Base(filePath)),
	)
	contentType := mime.TypeByExtension(filepath.Ext(filePath))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return modelProbeResult{}, err
	}
	if _, err := io.Copy(part, file); err != nil {
		return modelProbeResult{}, err
	}
	if err := writer.Close(); err != nil {
		return modelProbeResult{}, err
	}

	endpoint := strings.TrimRight(baseURL, "/") +
		"/api/v1/models/" + url.PathEscape(modelID) + "/debug"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &body)
	if err != nil {
		return modelProbeResult{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-API-Key", apiKey)
	response, err := client.Do(request)
	if err != nil {
		return modelProbeResult{}, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return modelProbeResult{}, err
	}
	if response.StatusCode != http.StatusOK {
		return modelProbeResult{}, fmt.Errorf(
			"%s probe returned HTTP %d: %s",
			modelID, response.StatusCode, strings.TrimSpace(string(responseBody)),
		)
	}
	var payload modelProbeResponse
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return modelProbeResult{}, fmt.Errorf("decode %s probe: %w", modelID, err)
	}
	result := modelProbeResult{
		ModelID:      modelID,
		OK:           payload.Success && payload.Data.OK,
		ElapsedMS:    payload.Data.ElapsedMS,
		Observations: payload.Data.Observations,
		Text:         strings.TrimSpace(payload.Data.RawResponse.Text),
		Error:        payload.Data.Error,
	}
	if !result.OK {
		return result, fmt.Errorf("%s probe failed: %s", modelID, result.Error)
	}
	return result, nil
}

func main() {
	var (
		baseURL          = flag.String("base-url", "http://127.0.0.1:8080", "WeKnora API base URL")
		tenant           = flag.Uint64("tenant-id", 10000, "tenant whose API key is used")
		kbID             = flag.String("knowledge-base-id", "", "knowledge base with real documents")
		timeout          = flag.Duration("timeout", 30*time.Second, "per-request timeout")
		probeModels      = flag.Bool("probe-models", false, "probe exact Qwen VLM and ASR through the backend")
		configureOmniASR = flag.Bool(
			"configure-omni-asr",
			false,
			"create/update a chat-audio ASR model, debug it, and enable it on the KB",
		)
		omniBaseURL = flag.String(
			"omni-base-url",
			"http://10.0.11.37:30002/v1",
			"OpenAI-compatible Qwen Omni API base URL",
		)
		omniModelName = flag.String(
			"omni-model-name",
			"/models/Qwen2.5-Omni-7B",
			"upstream Qwen Omni model name",
		)
		expectedASRMarker = flag.String(
			"expected-asr-marker",
			"WKNASR7319",
			"marker that must be present in the real transcription",
		)
		supportedFormatsPhase = flag.String(
			"supported-formats-phase",
			"",
			"run nonaudio/audio/status through the resumable Python harness using the decrypted tenant key",
		)
		productionXLSXLocalE2E = flag.Bool(
			"production-xlsx-local-e2e",
			false,
			"upload real XLSX files through the local API and audit split/content completeness",
		)
		productionXLSXFiles  multiStringFlag
		productionXLSXReport = flag.String(
			"production-xlsx-report",
			".tmp/production-xlsx-local-e2e-report.json",
			"sanitized JSON report written by the real XLSX local E2E",
		)
		supportedFormatsFixtureDir = flag.String(
			"supported-formats-fixture-dir",
			"/workspace/.local-data/e2e-fixtures/all-supported-formats-20260725-v1",
			"supported-format fixture directory",
		)
		supportedFormatsState = flag.String(
			"supported-formats-state",
			"/workspace/.local-data/e2e-runtime/supported-formats-20260725-v1.json",
			"resumable supported-format state file",
		)
		reparseIDs = flag.String(
			"reparse-ids",
			"",
			"comma-separated knowledge IDs to submit through the durable batch-reparse API",
		)
		deletionLifecycle = flag.Bool(
			"deletion-lifecycle",
			false,
			"run public-API deletion tests at pending, processing, finalizing, and completed boundaries",
		)
		deletionLifecycleScenarios = flag.String(
			"deletion-lifecycle-scenarios",
			"pending,processing,finalizing,completed",
			"comma-separated lifecycle boundaries for -deletion-lifecycle",
		)
		deletionLifecycleOutputDir = flag.String(
			"deletion-lifecycle-output-dir",
			"custom/tests/document_processing_cluster_e2e/deletion_lifecycle_outputs",
			"deletion-lifecycle report directory",
		)
		deletionLifecycleSizeKiB = flag.Int(
			"deletion-lifecycle-size-kib",
			384,
			"generated Markdown size for non-pending deletion lifecycle scenarios",
		)
		durableFailoverScenario = flag.String(
			"durable-failover-scenario",
			"",
			"run one live durability scenario with the decrypted tenant key",
		)
		durableFailoverWorkers = flag.String(
			"durable-failover-workers",
			"weknora-app-dev=WeKnora-app-dev,worker-cluster-e2e-a=WeKnora-worker-cluster-e2e-a,worker-cluster-e2e-b=WeKnora-worker-cluster-e2e-b,worker-cluster-e2e-e=WeKnora-worker-cluster-e2e-e,worker-cluster-e2e-f=WeKnora-worker-cluster-e2e-f",
			"comma-separated INSTANCE_ID=CONTAINER mappings for live durability scenarios",
		)
		durableFailoverFaultInstance = flag.String(
			"durable-failover-fault-instance",
			"",
			"stable instance ID to fault in a live durability scenario",
		)
		durableFailoverDocuments = flag.Int(
			"durable-failover-documents",
			8,
			"small live document batch size used by a durability scenario",
		)
		durableFailoverSizeKiB = flag.Int(
			"durable-failover-size-kib",
			64,
			"generated document size used by a durability scenario",
		)
		durableFailoverOutputDir = flag.String(
			"durable-failover-output-dir",
			"custom/tests/document_processing_cluster_e2e/durable_failover_outputs",
			"durability report directory",
		)
		durableSuiteTimeout = flag.Duration(
			"durable-suite-timeout",
			90*time.Minute,
			"maximum wall-clock duration for one live durability scenario",
		)
		pythonBinary = flag.String(
			"python-binary",
			"python3",
			"Python executable used for child E2E harnesses",
		)
		vlmImage = flag.String(
			"vlm-image",
			"custom/chrome-extension/offline-package-src/icons/icon128.png",
			"small image used by the VLM probe",
		)
		asrAudio = flag.String(
			"asr-audio",
			"internal/assets/asr_test.wav",
			"small audio file used by the ASR probe",
		)
	)
	flag.Var(
		&productionXLSXFiles,
		"production-xlsx-file",
		"XLSX path to upload; repeat for each file",
	)
	flag.Parse()
	if strings.TrimSpace(*kbID) == "" {
		fmt.Fprintln(os.Stderr, "-knowledge-base-id is required")
		os.Exit(2)
	}
	statuses := []string{"pending", "processing", "completed", "failed"}
	requestCount := len(statuses) + 1
	if *probeModels {
		requestCount += 2
	}
	if *configureOmniASR {
		requestCount = 10
	}
	if strings.TrimSpace(*supportedFormatsPhase) != "" {
		requestCount = 20
	}
	if *productionXLSXLocalE2E {
		requestCount = 100
	}
	if strings.TrimSpace(*reparseIDs) != "" {
		requestCount = 2
	}
	if *deletionLifecycle {
		requestCount = 200
	}
	contextTimeout := (*timeout) * time.Duration(requestCount)
	if strings.TrimSpace(*durableFailoverScenario) != "" {
		contextTimeout = *durableSuiteTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), contextTimeout)
	defer cancel()
	apiKey, err := loadTenantAPIKey(ctx, *tenant)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	client := &http.Client{Timeout: *timeout}
	if *productionXLSXLocalE2E {
		if len(productionXLSXFiles) == 0 {
			fmt.Fprintln(os.Stderr, "at least one -production-xlsx-file is required")
			os.Exit(2)
		}
		childArgs := []string{
			"custom/tests/document_processing_cluster_e2e/run_production_xlsx_local_e2e.py",
			"--base-url", *baseURL,
			"--kb-id", *kbID,
			"--report", *productionXLSXReport,
		}
		for _, file := range productionXLSXFiles {
			childArgs = append(childArgs, "--file", file)
		}
		command := exec.CommandContext(ctx, *pythonBinary, childArgs...)
		command.Env = append(
			os.Environ(),
			"WEKNORA_E2E_TENANT_API_KEY="+apiKey,
		)
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *deletionLifecycle {
		command := exec.CommandContext(
			ctx,
			*pythonBinary,
			"custom/tests/document_processing_cluster_e2e/run_deletion_lifecycle.py",
			"--base-url",
			*baseURL,
			"--kb-id",
			*kbID,
			"--scenarios",
			*deletionLifecycleScenarios,
			"--size-kib",
			strconv.Itoa(*deletionLifecycleSizeKiB),
			"--output-dir",
			*deletionLifecycleOutputDir,
		)
		command.Env = append(
			os.Environ(),
			"WEKNORA_E2E_TOKEN="+apiKey,
			"WEKNORA_E2E_KB_ID="+*kbID,
			"WEKNORA_E2E_HOST="+*baseURL,
		)
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if scenario := strings.TrimSpace(*durableFailoverScenario); scenario != "" {
		validScenarios := map[string]bool{
			"stable-reboot":           true,
			"cross-instance-takeover": true,
			"paused-old-owner":        true,
			"redis-restart":           true,
			"fleet-restart":           true,
			"api-restart":             true,
		}
		if !validScenarios[scenario] {
			fmt.Fprintln(os.Stderr, "-durable-failover-scenario is not supported")
			os.Exit(2)
		}
		if *durableFailoverDocuments <= 0 || *durableFailoverSizeKiB <= 0 {
			fmt.Fprintln(os.Stderr, "durability document count and size must be positive")
			os.Exit(2)
		}
		workers := make([]string, 0)
		for _, rawWorker := range strings.Split(*durableFailoverWorkers, ",") {
			worker := strings.TrimSpace(rawWorker)
			if worker == "" {
				continue
			}
			parts := strings.SplitN(worker, "=", 2)
			if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" ||
				strings.TrimSpace(parts[1]) == "" {
				fmt.Fprintln(os.Stderr, "invalid -durable-failover-workers mapping")
				os.Exit(2)
			}
			workers = append(workers, worker)
		}
		if len(workers) < 2 {
			fmt.Fprintln(os.Stderr, "at least two durability worker mappings are required")
			os.Exit(2)
		}
		childArgs := []string{
			"custom/tests/document_processing_cluster_e2e/run_durable_failover.py",
			"--skip-contracts",
			"--allow-chaos",
			"--scenario", scenario,
			"--base-url", *baseURL,
			"--kb-id", *kbID,
			"--documents", strconv.Itoa(*durableFailoverDocuments),
			"--upload-concurrency", "2",
			"--generated-size-kib", strconv.Itoa(*durableFailoverSizeKiB),
			"--poll-interval", "1",
			"--activity-timeout", "300",
			"--takeover-timeout", "300",
			"--completion-timeout", "3600",
			"--fault-down-seconds", "10",
			"--pause-seconds", "90",
			"--output-dir", *durableFailoverOutputDir,
		}
		for _, worker := range workers {
			childArgs = append(childArgs, "--worker", worker)
		}
		if faultInstance := strings.TrimSpace(*durableFailoverFaultInstance); faultInstance != "" {
			childArgs = append(childArgs, "--fault-instance", faultInstance)
		}
		switch scenario {
		case "redis-restart":
			childArgs = append(
				childArgs,
				"--allow-infrastructure-chaos",
				"--redis-container", "WeKnora-redis-dev",
			)
		case "fleet-restart":
			childArgs = append(childArgs, "--allow-full-worker-outage")
		}
		command := exec.CommandContext(ctx, *pythonBinary, childArgs...)
		command.Env = append(
			os.Environ(),
			"WEKNORA_E2E_TOKEN="+apiKey,
			"WEKNORA_E2E_KB_ID="+*kbID,
			"WEKNORA_E2E_HOST="+*baseURL,
		)
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if rawIDs := strings.TrimSpace(*reparseIDs); rawIDs != "" {
		seen := make(map[string]struct{})
		ids := make([]string, 0)
		for _, rawID := range strings.Split(rawIDs, ",") {
			id := strings.TrimSpace(rawID)
			if id == "" {
				continue
			}
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			fmt.Fprintln(os.Stderr, "-reparse-ids did not contain a knowledge ID")
			os.Exit(2)
		}
		var response map[string]any
		if err := requestJSON(
			ctx,
			client,
			http.MethodPost,
			strings.TrimRight(*baseURL, "/")+"/api/v1/knowledge/batch-reparse",
			apiKey,
			map[string]any{"kb_id": *kbID, "ids": ids},
			&response,
		); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		output := map[string]any{
			"tenant_id":         strconv.FormatUint(*tenant, 10),
			"knowledge_base_id": *kbID,
			"knowledge_ids":     ids,
			"response":          response,
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(output); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if phase := strings.TrimSpace(*supportedFormatsPhase); phase != "" {
		switch phase {
		case "nonaudio", "audio", "status":
		default:
			fmt.Fprintln(os.Stderr, "-supported-formats-phase must be nonaudio, audio, or status")
			os.Exit(2)
		}
		command := exec.CommandContext(
			ctx,
			*pythonBinary,
			"custom/tests/document_processing_cluster_e2e/run_supported_formats_e2e.py",
			phase,
			"--base-url",
			*baseURL,
			"--fixture-dir",
			*supportedFormatsFixtureDir,
			"--state",
			*supportedFormatsState,
			"--upload-concurrency",
			"2",
		)
		command.Env = append(
			os.Environ(),
			"WEKNORA_E2E_TENANT_API_KEY="+apiKey,
			"WEKNORA_E2E_HOST="+*baseURL,
		)
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *configureOmniASR {
		upstreamAPIKey, err := requiredEnv("QWEN_API_KEY")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		model, err := ensureOmniASRModel(
			ctx,
			client,
			*baseURL,
			apiKey,
			upstreamAPIKey,
			*omniBaseURL,
			*omniModelName,
		)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		probe, err := probeModel(
			ctx, client, *baseURL, apiKey, model.ID, "", *asrAudio,
		)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if marker := strings.TrimSpace(*expectedASRMarker); marker != "" &&
			!strings.Contains(strings.ToUpper(probe.Text), strings.ToUpper(marker)) {
			fmt.Fprintf(
				os.Stderr,
				"ASR probe transcription does not contain marker %q\n",
				marker,
			)
			os.Exit(1)
		}
		kb, err := enableKnowledgeBaseASR(
			ctx, client, *baseURL, apiKey, *kbID, model.ID,
		)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		payload := map[string]any{
			"tenant_id":         strconv.FormatUint(*tenant, 10),
			"knowledge_base_id": kb.ID,
			"model": map[string]any{
				"id": model.ID, "name": model.Name, "type": model.Type,
				"transport": model.Parameters.ExtraConfig["asr_transport"],
			},
			"probe": probe,
			"knowledge_base_asr": map[string]any{
				"enabled":  kb.ASRConfig.Enabled,
				"model_id": kb.ASRConfig.ModelID,
				"language": kb.ASRConfig.Language,
			},
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(payload); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	results := make([]statusResult, 0, 4)
	for _, status := range statuses {
		result, err := requestStatus(ctx, client, *baseURL, apiKey, *kbID, status)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		results = append(results, result)
	}
	payload := map[string]any{
		"tenant_id":         strconv.FormatUint(*tenant, 10),
		"knowledge_base_id": *kbID,
		"results":           results,
	}
	if *probeModels {
		models := []struct {
			id, input, file string
		}{
			{"local-qwen3-vl-32b-instruct", "请简洁描述图片中的主要内容。", *vlmImage},
			{"local-qwen3-asr-1-7b", "", *asrAudio},
		}
		probes := make([]modelProbeResult, 0, len(models))
		for _, model := range models {
			result, err := probeModel(
				ctx, client, *baseURL, apiKey, model.id, model.input, model.file,
			)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			probes = append(probes, result)
		}
		payload["model_probes"] = probes
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(payload); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
