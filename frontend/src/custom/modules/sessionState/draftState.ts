import type { SessionLastRequestStatePayload } from "@/stores/settings";
import type { AttachmentFile } from "@/components/AttachmentUpload.vue";

export interface SessionDraftState {
  settings: SessionLastRequestStatePayload;
  attachments: AttachmentFile[];
  images: File[];
}

const sessionDrafts = new Map<string, SessionDraftState>();

function normalizeSessionId(sessionId: unknown): string {
  return String(sessionId || "").trim();
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
): void {
  const id = normalizeSessionId(sessionId);
  if (!id) return;
  sessionDrafts.set(id, {
    settings: cloneSettings(settings),
    attachments: cloneAttachments(attachments),
    images: cloneImages(images),
  });
}

export function getSessionDraftState(sessionId: unknown): SessionDraftState | null {
  const id = normalizeSessionId(sessionId);
  if (!id) return null;
  const draft = sessionDrafts.get(id);
  if (!draft) return null;
  return {
    settings: cloneSettings(draft.settings),
    attachments: cloneAttachments(draft.attachments),
    images: cloneImages(draft.images),
  };
}

export function clearSessionDraftState(sessionId: unknown): void {
  const id = normalizeSessionId(sessionId);
  if (!id) return;
  sessionDrafts.delete(id);
}
