"""Read-only structural evidence and page images for human artifact inspection.

Nothing here assigns a semantic pass/fail result or repairs the generated file.
"""
import json
from pathlib import Path
import re
import sys
import xml.etree.ElementTree as ET
from zipfile import ZipFile

import fitz
from PIL import Image, ImageDraw

ROOT = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).resolve().parents[3] / '.local-data/agent-audit-20260905'
OUT = ROOT / 'plan-validation'
RENDER = OUT / 'pdf-render'
RENDER.mkdir(exist_ok=True)
NS = {'w': 'http://schemas.openxmlformats.org/wordprocessingml/2006/main'}
with ZipFile(ROOT / 'remaining/safety-original.docx') as z:
    tree = ET.fromstring(z.read('word/document.xml'))
    paragraphs = [''.join(n.text or '' for n in p.findall('.//w:t', NS))
                  for p in tree.findall('.//w:p', NS)]
pdf = fitz.open(OUT / '安全生产教育培训管理规定.pdf')
pages = []
images = []
for number, page in enumerate(pdf, 1):
    content = page.get_text()
    pages.append({'page': number, 'size': list(page.rect), 'text': content,
                  'images': page.get_image_info(hashes=True, xrefs=True)})
    pix = page.get_pixmap(matrix=fitz.Matrix(1.4, 1.4), alpha=False)
    path = RENDER / f'page-{number:02}.png'
    pix.save(path)
    image = Image.open(path).convert('RGB')
    image.thumbnail((550, 780))
    images.append(image)
for offset in range(0, len(images), 4):
    sheet = Image.new('RGB', (1140, 1650), '#dde1e5')
    draw = ImageDraw.Draw(sheet)
    for j, im in enumerate(images[offset:offset + 4]):
        x, y = 10 + j % 2 * 570, 30 + j // 2 * 820
        draw.text((x, y - 20), f'PDF page {offset + j + 1}', fill='black')
        sheet.paste(im, (x, y))
    sheet.save(RENDER / f'sheet-{offset // 4 + 1:02}.png')

def normalize(value):
    return re.sub(r'\s+', '', value)

combined = normalize(''.join(p['text'] for p in pages))
missing = [p for p in paragraphs if p.strip() and normalize(p) not in combined]
summary = {'original_paragraphs': paragraphs, 'pdf_pages': pages,
           'paragraphs_not_found_by_whitespace_normalized_text': missing,
           'note': 'Text mismatch is diagnostic only; visual/manual review decides meaning.'}
(OUT / 'artifact-qa.json').write_text(json.dumps(summary, ensure_ascii=False, indent=2,
                                               default=lambda value: value.hex() if isinstance(value, bytes) else str(value)), encoding='utf-8')
print(json.dumps({'pages': len(pdf), 'source_paragraphs': len(paragraphs),
                  'text_mismatches': missing, 'page_sizes': sorted(set((p.rect.width, p.rect.height) for p in pdf))}, ensure_ascii=False))
