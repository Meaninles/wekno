// Package capacitycontrol compiles the model control plane into one effective
// capacity report. It deliberately owns diagnostics and cross-module
// validation while modeladmission remains the hot-path scheduler.
package capacitycontrol

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/custom/modules/runtimeinstances"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	wikiCitationRequested  = 4
	graphEntityRequested   = 4
	graphRelationRequested = 4
)

type Issue struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Scope    string `json:"scope"`
	Message  string `json:"message"`
}

type ConfiguredCapacity struct {
	MaxInflight        int   `json:"max_inflight"`
	InteractiveReserve int   `json:"interactive_reserve"`
	TenantBurst        int   `json:"tenant_burst"`
	DocumentBurst      int   `json:"document_burst"`
	RPM                int   `json:"rpm"`
	TPM                int64 `json:"tpm"`
	ChatMaxWaiting     *int  `json:"chat_max_waiting"`
}

type EffectiveCapacity struct {
	ProviderTotal       int `json:"provider_total"`
	InteractiveReserved int `json:"interactive_reserved"`
	BackgroundMax       int `json:"background_max"`
	WorkWindow          int `json:"work_window"`
	DerivativeShare     int `json:"derivative_share"`
	WikiShare           int `json:"wiki_share"`
	TenantMax           int `json:"tenant_max"`
	DocumentMax         int `json:"document_max"`
	ChatSessions        int `json:"chat_sessions"`
}

type ModuleGrant struct {
	Module    string `json:"module"`
	Requested int    `json:"requested"`
	Effective int    `json:"effective"`
	WaitMode  string `json:"wait_mode"`
}

type PoolReport struct {
	ID             string                          `json:"id"`
	Name           string                          `json:"name"`
	ResourceKind   string                          `json:"resource_kind"`
	State          string                          `json:"state"`
	PolicyVersion  uint64                          `json:"policy_version"`
	BindingCount   int                             `json:"binding_count"`
	RouteCount     int                             `json:"route_count"`
	Configured     ConfiguredCapacity              `json:"configured"`
	Effective      EffectiveCapacity               `json:"effective"`
	Runtime        modeladmission.PoolRuntimeStats `json:"runtime"`
	ModuleGrants   []ModuleGrant                   `json:"module_grants"`
	Issues         []Issue                         `json:"issues"`
	QuotaPoolIDs   []string                        `json:"quota_pool_ids"`
	GatewayPoolIDs []string                        `json:"gateway_pool_ids"`
}

type RuntimeReport struct {
	Scheduler                   string                         `json:"scheduler"`
	BackgroundWaitMode          string                         `json:"background_wait_mode"`
	CapacityWaitCountsAsFailure bool                           `json:"capacity_wait_counts_as_failure"`
	SchedulerPolicy             modeladmission.SchedulerPolicy `json:"scheduler_policy"`
	Instances                   []runtimeinstances.Instance    `json:"instances"`
	InstanceStaleAfterSeconds   int                            `json:"instance_stale_after_seconds"`
	DerivativeReplicas          int                            `json:"derivative_replicas"`
	WikiReplicas                int                            `json:"wiki_replicas"`
	ParseReplicas               int                            `json:"parse_replicas"`
	BackgroundConsumerSlots     int                            `json:"background_consumer_slots"`
	WikiConsumerSlots           int                            `json:"wiki_consumer_slots"`
	ParseConsumerSlots          int                            `json:"parse_consumer_slots"`
	Admission                   modeladmission.Stats           `json:"admission"`
}

type Summary struct {
	Pools       int `json:"pools"`
	Bindings    int `json:"bindings"`
	Errors      int `json:"errors"`
	Warnings    int `json:"warnings"`
	Information int `json:"information"`
}

type Report struct {
	GeneratedAt   time.Time     `json:"generated_at"`
	Healthy       bool          `json:"healthy"`
	SourceOfTruth string        `json:"source_of_truth"`
	Summary       Summary       `json:"summary"`
	Runtime       RuntimeReport `json:"runtime"`
	Pools         []PoolReport  `json:"pools"`
	Issues        []Issue       `json:"issues"`
}

type Service struct {
	db        *gorm.DB
	admission *modeladmission.Manager
}

func NewService(db *gorm.DB, admission *modeladmission.Manager) *Service {
	return &Service{db: db, admission: admission}
}

func (s *Service) Compile(ctx context.Context) (Report, error) {
	if s == nil || s.db == nil {
		return Report{}, errors.New("capacity control database is unavailable")
	}
	var pools []modeladmission.ResourcePool
	if err := s.db.WithContext(ctx).Order("resource_kind, name, id").Find(&pools).Error; err != nil {
		return Report{}, err
	}
	var bindings []modeladmission.ResourceBinding
	if err := s.db.WithContext(ctx).Order("model_tenant_id, model_id").Find(&bindings).Error; err != nil {
		return Report{}, err
	}
	schedulerPolicy := modeladmission.DefaultSchedulerPolicy()
	if s.db.Migrator().HasTable(&modeladmission.SchedulerPolicy{}) {
		if err := s.db.WithContext(ctx).Where("id = ?", 1).First(&schedulerPolicy).Error; err != nil {
			return Report{}, err
		}
		if err := modeladmission.NormalizeSchedulerPolicy(&schedulerPolicy); err != nil {
			return Report{}, err
		}
	}
	instances := make([]runtimeinstances.Instance, 0)
	if s.db.Migrator().HasTable(&runtimeinstances.Instance{}) {
		var err error
		instances, err = runtimeinstances.ListActive(ctx, s.db)
		if err != nil {
			return Report{}, err
		}
	}

	byPool := make(map[string][]modeladmission.ResourceBinding)
	routePools := make(map[string]map[string]struct{})
	for _, binding := range bindings {
		byPool[binding.ResourcePoolID] = append(byPool[binding.ResourcePoolID], binding)
		if strings.TrimSpace(binding.RouteFingerprint) != "" {
			if routePools[binding.RouteFingerprint] == nil {
				routePools[binding.RouteFingerprint] = make(map[string]struct{})
			}
			routePools[binding.RouteFingerprint][binding.ResourcePoolID] = struct{}{}
		}
	}

	report := Report{
		GeneratedAt:   time.Now().UTC(),
		Healthy:       true,
		SourceOfTruth: "actual_model_resource_pool",
		Runtime: RuntimeReport{
			Scheduler:                   "redis_resource_pool_work_window",
			BackgroundWaitMode:          "bounded_wait_then_budget_free_durable_yield",
			CapacityWaitCountsAsFailure: false,
			SchedulerPolicy:             schedulerPolicy,
			Instances:                   instances,
			InstanceStaleAfterSeconds:   int(runtimeinstances.StaleAfter / time.Second),
		},
	}
	for _, instance := range instances {
		if instance.DerivativeConcurrency > 0 {
			report.Runtime.DerivativeReplicas++
			report.Runtime.BackgroundConsumerSlots += instance.DerivativeConcurrency
		}
		if instance.WikiConcurrency > 0 {
			report.Runtime.WikiReplicas++
			report.Runtime.WikiConsumerSlots += instance.WikiConcurrency
		}
		if instance.ParseConcurrency > 0 {
			report.Runtime.ParseReplicas++
			report.Runtime.ParseConsumerSlots += instance.ParseConcurrency
		}
	}
	if s.admission != nil {
		report.Runtime.Admission = s.admission.Snapshot()
	}

	for index := range pools {
		poolReport := compilePool(pools[index], byPool[pools[index].ID], schedulerPolicy)
		if s.admission != nil {
			runtimeStats, statsErr := s.admission.PoolRuntimeSnapshot(ctx, pools[index].ID)
			if statsErr != nil {
				poolReport.Issues = append(poolReport.Issues, Issue{
					Severity: "warning", Code: "runtime_snapshot_unavailable", Scope: "pool:" + pools[index].ID,
					Message: "实时执行窗口暂时无法读取：" + statsErr.Error(),
				})
			} else {
				poolReport.Runtime = runtimeStats
			}
		}
		report.Pools = append(report.Pools, poolReport)
		for _, issue := range poolReport.Issues {
			countIssue(&report, issue)
		}
	}
	for fingerprint, poolSet := range routePools {
		if len(poolSet) <= 1 {
			continue
		}
		ids := sortedSet(poolSet)
		issue := Issue{
			Severity: "error", Code: "route_split_across_pools", Scope: "route:" + fingerprint,
			Message: "同一个实际模型路由被绑定到多个资源池：" + strings.Join(ids, ", "),
		}
		report.Issues = append(report.Issues, issue)
		countIssue(&report, issue)
	}

	// Old rows may still exist after an upgrade. They are intentionally not
	// read by the scheduler; surfacing them prevents an operator mistaking a
	// stale setting for an effective limit.
	if s.db.Migrator().HasTable(&types.SystemSetting{}) {
		var legacy []types.SystemSetting
		if err := s.db.WithContext(ctx).
			Where("key IN ?", []string{"derivative.concurrency", "derivative.tpm", "chat.queue.default_max_concurrent"}).
			Find(&legacy).Error; err == nil {
			for _, row := range legacy {
				issue := Issue{
					Severity: "warning", Code: "legacy_setting_ignored", Scope: "system_setting:" + row.Key,
					Message: "旧配置 " + row.Key + " 不参与已绑定模型的有效容量；请使用实际模型资源池配置。",
				}
				report.Issues = append(report.Issues, issue)
				countIssue(&report, issue)
			}
		}
	}

	report.Summary.Pools = len(pools)
	report.Summary.Bindings = len(bindings)
	report.Healthy = report.Summary.Errors == 0
	return report, nil
}

func CompilePool(pool modeladmission.ResourcePool, bindings []modeladmission.ResourceBinding) PoolReport {
	return compilePool(pool, bindings, modeladmission.DefaultSchedulerPolicy())
}

func compilePool(
	pool modeladmission.ResourcePool,
	bindings []modeladmission.ResourceBinding,
	schedulerPolicy modeladmission.SchedulerPolicy,
) PoolReport {
	background := modeladmission.EffectiveBackgroundLimit(pool.MaxInflight, pool.InteractiveReserve)
	workWindow := modeladmission.WorkWindow(background, schedulerPolicy)
	derivativeShare := modeladmission.LaneWorkWindow(background, modeladmission.WorkLaneDerivative, schedulerPolicy)
	wikiShare := modeladmission.LaneWorkWindow(background, modeladmission.WorkLaneWiki, schedulerPolicy)
	tenant := minPositive(pool.TenantBurst, pool.MaxInflight)
	document := minPositive(pool.DocumentBurst, tenant, background)
	chatSessions := pool.MaxInflight
	if chatSessions < 1 && pool.MaxInflight > 0 {
		chatSessions = pool.MaxInflight
	}

	routes := make(map[string]struct{})
	quotas := make(map[string]struct{})
	gateways := make(map[string]struct{})
	for _, binding := range bindings {
		if binding.RouteFingerprint != "" {
			routes[binding.RouteFingerprint] = struct{}{}
		}
		if binding.QuotaPoolID != "" {
			quotas[binding.QuotaPoolID] = struct{}{}
		}
		if binding.GatewayPoolID != "" {
			gateways[binding.GatewayPoolID] = struct{}{}
		}
	}

	row := PoolReport{
		ID: pool.ID, Name: pool.Name, ResourceKind: pool.ResourceKind,
		State: pool.State, PolicyVersion: pool.PolicyVersion,
		BindingCount: len(bindings), RouteCount: len(routes),
		Configured: ConfiguredCapacity{
			MaxInflight: pool.MaxInflight, InteractiveReserve: pool.InteractiveReserve,
			TenantBurst: pool.TenantBurst, DocumentBurst: pool.DocumentBurst,
			RPM: pool.RPM, TPM: pool.TPM, ChatMaxWaiting: pool.ChatMaxWaiting,
		},
		Effective: EffectiveCapacity{
			ProviderTotal: pool.MaxInflight, InteractiveReserved: pool.InteractiveReserve,
			BackgroundMax: background, WorkWindow: workWindow,
			DerivativeShare: derivativeShare, WikiShare: wikiShare,
			TenantMax: tenant, DocumentMax: document,
			ChatSessions: chatSessions,
		},
		QuotaPoolIDs: sortedSet(quotas), GatewayPoolIDs: sortedSet(gateways),
	}
	if pool.ResourceKind == string(modeladmission.KindChat) ||
		pool.ResourceKind == string(modeladmission.KindDerivative) {
		row.ModuleGrants = []ModuleGrant{
			moduleGrant("question_batch", 1, document),
			moduleGrant("wiki_citation", wikiCitationRequested, document),
			moduleGrant("graph_entity", graphEntityRequested, document),
			moduleGrant("graph_relation", graphRelationRequested, document),
		}
	}

	add := func(severity, code, message string) {
		row.Issues = append(row.Issues, Issue{Severity: severity, Code: code, Scope: "pool:" + pool.ID, Message: message})
	}
	if pool.MaxInflight < 1 {
		add("error", "invalid_total", "总并发必须大于 0。")
	}
	if pool.InteractiveReserve < 0 || pool.InteractiveReserve >= pool.MaxInflight {
		add("error", "invalid_interactive_reserve", "交互预留必须小于总并发，确保后台工作最终可运行。")
	}
	if pool.MaxBackgroundInflight != background {
		add("error", "background_materialization_mismatch", "后台并发物化值与“总并发 - 交互预留”不一致。")
	}
	if pool.TenantBurst < 1 || pool.TenantBurst > pool.MaxInflight {
		add("error", "tenant_exceeds_pool", "租户上限必须在 1 到总并发之间。")
	}
	if pool.DocumentBurst < 1 || pool.DocumentBurst > tenant || pool.DocumentBurst > background {
		add("error", "document_exceeds_upstream", "文档上限不能超过租户上限或后台有效并发。")
	}
	if pool.ChatMaxConcurrent != nil {
		add("warning", "redundant_chat_concurrency", "对话执行并发已统一由资源池总并发约束；该旧覆盖值会被服务端清除。")
	}
	if pool.TenantGuaranteed != 1 || pool.DocumentGuaranteed != 1 || pool.TokenBurst != 0 {
		add("warning", "legacy_noop_fields", "检测到旧的保证值/Token 突发值；这些字段不再是独立配置。")
	}
	if len(bindings) == 0 {
		add("info", "unbound_pool", "当前没有模型绑定到该资源池。")
	}
	if len(routes) > 1 {
		add("warning", "multiple_routes_share_pool", "该资源池绑定了多个不同实际路由，请确认它们确实共享同一物理限额。")
	}
	for _, grant := range row.ModuleGrants {
		if grant.Effective < grant.Requested {
			add("info", "module_fanout_clamped", grant.Module+" 的本地并行度会被统一调度器收敛到有效文档额度。")
		}
	}
	return row
}

func moduleGrant(module string, requested, documentLimit int) ModuleGrant {
	effective := requested
	if documentLimit > 0 && effective > documentLimit {
		effective = documentLimit
	}
	if effective < 1 {
		effective = 1
	}
	return ModuleGrant{Module: module, Requested: requested, Effective: effective, WaitMode: "scheduler"}
}

func minPositive(values ...int) int {
	result := 0
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if result == 0 || value < result {
			result = value
		}
	}
	return result
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func countIssue(report *Report, issue Issue) {
	if report == nil {
		return
	}
	switch issue.Severity {
	case "error":
		report.Summary.Errors++
	case "warning":
		report.Summary.Warnings++
	default:
		report.Summary.Information++
	}
}
