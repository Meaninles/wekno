import type { Token } from 'marked'

const CITATIONS_ONLY = /^(?:\s*@@WEKNORA_HTML_PLACEHOLDER_\d+@@)+\s*$/

/** Arrange citations only after Markdown has established its block boundaries.
 * Joining source lines before parsing can turn table citations into surplus
 * cells (which GFM discards), or change fences, headings and code blocks.
 */
export function attachCitationParagraphs(tokens: Token[]): void {
  let previous: Token | undefined
  for (let index = 0; index < tokens.length; index++) {
    const token = tokens[index]
    if (token.type === 'space') continue
    if (token.type === 'list') {
      for (const item of token.items) attachCitationParagraphs(item.tokens)
    } else if (token.type === 'blockquote' && token.tokens) {
      attachCitationParagraphs(token.tokens)
    }

    if (token.type === 'paragraph' && token.tokens && CITATIONS_ONLY.test(token.text)) {
      let target = previous
      if (target?.type === 'list') {
        target = target.items.at(-1)?.tokens.filter((child: Token) => child.type !== 'space').at(-1)
      }
      if ((target?.type === 'paragraph' || target?.type === 'text') && target.tokens) {
        target.tokens.push({ type: 'text', raw: ' ', text: ' ' }, ...token.tokens)
        target.text += ` ${token.text}`
        tokens.splice(index--, 1)
        continue
      }
    }
    previous = token
  }
}
