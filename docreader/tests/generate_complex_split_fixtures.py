"""Generate deterministic, structurally rich fixtures for splitter smoke tests.

The binary outputs intentionally live outside the repository.  They exercise
semantic boundaries and real parser workload limits instead of merely padding
otherwise empty files.
"""

from __future__ import annotations

import argparse
import csv
import io
import json
import math
import wave
from email import policy as email_policy
from email.message import EmailMessage
from pathlib import Path

from ebooklib import epub
from PIL import Image, ImageChops, ImageDraw, ImageEnhance


ASSET_FIELDS = [
    "asset_id",
    "department",
    "system",
    "domain",
    "table_name",
    "field_name",
    "data_type",
    "classification",
    "owner",
    "status",
    "update_cycle",
    "quality_score",
    "row_count",
    "storage_gb",
]


def _structured_record(index: int) -> dict[str, object]:
    return {
        "asset_id": f"ASSET-{index:08d}",
        "department": f"部门-{index % 37:02d}",
        "system": f"核心系统-{index % 19:02d}",
        "domain": ("财务", "客户", "供应链", "风控", "人力", "审计")[index % 6],
        "table_name": f"ods_business_event_{index % 173:03d}",
        "field_name": f"metric_value_{index % 97:02d}",
        "data_type": ("DECIMAL(20,4)", "VARCHAR(256)", "TIMESTAMP", "BIGINT")[
            index % 4
        ],
        "classification": ("公开", "内部", "敏感", "核心")[index % 4],
        "owner": f"数据责任人-{index % 53:02d}",
        "status": ("草稿", "已发布", "待复核", "已归档")[index % 4],
        "update_cycle": ("实时", "小时", "日", "月")[index % 4],
        "quality_score": round(78.0 + (index % 221) / 10, 1),
        "row_count": 100_000 + index * 137,
        "storage_gb": round(0.25 + (index % 901) / 20, 2),
        "description": (
            f"第 {index} 项数据资产记录，包含业务口径、数据血缘、质量规则、"
            "权限分级、生命周期和责任人信息；用于跨物理分片的全局检索与摘要验证。"
        ),
        "lineage": {
            "upstream": [
                f"src_{(index + offset) % 211:03d}" for offset in range(3)
            ],
            "downstream": [
                f"mart_{(index + offset) % 89:03d}" for offset in range(2)
            ],
        },
        "quality_rules": [
            {"name": "非空校验", "threshold": 0.995},
            {"name": "唯一性校验", "threshold": 0.999},
            {"name": "时效性校验", "threshold_minutes": 60 + index % 120},
        ],
        "tags": [f"标签-{(index + offset) % 41:02d}" for offset in range(5)],
    }


def generate_text(output: Path) -> Path:
    path = output / "complex-structured-large.txt"
    with path.open("w", encoding="utf-8", newline="\n") as stream:
        stream.write("企业数据资产治理实施手册\n版本：2026.07\n")
        for chapter in range(1, 601):
            stream.write(f"\n第 {chapter} 章 主题域与治理规则\n")
            stream.write(
                f"章节摘要：主题域 {chapter} 负责口径一致性、血缘追踪、质量监控和权限审计。\n"
            )
            for section in range(1, 36):
                marker = f"TXT-C{chapter:03d}-S{section:02d}"
                stream.write(
                    f"{chapter}.{section} {marker}："
                    "本条规则覆盖采集、加工、服务、归档四个阶段；"
                    "责任人必须记录审批依据、异常处置和复核结论。"
                    f"边界校验码={chapter * 10_000 + section}。\n"
                )
        stream.write("\n文档终点标记：TXT-DOCUMENT-END-9F71\n")
    return path


def generate_markdown(output: Path) -> Path:
    path = output / "complex-structured-large.md"
    with path.open("w", encoding="utf-8", newline="\n") as stream:
        stream.write(
            "# 企业数据资产目录\n\n"
            "> 本文档用于验证拆分后标题面包屑、表格、代码块、链接和跨边界上下文。\n\n"
            "## 全局术语\n\n"
            "| 术语 | 定义 | 责任方 |\n|---|---|---|\n"
            "| 数据资产 | 可治理、可复用、可审计的数据集合 | 数据管理委员会 |\n"
            "| 质量规则 | 可自动验证的数据约束 | 数据责任人 |\n\n"
        )
        for domain in range(1, 361):
            stream.write(f"# 主题域 {domain:03d}\n\n")
            stream.write(
                f"主题域锚点：`MD-DOMAIN-{domain:03d}`。"
                "本章关联 [全局术语](#全局术语)，并保留脚注[^audit]。\n\n"
            )
            for section in range(1, 21):
                stream.write(f"## {domain}.{section} 数据对象与口径\n\n")
                stream.write(
                    f"- 唯一标识：`OBJ-{domain:03d}-{section:02d}`\n"
                    f"- 业务口径：第 {domain} 主题域第 {section} 类指标，"
                    "按自然日、组织和币种聚合。\n"
                    "- 质量约束：完整率 ≥ 99.5%，唯一率 ≥ 99.9%，延迟 ≤ 60 分钟。\n\n"
                    "| 字段 | 类型 | 分级 | 说明 |\n"
                    "|---|---:|---|---|\n"
                    f"| object_id | string | 内部 | OBJ-{domain:03d}-{section:02d} |\n"
                    f"| amount | decimal(20,4) | 敏感 | 金额口径 {domain * section} |\n"
                    "| event_time | timestamp | 内部 | 业务事件时间 |\n\n"
                    "```sql\n"
                    f"select organization_id, sum(amount) as metric_{domain}_{section}\n"
                    f"from dwd_domain_{domain:03d}_event\n"
                    "where event_date = :biz_date group by organization_id;\n"
                    "```\n\n"
                )
        stream.write(
            "[^audit]: 所有发布操作必须记录申请人、审批人、时间戳和变更摘要。\n\n"
            "<!-- MD-DOCUMENT-END-4A2C -->\n"
        )
    return path


def generate_json(output: Path) -> Path:
    path = output / "complex-nested-large.json"
    payload = {
        "schema_version": "2026-07",
        "catalog": {
            "name": "企业级数据资产目录",
            "owners": [
                {"department": f"部门-{index:02d}", "contact": f"owner{index}@example.cn"}
                for index in range(37)
            ],
            "governance": {
                "classification_levels": ["公开", "内部", "敏感", "核心"],
                "required_controls": {
                    "核心": ["双人审批", "字段脱敏", "访问审计", "跨境检查"],
                    "敏感": ["责任人审批", "最小权限", "访问审计"],
                },
            },
        },
        "records": [_structured_record(index) for index in range(36_000)],
        "terminal_marker": "JSON-DOCUMENT-END-8D03",
    }
    path.write_text(
        json.dumps(payload, ensure_ascii=False, separators=(",", ":")),
        encoding="utf-8",
    )
    return path


def generate_csv(output: Path) -> Path:
    path = output / "complex-tabular-large.csv"
    with path.open("w", encoding="utf-8", newline="") as stream:
        writer = csv.writer(stream)
        writer.writerow(ASSET_FIELDS + ["description", "boundary_marker"])
        for index in range(160_000):
            record = _structured_record(index)
            writer.writerow(
                [record[field] for field in ASSET_FIELDS]
                + [
                    record["description"],
                    f"CSV-ROW-{index:08d}",
                ]
            )
    return path


def _complex_image(size: tuple[int, int]) -> Image.Image:
    width, height = size
    horizontal = Image.linear_gradient("L").resize(size)
    vertical = Image.linear_gradient("L").rotate(90, expand=True).resize(size)
    noise = Image.effect_noise(size, 92)
    red = ImageChops.add(horizontal, noise, scale=2.0)
    green = ImageChops.add(vertical, noise, scale=2.0)
    blue = ImageChops.difference(horizontal, vertical)
    image = Image.merge("RGB", (red, green, blue))
    image = ImageEnhance.Contrast(image).enhance(1.3)
    draw = ImageDraw.Draw(image)
    for row in range(0, height, max(80, height // 24)):
        draw.line((0, row, width, row), fill=(245, 245, 245), width=3)
    for column in range(0, width, max(100, width // 28)):
        draw.line((column, 0, column, height), fill=(20, 20, 20), width=2)
    for index in range(48):
        left = (index * 193) % max(1, width - 360)
        top = (index * 137) % max(1, height - 120)
        draw.rectangle(
            (left, top, left + 340, top + 92),
            outline=(255, 210, 40),
            width=5,
        )
        draw.text(
            (left + 14, top + 20),
            f"ASSET-{index:03d} / Q{index % 4 + 1}",
            fill=(255, 255, 255),
        )
    return image


def generate_images(output: Path) -> dict[str, Path]:
    image = _complex_image((4200, 3000))
    paths = {
        "png": output / "complex-high-resolution.png",
        "jpg": output / "complex-high-resolution.jpg",
        "jpeg": output / "complex-high-resolution.jpeg",
        "webp": output / "complex-high-resolution.webp",
        "bmp": output / "complex-high-resolution.bmp",
        "tiff": output / "complex-high-resolution.tiff",
    }
    image.save(paths["png"], format="PNG", compress_level=1)
    image.save(paths["jpg"], format="JPEG", quality=98, subsampling=0)
    image.save(paths["jpeg"], format="JPEG", quality=96, subsampling=0)
    image.save(paths["webp"], format="WEBP", lossless=True, quality=100)
    image.save(paths["bmp"], format="BMP")
    image.save(paths["tiff"], format="TIFF", compression="raw")

    frames = []
    for index in range(12):
        frame = _complex_image((1400, 1000))
        frame = ImageEnhance.Color(frame).enhance(0.65 + index * 0.08)
        draw = ImageDraw.Draw(frame)
        draw.text(
            (50, 45),
            f"FRAME {index + 1:02d}/12  EVENT-{index * 125:04d}",
            fill=(255, 255, 0),
        )
        frames.append(frame.quantize(colors=256))
    gif_path = output / "complex-multiframe.gif"
    frames[0].save(
        gif_path,
        format="GIF",
        save_all=True,
        append_images=frames[1:],
        duration=[140 + index * 10 for index in range(len(frames))],
        loop=0,
        optimize=False,
    )
    paths["gif"] = gif_path
    return paths


def _image_bytes(image: Image.Image, *, size: tuple[int, int]) -> bytes:
    resized = image.resize(size)
    buffer = io.BytesIO()
    resized.save(buffer, format="PNG", compress_level=1)
    return buffer.getvalue()


def generate_epub(output: Path, image: Image.Image) -> Path:
    path = output / "complex-linked-large.epub"
    book = epub.EpubBook()
    book.set_identifier("weknora-complex-split-fixture-2026")
    book.set_title("企业数据治理操作手册")
    book.set_language("zh-CN")
    book.add_author("WeKnora Split Quality")

    resource_data = []
    for index in range(6):
        crop = image.crop(
            (
                index * 350,
                index * 210,
                index * 350 + 1800,
                index * 210 + 1300,
            )
        )
        resource_data.append(_image_bytes(crop, size=(1600, 1100)))

    resources = []
    for index, data in enumerate(resource_data):
        resource = epub.EpubImage(
            uid=f"diagram-{index}",
            file_name=f"images/diagram-{index}.png",
            media_type="image/png",
        )
        resource.set_content(data)
        book.add_item(resource)
        resources.append(resource)

    chapters = []
    for chapter_index in range(1, 91):
        resource_index = (chapter_index - 1) % len(resources)
        chapter = epub.EpubHtml(
            uid=f"chapter-{chapter_index:03d}",
            file_name=f"chapters/chapter-{chapter_index:03d}.xhtml",
            title=f"第 {chapter_index} 章",
            lang="zh-CN",
        )
        rows = "".join(
            f"<tr><td>EPUB-{chapter_index:03d}-{row:03d}</td>"
            f"<td>数据对象 {row}</td><td>{78 + row % 22}%</td>"
            f"<td>责任人 {row % 17}</td></tr>"
            for row in range(1, 61)
        )
        paragraphs = "".join(
            f"<p>规则 {chapter_index}.{section}：采集、加工、服务与归档阶段"
            "均需记录业务口径、质量阈值、审批依据和血缘关系；"
            f"跨章节校验码 EPUB-C{chapter_index:03d}-S{section:02d}。</p>"
            for section in range(1, 46)
        )
        next_href = (
            f"chapter-{chapter_index + 1:03d}.xhtml"
            if chapter_index < 90
            else "chapter-001.xhtml"
        )
        chapter.set_content(
            (
                "<html xmlns='http://www.w3.org/1999/xhtml'><body>"
                f"<h1>第 {chapter_index} 章：数据主题域</h1>"
                f"<img alt='主题域图示' src='../images/diagram-{resource_index}.png'/>"
                f"{paragraphs}<table><thead><tr><th>标识</th><th>对象</th>"
                f"<th>质量</th><th>责任人</th></tr></thead><tbody>{rows}</tbody></table>"
                f"<p><a href='{next_href}'>下一章</a></p></body></html>"
            ).encode("utf-8")
        )
        book.add_item(chapter)
        chapters.append(chapter)

    book.toc = tuple(chapters)
    book.spine = ["nav", *chapters]
    book.add_item(epub.EpubNcx())
    book.add_item(epub.EpubNav())
    epub.write_epub(str(path), book, {"compresslevel": 1})
    return path


def generate_mhtml(output: Path, image: Image.Image) -> Path:
    path = output / "complex-linked-large.mhtml"
    message = EmailMessage()
    message["Subject"] = "企业数据资产目录网页归档"
    message["MIME-Version"] = "1.0"
    message.make_related()

    sections = []
    for chapter in range(1, 81):
        rows = "".join(
            f"<tr><td>MHTML-{chapter:03d}-{row:03d}</td>"
            f"<td>资产对象 {chapter * 1000 + row}</td>"
            f"<td>{('核心', '敏感', '内部')[row % 3]}</td></tr>"
            for row in range(1, 71)
        )
        prose = "".join(
            f"<p>第 {chapter}.{section} 条：本条描述数据口径、质量规则、"
            f"访问审计和生命周期，边界码 WEB-{chapter:03d}-{section:02d}。</p>"
            for section in range(1, 51)
        )
        sections.append(
            f"<section id='chapter-{chapter:03d}'><h1>主题域 {chapter:03d}</h1>"
            f"<img src='cid:diagram-{(chapter - 1) % 5}' alt='图示'/>"
            f"{prose}<table><tbody>{rows}</tbody></table></section>"
        )

    body = EmailMessage()
    navigation = "".join(
        f"<a href='#chapter-{index:03d}'>{index}</a>" for index in range(1, 81)
    )
    body.set_content(
        "<html><head><meta charset='utf-8'/><title>数据资产目录</title></head>"
        f"<body><nav>{navigation}</nav>"
        f"{''.join(sections)}<footer>MHTML-DOCUMENT-END-7B22</footer></body></html>",
        subtype="html",
        charset="utf-8",
    )
    message.attach(body)

    for index in range(5):
        crop = image.crop(
            (index * 420, index * 250, index * 420 + 1900, index * 250 + 1400)
        )
        data = _image_bytes(crop, size=(1750, 1250))
        part = EmailMessage()
        part.set_content(data, maintype="image", subtype="png")
        part["Content-ID"] = f"<diagram-{index}>"
        part["Content-Location"] = f"images/diagram-{index}.png"
        message.attach(part)

    path.write_bytes(message.as_bytes(policy=email_policy.default))
    return path


def generate_wav(output: Path) -> Path:
    path = output / "complex-timeline-large.wav"
    sample_rate = 48_000
    duration_seconds = 96
    frames_per_chunk = sample_rate
    with wave.open(str(path), "wb") as stream:
        stream.setnchannels(2)
        stream.setsampwidth(2)
        stream.setframerate(sample_rate)
        for second in range(duration_seconds):
            frequency_left = (220, 330, 440, 550)[(second // 8) % 4]
            frequency_right = (275, 385, 495, 660)[(second // 12) % 4]
            buffer = bytearray()
            for offset in range(frames_per_chunk):
                absolute = second * sample_rate + offset
                envelope = 0.55 + 0.35 * math.sin(
                    2 * math.pi * absolute / (sample_rate * 7)
                )
                marker = 0.35 if offset < 480 and second % 12 == 0 else 0.0
                left = int(
                    25_000
                    * (
                        envelope
                        * math.sin(2 * math.pi * frequency_left * absolute / sample_rate)
                        + marker
                    )
                    / 1.35
                )
                right = int(
                    25_000
                    * (
                        envelope
                        * math.sin(2 * math.pi * frequency_right * absolute / sample_rate)
                        - marker
                    )
                    / 1.35
                )
                buffer.extend(left.to_bytes(2, "little", signed=True))
                buffer.extend(right.to_bytes(2, "little", signed=True))
            stream.writeframesraw(buffer)
    return path


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)

    generated: list[Path] = [
        generate_text(output),
        generate_markdown(output),
        generate_json(output),
        generate_csv(output),
    ]
    images = generate_images(output)
    generated.extend(images.values())
    source_image = Image.open(images["png"]).convert("RGB")
    try:
        generated.append(generate_epub(output, source_image))
        generated.append(generate_mhtml(output, source_image))
    finally:
        source_image.close()
    generated.append(generate_wav(output))

    for path in sorted(generated):
        print(f"{path.name}\t{path.stat().st_size}")


if __name__ == "__main__":
    main()
