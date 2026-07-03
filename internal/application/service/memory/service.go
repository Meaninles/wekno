package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
)

// MemoryService implements the MemoryService interface
type MemoryService struct {
	repo         interfaces.MemoryRepository
	modelService interfaces.ModelService
}

// NewMemoryService creates a new memory service
func NewMemoryService(repo interfaces.MemoryRepository, modelService interfaces.ModelService) interfaces.MemoryService {
	return &MemoryService{
		repo:         repo,
		modelService: modelService,
	}
}

const extractGraphPrompt = `
你是从对话中抽取知识图谱的 AI 助手。
给定以下对话，请抽取实体和关系。
用 JSON 格式输出结果，结构如下：
{
  "summary": "对话简要摘要",
  "entities": [
    {
      "title": "实体名称",
      "type": "实体类型（例如 Person、Location、Concept）",
      "description": "实体描述"
    }
  ],
  "relationships": [
    {
      "source": "源实体名称",
      "target": "目标实体名称",
      "description": "关系描述",
      "weight": 1.0
    }
  ]
}

对话：
%s
`

const extractKeywordsPrompt = `
你是从用户查询中抽取搜索关键词的 AI 助手。
给定以下查询，请抽取用于搜索知识图谱的相关关键词。
用 JSON 格式输出结果：
{
  "keywords": ["关键词1", "关键词2"]
}

查询：
%s
`

type extractionResult struct {
	Summary       string                `json:"summary" jsonschema:"对话的简要摘要"`
	Entities      []*types.Entity       `json:"entities"`
	Relationships []*types.Relationship `json:"relationships"`
}

type keywordsResult struct {
	Keywords []string `json:"keywords" jsonschema:"用于搜索知识图谱的相关关键词"`
}

func (s *MemoryService) getChatModel(ctx context.Context) (chat.Chat, error) {
	// Find the first available KnowledgeQA model
	models, err := s.modelService.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list models: %v", err)
	}

	var modelID string
	for _, model := range models {
		if model.Type == types.ModelTypeKnowledgeQA {
			modelID = model.ID
			break
		}
	}

	if modelID == "" {
		return nil, fmt.Errorf("no KnowledgeQA model found")
	}

	return s.modelService.GetChatModel(ctx, modelID)
}

// AddEpisode adds a new episode to the memory graph
func (s *MemoryService) AddEpisode(ctx context.Context, userID string, sessionID string, messages []types.Message) error {
	if !s.repo.IsAvailable(ctx) {
		return fmt.Errorf("memory repository is not available")
	}
	chatModel, err := s.getChatModel(ctx)
	if err != nil {
		return err
	}

	// 1. Construct conversation string
	var conversation string
	for _, msg := range messages {
		conversation += fmt.Sprintf("%s: %s\n", msg.Role, msg.Content)
	}

	// 2. Call LLM to extract graph
	prompt := fmt.Sprintf(extractGraphPrompt, conversation)
	resp, err := chatModel.Chat(ctx, []chat.Message{{Role: "user", Content: prompt}}, &chat.ChatOptions{
		Format: utils.GenerateSchema[extractionResult](),
	})
	if err != nil {
		return fmt.Errorf("failed to call LLM: %v", err)
	}

	var result extractionResult
	if err := json.Unmarshal([]byte(resp.Content), &result); err != nil {
		return fmt.Errorf("failed to parse LLM response: %v", err)
	}

	// 3. Create Episode object
	episode := &types.Episode{
		ID:        uuid.New().String(),
		UserID:    userID,
		SessionID: sessionID,
		Summary:   result.Summary,
		CreatedAt: time.Now(),
	}

	// 4. Save to repository
	if err := s.repo.SaveEpisode(ctx, episode, result.Entities, result.Relationships); err != nil {
		return fmt.Errorf("failed to save episode: %v", err)
	}

	return nil
}

// RetrieveMemory retrieves relevant memory context based on the current query and user
func (s *MemoryService) RetrieveMemory(ctx context.Context, userID string, query string) (*types.MemoryContext, error) {
	if !s.repo.IsAvailable(ctx) {
		return nil, fmt.Errorf("memory repository is not available")
	}
	chatModel, err := s.getChatModel(ctx)
	if err != nil {
		return nil, err
	}

	// 1. Extract keywords
	prompt := fmt.Sprintf(extractKeywordsPrompt, query)
	resp, err := chatModel.Chat(ctx, []chat.Message{{Role: "user", Content: prompt}}, &chat.ChatOptions{
		Format: utils.GenerateSchema[keywordsResult](),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to call LLM: %v", err)
	}

	var result keywordsResult
	if err := json.Unmarshal([]byte(resp.Content), &result); err != nil {
		return nil, fmt.Errorf("failed to parse LLM response: %v", err)
	}

	// 2. Retrieve related episodes
	episodes, err := s.repo.FindRelatedEpisodes(ctx, userID, result.Keywords, 5)
	if err != nil {
		return nil, fmt.Errorf("failed to find related episodes: %v", err)
	}

	// 3. Construct MemoryContext
	memoryContext := &types.MemoryContext{
		RelatedEpisodes: make([]types.Episode, len(episodes)),
	}
	for i, ep := range episodes {
		memoryContext.RelatedEpisodes[i] = *ep
	}

	return memoryContext, nil
}
