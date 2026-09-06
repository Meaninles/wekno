import type { SessionLastRequestStatePayload } from "@/stores/settings";
import type { AttachmentFile } from "@/components/AttachmentUpload.vue";
import { MessagePlugin } from 'tdesign-vue-next';
import { draftKey, fileId, flushDraft, readDraft, writeDraft } from './storage';

export interface SessionDraftState {
  query: string;
  settings: SessionLastRequestStatePayload;
  attachments: AttachmentFile[];
  images: File[];
}

const sessionDrafts = new Map<string, SessionDraftState>();

type StoredDraft = Omit<SessionDraftState, 'attachments' | 'images'> & {
  attachments: Array<Omit<AttachmentFile, 'file' | 'preview'> & { fileId: string }>;
  images: string[];
};
let storageErrorShown = false;
function reportStorageError(error: unknown) {
  if (!storageErrorShown) {
    storageErrorShown = true;
    void MessagePlugin.error('无法保存对话草稿，请检查浏览器可用存储空间后重试');
  }
  console.error('[conversation-draft] storage failed', error);
}

function cloneSettings(settings: SessionLastRequestStatePayload): SessionLastRequestStatePayload {
  return JSON.parse(JSON.stringify(settings || {})) as SessionLastRequestStatePayload;
}

function cloneAttachments(attachments: AttachmentFile[] = []): AttachmentFile[] {
  return attachments
    .filter((attachment): attachment is AttachmentFile => !!attachment?.file)
    .map((attachment) => ({ ...attachment }));
}

function cloneImages(images: File[] = []): File[] {
  return images.filter((file): file is File => file instanceof File);
}

export function saveSessionDraftState(
  sessionId: unknown,
  settings: SessionLastRequestStatePayload,
  attachments: AttachmentFile[] = [],
  images: File[] = [],
  query?: string,
  scope?: string,
): void {
  const id = draftKey(sessionId, scope);
  if (!id) return;
  sessionDrafts.set(id, {
    query: query ?? sessionDrafts.get(id)?.query ?? "",
    settings: cloneSettings(settings),
    attachments: cloneAttachments(attachments),
    images: cloneImages(images),
  });
  const draft = sessionDrafts.get(id)!;
  const stored: StoredDraft = {
    query: draft.query, settings: draft.settings,
    attachments: draft.attachments.map(({ file, preview: _preview, ...metadata }) => ({ ...metadata, fileId: fileId(file) })),
    images: draft.images.map(fileId),
  };
  void writeDraft(id, stored, [...draft.attachments.map(a => a.file), ...draft.images]).then(() => { storageErrorShown = false; }).catch(reportStorageError);
}

export async function getSessionDraftState(sessionId: unknown, scope?: string): Promise<SessionDraftState | null> {
  const id = draftKey(sessionId, scope);
  if (!id) return null;
  if (!sessionDrafts.has(id)) {
    const stored = await readDraft<StoredDraft>(id).catch(error => { reportStorageError(error); return null; });
    if (stored && !sessionDrafts.has(id)) {
      const attachments = stored.value.attachments.map(({ fileId: id, ...metadata }) => {
        const file = stored.files.get(id);
        if (!file) throw new Error('附件草稿不完整，请重新选择文件');
        return { ...metadata, file };
      });
      const images = stored.value.images.map(id => {
        const file = stored.files.get(id);
        if (!file) throw new Error('图片草稿不完整，请重新选择文件');
        return file;
      });
      sessionDrafts.set(id, { ...stored.value, attachments, images });
    }
  }
  const draft = sessionDrafts.get(id);
  if (!draft) return null;
  return {
    query: draft.query,
    settings: cloneSettings(draft.settings),
    attachments: cloneAttachments(draft.attachments),
    images: cloneImages(draft.images),
  };
}

export function clearSessionDraftState(sessionId: unknown, scope?: string): void {
  const id = draftKey(sessionId, scope);
  if (!id) return;
  sessionDrafts.delete(id);
  void writeDraft(id, null, []).catch(reportStorageError);
}

export async function flushSessionDraftState(sessionId: unknown, scope?: string): Promise<void> {
  await flushDraft(draftKey(sessionId, scope));
}
