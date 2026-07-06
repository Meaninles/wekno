import {
  createChatMarkdownRenderer,
  renderChatMarkdown,
} from "@/utils/chatMarkdownRenderer";
import type { CitationKnowledgeRef } from "@/utils/citationMarkdown";
import {
  createSafeImage,
  isValidImageURL,
  safeMarkdownToHTML,
  sanitizeMarkdownHTML,
} from "@/utils/security";

type RenderMobileMarkdownOptions = {
  knowledgeReferences?: CitationKnowledgeRef[] | null;
  streaming?: boolean;
  citationNumberById?: Map<string, number>;
};

const renderer = createChatMarkdownRenderer({
  imageRenderer: ({ href, title, text }) => createSafeImage(href, text || "", title || ""),
  invalidImageHtml: () => "",
  isValidImageUrl: isValidImageURL,
});

function normalizeCitationNumbers(html: string, numberById?: Map<string, number>) {
  if (!html || !numberById?.size || typeof document === "undefined") return html;

  const template = document.createElement("template");
  template.innerHTML = html;
  template.content.querySelectorAll<HTMLElement>(".citation-source").forEach((node) => {
    const citationId = node.dataset.sourceId || "";
    const number = citationId ? numberById.get(citationId) : undefined;
    if (!number) return;
    node.dataset.citationNumber = String(number);
    node.setAttribute("aria-label", `引用 ${number}：${node.dataset.title || ""}`);
    const numberNode = node.querySelector<HTMLElement>(".citation-number");
    if (numberNode) numberNode.textContent = String(number);
  });
  return template.innerHTML;
}

export function renderMobileMarkdown(content?: string, options: RenderMobileMarkdownOptions = {}) {
  if (!content) return "";
  const html = renderChatMarkdown(content, {
    renderer,
    escapeMarkdown: safeMarkdownToHTML,
    sanitizeHtml: sanitizeMarkdownHTML,
    streaming: options.streaming,
    knowledgeReferences: options.knowledgeReferences,
  });
  return normalizeCitationNumbers(html, options.citationNumberById);
}
