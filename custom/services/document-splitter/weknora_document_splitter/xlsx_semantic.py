from __future__ import annotations

import posixpath
import re
import zipfile
from dataclasses import dataclass
from pathlib import Path
from xml.etree import ElementTree


_CELL_REFERENCE = re.compile(r"^\$?([A-Za-z]{1,3})\$?([1-9][0-9]*)$")


@dataclass(frozen=True)
class XLSXSheetBounds:
    name: str
    worksheet_path: str
    raw_rows: int
    raw_cells: int
    raw_max_row: int
    raw_max_column: int
    semantic_rows: int
    semantic_cells: int
    semantic_max_row: int
    semantic_max_column: int
    style_only_cells: int
    formula_cells: int


def inspect_xlsx_semantic_ranges(source: Path) -> list[XLSXSheetBounds]:
    """Return physical and logical worksheet bounds directly from OOXML.

    Spreadsheet applications often persist a style-only ``<c>`` at XFD even
    though the cell has no value. Those records are real archive workload but
    not table data. A semantic cell has a value, formula, or inline string;
    empty cells that only carry style metadata do not widen the split range.
    """

    with zipfile.ZipFile(source) as archive:
        workbook = ElementTree.fromstring(archive.read("xl/workbook.xml"))
        relationships = ElementTree.fromstring(
            archive.read("xl/_rels/workbook.xml.rels")
        )
        targets = {
            relation.attrib.get("Id", ""): _normalise_worksheet_target(
                relation.attrib.get("Target", "")
            )
            for relation in relationships
            if _local_name(relation.tag) == "Relationship"
        }

        result: list[XLSXSheetBounds] = []
        for sheet in workbook.iter():
            if _local_name(sheet.tag) != "sheet":
                continue
            relationship_id = next(
                (
                    value
                    for key, value in sheet.attrib.items()
                    if _local_name(key) == "id"
                ),
                "",
            )
            worksheet_path = targets.get(relationship_id, "")
            if not worksheet_path or worksheet_path not in archive.namelist():
                raise ValueError(
                    f"worksheet relationship {relationship_id!r} has no target"
                )
            with archive.open(worksheet_path) as stream:
                result.append(
                    _inspect_worksheet(
                        stream,
                        name=sheet.attrib.get("name", "Sheet"),
                        worksheet_path=worksheet_path,
                    )
                )
        return result


def _inspect_worksheet(stream, *, name: str, worksheet_path: str) -> XLSXSheetBounds:
    raw_rows = 0
    raw_cells = 0
    raw_max_row = 0
    raw_max_column = 0
    semantic_rows = 0
    semantic_cells = 0
    semantic_max_row = 0
    semantic_max_column = 0
    current_row = 0
    last_semantic_row = -1
    formula_cells = 0

    for event, element in ElementTree.iterparse(stream, events=("start", "end")):
        local = _local_name(element.tag)
        if event == "start" and local == "row":
            raw_rows += 1
            current_row = _positive_int(element.attrib.get("r"), raw_rows)
            raw_max_row = max(raw_max_row, current_row)
            continue
        if event != "end" or local != "c":
            if event == "end" and local not in {"v", "f", "is", "t"}:
                element.clear()
            continue

        raw_cells += 1
        column, row = _cell_coordinate(element.attrib.get("r", ""))
        row = row or current_row or raw_rows
        raw_max_row = max(raw_max_row, row)
        raw_max_column = max(raw_max_column, column)
        child_names = {_local_name(child.tag) for child in element}
        semantic = bool(child_names.intersection({"v", "f", "is"}))
        if "f" in child_names:
            formula_cells += 1
        if semantic:
            semantic_cells += 1
            semantic_max_row = max(semantic_max_row, row)
            semantic_max_column = max(semantic_max_column, column)
            if row != last_semantic_row:
                semantic_rows += 1
                last_semantic_row = row
        element.clear()

    return XLSXSheetBounds(
        name=name,
        worksheet_path=worksheet_path,
        raw_rows=raw_rows,
        raw_cells=raw_cells,
        raw_max_row=raw_max_row,
        raw_max_column=raw_max_column,
        semantic_rows=semantic_rows,
        semantic_cells=semantic_cells,
        semantic_max_row=semantic_max_row,
        semantic_max_column=semantic_max_column,
        style_only_cells=max(0, raw_cells - semantic_cells),
        formula_cells=formula_cells,
    )


def _normalise_worksheet_target(target: str) -> str:
    target = target.replace("\\", "/")
    if target.startswith("/"):
        return posixpath.normpath(target.lstrip("/"))
    if target.startswith("xl/"):
        return posixpath.normpath(target)
    return posixpath.normpath(posixpath.join("xl", target))


def _cell_coordinate(reference: str) -> tuple[int, int]:
    match = _CELL_REFERENCE.match(reference)
    if not match:
        return 0, 0
    column = 0
    for char in match.group(1).upper():
        column = column * 26 + ord(char) - ord("A") + 1
    return column, int(match.group(2))


def _positive_int(value: str | None, fallback: int) -> int:
    try:
        parsed = int(value or "")
    except ValueError:
        return fallback
    return parsed if parsed > 0 else fallback


def _local_name(tag: str) -> str:
    return tag.rsplit("}", 1)[-1]
