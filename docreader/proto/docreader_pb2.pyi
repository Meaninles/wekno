from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ReadConfig(_message.Message):
    __slots__ = ("parser_engine", "parser_engine_overrides")
    class ParserEngineOverridesEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    PARSER_ENGINE_FIELD_NUMBER: _ClassVar[int]
    PARSER_ENGINE_OVERRIDES_FIELD_NUMBER: _ClassVar[int]
    parser_engine: str
    parser_engine_overrides: _containers.ScalarMap[str, str]
    def __init__(self, parser_engine: _Optional[str] = ..., parser_engine_overrides: _Optional[_Mapping[str, str]] = ...) -> None: ...

class ReadRequest(_message.Message):
    __slots__ = ("file_content", "file_name", "file_type", "url", "title", "config", "request_id")
    FILE_CONTENT_FIELD_NUMBER: _ClassVar[int]
    FILE_NAME_FIELD_NUMBER: _ClassVar[int]
    FILE_TYPE_FIELD_NUMBER: _ClassVar[int]
    URL_FIELD_NUMBER: _ClassVar[int]
    TITLE_FIELD_NUMBER: _ClassVar[int]
    CONFIG_FIELD_NUMBER: _ClassVar[int]
    REQUEST_ID_FIELD_NUMBER: _ClassVar[int]
    file_content: bytes
    file_name: str
    file_type: str
    url: str
    title: str
    config: ReadConfig
    request_id: str
    def __init__(self, file_content: _Optional[bytes] = ..., file_name: _Optional[str] = ..., file_type: _Optional[str] = ..., url: _Optional[str] = ..., title: _Optional[str] = ..., config: _Optional[_Union[ReadConfig, _Mapping]] = ..., request_id: _Optional[str] = ...) -> None: ...

class ImageRef(_message.Message):
    __slots__ = ("filename", "original_ref", "mime_type", "storage_key", "image_data")
    FILENAME_FIELD_NUMBER: _ClassVar[int]
    ORIGINAL_REF_FIELD_NUMBER: _ClassVar[int]
    MIME_TYPE_FIELD_NUMBER: _ClassVar[int]
    STORAGE_KEY_FIELD_NUMBER: _ClassVar[int]
    IMAGE_DATA_FIELD_NUMBER: _ClassVar[int]
    filename: str
    original_ref: str
    mime_type: str
    storage_key: str
    image_data: bytes
    def __init__(self, filename: _Optional[str] = ..., original_ref: _Optional[str] = ..., mime_type: _Optional[str] = ..., storage_key: _Optional[str] = ..., image_data: _Optional[bytes] = ...) -> None: ...

class ReadResponse(_message.Message):
    __slots__ = ("markdown_content", "image_refs", "image_dir_path", "metadata", "error")
    class MetadataEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    MARKDOWN_CONTENT_FIELD_NUMBER: _ClassVar[int]
    IMAGE_REFS_FIELD_NUMBER: _ClassVar[int]
    IMAGE_DIR_PATH_FIELD_NUMBER: _ClassVar[int]
    METADATA_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    markdown_content: str
    image_refs: _containers.RepeatedCompositeFieldContainer[ImageRef]
    image_dir_path: str
    metadata: _containers.ScalarMap[str, str]
    error: str
    def __init__(self, markdown_content: _Optional[str] = ..., image_refs: _Optional[_Iterable[_Union[ImageRef, _Mapping]]] = ..., image_dir_path: _Optional[str] = ..., metadata: _Optional[_Mapping[str, str]] = ..., error: _Optional[str] = ...) -> None: ...

class ReadStreamMeta(_message.Message):
    __slots__ = ("markdown_content", "image_dir_path", "metadata", "error", "image_count")
    class MetadataEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    MARKDOWN_CONTENT_FIELD_NUMBER: _ClassVar[int]
    IMAGE_DIR_PATH_FIELD_NUMBER: _ClassVar[int]
    METADATA_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    IMAGE_COUNT_FIELD_NUMBER: _ClassVar[int]
    markdown_content: str
    image_dir_path: str
    metadata: _containers.ScalarMap[str, str]
    error: str
    image_count: int
    def __init__(self, markdown_content: _Optional[str] = ..., image_dir_path: _Optional[str] = ..., metadata: _Optional[_Mapping[str, str]] = ..., error: _Optional[str] = ..., image_count: _Optional[int] = ...) -> None: ...

class ReadStreamResponse(_message.Message):
    __slots__ = ("meta", "image")
    META_FIELD_NUMBER: _ClassVar[int]
    IMAGE_FIELD_NUMBER: _ClassVar[int]
    meta: ReadStreamMeta
    image: ImageRef
    def __init__(self, meta: _Optional[_Union[ReadStreamMeta, _Mapping]] = ..., image: _Optional[_Union[ImageRef, _Mapping]] = ...) -> None: ...

class SplitHeader(_message.Message):
    __slots__ = ("file_name", "file_type", "source_size", "source_sha256", "minimum_parts", "target_ratio", "request_id")
    FILE_NAME_FIELD_NUMBER: _ClassVar[int]
    FILE_TYPE_FIELD_NUMBER: _ClassVar[int]
    SOURCE_SIZE_FIELD_NUMBER: _ClassVar[int]
    SOURCE_SHA256_FIELD_NUMBER: _ClassVar[int]
    MINIMUM_PARTS_FIELD_NUMBER: _ClassVar[int]
    TARGET_RATIO_FIELD_NUMBER: _ClassVar[int]
    REQUEST_ID_FIELD_NUMBER: _ClassVar[int]
    file_name: str
    file_type: str
    source_size: int
    source_sha256: str
    minimum_parts: int
    target_ratio: float
    request_id: str
    def __init__(self, file_name: _Optional[str] = ..., file_type: _Optional[str] = ..., source_size: _Optional[int] = ..., source_sha256: _Optional[str] = ..., minimum_parts: _Optional[int] = ..., target_ratio: _Optional[float] = ..., request_id: _Optional[str] = ...) -> None: ...

class SplitRequest(_message.Message):
    __slots__ = ("header", "data")
    HEADER_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    header: SplitHeader
    data: bytes
    def __init__(self, header: _Optional[_Union[SplitHeader, _Mapping]] = ..., data: _Optional[bytes] = ...) -> None: ...

class SplitArchiveHeader(_message.Message):
    __slots__ = ("archive_size", "archive_sha256", "part_count", "planner_version")
    ARCHIVE_SIZE_FIELD_NUMBER: _ClassVar[int]
    ARCHIVE_SHA256_FIELD_NUMBER: _ClassVar[int]
    PART_COUNT_FIELD_NUMBER: _ClassVar[int]
    PLANNER_VERSION_FIELD_NUMBER: _ClassVar[int]
    archive_size: int
    archive_sha256: str
    part_count: int
    planner_version: str
    def __init__(self, archive_size: _Optional[int] = ..., archive_sha256: _Optional[str] = ..., part_count: _Optional[int] = ..., planner_version: _Optional[str] = ...) -> None: ...

class SplitError(_message.Message):
    __slots__ = ("code", "message", "retryable")
    CODE_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    RETRYABLE_FIELD_NUMBER: _ClassVar[int]
    code: str
    message: str
    retryable: bool
    def __init__(self, code: _Optional[str] = ..., message: _Optional[str] = ..., retryable: bool = ...) -> None: ...

class SplitResponse(_message.Message):
    __slots__ = ("header", "data", "error")
    HEADER_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    header: SplitArchiveHeader
    data: bytes
    error: SplitError
    def __init__(self, header: _Optional[_Union[SplitArchiveHeader, _Mapping]] = ..., data: _Optional[bytes] = ..., error: _Optional[_Union[SplitError, _Mapping]] = ...) -> None: ...

class ListEnginesRequest(_message.Message):
    __slots__ = ("config_overrides",)
    class ConfigOverridesEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    CONFIG_OVERRIDES_FIELD_NUMBER: _ClassVar[int]
    config_overrides: _containers.ScalarMap[str, str]
    def __init__(self, config_overrides: _Optional[_Mapping[str, str]] = ...) -> None: ...

class ParserEngineInfo(_message.Message):
    __slots__ = ("name", "description", "file_types", "available", "unavailable_reason")
    NAME_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTION_FIELD_NUMBER: _ClassVar[int]
    FILE_TYPES_FIELD_NUMBER: _ClassVar[int]
    AVAILABLE_FIELD_NUMBER: _ClassVar[int]
    UNAVAILABLE_REASON_FIELD_NUMBER: _ClassVar[int]
    name: str
    description: str
    file_types: _containers.RepeatedScalarFieldContainer[str]
    available: bool
    unavailable_reason: str
    def __init__(self, name: _Optional[str] = ..., description: _Optional[str] = ..., file_types: _Optional[_Iterable[str]] = ..., available: bool = ..., unavailable_reason: _Optional[str] = ...) -> None: ...

class ListEnginesResponse(_message.Message):
    __slots__ = ("engines",)
    ENGINES_FIELD_NUMBER: _ClassVar[int]
    engines: _containers.RepeatedCompositeFieldContainer[ParserEngineInfo]
    def __init__(self, engines: _Optional[_Iterable[_Union[ParserEngineInfo, _Mapping]]] = ...) -> None: ...
