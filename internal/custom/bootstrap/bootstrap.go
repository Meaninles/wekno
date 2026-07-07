package bootstrap

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	appservice "github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/custom/modules/answerfeedback"
	"github.com/Tencent/WeKnora/internal/custom/modules/builtinagentdefaults"
	"github.com/Tencent/WeKnora/internal/custom/modules/configcenter"
	"github.com/Tencent/WeKnora/internal/custom/modules/dbanalytics"
	"github.com/Tencent/WeKnora/internal/custom/modules/generalagent"
	"github.com/Tencent/WeKnora/internal/custom/modules/iam"
	"github.com/Tencent/WeKnora/internal/custom/modules/scheduledchat"
	"github.com/Tencent/WeKnora/internal/custom/modules/sessionstate"
	"github.com/Tencent/WeKnora/internal/custom/modules/skillhub"
	"github.com/Tencent/WeKnora/internal/custom/modules/userguide"
	"github.com/Tencent/WeKnora/internal/handler"
	sessionhandler "github.com/Tencent/WeKnora/internal/handler/session"
	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type Handlers struct {
	ConfigCenter         *configcenter.Handler
	IAM                  *iam.Handler
	ScheduledChat        *scheduledchat.Handler
	SessionState         *sessionstate.Handler
	SkillHub             *skillhub.Handler
	DBAnalytics          *dbanalytics.Handler
	GeneralAgent         *generalagent.Handler
	AnswerFeedback       *answerfeedback.Handler
	BuiltinAgentDefaults *builtinagentdefaults.Handler

	configCenterService         *configcenter.Service
	answerFeedbackService       *answerfeedback.Service
	builtinAgentDefaultsService *builtinagentdefaults.Service
	dbAnalyticsService          *dbanalytics.Service
	generalAgentService         *generalagent.Service
	iamService                  *iam.Service
	scheduledChatService        *scheduledchat.Service
	sessionStateService         *sessionstate.Service
	skillHubService             *skillhub.Service
	userGuideService            *userguide.Service
}

func NewHandlers(
	cfg *config.Config,
	db *gorm.DB,
	duckdb *sql.DB,
	userService interfaces.UserService,
	orgService interfaces.OrganizationService,
	knowledgeBaseService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	kbShareService interfaces.KBShareService,
	sessionService interfaces.SessionService,
	agentService interfaces.AgentService,
	messageService interfaces.MessageService,
	customAgentService interfaces.CustomAgentService,
	agentShareService interfaces.AgentShareService,
	tenantService interfaces.TenantService,
	tenantMemberService interfaces.TenantMemberService,
	streamManager interfaces.StreamManager,
	modelService interfaces.ModelService,
	fileService interfaces.FileService,
	documentReader interfaces.DocumentReader,
	imageResolver *docparser.ImageResolver,
) (*Handlers, error) {
	ctx := context.Background()
	configCenterService := configcenter.NewService(db)
	answerFeedbackService := answerfeedback.NewService(db, answerfeedback.LoadConfigFromEnv())
	builtinAgentDefaultsService := builtinagentdefaults.NewService(db, customAgentService)
	dbAnalyticsService := dbanalytics.NewService(db, duckdb)
	generalAgentService := generalagent.NewService(db, sessionService, agentService, messageService, modelService, knowledgeService, fileService)
	iamService := iam.NewService(db, userService)
	userGuideService := userguide.NewService(db, orgService, knowledgeBaseService, kbShareService)
	scheduledChatService := scheduledchat.NewService(
		db,
		sessionService,
		messageService,
		customAgentService,
		agentShareService,
		tenantService,
		tenantMemberService,
		userService,
		streamManager,
		fileService,
		modelService,
		sessionhandler.NewAttachmentProcessor(fileService, documentReader, imageResolver, modelService),
	)
	sessionStateService := sessionstate.NewService(db)
	skillHubService := skillhub.NewService(db)
	if err := configCenterService.Migrate(ctx); err != nil {
		return nil, err
	}
	if err := answerFeedbackService.Migrate(ctx); err != nil {
		return nil, err
	}
	if err := dbAnalyticsService.Migrate(ctx); err != nil {
		return nil, err
	}
	if err := generalAgentService.Migrate(ctx); err != nil {
		return nil, err
	}
	if err := iamService.Migrate(ctx); err != nil {
		return nil, err
	}
	if err := scheduledChatService.Migrate(ctx); err != nil {
		return nil, err
	}
	if err := sessionStateService.Migrate(ctx); err != nil {
		return nil, err
	}
	if err := skillHubService.Migrate(ctx); err != nil {
		return nil, err
	}
	if err := userGuideService.EnsureAllUsers(ctx); err != nil {
		logger.Warnf(ctx, "[custom bootstrap] failed to provision guide KB for existing users: %v", err)
	}
	if err := builtinAgentDefaultsService.EnsureAllUsers(ctx); err != nil {
		logger.Warnf(ctx, "[custom bootstrap] failed to provision built-in agent defaults for existing users: %v", err)
	}
	provisionUser := func(ctx context.Context, user *types.User) error {
		baseCtx := context.Background()
		if ctx != nil {
			baseCtx = context.WithoutCancel(ctx)
		}
		provisionCtx, cancel := context.WithTimeout(baseCtx, 30*time.Second)
		defer cancel()

		configErr := configCenterService.EnsureUserProvisioned(provisionCtx, user)
		builtinErr := builtinAgentDefaultsService.EnsureUserProvisioned(provisionCtx, user)
		guideErr := userGuideService.EnsureUserProvisioned(provisionCtx, user)
		errs := make([]string, 0, 3)
		if configErr != nil {
			errs = append(errs, fmt.Sprintf("config center: %v", configErr))
		}
		if builtinErr != nil {
			errs = append(errs, fmt.Sprintf("built-in agent defaults: %v", builtinErr))
		}
		if guideErr != nil {
			errs = append(errs, fmt.Sprintf("user guide: %v", guideErr))
		}
		if len(errs) > 0 {
			return fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		return nil
	}
	handler.RegisterCustomProvisionerHook(provisionUser)
	iamService.SetProvisioner(provisionUser)
	appservice.RegisterAdditionalSkillLister(skillHubService.AdditionalMetadata)
	appservice.RegisterProfessionalSkillLister(skillhub.ProfessionalMetadata)
	appservice.RegisterRuntimeSkillConfigurer(skillHubService.ConfigureRuntimeSkills)
	appservice.RegisterSelectedSkillContextResolver(skillHubService.SelectedSkillContext)
	appservice.RegisterAllSkillContextResolver(skillHubService.AllSkillContext)
	appservice.RegisterBuiltinAgentConfigOverlay(builtinAgentDefaultsService.ApplyReferenceModelDefaults)
	handler.RegisterMessageClientEnricher(answerFeedbackService.EnrichMessagesForClient)
	sessionhandler.RegisterAssistantRunSnapshotHook(answerFeedbackService.HandleAssistantRunSnapshot)
	sessionhandler.RegisterAgentQARunner(types.AgentTypeGeneralAgent, generalAgentService.Run)
	sessionhandler.RegisterAgentQARunner(types.AgentTypeDocumentProcessingAgent, generalAgentService.Run)
	sessionhandler.RegisterAgentQARunner(types.AgentTypeDataAnalysis, generalAgentService.Run)
	appservice.RegisterRuntimeToolRecognizer(func(_ context.Context, config *types.AgentConfig, toolName string) bool {
		if config == nil {
			return false
		}
		if types.IsClaudeSDKAgentType(config.AgentType) {
			return toolName == dbanalytics.ToolDBCatalog ||
				toolName == dbanalytics.ToolDBSchema ||
				toolName == dbanalytics.ToolDBQuery
		}
		return false
	})
	appservice.RegisterRuntimeToolRegistrar(func(ctx context.Context, registry *agenttools.ToolRegistry, config *types.AgentConfig, sessionID string) error {
		if config == nil || len(config.DBDataSources) == 0 {
			return nil
		}
		if !types.IsClaudeSDKAgentType(config.AgentType) {
			return nil
		}
		allowed := map[string]bool{}
		for _, tool := range config.AllowedTools {
			allowed[tool] = true
		}
		scope := dbanalytics.ToolScope{
			AgentID:        config.AgentID,
			AgentType:      config.AgentType,
			SessionID:      sessionID,
			SourceTenantID: config.AgentTenantID,
			SourceIDs:      append([]string(nil), config.DBDataSources...),
		}
		allowChart := types.IsClaudeSDKAgentType(config.AgentType)
		if allowed[dbanalytics.ToolDBCatalog] {
			registry.RegisterTool(dbanalytics.NewCatalogTool(dbAnalyticsService, scope))
		}
		if allowed[dbanalytics.ToolDBSchema] {
			registry.RegisterTool(dbanalytics.NewSchemaTool(dbAnalyticsService, scope))
		}
		if allowed[dbanalytics.ToolDBQuery] {
			registry.RegisterTool(dbanalytics.NewQueryTool(dbAnalyticsService, scope, allowChart))
		}
		return nil
	})
	return &Handlers{
		ConfigCenter:                configcenter.NewHandler(configCenterService),
		IAM:                         iam.NewHandler(iamService, orgService),
		ScheduledChat:               scheduledchat.NewHandler(scheduledChatService),
		SessionState:                sessionstate.NewHandler(sessionStateService),
		SkillHub:                    skillhub.NewHandler(skillHubService, db),
		DBAnalytics:                 dbanalytics.NewHandler(dbAnalyticsService),
		GeneralAgent:                generalagent.NewHandler(generalAgentService),
		AnswerFeedback:              answerfeedback.NewHandler(answerFeedbackService, messageService),
		BuiltinAgentDefaults:        builtinagentdefaults.NewHandler(builtinAgentDefaultsService),
		configCenterService:         configCenterService,
		answerFeedbackService:       answerFeedbackService,
		builtinAgentDefaultsService: builtinAgentDefaultsService,
		dbAnalyticsService:          dbAnalyticsService,
		generalAgentService:         generalAgentService,
		iamService:                  iamService,
		scheduledChatService:        scheduledChatService,
		sessionStateService:         sessionStateService,
		skillHubService:             skillHubService,
		userGuideService:            userGuideService,
	}, nil
}

func StartSchedulers(handlers *Handlers) {
	if handlers == nil {
		return
	}
	ctx := context.Background()
	if handlers.iamService != nil {
		if err := handlers.iamService.StartScheduler(ctx); err != nil {
			logger.Warnf(ctx, "[custom bootstrap] failed to start IAM scheduler: %v", err)
		}
	}
	if handlers.scheduledChatService != nil {
		if err := handlers.scheduledChatService.StartScheduler(ctx); err != nil {
			logger.Warnf(ctx, "[custom bootstrap] failed to start scheduled chat scheduler: %v", err)
		}
	}
	if handlers.answerFeedbackService != nil {
		handlers.answerFeedbackService.Start()
	}
}

func RegisterEmbedRoutes(embed *gin.RouterGroup, handlers *Handlers) {
	if embed == nil || handlers == nil || handlers.GeneralAgent == nil {
		return
	}
	embed.GET("/sessions/:session_id/artifacts/:id/download", handlers.GeneralAgent.DownloadEmbedArtifact)
}

func RegisterRoutes(v1 *gin.RouterGroup, handlers *Handlers, systemAdmin gin.HandlerFunc, ownedAgentOrAdmin gin.HandlerFunc) {
	if handlers == nil {
		return
	}
	customPublic := v1.Group("/custom")
	{
		ssoRoutes := customPublic.Group("/iam/sso")
		{
			ssoRoutes.GET("/config", handlers.IAM.GetSSOConfig)
			ssoRoutes.GET("/entry", handlers.IAM.SSOEntry)
			ssoRoutes.GET("/url", handlers.IAM.GetSSOAuthorizationURL)
			ssoRoutes.GET("/callback", handlers.IAM.SSOCallback)
		}
		generalAgentInternalRoutes := customPublic.Group("/general-agent/internal")
		{
			generalAgentInternalRoutes.POST("/tools/call", handlers.GeneralAgent.CallTool)
		}
	}

	custom := v1.Group("/custom", systemAdmin)
	{
		configCenterRoutes := custom.Group("/config-center")
		{
			configCenterRoutes.GET("/users", handlers.ConfigCenter.ListUsers)
			configCenterRoutes.GET("/resources", handlers.ConfigCenter.ListResources)
			configCenterRoutes.GET("/defaults", handlers.ConfigCenter.GetDefaults)
			configCenterRoutes.PUT("/defaults", handlers.ConfigCenter.SaveDefaults)
			configCenterRoutes.GET("/users/:user_id/grants", handlers.ConfigCenter.GetUserGrants)
			configCenterRoutes.PUT("/users/:user_id/grants", handlers.ConfigCenter.SaveUserGrants)
			configCenterRoutes.POST("/apply", handlers.ConfigCenter.ApplyAll)
			configCenterRoutes.POST("/users/:user_id/apply", handlers.ConfigCenter.ApplyUser)
		}

		iamRoutes := custom.Group("/iam")
		{
			iamRoutes.GET("/settings", handlers.IAM.GetSetting)
			iamRoutes.PUT("/settings", handlers.IAM.SaveSetting)
			iamRoutes.POST("/sync", handlers.IAM.RunSync)
			iamRoutes.GET("/sync-runs", handlers.IAM.ListRuns)
		}
	}

	if handlers.BuiltinAgentDefaults != nil && ownedAgentOrAdmin != nil {
		builtinAgentDefaultRoutes := v1.Group("/custom/builtin-agent-defaults", ownedAgentOrAdmin)
		{
			builtinAgentDefaultRoutes.POST("/agents/:id/reset", handlers.BuiltinAgentDefaults.Reset)
		}
	}

	spaceMemberRoutes := v1.Group("/custom/iam")
	{
		spaceMemberRoutes.GET("/space-member-organizations", handlers.IAM.ListSpaceMemberCandidateOrganizations)
		spaceMemberRoutes.GET("/space-member-candidates", handlers.IAM.ListSpaceMemberCandidateUsers)
	}

	if handlers.SessionState != nil {
		sessionStateRoutes := v1.Group("/custom/session-state")
		{
			sessionStateRoutes.POST("/status", handlers.SessionState.ListStatus)
			sessionStateRoutes.POST("/sessions/:session_id/read", handlers.SessionState.MarkRead)
		}
	}

	skillRoutes := v1.Group("/custom/skills")
	{
		skillRoutes.GET("", handlers.SkillHub.List)
		skillRoutes.POST("", handlers.SkillHub.Create)
		skillRoutes.GET("/professional", handlers.SkillHub.ListProfessional)
		skillRoutes.POST("/professional", handlers.SkillHub.ImportProfessional)
		skillRoutes.PUT("/professional/:id", handlers.SkillHub.UpdateProfessional)
		skillRoutes.DELETE("/professional/:id", handlers.SkillHub.DeleteProfessional)
		skillRoutes.GET("/professional/:id/download", handlers.SkillHub.DownloadProfessional)
		skillRoutes.GET("/professional/:id/shares", handlers.SkillHub.ListProfessionalShares)
		skillRoutes.POST("/professional/:id/shares/organizations", handlers.SkillHub.ShareProfessionalOrganization)
		skillRoutes.POST("/professional/:id/shares/users", handlers.SkillHub.ShareProfessionalUser)
		skillRoutes.DELETE("/professional/:id/shares/organizations/:share_id", handlers.SkillHub.RemoveProfessionalOrganizationShare)
		skillRoutes.DELETE("/professional/:id/shares/users/:share_id", handlers.SkillHub.RemoveProfessionalUserShare)
		skillRoutes.GET("/users", handlers.SkillHub.SearchUsers)
		skillRoutes.GET("/organizations/:id", handlers.SkillHub.ListByOrganization)
		skillRoutes.PUT("/:id", handlers.SkillHub.Update)
		skillRoutes.DELETE("/:id", handlers.SkillHub.Delete)
		skillRoutes.GET("/:id/shares", handlers.SkillHub.ListShares)
		skillRoutes.POST("/:id/shares/organizations", handlers.SkillHub.ShareOrganization)
		skillRoutes.POST("/:id/shares/users", handlers.SkillHub.ShareUser)
		skillRoutes.DELETE("/:id/shares/organizations/:share_id", handlers.SkillHub.RemoveOrganizationShare)
		skillRoutes.DELETE("/:id/shares/users/:share_id", handlers.SkillHub.RemoveUserShare)
	}

	scheduledChatRoutes := v1.Group("/custom/scheduled-chat")
	{
		scheduledChatRoutes.GET("/variables", handlers.ScheduledChat.Variables)
		scheduledChatRoutes.GET("/prompt-templates", handlers.ScheduledChat.PromptTemplates)
		scheduledChatRoutes.POST("/render-preview", handlers.ScheduledChat.RenderPreview)
		scheduledChatRoutes.GET("/tasks", handlers.ScheduledChat.ListTasks)
		scheduledChatRoutes.POST("/tasks", handlers.ScheduledChat.CreateTask)
		scheduledChatRoutes.GET("/tasks/:id", handlers.ScheduledChat.GetTask)
		scheduledChatRoutes.PUT("/tasks/:id", handlers.ScheduledChat.UpdateTask)
		scheduledChatRoutes.DELETE("/tasks/:id", handlers.ScheduledChat.DeleteTask)
		scheduledChatRoutes.POST("/tasks/:id/run-now", handlers.ScheduledChat.RunTaskNow)
		scheduledChatRoutes.GET("/tasks/:id/runs", handlers.ScheduledChat.ListRuns)
	}

	dbAnalyticsRoutes := v1.Group("/custom/db-analytics")
	{
		dbAnalyticsRoutes.GET("/sources", handlers.DBAnalytics.ListSources)
		dbAnalyticsRoutes.POST("/sources", handlers.DBAnalytics.CreateSource)
		dbAnalyticsRoutes.POST("/source-test", handlers.DBAnalytics.TestSourceConfig)
		dbAnalyticsRoutes.GET("/shared-sources", handlers.DBAnalytics.ListSharedSources)
		dbAnalyticsRoutes.GET("/organizations/:id/shared-sources", handlers.DBAnalytics.ListOrganizationSharedSources)
		dbAnalyticsRoutes.GET("/sources/:id", handlers.DBAnalytics.GetSource)
		dbAnalyticsRoutes.PUT("/sources/:id", handlers.DBAnalytics.UpdateSource)
		dbAnalyticsRoutes.DELETE("/sources/:id", handlers.DBAnalytics.DeleteSource)
		dbAnalyticsRoutes.GET("/sources/:id/shares", handlers.DBAnalytics.ListSourceShares)
		dbAnalyticsRoutes.POST("/sources/:id/shares", handlers.DBAnalytics.ShareSource)
		dbAnalyticsRoutes.PUT("/sources/:id/shares/:share_id", handlers.DBAnalytics.UpdateSourceSharePermission)
		dbAnalyticsRoutes.DELETE("/sources/:id/shares/:share_id", handlers.DBAnalytics.RemoveSourceShare)
		dbAnalyticsRoutes.POST("/sources/:id/test", handlers.DBAnalytics.TestSource)
		dbAnalyticsRoutes.GET("/sources/:id/schemas", handlers.DBAnalytics.ListSchemas)
		dbAnalyticsRoutes.GET("/sources/:id/tables", handlers.DBAnalytics.ListTables)
		dbAnalyticsRoutes.POST("/sources/:id/refresh-metadata", handlers.DBAnalytics.RefreshMetadata)
		dbAnalyticsRoutes.PUT("/sources/:id/tables/scope", handlers.DBAnalytics.SetTableScope)
		dbAnalyticsRoutes.PUT("/columns/:column_id", handlers.DBAnalytics.UpdateColumn)
		dbAnalyticsRoutes.GET("/agents/:agent_id/bindings", handlers.DBAnalytics.GetAgentBindings)
		dbAnalyticsRoutes.PUT("/agents/:agent_id/bindings", handlers.DBAnalytics.SetAgentBindings)
	}

	generalAgentRoutes := v1.Group("/custom/general-agent")
	{
		generalAgentRoutes.GET("/artifacts/:id/download", handlers.GeneralAgent.DownloadArtifact)
	}

	answerFeedbackRoutes := v1.Group("/custom/answer-feedback")
	{
		answerFeedbackRoutes.PUT("/messages/:session_id/:message_id", handlers.AnswerFeedback.SetMessageFeedback)
		answerFeedbackRoutes.POST("/messages/:session_id/:message_id", handlers.AnswerFeedback.SetMessageFeedback)
		answerFeedbackRoutes.GET("/messages", handlers.AnswerFeedback.ListMessageFeedback)
	}
}
