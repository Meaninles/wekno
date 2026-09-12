import { get, put } from '@/utils/request';
import type { SourceReference } from '@/utils/sourceReferences';

export type AnswerFeedbackValue = 'solved' | 'off_topic' | 'inaccurate' | 'unsolved' | '';
type ApiResponse<T> = { success: boolean; data: T };

export type FeedbackAnalyticsResourceOption = {
  key: string;
  id: string;
  name: string;
  tenant_id: number;
  is_builtin?: boolean;
  is_deleted?: boolean;
};

export type FeedbackAnalyticsResource = {
  id: string;
  name: string;
  tenant_id?: number;
  is_builtin?: boolean;
  is_deleted?: boolean;
};

export type FeedbackAnalyticsItem = {
  id: string;
  tenant_id: number;
  tenant_name?: string;
  user_id?: string;
  user_name?: string;
  session_id: string;
  session_title?: string;
  request_id?: string;
  user_message_id?: string;
  assistant_message_id: string;
  feedback: Exclude<AnswerFeedbackValue, ''>;
  channel?: string;
  question?: string;
  answer?: string;
  knowledge_references?: SourceReference[];
  agent?: FeedbackAnalyticsResource | null;
  knowledge_bases?: FeedbackAnalyticsResource[];
  created_at: string;
  updated_at: string;
  answer_created_at?: string;
  has_snapshot: boolean;
};

export type FeedbackAnalyticsSummary = {
  total: number;
  solved: number;
  off_topic: number;
  inaccurate: number;
  unsolved: number;
};

export type FeedbackAnalyticsPage = {
  items: FeedbackAnalyticsItem[];
  total: number;
  page: number;
  page_size: number;
  summary: FeedbackAnalyticsSummary;
};

export type FeedbackAnalyticsGroup = {
  key: string;
  id: string;
  tenant_id?: number;
  name: string;
  feedback_count: number;
  solved: number;
  off_topic: number;
  inaccurate: number;
  unsolved: number;
};

export type FeedbackAnalyticsConversationMessage = {
  id: string;
  request_id?: string;
  role: 'user' | 'assistant' | 'system' | string;
  content: string;
  knowledge_references?: SourceReference[];
  error_code?: string;
  is_completed: boolean;
  channel?: string;
  created_at: string;
};

export type FeedbackAnalyticsDetail = {
  feedback: FeedbackAnalyticsItem;
  conversation: {
    items: FeedbackAnalyticsConversationMessage[];
    total: number;
    page: number;
    page_size: number;
  };
};

export type FeedbackAnalyticsQuery = {
  page?: number;
  page_size?: number;
  user_id?: string;
  user_q?: string;
  agent_keys?: string[];
  agent_q?: string;
  knowledge_base_keys?: string[];
  knowledge_base_q?: string;
  feedback?: Exclude<AnswerFeedbackValue, ''>;
  channel?: string;
};

export function setAnswerFeedback(sessionId: string, messageId: string, feedback: AnswerFeedbackValue) {
  return put<ApiResponse<{ message_id: string; feedback: AnswerFeedbackValue }>>(`/api/v1/custom/answer-feedback/messages/${encodeURIComponent(sessionId)}/${encodeURIComponent(messageId)}`, {
    feedback: feedback || 'none',
  });
}

export function listAnswerFeedback(sessionId: string, messageIds: string[]) {
  const params = new URLSearchParams();
  if (sessionId) params.set('session_id', sessionId);
  params.set('message_ids', messageIds.join(','));
  return get<ApiResponse<Record<string, AnswerFeedbackValue>>>(`/api/v1/custom/answer-feedback/messages?${params.toString()}`);
}

function analyticsQueryParams(query: FeedbackAnalyticsQuery = {}) {
  const params = new URLSearchParams();
  if (query.page) params.set('page', String(query.page));
  if (query.page_size) params.set('page_size', String(query.page_size));
  if (query.user_id) params.set('user_id', query.user_id);
  if (query.user_q) params.set('user_q', query.user_q);
  if (query.agent_keys?.length) params.set('agent_keys', query.agent_keys.join(','));
  if (query.agent_q) params.set('agent_q', query.agent_q);
  if (query.knowledge_base_keys?.length) params.set('knowledge_base_keys', query.knowledge_base_keys.join(','));
  if (query.knowledge_base_q) params.set('knowledge_base_q', query.knowledge_base_q);
  if (query.feedback) params.set('feedback', query.feedback);
  if (query.channel) params.set('channel', query.channel);
  return params;
}

export function listFeedbackAnalytics(query: FeedbackAnalyticsQuery = {}) {
  const params = analyticsQueryParams(query);
  const suffix = params.toString() ? '?' + params.toString() : '';
  return get<ApiResponse<FeedbackAnalyticsPage>>('/api/v1/custom/answer-feedback/analytics' + suffix);
}

export function listFeedbackAnalyticsGroups(dimension: 'agent' | 'knowledge_base', query: FeedbackAnalyticsQuery = {}) {
  const params = analyticsQueryParams(query);
  params.set('dimension', dimension);
  return get<ApiResponse<FeedbackAnalyticsGroup[]>>('/api/v1/custom/answer-feedback/analytics/groups?' + params.toString());
}

export function listFeedbackAnalyticsOptions(type: 'agents' | 'knowledge_bases', query = '') {
  const params = new URLSearchParams({ type, limit: '100' });
  if (query) params.set('q', query);
  return get<ApiResponse<FeedbackAnalyticsResourceOption[]>>('/api/v1/custom/answer-feedback/analytics/options?' + params.toString());
}

export function getFeedbackAnalyticsDetail(id: string, page = 1, pageSize = 50, includeConversation = false) {
  const params = new URLSearchParams({
    page: String(page),
    page_size: String(pageSize),
    include_conversation: String(includeConversation),
  });
  return get<ApiResponse<FeedbackAnalyticsDetail>>('/api/v1/custom/answer-feedback/analytics/' + encodeURIComponent(id) + '?' + params.toString());
}
