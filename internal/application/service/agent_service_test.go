package service

import (
	"context"
	"testing"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	internalmcp "github.com/Tencent/WeKnora/internal/mcp"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/assert"
)

type countingAgentMCPService struct {
	interfaces.MCPServiceService
	listCalls      int
	listByIDsCalls int
	lastIDs        []string
}

func (s *countingAgentMCPService) ListMCPServices(context.Context, uint64) ([]*types.MCPService, error) {
	s.listCalls++
	return []*types.MCPService{}, nil
}

func (s *countingAgentMCPService) ListMCPServicesByIDs(_ context.Context, _ uint64, ids []string) ([]*types.MCPService, error) {
	s.listByIDsCalls++
	s.lastIDs = append([]string(nil), ids...)
	return []*types.MCPService{}, nil
}

func TestRegisterMCPTools_DefaultEmptyModeLoadsAll(t *testing.T) {
	mcpSvc := &countingAgentMCPService{}
	service := &agentService{
		mcpServiceService: mcpSvc,
		mcpManager:        internalmcp.NewMCPManager(nil),
	}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(10000))

	service.registerMCPTools(ctx, agenttools.NewToolRegistry(), &types.AgentConfig{}, nil, "", "")

	assert.Equal(t, 1, mcpSvc.listCalls)
	assert.Equal(t, 0, mcpSvc.listByIDsCalls)
}

func TestRegisterMCPTools_SelectedEmptyDoesNotLoadAll(t *testing.T) {
	mcpSvc := &countingAgentMCPService{}
	service := &agentService{
		mcpServiceService: mcpSvc,
		mcpManager:        internalmcp.NewMCPManager(nil),
	}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(10000))

	service.registerMCPTools(ctx, agenttools.NewToolRegistry(), &types.AgentConfig{
		MCPSelectionMode: "selected",
	}, nil, "", "")

	assert.Equal(t, 0, mcpSvc.listCalls)
	assert.Equal(t, 0, mcpSvc.listByIDsCalls)
}

func TestRegisterMCPTools_SelectedIDsOnlyLoadsSelected(t *testing.T) {
	mcpSvc := &countingAgentMCPService{}
	service := &agentService{
		mcpServiceService: mcpSvc,
		mcpManager:        internalmcp.NewMCPManager(nil),
	}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(10000))

	service.registerMCPTools(ctx, agenttools.NewToolRegistry(), &types.AgentConfig{
		MCPSelectionMode: "selected",
		MCPServices:      []string{"mcp-1", "mcp-2"},
	}, nil, "", "")

	assert.Equal(t, 0, mcpSvc.listCalls)
	assert.Equal(t, 1, mcpSvc.listByIDsCalls)
	assert.Equal(t, []string{"mcp-1", "mcp-2"}, mcpSvc.lastIDs)
}
