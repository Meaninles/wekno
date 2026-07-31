// Package chatqueue provides conversation-level FIFO admission for chat
// models. Redis is the shared source of truth across API replicas; Lite mode
// uses the same semantics in process so a Redis-less single instance remains
// usable.
package chatqueue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	sessionhandler "github.com/Tencent/WeKnora/internal/handler/session"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	defaultMaxConcurrent = 8
	defaultMaxWaiting    = 500
	defaultPerUser       = 3

	queuePollInterval = 500 * time.Millisecond
	activeLeaseTTL    = 90 * time.Second
	activeHeartbeat   = 25 * time.Second
	waiterLeaseTTL    = 45 * time.Second
	defaultMaxWait    = time.Hour
)

var (
	errTicketMissing = errors.New("chat queue ticket no longer exists")
	errWaitExpired   = errors.New("chat queue wait timeout")
)

type queuePolicy struct {
	Enabled       bool
	MaxConcurrent int
	MaxWaiting    int
	MaxPerUser    int
}

type Manager struct {
	redis         *redis.Client
	settings      interfaces.SystemSettingService
	admission     *modeladmission.Manager
	models        interfaces.ModelService
	knowledgeBase interfaces.KnowledgeBaseService
	knowledge     interfaces.KnowledgeService
	keyPrefix     string
	maxWait       time.Duration

	localMu          sync.Mutex
	localSequence    int64
	localPools       map[string]*localPool
	localUserWaiting map[string]map[string]int64

	poolCacheMu sync.Mutex
	poolCache   map[string]cachedPool
}

type cachedPool struct {
	pool      *modeladmission.ResourcePool
	err       error
	expiresAt time.Time
}

type localPool struct {
	active  map[string]int64
	waiting []string
	waiters map[string]localWaiter
}

type localWaiter struct {
	principal string
	expiresAt int64
}

// NewManager is the DI provider. All dependencies are read-only from the
// queue's perspective; resource-pool and system-setting edits remain owned by
// their existing admin control planes.
func NewManager(
	redisClient *redis.Client,
	settings interfaces.SystemSettingService,
	admission *modeladmission.Manager,
	models interfaces.ModelService,
	knowledgeBase interfaces.KnowledgeBaseService,
	knowledge interfaces.KnowledgeService,
) *Manager {
	namespace := strings.TrimSpace(os.Getenv("WEKNORA_REDIS_NAMESPACE"))
	if namespace == "" {
		namespace = "default"
	}
	maxWait := envSeconds("WEKNORA_CHAT_QUEUE_MAX_WAIT_SECONDS", defaultMaxWait)
	return &Manager{
		redis:            redisClient,
		settings:         settings,
		admission:        admission,
		models:           models,
		knowledgeBase:    knowledgeBase,
		knowledge:        knowledge,
		keyPrefix:        "weknora:{chat-queue}:" + namespace + ":",
		maxWait:          maxWait,
		localPools:       make(map[string]*localPool),
		localUserWaiting: make(map[string]map[string]int64),
		poolCache:        make(map[string]cachedPool),
	}
}

func envSeconds(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 1 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

// Admit implements session.RegisterChatQueueAdmissionHook.
func (m *Manager) Admit(
	ctx context.Context,
	request sessionhandler.ChatQueueAdmissionRequest,
) (sessionhandler.ChatQueueTicket, *sessionhandler.ChatQueueRejection, error) {
	if m == nil {
		return nil, unavailableRejection("聊天排队服务未初始化"), errors.New("chat queue manager is nil")
	}
	if !m.basePolicy(ctx).Enabled {
		return nil, nil, nil
	}

	model, err := m.resolveModel(ctx, request)
	if err != nil {
		rejection := &sessionhandler.ChatQueueRejection{
			Code:    sessionhandler.ChatQueueRejectModelUnavailable,
			Message: "无法确定本次对话使用的聊天模型，请检查模型配置后重试",
		}
		return nil, rejection, err
	}
	pool, err := m.admission.ResolveResourcePool(ctx, model)
	if err != nil {
		return nil, unavailableRejection("聊天模型资源池当前不可用，请稍后重试"), err
	}
	policy := m.policyWithPool(ctx, pool)
	if !policy.Enabled {
		return nil, nil, nil
	}

	principal := strings.TrimSpace(request.PrincipalID)
	if principal == "" {
		principal = fmt.Sprintf("tenant:%d:anonymous", request.TenantID)
	}
	ticket := &Ticket{
		manager:       m,
		token:         uuid.NewString(),
		principalHash: digest(principal),
		poolID:        pool.ID,
		modelID:       model.ID,
		initialPool:   clonePool(pool),
		queuedAt:      time.Now().UTC(),
		stopHeartbeat: make(chan struct{}),
	}

	var result admissionResult
	if m.redis != nil {
		result, err = m.admitRedis(ctx, ticket, policy)
	} else {
		result, err = m.admitLocal(ticket, policy)
		ticket.local = true
	}
	if err != nil {
		return nil, unavailableRejection("聊天排队服务暂时不可用，请稍后重试"), err
	}
	if result.code < 0 {
		return nil, rejectionFor(result, ticket, policy), nil
	}

	ticket.queued = result.code == 0
	ticket.lastSnapshot = ticket.snapshot(
		mapState(result.code == 1), result.position, result.active, result.waiting, policy,
	)
	if result.code == 1 {
		ticket.admitted.Store(true)
		ticket.startHeartbeat()
	}
	return ticket, nil, nil
}

func unavailableRejection(message string) *sessionhandler.ChatQueueRejection {
	return &sessionhandler.ChatQueueRejection{
		Code:    sessionhandler.ChatQueueRejectUnavailable,
		Message: message,
	}
}

func rejectionFor(
	result admissionResult,
	ticket *Ticket,
	policy queuePolicy,
) *sessionhandler.ChatQueueRejection {
	rejection := &sessionhandler.ChatQueueRejection{
		ModelID:        ticket.modelID,
		ResourcePoolID: ticket.poolID,
		Waiting:        result.waiting,
		Active:         result.active,
		MaxConcurrent:  policy.MaxConcurrent,
		MaxWaiting:     policy.MaxWaiting,
		UserWaiting:    result.userWaiting,
		UserMaxWaiting: policy.MaxPerUser,
	}
	if result.code == -2 {
		rejection.Code = sessionhandler.ChatQueueRejectUserLimit
		rejection.Message = fmt.Sprintf(
			"你已有 %d 个会话正在排队，已达到个人上限 %d；请等待或取消一个排队会话后再试",
			result.userWaiting, policy.MaxPerUser,
		)
		return rejection
	}
	rejection.Code = sessionhandler.ChatQueueRejectPoolFull
	rejection.Message = fmt.Sprintf(
		"当前模型的系统排队队列已满（%d/%d），请稍后重试",
		result.waiting, policy.MaxWaiting,
	)
	return rejection
}

func (m *Manager) resolveModel(
	ctx context.Context,
	request sessionhandler.ChatQueueAdmissionRequest,
) (*types.Model, error) {
	if m.models == nil {
		return nil, errors.New("model service is unavailable")
	}
	loadInteractive := func(id string) (*types.Model, bool) {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, false
		}
		model, err := m.models.GetModelByID(ctx, id)
		return model, err == nil && model != nil && model.IsInteractiveChatModel()
	}
	if model, ok := loadInteractive(request.SummaryModelID); ok {
		return model, nil
	}
	if model, ok := loadInteractive(request.AgentModelID); ok {
		return model, nil
	}

	kbIDs := deduplicate(request.KnowledgeBaseIDs)
	if len(request.KnowledgeIDs) > 0 && m.knowledge != nil {
		items, err := m.knowledge.GetKnowledgeBatchWithSharedAccess(
			ctx, request.TenantID, request.KnowledgeIDs,
		)
		if err == nil {
			for _, item := range items {
				if item != nil && item.KnowledgeBaseID != "" {
					kbIDs = append(kbIDs, item.KnowledgeBaseID)
				}
			}
			kbIDs = deduplicate(kbIDs)
		}
	}
	var firstValid *types.Model
	for _, kbID := range kbIDs {
		if m.knowledgeBase == nil {
			break
		}
		kb, err := m.knowledgeBase.GetKnowledgeBaseByID(ctx, kbID)
		if err != nil || kb == nil {
			continue
		}
		model, ok := loadInteractive(kb.SummaryModelID)
		if !ok {
			continue
		}
		if firstValid == nil {
			firstValid = model
		}
		if model.Source == types.ModelSourceRemote {
			return model, nil
		}
	}
	if firstValid != nil {
		return firstValid, nil
	}
	models, err := m.models.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	for _, model := range models {
		if model != nil && model.IsInteractiveChatModel() {
			return model, nil
		}
	}
	return nil, errors.New("no interactive chat model is available")
}

func deduplicate(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func clonePool(pool *modeladmission.ResourcePool) *modeladmission.ResourcePool {
	if pool == nil {
		return nil
	}
	cloned := *pool
	if pool.ChatMaxConcurrent != nil {
		value := *pool.ChatMaxConcurrent
		cloned.ChatMaxConcurrent = &value
	}
	if pool.ChatMaxWaiting != nil {
		value := *pool.ChatMaxWaiting
		cloned.ChatMaxWaiting = &value
	}
	return &cloned
}

func (m *Manager) basePolicy(ctx context.Context) queuePolicy {
	policy := queuePolicy{
		Enabled:       true,
		MaxConcurrent: defaultMaxConcurrent,
		MaxWaiting:    defaultMaxWaiting,
		MaxPerUser:    defaultPerUser,
	}
	if m.settings == nil {
		return policy
	}
	policy.Enabled = m.settings.GetBool(
		ctx, "chat.queue.enabled", "WEKNORA_CHAT_QUEUE_ENABLED", true,
	)
	policy.MaxConcurrent = boundedInt(
		m.settings.GetInt(
			ctx,
			"chat.queue.default_max_concurrent",
			"WEKNORA_CHAT_QUEUE_DEFAULT_MAX_CONCURRENT",
			defaultMaxConcurrent,
		),
		1, 4096, defaultMaxConcurrent,
	)
	policy.MaxWaiting = boundedInt(
		m.settings.GetInt(
			ctx,
			"chat.queue.default_max_waiting",
			"WEKNORA_CHAT_QUEUE_DEFAULT_MAX_WAITING",
			defaultMaxWaiting,
		),
		0, 100000, defaultMaxWaiting,
	)
	policy.MaxPerUser = boundedInt(
		m.settings.GetInt(
			ctx,
			"chat.queue.max_waiting_per_user",
			"WEKNORA_CHAT_QUEUE_MAX_WAITING_PER_USER",
			defaultPerUser,
		),
		1, 1000, defaultPerUser,
	)
	return policy
}

func (m *Manager) policyWithPool(
	ctx context.Context,
	pool *modeladmission.ResourcePool,
) queuePolicy {
	policy := m.basePolicy(ctx)
	if pool == nil {
		return policy
	}
	if pool.ChatMaxConcurrent != nil {
		policy.MaxConcurrent = boundedInt(
			int64(*pool.ChatMaxConcurrent), 1, 4096, policy.MaxConcurrent,
		)
	}
	if pool.ChatMaxWaiting != nil {
		policy.MaxWaiting = boundedInt(
			int64(*pool.ChatMaxWaiting), 0, 100000, policy.MaxWaiting,
		)
	}
	return policy
}

func (m *Manager) livePolicy(ctx context.Context, ticket *Ticket) (queuePolicy, error) {
	pool := ticket.initialPool
	if m.admission != nil {
		reloaded, err := m.cachedResourcePool(ctx, ticket.poolID)
		switch {
		case err == nil:
			pool = reloaded
		case errors.Is(err, gorm.ErrRecordNotFound):
			// A deterministic fallback pool may not have been reconciled yet.
		default:
			return queuePolicy{}, err
		}
	}
	return m.policyWithPool(ctx, pool), nil
}

// cachedResourcePool bounds hot-policy reloads to one database read per pool
// and API replica per second, regardless of how many conversations are waiting.
func (m *Manager) cachedResourcePool(
	ctx context.Context,
	poolID string,
) (*modeladmission.ResourcePool, error) {
	now := time.Now()
	m.poolCacheMu.Lock()
	if cached, ok := m.poolCache[poolID]; ok && now.Before(cached.expiresAt) {
		pool := clonePool(cached.pool)
		err := cached.err
		m.poolCacheMu.Unlock()
		return pool, err
	}
	// Keep the lock through the read to collapse concurrent cache misses from
	// hundreds of waiter goroutines into one control-plane query.
	pool, err := m.admission.GetResourcePool(ctx, poolID)
	m.poolCache[poolID] = cachedPool{
		pool:      clonePool(pool),
		err:       err,
		expiresAt: now.Add(time.Second),
	}
	m.poolCacheMu.Unlock()
	return pool, err
}

func boundedInt(value int64, minimum, maximum, fallback int) int {
	if value < int64(minimum) || value > int64(maximum) {
		return fallback
	}
	return int(value)
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

type queueKeys struct {
	active   string
	waiting  string
	meta     string
	user     string
	sequence string
}

func (m *Manager) keys(ticket *Ticket) queueKeys {
	poolDigest := digest(ticket.poolID)
	return queueKeys{
		active:   m.keyPrefix + "pool:" + poolDigest + ":active",
		waiting:  m.keyPrefix + "pool:" + poolDigest + ":waiting",
		meta:     m.keyPrefix + "pool:" + poolDigest + ":wait-meta",
		user:     m.keyPrefix + "user:" + ticket.principalHash + ":waiting",
		sequence: m.keyPrefix + "sequence",
	}
}

type admissionResult struct {
	code        int64
	position    int64
	active      int64
	waiting     int64
	userWaiting int64
}

var admitScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local max_active = tonumber(ARGV[2])
local max_waiting = tonumber(ARGV[3])
local max_user = tonumber(ARGV[4])
local token = ARGV[5]
local active_expiry = tonumber(ARGV[6])
local wait_expiry = tonumber(ARGV[7])
local ttl = tonumber(ARGV[8])

redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
redis.call('ZREMRANGEBYSCORE', KEYS[4], '-inf', now)

for i = 1, 256 do
  local head = redis.call('ZRANGE', KEYS[2], 0, 0)
  if #head == 0 then break end
  local expiry = tonumber(redis.call('HGET', KEYS[3], head[1]) or '0')
  if expiry > now then break end
  redis.call('ZREM', KEYS[2], head[1])
  redis.call('HDEL', KEYS[3], head[1])
end

local active = redis.call('ZCARD', KEYS[1])
local waiting = redis.call('ZCARD', KEYS[2])
if waiting == 0 and active < max_active then
  redis.call('ZADD', KEYS[1], active_expiry, token)
  redis.call('PEXPIRE', KEYS[1], ttl)
  return {1, 0, active + 1, 0, redis.call('ZCARD', KEYS[4])}
end

local user_waiting = redis.call('ZCARD', KEYS[4])
if max_waiting <= 0 or waiting >= max_waiting then
  return {-1, 0, active, waiting, user_waiting}
end
if user_waiting >= max_user then
  return {-2, 0, active, waiting, user_waiting}
end

local sequence = redis.call('INCR', KEYS[5])
redis.call('ZADD', KEYS[2], sequence, token)
redis.call('HSET', KEYS[3], token, wait_expiry)
redis.call('ZADD', KEYS[4], wait_expiry, token)
redis.call('PEXPIRE', KEYS[2], ttl)
redis.call('PEXPIRE', KEYS[3], ttl)
redis.call('PEXPIRE', KEYS[4], ttl)
return {0, waiting + 1, active, waiting + 1, user_waiting + 1}
`)

var promoteScript = redis.NewScript(`
local token = ARGV[1]
local now = tonumber(ARGV[2])
local active_expiry = tonumber(ARGV[3])
local wait_expiry = tonumber(ARGV[4])
local max_active = tonumber(ARGV[5])
local ttl = tonumber(ARGV[6])

redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
if not redis.call('ZSCORE', KEYS[2], token) then
  return {-1, 0, redis.call('ZCARD', KEYS[1]), redis.call('ZCARD', KEYS[2])}
end
redis.call('HSET', KEYS[3], token, wait_expiry)
redis.call('ZADD', KEYS[4], wait_expiry, token)

for i = 1, 256 do
  local head = redis.call('ZRANGE', KEYS[2], 0, 0)
  if #head == 0 then break end
  local expiry = tonumber(redis.call('HGET', KEYS[3], head[1]) or '0')
  if expiry > now then break end
  redis.call('ZREM', KEYS[2], head[1])
  redis.call('HDEL', KEYS[3], head[1])
end

local rank = redis.call('ZRANK', KEYS[2], token)
if not rank then
  return {-1, 0, redis.call('ZCARD', KEYS[1]), redis.call('ZCARD', KEYS[2])}
end
local active = redis.call('ZCARD', KEYS[1])
if rank == 0 and active < max_active then
  redis.call('ZREM', KEYS[2], token)
  redis.call('HDEL', KEYS[3], token)
  redis.call('ZREM', KEYS[4], token)
  redis.call('ZADD', KEYS[1], active_expiry, token)
  redis.call('PEXPIRE', KEYS[1], ttl)
  return {1, 0, active + 1, redis.call('ZCARD', KEYS[2])}
end

redis.call('PEXPIRE', KEYS[2], ttl)
redis.call('PEXPIRE', KEYS[3], ttl)
redis.call('PEXPIRE', KEYS[4], ttl)
return {0, rank + 1, active, redis.call('ZCARD', KEYS[2])}
`)

var heartbeatScript = redis.NewScript(`
if not redis.call('ZSCORE', KEYS[1], ARGV[1]) then
  return 0
end
redis.call('ZADD', KEYS[1], tonumber(ARGV[2]), ARGV[1])
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[3]))
return 1
`)

var cancelScript = redis.NewScript(`
redis.call('ZREM', KEYS[1], ARGV[1])
redis.call('ZREM', KEYS[2], ARGV[1])
redis.call('HDEL', KEYS[3], ARGV[1])
redis.call('ZREM', KEYS[4], ARGV[1])
return 1
`)

func (m *Manager) admitRedis(
	ctx context.Context,
	ticket *Ticket,
	policy queuePolicy,
) (admissionResult, error) {
	now := time.Now().UnixMilli()
	keys := m.keys(ticket)
	values, err := admitScript.Run(
		ctx,
		m.redis,
		[]string{keys.active, keys.waiting, keys.meta, keys.user, keys.sequence},
		now,
		policy.MaxConcurrent,
		policy.MaxWaiting,
		policy.MaxPerUser,
		ticket.token,
		now+activeLeaseTTL.Milliseconds(),
		now+waiterLeaseTTL.Milliseconds(),
		(m.maxWait + activeLeaseTTL).Milliseconds(),
	).Slice()
	if err != nil {
		return admissionResult{}, err
	}
	return parseAdmissionResult(values, 5)
}

func parseAdmissionResult(values []interface{}, minimum int) (admissionResult, error) {
	if len(values) < minimum {
		return admissionResult{}, fmt.Errorf("unexpected chat queue response length %d", len(values))
	}
	numbers := make([]int64, len(values))
	for index, value := range values {
		number, err := redisInt(value)
		if err != nil {
			return admissionResult{}, err
		}
		numbers[index] = number
	}
	result := admissionResult{
		code:     numbers[0],
		position: numbers[1],
		active:   numbers[2],
		waiting:  numbers[3],
	}
	if len(numbers) > 4 {
		result.userWaiting = numbers[4]
	}
	return result, nil
}

func redisInt(value interface{}) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	case []byte:
		return strconv.ParseInt(string(typed), 10, 64)
	default:
		return 0, fmt.Errorf("unexpected Redis number type %T", value)
	}
}

func (m *Manager) promoteRedis(
	ctx context.Context,
	ticket *Ticket,
	policy queuePolicy,
) (admissionResult, error) {
	now := time.Now().UnixMilli()
	keys := m.keys(ticket)
	values, err := promoteScript.Run(
		ctx,
		m.redis,
		[]string{keys.active, keys.waiting, keys.meta, keys.user},
		ticket.token,
		now,
		now+activeLeaseTTL.Milliseconds(),
		now+waiterLeaseTTL.Milliseconds(),
		policy.MaxConcurrent,
		(m.maxWait + activeLeaseTTL).Milliseconds(),
	).Slice()
	if err != nil {
		return admissionResult{}, err
	}
	return parseAdmissionResult(values, 4)
}

func (m *Manager) admitLocal(
	ticket *Ticket,
	policy queuePolicy,
) (admissionResult, error) {
	now := time.Now().UnixMilli()
	m.localMu.Lock()
	defer m.localMu.Unlock()
	m.cleanupLocalUser(ticket.principalHash, now)
	pool := m.localPool(ticket.poolID)
	m.cleanupLocalPool(pool, now)
	active := int64(len(pool.active))
	waiting := int64(len(pool.waiting))
	userWaiting := int64(len(m.localUserWaiting[ticket.principalHash]))
	if waiting == 0 && int(active) < policy.MaxConcurrent {
		pool.active[ticket.token] = now + activeLeaseTTL.Milliseconds()
		return admissionResult{code: 1, active: active + 1}, nil
	}
	if policy.MaxWaiting <= 0 || int(waiting) >= policy.MaxWaiting {
		return admissionResult{
			code: -1, active: active, waiting: waiting, userWaiting: userWaiting,
		}, nil
	}
	if int(userWaiting) >= policy.MaxPerUser {
		return admissionResult{
			code: -2, active: active, waiting: waiting, userWaiting: userWaiting,
		}, nil
	}
	m.localSequence++
	_ = m.localSequence
	pool.waiting = append(pool.waiting, ticket.token)
	pool.waiters[ticket.token] = localWaiter{
		principal: ticket.principalHash,
		expiresAt: now + waiterLeaseTTL.Milliseconds(),
	}
	userTickets := m.localUserWaiting[ticket.principalHash]
	if userTickets == nil {
		userTickets = make(map[string]int64)
		m.localUserWaiting[ticket.principalHash] = userTickets
	}
	userTickets[ticket.token] = now + waiterLeaseTTL.Milliseconds()
	return admissionResult{
		code: 0, position: waiting + 1, active: active, waiting: waiting + 1,
		userWaiting: userWaiting + 1,
	}, nil
}

func (m *Manager) localPool(poolID string) *localPool {
	pool := m.localPools[poolID]
	if pool == nil {
		pool = &localPool{
			active:  make(map[string]int64),
			waiters: make(map[string]localWaiter),
		}
		m.localPools[poolID] = pool
	}
	return pool
}

func (m *Manager) cleanupLocalUser(principal string, now int64) {
	tickets := m.localUserWaiting[principal]
	for token, expiry := range tickets {
		if expiry <= now {
			delete(tickets, token)
		}
	}
	if len(tickets) == 0 {
		delete(m.localUserWaiting, principal)
	}
}

func (m *Manager) cleanupLocalPool(pool *localPool, now int64) {
	for token, expiry := range pool.active {
		if expiry <= now {
			delete(pool.active, token)
		}
	}
	next := pool.waiting[:0]
	for _, token := range pool.waiting {
		waiter, ok := pool.waiters[token]
		if !ok || waiter.expiresAt <= now {
			delete(pool.waiters, token)
			if tickets := m.localUserWaiting[waiter.principal]; tickets != nil {
				delete(tickets, token)
			}
			continue
		}
		next = append(next, token)
	}
	pool.waiting = next
}

func (m *Manager) promoteLocal(
	ticket *Ticket,
	policy queuePolicy,
) (admissionResult, error) {
	now := time.Now().UnixMilli()
	m.localMu.Lock()
	defer m.localMu.Unlock()
	pool := m.localPool(ticket.poolID)
	m.cleanupLocalPool(pool, now)
	waiter, ok := pool.waiters[ticket.token]
	if !ok {
		return admissionResult{code: -1}, errTicketMissing
	}
	waiter.expiresAt = now + waiterLeaseTTL.Milliseconds()
	pool.waiters[ticket.token] = waiter
	if tickets := m.localUserWaiting[ticket.principalHash]; tickets != nil {
		tickets[ticket.token] = waiter.expiresAt
	}
	position := int64(0)
	for index, token := range pool.waiting {
		if token == ticket.token {
			position = int64(index + 1)
			break
		}
	}
	if position == 0 {
		return admissionResult{code: -1}, errTicketMissing
	}
	active := int64(len(pool.active))
	if position == 1 && int(active) < policy.MaxConcurrent {
		pool.waiting = pool.waiting[1:]
		delete(pool.waiters, ticket.token)
		if tickets := m.localUserWaiting[ticket.principalHash]; tickets != nil {
			delete(tickets, ticket.token)
		}
		pool.active[ticket.token] = now + activeLeaseTTL.Milliseconds()
		return admissionResult{
			code: 1, active: active + 1, waiting: int64(len(pool.waiting)),
		}, nil
	}
	return admissionResult{
		code: 0, position: position, active: active, waiting: int64(len(pool.waiting)),
	}, nil
}

func mapState(admitted bool) string {
	if admitted {
		return "admitted"
	}
	return "waiting"
}

// Ticket implements the native queue ticket boundary.
type Ticket struct {
	manager       *Manager
	token         string
	principalHash string
	poolID        string
	modelID       string
	initialPool   *modeladmission.ResourcePool
	queuedAt      time.Time
	queued        bool
	local         bool
	lastSnapshot  sessionhandler.ChatQueueSnapshot

	admitted      atomic.Bool
	finished      atomic.Bool
	heartbeatOnce sync.Once
	finishOnce    sync.Once
	stopHeartbeat chan struct{}
}

func (t *Ticket) Queued() bool { return t != nil && t.queued }

func (t *Ticket) snapshot(
	state string,
	position, active, waiting int64,
	policy queuePolicy,
) sessionhandler.ChatQueueSnapshot {
	return sessionhandler.ChatQueueSnapshot{
		State:          state,
		ModelID:        t.modelID,
		ResourcePoolID: t.poolID,
		Position:       position,
		Waiting:        waiting,
		Active:         active,
		MaxConcurrent:  policy.MaxConcurrent,
		MaxWaiting:     policy.MaxWaiting,
		QueuedAtUnix:   t.queuedAt.Unix(),
	}
}

func (t *Ticket) Wait(
	ctx context.Context,
	onUpdate func(sessionhandler.ChatQueueSnapshot),
) error {
	if t == nil || !t.queued {
		return nil
	}
	if onUpdate != nil {
		onUpdate(t.lastSnapshot)
	}
	ticker := time.NewTicker(queuePollInterval)
	defer ticker.Stop()
	timeout := time.NewTimer(t.manager.maxWait)
	defer timeout.Stop()
	lastPosition := t.lastSnapshot.Position
	lastNotice := time.Now()

	for {
		select {
		case <-ctx.Done():
			t.Cancel(context.WithoutCancel(ctx))
			return ctx.Err()
		case <-timeout.C:
			t.Cancel(context.WithoutCancel(ctx))
			return errWaitExpired
		case <-ticker.C:
			policy, err := t.manager.livePolicy(ctx, t)
			if err != nil {
				t.Cancel(context.WithoutCancel(ctx))
				return err
			}
			if !policy.Enabled {
				t.Cancel(context.WithoutCancel(ctx))
				t.admitted.Store(true)
				if onUpdate != nil {
					onUpdate(t.snapshot("admitted", 0, 0, 0, policy))
				}
				return nil
			}
			var result admissionResult
			if t.local {
				result, err = t.manager.promoteLocal(t, policy)
			} else {
				result, err = t.manager.promoteRedis(ctx, t, policy)
			}
			if err != nil {
				t.Cancel(context.WithoutCancel(ctx))
				return err
			}
			if result.code < 0 {
				t.Cancel(context.WithoutCancel(ctx))
				return errTicketMissing
			}
			if result.code == 1 {
				t.admitted.Store(true)
				t.startHeartbeat()
				if onUpdate != nil {
					onUpdate(t.snapshot(
						"admitted", 0, result.active, result.waiting, policy,
					))
				}
				return nil
			}
			if onUpdate != nil &&
				(result.position != lastPosition || time.Since(lastNotice) >= 10*time.Second) {
				onUpdate(t.snapshot(
					"waiting", result.position, result.active, result.waiting, policy,
				))
				lastPosition = result.position
				lastNotice = time.Now()
			}
		}
	}
}

func (t *Ticket) startHeartbeat() {
	if t == nil || !t.admitted.Load() {
		return
	}
	t.heartbeatOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(activeHeartbeat)
			defer ticker.Stop()
			for {
				select {
				case <-t.stopHeartbeat:
					return
				case <-ticker.C:
					t.renewActive()
				}
			}
		}()
	})
}

func (t *Ticket) renewActive() {
	if t == nil || t.finished.Load() {
		return
	}
	expiry := time.Now().Add(activeLeaseTTL).UnixMilli()
	if t.local {
		t.manager.localMu.Lock()
		if pool := t.manager.localPools[t.poolID]; pool != nil {
			if _, ok := pool.active[t.token]; ok {
				pool.active[t.token] = expiry
			}
		}
		t.manager.localMu.Unlock()
		return
	}
	keys := t.manager.keys(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = heartbeatScript.Run(
		ctx,
		t.manager.redis,
		[]string{keys.active},
		t.token,
		expiry,
		(t.manager.maxWait + activeLeaseTTL).Milliseconds(),
	).Result()
}

func (t *Ticket) Release(ctx context.Context) {
	t.finish(ctx)
}

func (t *Ticket) Cancel(ctx context.Context) {
	t.finish(ctx)
}

func (t *Ticket) finish(ctx context.Context) {
	if t == nil || t.manager == nil {
		return
	}
	t.finishOnce.Do(func() {
		t.finished.Store(true)
		close(t.stopHeartbeat)
		if t.local {
			t.manager.localMu.Lock()
			if pool := t.manager.localPools[t.poolID]; pool != nil {
				delete(pool.active, t.token)
				delete(pool.waiters, t.token)
				next := pool.waiting[:0]
				for _, token := range pool.waiting {
					if token != t.token {
						next = append(next, token)
					}
				}
				pool.waiting = next
			}
			if tickets := t.manager.localUserWaiting[t.principalHash]; tickets != nil {
				delete(tickets, t.token)
			}
			t.manager.localMu.Unlock()
			return
		}
		keys := t.manager.keys(t)
		releaseCtx := ctx
		if releaseCtx == nil || releaseCtx.Err() != nil {
			var cancel context.CancelFunc
			releaseCtx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
		}
		_, _ = cancelScript.Run(
			releaseCtx,
			t.manager.redis,
			[]string{keys.active, keys.waiting, keys.meta, keys.user},
			t.token,
		).Result()
	})
}
