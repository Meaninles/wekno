"""Deterministic delivery validation, without an additional model call."""
import io
import mimetypes
from pathlib import PurePosixPath
from zipfile import ZipFile
from xml.etree import ElementTree


OFFICE = {
    ".docx": ("word/document.xml", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"),
    ".xlsx": ("xl/workbook.xml", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"),
    ".pptx": ("ppt/presentation.xml", "application/vnd.openxmlformats-officedocument.presentationml.presentation"),
}


def validate_artifact(name, content):
    if not content:
        raise ValueError("Cannot publish an empty file")
    suffix = PurePosixPath(name).suffix.lower()
    if suffix in OFFICE:
        required, media = OFFICE[suffix]
        with ZipFile(io.BytesIO(content)) as archive:
            if sum(i.file_size for i in archive.infolist()) > 512*1024**2:
                raise ValueError("Office package expanded size exceeds limit")
            for path in ("[Content_Types].xml", "_rels/.rels", required):
                ElementTree.fromstring(archive.read(path))
            if archive.testzip() is not None:
                raise ValueError("Office package checksum is invalid")
        return media
    if suffix == ".pdf":
        if not content.startswith(b"%PDF-") or b"%%EOF" not in content[-4096:]:
            raise ValueError("PDF is incomplete")
    return mimetypes.guess_type(name)[0] or "application/octet-stream"
