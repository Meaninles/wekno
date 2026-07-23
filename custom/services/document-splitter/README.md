# WeKnora production document splitter

This module is loaded by the bundled DocReader service through the small gRPC
registration point in `docreader/main.py`. It accepts a streamed source file
and produces a streamed ZIP archive containing:

- `manifest.json` — immutable source identity, planner version, format-aware
  source locators and per-part hashes;
- `parts/part-000001.<ext>` — independently parseable physical parts.

The implementation keeps source and output bytes on disk, validates every
generated part, preserves source ordering, and never exposes temporary paths.
All limits are conservative targets below the parser hard limits. Unsafe or
atomic-unsplittable inputs return a structured error instead of silently
degrading content.

## Format strategies

| Logical source | Physical strategy | Parser ceiling per part |
| --- | --- | ---: |
| PDF | balanced page ranges | 50 MB |
| DOC / DOCX | layout-preserving LibreOffice page rendering, then PDF page ranges | 5 / 10 MB source rule; 50 MB PDF part |
| PPT / PPTX | layout-preserving slide rendering, then PDF page ranges | 50 MB |
| XLS / XLSX | worksheet + row + column windows; continuation headers, key columns and merged values are materialized | 3 / 10 MB |
| CSV | record + column windows; continuation headers and key columns are repeated | 20 MB |
| TXT / TEXT | UTF-8/GB18030 semantic line windows with long-line coordinates | 3 MB |
| MD / MARKDOWN | heading-aware line windows with breadcrumb continuation context | 3 MB |
| JSON | independently valid top-level/path record documents | 1 MB |
| EPUB | ordered spine-item packages with referenced assets only | 5 MB |
| MHTML | ordered DOM units; referenced resources are emitted once and oversized raster resources are losslessly tiled | 5 MB |
| JPG / JPEG / PNG / WEBP / GIF / BMP / TIFF | frame-aware, overlapping lossless PNG tiles with source coordinates | 2–5 MB |
| MP3 / M4A / OGG / FLAC / WAV | overlapping timestamp windows; exact transcript overlap is removed during logical publication | 5–100 MB |

The backend keeps these files physically independent while publishing one
generation-scoped logical chunk sequence. Source locators are retained through
retrieval and citations. Summary, Wiki, table description, generated
questions and graph extraction operate over deterministic strata spanning the
whole logical document, with smaller table-specific enrichment bounds to avoid
turning repetitive rows into noisy duplicated output.
