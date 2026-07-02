export type DocumentPreviewType =
  | 'pdf'
  | 'docx'
  | 'image'
  | 'excel'
  | 'text'
  | 'markdown'
  | 'pptx'
  | 'audio'
  | 'unsupported';

const fileTypeMap: Record<string, DocumentPreviewType> = {};

['pdf'].forEach((t) => { fileTypeMap[t] = 'pdf'; });
['docx'].forEach((t) => { fileTypeMap[t] = 'docx'; });
['pptx', 'ppt'].forEach((t) => { fileTypeMap[t] = 'pptx'; });
['jpg', 'jpeg', 'png', 'gif', 'bmp', 'webp', 'tiff', 'svg'].forEach((t) => { fileTypeMap[t] = 'image'; });
['xlsx', 'xls', 'csv'].forEach((t) => { fileTypeMap[t] = 'excel'; });
['md', 'markdown'].forEach((t) => { fileTypeMap[t] = 'markdown'; });
[
  'txt', 'json', 'xml', 'html', 'css', 'js', 'ts', 'py', 'java', 'go',
  'cpp', 'c', 'h', 'sh', 'yaml', 'yml', 'ini', 'conf', 'log', 'sql', 'rs',
  'rb', 'php', 'swift', 'kt', 'scala', 'r', 'lua', 'pl', 'toml',
].forEach((t) => { fileTypeMap[t] = 'text'; });
['mp3', 'wav', 'm4a', 'flac', 'ogg'].forEach((t) => { fileTypeMap[t] = 'audio'; });

const mimeTypeMap: Record<string, string> = {
  pdf: 'application/pdf',
  docx: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  doc: 'application/msword',
  pptx: 'application/vnd.openxmlformats-officedocument.presentationml.presentation',
  ppt: 'application/vnd.ms-powerpoint',
  xlsx: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  xls: 'application/vnd.ms-excel',
  csv: 'text/csv',
  jpg: 'image/jpeg',
  jpeg: 'image/jpeg',
  png: 'image/png',
  gif: 'image/gif',
  bmp: 'image/bmp',
  webp: 'image/webp',
  tiff: 'image/tiff',
  svg: 'image/svg+xml',
  txt: 'text/plain',
  md: 'text/markdown',
  markdown: 'text/markdown',
  json: 'application/json',
  xml: 'application/xml',
  html: 'text/html',
  css: 'text/css',
  js: 'text/javascript',
  ts: 'text/typescript',
  py: 'text/x-python',
  java: 'text/x-java',
  go: 'text/x-go',
  mp3: 'audio/mpeg',
  wav: 'audio/wav',
  m4a: 'audio/mp4',
  flac: 'audio/flac',
  ogg: 'audio/ogg',
};

export function normalizePreviewFileType(value?: string): string {
  return String(value || '').replace(/^\./, '').toLowerCase();
}

export function resolveDocumentPreviewType(fileType?: string): DocumentPreviewType {
  return fileTypeMap[normalizePreviewFileType(fileType)] || 'unsupported';
}

export function isDocumentPreviewSupported(fileType?: string): boolean {
  return resolveDocumentPreviewType(fileType) !== 'unsupported';
}

export function getDocumentPreviewMimeType(fileType?: string): string {
  return mimeTypeMap[normalizePreviewFileType(fileType)] || 'application/octet-stream';
}

export function ensureDocumentPreviewBlobType(blob: Blob, fileType?: string): Blob {
  const expected = getDocumentPreviewMimeType(fileType);
  if (blob.type === expected) return blob;
  return new Blob([blob], { type: expected });
}
