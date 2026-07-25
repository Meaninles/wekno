package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type recallCase struct {
	Name                     string     `json:"name"`
	Query                    string     `json:"query"`
	ExpectedSourceSubstrings []string   `json:"expected_source_substrings"`
	RequiredEvidenceGroups   [][]string `json:"required_evidence_groups"`
	RequiredAnswerGroups     [][]string `json:"required_answer_groups"`
}

type sanitizedReference struct {
	KnowledgeID    string  `json:"knowledge_id"`
	KnowledgeTitle string  `json:"knowledge_title"`
	ChunkIndex     int     `json:"chunk_index"`
	Score          float64 `json:"score"`
	MatchType      string  `json:"match_type"`
	Excerpt        string  `json:"excerpt"`
}

type caseResult struct {
	Name                  string               `json:"name"`
	Query                 string               `json:"query"`
	SessionID             string               `json:"session_id"`
	ElapsedMS             int64                `json:"elapsed_ms"`
	HTTPStatus            int                  `json:"http_status"`
	StreamCompleted       bool                 `json:"stream_completed"`
	AnswerDone            bool                 `json:"answer_done"`
	Answer                string               `json:"answer"`
	HybridSearchElapsedMS int64                `json:"hybrid_search_elapsed_ms"`
	HybridReferences      []sanitizedReference `json:"hybrid_references"`
	HybridMatchedSources  map[string]bool      `json:"hybrid_matched_source_checks"`
	HybridMatchedEvidence []bool               `json:"hybrid_matched_evidence_groups"`
	HybridRecallOK        bool                 `json:"hybrid_recall_ok"`
	HybridDiagnosticNotes []string             `json:"hybrid_diagnostic_notes,omitempty"`
	References            []sanitizedReference `json:"references"`
	MatchedSourceChecks   map[string]bool      `json:"matched_source_checks"`
	MatchedEvidenceGroups []bool               `json:"matched_evidence_groups"`
	MatchedAnswerGroups   []bool               `json:"matched_answer_groups"`
	AnswerDiagnosticOK    bool                 `json:"answer_diagnostic_ok"`
	AnswerDiagnosticNotes []string             `json:"answer_diagnostic_notes,omitempty"`
	AssistantMessageID    string               `json:"assistant_message_id,omitempty"`
	FinishReason          string               `json:"finish_reason,omitempty"`
	PromptTokens          int                  `json:"prompt_tokens,omitempty"`
	CompletionTokens      int                  `json:"completion_tokens,omitempty"`
	TotalTokens           int                  `json:"total_tokens,omitempty"`
	SessionCleanupOK      bool                 `json:"session_cleanup_ok"`
	Passed                bool                 `json:"recall_passed"`
	FailureReasons        []string             `json:"recall_failure_reasons,omitempty"`
	ObservedResponseTypes []string             `json:"observed_response_types"`
}

type report struct {
	GeneratedAt          time.Time    `json:"generated_at"`
	BaseURL              string       `json:"base_url"`
	TenantID             uint64       `json:"tenant_id"`
	KnowledgeBaseID      string       `json:"knowledge_base_id"`
	KnowledgeBaseName    string       `json:"knowledge_base_name"`
	ConfiguredModelID    string       `json:"configured_summary_model_id"`
	RequestedModelID     string       `json:"requested_model_id"`
	ModelName            string       `json:"model_name"`
	ExpectedModelName    string       `json:"expected_model_name"`
	ModelProvider        string       `json:"model_provider"`
	ModelEndpointHost    string       `json:"model_endpoint_host"`
	ModelConfigMatched   bool         `json:"model_config_matched"`
	KnowledgeBaseMatched bool         `json:"knowledge_base_config_matched"`
	Cases                []caseResult `json:"cases"`
	Passed               bool         `json:"recall_passed"`
}

type apiEnvelope[T any] struct {
	Success bool `json:"success"`
	Data    T    `json:"data"`
}

type sessionData struct {
	ID string `json:"id"`
}

type modelData struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Parameters struct {
		BaseURL  string `json:"base_url"`
		Provider string `json:"provider"`
	} `json:"parameters"`
}

type knowledgeBaseData struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	SummaryModelID string `json:"summary_model_id"`
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

func requestJSON[T any](
	ctx context.Context,
	client *http.Client,
	method, endpoint, apiKey string,
	payload any,
) (T, error) {
	var zero T
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return zero, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return zero, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return zero, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return zero, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return zero, fmt.Errorf(
			"%s %s returned %d: %s",
			method,
			endpoint,
			response.StatusCode,
			strings.TrimSpace(string(responseBody)),
		)
	}
	var result T
	if len(bytes.TrimSpace(responseBody)) == 0 {
		return result, nil
	}
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return zero, fmt.Errorf("decode %s %s: %w", method, endpoint, err)
	}
	return result, nil
}

func modelEndpointHost(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	return parsed.Host
}

func truncateRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:limit])) + "…"
}

func matchTypeLabel(matchType types.MatchType) string {
	switch matchType {
	case types.MatchTypeEmbedding:
		return "embedding"
	case types.MatchTypeKeywords:
		return "keywords"
	case types.MatchTypeNearByChunk:
		return "nearby_chunk"
	case types.MatchTypeHistory:
		return "history"
	case types.MatchTypeParentChunk:
		return "parent_chunk"
	case types.MatchTypeRelationChunk:
		return "relation_chunk"
	case types.MatchTypeGraph:
		return "graph"
	case types.MatchTypeWebSearch:
		return "web_search"
	case types.MatchTypeDirectLoad:
		return "direct_load"
	case types.MatchTypeDataAnalysis:
		return "data_analysis"
	default:
		return fmt.Sprintf("unknown_%d", matchType)
	}
}

func normalizeAnswerChunk(answer, chunk string) string {
	if chunk == "" {
		return answer
	}
	// Some providers emit a final cumulative answer. Do not duplicate it after
	// already receiving incremental deltas.
	if answer != "" && strings.HasPrefix(chunk, answer) {
		return chunk
	}
	if strings.HasSuffix(answer, chunk) {
		return answer
	}
	return answer + chunk
}

func uniqueReferences(references types.References) []sanitizedReference {
	seen := make(map[string]struct{})
	result := make([]sanitizedReference, 0, len(references))
	for _, reference := range references {
		if reference == nil {
			continue
		}
		key := reference.ID
		if key == "" {
			key = fmt.Sprintf(
				"%s:%d:%s",
				reference.KnowledgeID,
				reference.ChunkIndex,
				reference.KnowledgeTitle,
			)
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, sanitizedReference{
			KnowledgeID:    reference.KnowledgeID,
			KnowledgeTitle: reference.KnowledgeTitle,
			ChunkIndex:     reference.ChunkIndex,
			Score:          reference.Score,
			MatchType:      matchTypeLabel(reference.MatchType),
			Excerpt:        truncateRunes(reference.Content, 280),
		})
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Score == result[j].Score {
			return result[i].KnowledgeTitle < result[j].KnowledgeTitle
		}
		return result[i].Score > result[j].Score
	})
	return result
}

func referenceEvidence(references types.References) string {
	var evidence strings.Builder
	for _, reference := range references {
		if reference == nil {
			continue
		}
		evidence.WriteString(reference.KnowledgeTitle)
		evidence.WriteByte('\n')
		evidence.WriteString(reference.KnowledgeFilename)
		evidence.WriteByte('\n')
		evidence.WriteString(reference.Content)
		evidence.WriteByte('\n')
		evidence.WriteString(reference.MatchedContent)
		evidence.WriteByte('\n')
	}
	return strings.ToLower(evidence.String())
}

func evaluateReferences(
	testCase recallCase,
	references types.References,
) (map[string]bool, []bool, []string) {
	sourceChecks := make(
		map[string]bool,
		len(testCase.ExpectedSourceSubstrings),
	)
	var notes []string
	for _, expected := range testCase.ExpectedSourceSubstrings {
		matched := false
		for _, reference := range references {
			if reference == nil {
				continue
			}
			if strings.Contains(
				strings.ToLower(reference.KnowledgeTitle),
				strings.ToLower(expected),
			) {
				matched = true
				break
			}
		}
		sourceChecks[expected] = matched
		if !matched {
			notes = append(notes, "missing expected source: "+expected)
		}
	}

	evidence := referenceEvidence(references)
	evidenceChecks := make([]bool, 0, len(testCase.RequiredEvidenceGroups))
	for index, alternatives := range testCase.RequiredEvidenceGroups {
		matched := false
		for _, alternative := range alternatives {
			if strings.Contains(evidence, strings.ToLower(alternative)) {
				matched = true
				break
			}
		}
		evidenceChecks = append(evidenceChecks, matched)
		if !matched {
			notes = append(
				notes,
				fmt.Sprintf("retrieved evidence group %d was not matched", index+1),
			)
		}
	}
	return sourceChecks, evidenceChecks, notes
}

func validateCase(
	testCase recallCase,
	rawReferences types.References,
	result *caseResult,
) {
	var recallNotes []string
	result.MatchedSourceChecks, result.MatchedEvidenceGroups, recallNotes =
		evaluateReferences(testCase, rawReferences)
	result.FailureReasons = append(result.FailureReasons, recallNotes...)

	lowerAnswer := strings.ToLower(result.Answer)
	result.MatchedAnswerGroups = make([]bool, 0, len(testCase.RequiredAnswerGroups))
	for index, alternatives := range testCase.RequiredAnswerGroups {
		matched := false
		for _, alternative := range alternatives {
			if strings.Contains(lowerAnswer, strings.ToLower(alternative)) {
				matched = true
				break
			}
		}
		result.MatchedAnswerGroups = append(result.MatchedAnswerGroups, matched)
		if !matched {
			result.AnswerDiagnosticNotes = append(
				result.AnswerDiagnosticNotes,
				fmt.Sprintf("answer fact group %d was not matched", index+1),
			)
		}
	}
	result.AnswerDiagnosticOK = len(result.AnswerDiagnosticNotes) == 0
	if !result.StreamCompleted {
		result.FailureReasons = append(result.FailureReasons, "stream did not emit complete")
	}
	if !result.AnswerDone {
		result.FailureReasons = append(result.FailureReasons, "answer did not emit done")
	}
	if strings.TrimSpace(result.Answer) == "" {
		result.FailureReasons = append(result.FailureReasons, "answer is empty")
	}
	if len(rawReferences) == 0 {
		result.FailureReasons = append(result.FailureReasons, "knowledge references are empty")
	}
	result.Passed = len(result.FailureReasons) == 0
}

func runRecallCase(
	parentCtx context.Context,
	client *http.Client,
	baseURL, apiKey, kbID, modelID string,
	testCase recallCase,
	timeout time.Duration,
	keepSession bool,
) (result caseResult) {
	result = caseResult{
		Name:                testCase.Name,
		Query:               testCase.Query,
		HybridReferences:    make([]sanitizedReference, 0),
		References:          make([]sanitizedReference, 0),
		FailureReasons:      make([]string, 0),
		SessionCleanupOK:    keepSession,
		MatchedSourceChecks: make(map[string]bool),
	}
	caseCtx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	hybridStartedAt := time.Now()
	hybridResponse, hybridErr := requestJSON[apiEnvelope[[]*types.SearchResult]](
		caseCtx,
		client,
		http.MethodPost,
		baseURL+"/knowledge-bases/"+kbID+"/hybrid-search",
		apiKey,
		types.SearchParams{
			QueryText:        testCase.Query,
			VectorThreshold:  0.2,
			KeywordThreshold: 0.3,
			MatchCount:       30,
		},
	)
	result.HybridSearchElapsedMS = time.Since(hybridStartedAt).Milliseconds()
	if hybridErr != nil {
		result.HybridDiagnosticNotes = append(
			result.HybridDiagnosticNotes,
			"hybrid search request: "+hybridErr.Error(),
		)
	} else if !hybridResponse.Success {
		result.HybridDiagnosticNotes = append(
			result.HybridDiagnosticNotes,
			"hybrid search response success=false",
		)
	} else {
		hybridReferences := types.References(hybridResponse.Data)
		result.HybridReferences = uniqueReferences(hybridReferences)
		result.HybridMatchedSources,
			result.HybridMatchedEvidence,
			result.HybridDiagnosticNotes = evaluateReferences(
			testCase,
			hybridReferences,
		)
	}
	result.HybridRecallOK = len(result.HybridDiagnosticNotes) == 0

	sessionResponse, err := requestJSON[apiEnvelope[sessionData]](
		caseCtx,
		client,
		http.MethodPost,
		baseURL+"/sessions",
		apiKey,
		map[string]any{
			"title":       "E2E recall " + testCase.Name,
			"description": "Production recall verification; safe to delete.",
		},
	)
	if err != nil {
		result.FailureReasons = append(result.FailureReasons, "create session: "+err.Error())
		return result
	}
	if !sessionResponse.Success || strings.TrimSpace(sessionResponse.Data.ID) == "" {
		result.FailureReasons = append(result.FailureReasons, "create session returned no ID")
		return result
	}
	result.SessionID = sessionResponse.Data.ID

	if !keepSession {
		defer func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(parentCtx, 20*time.Second)
			defer cleanupCancel()
			_, cleanupErr := requestJSON[map[string]any](
				cleanupCtx,
				client,
				http.MethodDelete,
				baseURL+"/sessions/"+result.SessionID,
				apiKey,
				nil,
			)
			result.SessionCleanupOK = cleanupErr == nil
			if cleanupErr != nil {
				result.FailureReasons = append(
					result.FailureReasons,
					"cleanup temporary session: "+cleanupErr.Error(),
				)
				result.Passed = false
			}
		}()
	}

	enableMemory := false
	payload := map[string]any{
		"query":                    testCase.Query,
		"knowledge_base_ids":       []string{kbID},
		"knowledge_ids":            []string{},
		"agent_enabled":            false,
		"web_search_enabled":       false,
		"summary_model_id":         modelID,
		"disable_title":            true,
		"enable_memory":            enableMemory,
		"channel":                  "api",
		"mcp_service_ids":          []string{},
		"skill_names":              []string{},
		"professional_skill_names": []string{},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		result.FailureReasons = append(result.FailureReasons, "encode chat payload: "+err.Error())
		return result
	}
	request, err := http.NewRequestWithContext(
		caseCtx,
		http.MethodPost,
		baseURL+"/knowledge-chat/"+result.SessionID,
		bytes.NewReader(encoded),
	)
	if err != nil {
		result.FailureReasons = append(result.FailureReasons, "create chat request: "+err.Error())
		return result
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", apiKey)

	startedAt := time.Now()
	response, err := client.Do(request)
	if err != nil {
		result.FailureReasons = append(result.FailureReasons, "chat request: "+err.Error())
		return result
	}
	defer response.Body.Close()
	result.HTTPStatus = response.StatusCode
	if response.StatusCode != http.StatusOK {
		responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if readErr != nil {
			result.FailureReasons = append(result.FailureReasons, "read error response: "+readErr.Error())
		} else {
			result.FailureReasons = append(
				result.FailureReasons,
				fmt.Sprintf(
					"chat returned %d: %s",
					response.StatusCode,
					truncateRunes(string(responseBody), 800),
				),
			)
		}
		return result
	}

	allReferences := make(types.References, 0)
	responseTypes := make(map[string]struct{})
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		rawData := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if rawData == "" || rawData == "[DONE]" {
			continue
		}
		var event types.StreamResponse
		if err := json.Unmarshal([]byte(rawData), &event); err != nil {
			result.FailureReasons = append(
				result.FailureReasons,
				"decode stream event: "+err.Error(),
			)
			continue
		}
		responseTypes[string(event.ResponseType)] = struct{}{}
		switch event.ResponseType {
		case types.ResponseTypeAnswer:
			result.Answer = normalizeAnswerChunk(result.Answer, event.Content)
			if event.Done {
				result.AnswerDone = true
			}
		case types.ResponseTypeReferences:
			allReferences = append(allReferences, event.KnowledgeReferences...)
		case types.ResponseTypeComplete:
			result.StreamCompleted = true
		case types.ResponseTypeError:
			result.FailureReasons = append(
				result.FailureReasons,
				"stream error: "+truncateRunes(event.Content, 800),
			)
		}
		if len(event.KnowledgeReferences) > 0 &&
			event.ResponseType != types.ResponseTypeReferences {
			allReferences = append(allReferences, event.KnowledgeReferences...)
		}
		if event.AssistantMessageID != "" {
			result.AssistantMessageID = event.AssistantMessageID
		}
		if event.FinishReason != "" {
			result.FinishReason = event.FinishReason
		}
		if event.Usage != nil {
			result.PromptTokens = event.Usage.PromptTokens
			result.CompletionTokens = event.Usage.CompletionTokens
			result.TotalTokens = event.Usage.TotalTokens
		}
	}
	result.ElapsedMS = time.Since(startedAt).Milliseconds()
	if err := scanner.Err(); err != nil {
		result.FailureReasons = append(result.FailureReasons, "read stream: "+err.Error())
	}
	result.References = uniqueReferences(allReferences)
	for responseType := range responseTypes {
		result.ObservedResponseTypes = append(result.ObservedResponseTypes, responseType)
	}
	sort.Strings(result.ObservedResponseTypes)
	validateCase(testCase, allReferences, &result)
	return result
}

func main() {
	var (
		baseURL = flag.String(
			"base-url",
			"http://127.0.0.1:8080/api/v1",
			"WeKnora API base URL",
		)
		tenantID  = flag.Uint64("tenant-id", 0, "tenant ID")
		kbID      = flag.String("knowledge-base-id", "", "knowledge base ID")
		modelID   = flag.String("model-id", "", "chat/summary model ID")
		modelName = flag.String(
			"expected-model-name",
			"deepseek-v4-flash-int8",
			"exact expected model name",
		)
		casesFile   = flag.String("cases-file", "", "JSON recall case file")
		output      = flag.String("output", "", "sanitized JSON report path")
		timeout     = flag.Duration("case-timeout", 8*time.Minute, "timeout per recall case")
		parallel    = flag.Int("parallel", 1, "maximum concurrent recall cases")
		keepSession = flag.Bool(
			"keep-sessions",
			false,
			"keep temporary E2E sessions after the probe",
		)
	)
	flag.Parse()

	*baseURL = strings.TrimRight(strings.TrimSpace(*baseURL), "/")
	*kbID = strings.TrimSpace(*kbID)
	*modelID = strings.TrimSpace(*modelID)
	*casesFile = strings.TrimSpace(*casesFile)
	if *tenantID == 0 || *kbID == "" || *modelID == "" || *casesFile == "" {
		fmt.Fprintln(
			os.Stderr,
			"-tenant-id, -knowledge-base-id, -model-id and -cases-file are required",
		)
		os.Exit(2)
	}

	caseBytes, err := os.ReadFile(*casesFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var cases []recallCase
	if err := json.Unmarshal(caseBytes, &cases); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(cases) == 0 {
		fmt.Fprintln(os.Stderr, "at least one recall case is required")
		os.Exit(2)
	}
	if *parallel <= 0 || *parallel > 8 {
		fmt.Fprintln(os.Stderr, "-parallel must be between 1 and 8")
		os.Exit(2)
	}
	for _, testCase := range cases {
		if strings.TrimSpace(testCase.Name) == "" || strings.TrimSpace(testCase.Query) == "" {
			fmt.Fprintln(os.Stderr, "each recall case requires name and query")
			os.Exit(2)
		}
		if len(testCase.ExpectedSourceSubstrings) == 0 ||
			len(testCase.RequiredEvidenceGroups) == 0 {
			fmt.Fprintln(
				os.Stderr,
				"each recall case requires source and retrieved evidence assertions",
			)
			os.Exit(2)
		}
	}

	rootCtx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(len(cases)+2)*(*timeout),
	)
	defer cancel()
	apiKey, err := loadTenantAPIKey(rootCtx, *tenantID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	client := &http.Client{
		// The per-case context owns the hard timeout. A zero client timeout is
		// intentional so a healthy SSE stream is not cut between events.
		Timeout: 0,
	}

	modelResponse, err := requestJSON[apiEnvelope[modelData]](
		rootCtx,
		client,
		http.MethodGet,
		*baseURL+"/models/"+*modelID,
		apiKey,
		nil,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	kbResponse, err := requestJSON[apiEnvelope[knowledgeBaseData]](
		rootCtx,
		client,
		http.MethodGet,
		*baseURL+"/knowledge-bases/"+*kbID,
		apiKey,
		nil,
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	result := report{
		GeneratedAt:       time.Now().UTC(),
		BaseURL:           *baseURL,
		TenantID:          *tenantID,
		KnowledgeBaseID:   *kbID,
		KnowledgeBaseName: kbResponse.Data.Name,
		ConfiguredModelID: kbResponse.Data.SummaryModelID,
		RequestedModelID:  *modelID,
		ModelName:         modelResponse.Data.Name,
		ExpectedModelName: strings.TrimSpace(*modelName),
		ModelProvider:     modelResponse.Data.Parameters.Provider,
		ModelEndpointHost: modelEndpointHost(modelResponse.Data.Parameters.BaseURL),
		ModelConfigMatched: modelResponse.Success &&
			modelResponse.Data.ID == *modelID &&
			modelResponse.Data.Name == strings.TrimSpace(*modelName) &&
			modelResponse.Data.Type == string(types.ModelTypeKnowledgeQA),
		KnowledgeBaseMatched: kbResponse.Success &&
			kbResponse.Data.ID == *kbID &&
			kbResponse.Data.SummaryModelID == *modelID,
		Cases: make([]caseResult, len(cases)),
	}
	result.Passed = result.ModelConfigMatched && result.KnowledgeBaseMatched
	sem := make(chan struct{}, *parallel)
	var wg sync.WaitGroup
	for index, testCase := range cases {
		index := index
		testCase := testCase
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			result.Cases[index] = runRecallCase(
				rootCtx,
				client,
				*baseURL,
				apiKey,
				*kbID,
				*modelID,
				testCase,
				*timeout,
				*keepSession,
			)
			fmt.Printf(
				"%s recall_passed=%t answer_diagnostic_ok=%t elapsed_ms=%d references=%d\n",
				result.Cases[index].Name,
				result.Cases[index].Passed,
				result.Cases[index].AnswerDiagnosticOK,
				result.Cases[index].ElapsedMS,
				len(result.Cases[index].References),
			)
		}()
	}
	wg.Wait()
	for _, caseResult := range result.Cases {
		if !caseResult.Passed {
			result.Passed = false
		}
	}

	encodedReport, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if strings.TrimSpace(*output) != "" {
		outputPath := filepath.Clean(*output)
		if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := os.WriteFile(outputPath, append(encodedReport, '\n'), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("report=%s\n", outputPath)
	} else {
		fmt.Println(string(encodedReport))
	}
	if !result.Passed {
		os.Exit(1)
	}
}
