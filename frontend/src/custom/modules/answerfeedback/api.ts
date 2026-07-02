import { get, put } from '@/utils/request';

export type AnswerFeedbackValue = 'like' | 'dislike' | '';
type ApiResponse<T> = { success: boolean; data: T };

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
