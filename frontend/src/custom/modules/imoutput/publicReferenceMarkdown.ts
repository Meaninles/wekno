import {
  createChatMarkdownRenderer,
  renderChatMarkdown,
} from "@/utils/chatMarkdownRenderer";
import {
  createSafeImage,
  isValidImageURL,
  safeMarkdownToHTML,
  sanitizeMarkdownHTML,
} from "@/utils/security";

const renderer = createChatMarkdownRenderer({
  imageRenderer: ({ href, title, text }) => createSafeImage(href, text || "", title || ""),
  invalidImageHtml: () => "",
  isValidImageUrl: isValidImageURL,
});

export function renderPublicReferenceMarkdown(content?: string): string {
  if (!content) return "";
  return renderChatMarkdown(content, {
    renderer,
    escapeMarkdown: safeMarkdownToHTML,
    sanitizeHtml: sanitizeMarkdownHTML,
    streaming: false,
    knowledgeReferences: [],
  });
}
