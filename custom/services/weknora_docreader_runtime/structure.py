"""Preserve source table semantics when projecting DOCX into plain Markdown."""
from copy import deepcopy
from io import BytesIO


def expand_merged_docx_cells(content: bytes) -> bytes:
    """Expand the *declared* grid merges in a parsing copy of the document.

    Mammoth/Markdown cannot represent rowspan, so empty continuation cells
    otherwise lose their owning subject. python-docx resolves grid cells to
    their OOXML merge origin. We repeat that exact origin, never infer values
    from an empty cell. Uploaded bytes and their layout remain untouched.
    """
    from docx import Document
    from docx.oxml.ns import qn
    from docx.table import Table

    document = Document(BytesIO(content))
    changed = False
    # Work inside-out so nested tables are normalized before an outer merge
    # duplicates their containing cell. Omitted grid cells stay omitted: row
    # gridBefore/gridAfter are retained and no blank value is inferred.
    for element in reversed(document._element.xpath('.//w:tbl')):
        table = Table(element, document._body)
        if not table._tbl.xpath('./w:tr/w:tc/w:tcPr/w:vMerge | ./w:tr/w:tc/w:tcPr/w:gridSpan'):
            continue
        rows = []
        for row in table.rows:
            # Materialize all anchors before editing the XML tree: later
            # vertical continuation lookup still needs the original rows.
            cells = []
            for cell in row.cells:
                tc = deepcopy(cell._tc)
                for marker in tc.xpath('./w:tcPr/w:vMerge | ./w:tcPr/w:gridSpan'):
                    marker.getparent().remove(marker)
                cells.append(tc)
            rows.append((row._tr, cells))
        for tr, cells in rows:
            for tc in list(tr.findall(qn('w:tc'))):
                tr.remove(tc)
            for tc in cells:
                tr.append(tc)
        changed = True
    if not changed:
        return content
    output = BytesIO()
    document.save(output)
    return output.getvalue()
