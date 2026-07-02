import { applyStreamingTailFade } from './chatMarkdownRenderer.ts'

const cases = [
  '<p>Great, that narrows it down a lot. Two more quick</p>',
  '<p>Great, that narrows it down a lot. <strong>Two</strong> more quick</p>\n',
  '<ol>\n<li>第一项</li>\n<li>正在生成的第二项内容</li>\n</ol>',
  '<p>结尾是引用 <span class="citation">doc</span></p>',
  '<p></p>',
  '',
]
for (const c of cases) {
  process.stdout.write(`IN : ${JSON.stringify(c)}\n`)
  process.stdout.write(`OUT: ${JSON.stringify(applyStreamingTailFade(c))}\n\n`)
}
