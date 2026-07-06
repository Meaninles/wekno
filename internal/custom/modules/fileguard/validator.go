package fileguard

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/xml"
	"fmt"
	"io"
	"mime/multipart"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

const mb = 1024 * 1024

const maxXMLScanBytes int64 = 256 * mb

type sizeLimit struct {
	mb    int64
	label string
}

type byteLimit struct {
	bytes int64
	label string
}

var fileSizeLimits = map[string]sizeLimit{
	"docx":     {10, "Word 文档"},
	"xlsx":     {10, "Excel 文件"},
	"pdf":      {50, "PDF 文件"},
	"doc":      {5, "Word 旧格式文件"},
	"xls":      {3, "Excel 旧格式文件"},
	"pptx":     {50, "PPT 文件"},
	"ppt":      {50, "PPT 旧格式文件"},
	"csv":      {20, "CSV 文件"},
	"txt":      {3, "文本文件"},
	"text":     {3, "文本文件"},
	"md":       {3, "Markdown 文件"},
	"markdown": {3, "Markdown 文件"},
	"json":     {1, "JSON 文件"},
	"epub":     {5, "EPUB 文件"},
	"mhtml":    {5, "MHTML 网页归档文件"},
	"jpg":      {5, "图片文件"},
	"jpeg":     {5, "图片文件"},
	"png":      {5, "图片文件"},
	"webp":     {5, "图片文件"},
	"gif":      {3, "GIF 图片"},
	"bmp":      {2, "BMP/TIFF 图片"},
	"tiff":     {2, "BMP/TIFF 图片"},
	"mp3":      {100, "音频文件"},
	"m4a":      {100, "音频文件"},
	"ogg":      {100, "音频文件"},
	"flac":     {100, "音频文件"},
	"wav":      {5, "WAV 音频文件"},
}

var heavySizeLimits = map[string]byteLimit{
	"docx":     {5 * mb, "Word 文档"},
	"xlsx":     {5 * mb / 2, "Excel 文件"},
	"pdf":      {10 * mb, "PDF 文件"},
	"doc":      {5 * mb / 2, "Word 旧格式文件"},
	"xls":      {3 * mb / 2, "Excel 旧格式文件"},
	"pptx":     {10 * mb, "PPT 文件"},
	"ppt":      {8 * mb, "PPT 旧格式文件"},
	"csv":      {10 * mb, "CSV 文件"},
	"txt":      {3 * mb / 2, "文本文件"},
	"text":     {3 * mb / 2, "文本文件"},
	"md":       {3 * mb / 2, "Markdown 文件"},
	"markdown": {3 * mb / 2, "Markdown 文件"},
	"json":     {512 * 1024, "JSON 文件"},
	"epub":     {5 * mb / 2, "EPUB 文件"},
	"mhtml":    {5 * mb / 2, "MHTML 网页归档文件"},
	"jpg":      {5 * mb / 2, "图片文件"},
	"jpeg":     {5 * mb / 2, "图片文件"},
	"png":      {5 * mb / 2, "图片文件"},
	"webp":     {5 * mb / 2, "图片文件"},
	"gif":      {3 * mb / 2, "GIF 图片"},
	"bmp":      {mb, "BMP/TIFF 图片"},
	"tiff":     {mb, "BMP/TIFF 图片"},
	"mp3":      {50 * mb, "音频文件"},
	"m4a":      {50 * mb, "音频文件"},
	"ogg":      {50 * mb, "音频文件"},
	"flac":     {50 * mb, "音频文件"},
	"wav":      {5 * mb / 2, "WAV 音频文件"},
}

type Weight string

const (
	WeightLight Weight = "light"
	WeightHeavy Weight = "heavy"
)

type Report struct {
	Issues       []string
	Weight       Weight
	HeavyReasons []string
	Metrics      map[string]any
}

func (r Report) ValidationError() error {
	return newValidationError(r.Issues)
}

func (r Report) IsHeavy() bool {
	return r.Weight == WeightHeavy
}

func newReport() Report {
	return Report{
		Weight:  WeightLight,
		Metrics: map[string]any{},
	}
}

func (r *Report) addIssue(format string, args ...any) {
	r.Issues = append(r.Issues, fmt.Sprintf(format, args...))
}

func (r *Report) addIssueText(message string) {
	r.Issues = append(r.Issues, message)
}

func (r *Report) markHeavy(format string, args ...any) {
	r.Weight = WeightHeavy
	reason := fmt.Sprintf(format, args...)
	for _, existing := range r.HeavyReasons {
		if existing == reason {
			return
		}
	}
	r.HeavyReasons = append(r.HeavyReasons, reason)
}

func (r *Report) metric(name string, value any) {
	if r.Metrics == nil {
		r.Metrics = map[string]any{}
	}
	r.Metrics[name] = value
}

type docxLimits struct {
	entries         int
	uncompressedMB  int64
	documentXMLMB   int64
	compressionRate float64
	paragraphs      int64
	textNodes       int64
	tables          int64
	tableRows       int64
	tableCells      int64
	pageBreakHints  int64
	mediaFiles      int64
	mediaMB         int64
}

var defaultDOCXLimits = docxLimits{
	entries:         500,
	uncompressedMB:  80,
	documentXMLMB:   30,
	compressionRate: 30,
	paragraphs:      100000,
	textNodes:       150000,
	tables:          1000,
	tableRows:       20000,
	tableCells:      80000,
	pageBreakHints:  800,
	mediaFiles:      100,
	mediaMB:         50,
}

type xlsxLimits struct {
	entries             int
	uncompressedMB      int64
	worksheetXMLMB      int64
	worksheetXMLTotalMB int64
	sharedStringsMB     int64
	sharedStringItems   int64
	sheetRows           int64
	totalRows           int64
	sheetColumns        int64
	totalCells          int64
	formulaCells        int64
	conditionalFormats  int64
	mergeRanges         int64
	mergedCoveredCells  int64
	drawingObjects      int64
	mediaMB             int64
	cellStyles          int64
}

var defaultXLSXLimits = xlsxLimits{
	entries:             300,
	uncompressedMB:      80,
	worksheetXMLMB:      15,
	worksheetXMLTotalMB: 40,
	sharedStringsMB:     8,
	sharedStringItems:   150000,
	sheetRows:           30000,
	totalRows:           50000,
	sheetColumns:        100,
	totalCells:          300000,
	formulaCells:        30000,
	conditionalFormats:  500,
	mergeRanges:         5000,
	mergedCoveredCells:  100000,
	drawingObjects:      100,
	mediaMB:             50,
	cellStyles:          1000,
}

type pptxLimits struct {
	entries         int
	uncompressedMB  int64
	slideXMLMB      int64
	slideXMLTotalMB int64
	compressionRate float64
	slides          int64
	slideShapes     int64
	mediaFiles      int64
	mediaMB         int64
}

var defaultPPTXLimits = pptxLimits{
	entries:         2000,
	uncompressedMB:  200,
	slideXMLMB:      8,
	slideXMLTotalMB: 80,
	compressionRate: 10,
	slides:          200,
	slideShapes:     100000,
	mediaFiles:      1000,
	mediaMB:         450,
}

type csvLimits struct {
	scanMB     int64
	rows       int64
	columns    int64
	cells      int64
	rowBytes   int64
	fieldBytes int64
}

var defaultCSVLimits = csvLimits{
	scanMB:     80,
	rows:       100000,
	columns:    100,
	cells:      300000,
	rowBytes:   mb,
	fieldBytes: 512 * 1024,
}

// ValidateMultipartFile checks upload limits before the document enters the
// parser. It intentionally avoids Office parser libraries and only inspects
// zip metadata plus selected XML streams.
func ValidateMultipartFile(file *multipart.FileHeader, displayName string) error {
	return AnalyzeMultipartFile(file, displayName).ValidationError()
}

func AnalyzeMultipartFile(file *multipart.FileHeader, displayName string) Report {
	if file == nil {
		return newReport()
	}
	name := displayName
	if strings.TrimSpace(name) == "" {
		name = file.Filename
	}
	return analyze(name, "", file.Size, func() (readerAtSize, error) {
		f, err := file.Open()
		if err != nil {
			return nil, err
		}
		return multipartReaderAt{File: f, size: file.Size}, nil
	})
}

// ValidateBytes checks downloaded file-url content with the same rules.
func ValidateBytes(fileName, fileType string, data []byte) error {
	return AnalyzeBytes(fileName, fileType, data).ValidationError()
}

func AnalyzeBytes(fileName, fileType string, data []byte) Report {
	name := analysisName(fileName, fileType)
	reader := bytes.NewReader(data)
	return analyze(name, fileType, int64(len(data)), func() (readerAtSize, error) {
		return byteReaderAt{Reader: reader, size: int64(len(data))}, nil
	})
}

func AnalyzeSize(fileName, fileType string, size int64) Report {
	return analyzeSizeOnly(analysisName(fileName, fileType), fileType, size)
}

func NeedsContentAnalysis(fileName, fileType string) bool {
	switch fileTypeFromName(analysisName(fileName, fileType)) {
	case "docx", "xlsx", "csv":
		return true
	default:
		return false
	}
}

type readerAtSize interface {
	io.ReaderAt
	io.Closer
	Size() int64
}

type multipartReaderAt struct {
	multipart.File
	size int64
}

func (r multipartReaderAt) Size() int64 { return r.size }

type byteReaderAt struct {
	*bytes.Reader
	size int64
}

func (r byteReaderAt) Size() int64  { return r.size }
func (r byteReaderAt) Close() error { return nil }

func analyze(fileName string, fileTypeHint string, size int64, open func() (readerAtSize, error)) Report {
	report := analyzeSizeOnly(fileName, fileTypeHint, size)
	ext := fileTypeFromName(fileName)

	switch ext {
	case "docx", "xlsx", "pptx":
		ra, err := open()
		if err != nil {
			report.addIssueText("文件结构异常或已损坏，无法解析")
			return report
		}
		defer ra.Close()

		zr, err := zip.NewReader(ra, ra.Size())
		if err != nil {
			report.addIssueText("文件结构异常或已损坏，无法解析")
			return report
		}
		if zipEncrypted(zr) {
			report.addIssueText("文件已加密或受密码保护，暂不支持上传解析")
			return report
		}
		if ext == "docx" {
			validateDOCX(zr, size, &report)
		} else if ext == "xlsx" {
			validateXLSX(zr, &report)
		} else {
			validatePPTX(zr, size, &report)
		}
	case "csv":
		ra, err := open()
		if err != nil {
			report.addIssueText("CSV 文件结构异常，无法解析，请检查引号、分隔符或换行是否正确")
			return report
		}
		defer ra.Close()
		validateCSV(ra, &report)
	}

	return report
}

func analyzeSizeOnly(fileName string, fileTypeHint string, size int64) Report {
	report := newReport()
	ext := fileTypeFromName(fileName)
	if ext == "" {
		ext = normalizeExt(fileTypeHint)
	}
	report.metric("file_size_bytes", size)

	base := filepath.Base(fileName)
	if strings.HasPrefix(base, "~$") {
		report.addIssueText("该文件是 Office 临时文件，不能上传，请关闭文档后选择正式文件上传")
	}

	if heavy, ok := heavySizeLimits[ext]; ok && size > heavy.bytes {
		report.markHeavy("%s大小超过重型阈值 %s（当前 %s）", heavy.label, formatByteSize(heavy.bytes), formatByteSize(size))
	}
	if limit, ok := fileSizeLimits[ext]; ok && size > limit.mb*mb {
		report.addIssue("%s大小不能超过 %dMB（当前 %s）", limit.label, limit.mb, formatMB(size))
	}
	return report
}

type validationError []string

func (e validationError) Error() string {
	return strings.Join(e, "，")
}

func newValidationError(issues []string) error {
	if len(issues) == 0 {
		return nil
	}
	return validationError(issues)
}

func zipEncrypted(zr *zip.Reader) bool {
	for _, f := range zr.File {
		if f.Flags&0x1 != 0 {
			return true
		}
	}
	return false
}

func validateDOCX(zr *zip.Reader, fileBytes int64, report *Report) {
	limits := defaultDOCXLimits
	stats := zipStats(zr)
	doc := findZipEntry(zr, "word/document.xml")
	report.metric("docx_zip_entries", len(zr.File))
	report.metric("docx_uncompressed_bytes", stats.uncompressedBytes)
	report.metric("docx_word_media_count", stats.wordMediaCount)
	report.metric("docx_word_media_bytes", stats.wordMediaBytes)

	if len(zr.File) > limits.entries {
		report.addIssue("Word 文档内部文件数量不能超过 %s 个（当前 %s 个）", formatInt(int64(limits.entries)), formatInt(int64(len(zr.File))))
	}
	if stats.uncompressedBytes > limits.uncompressedMB*mb {
		report.addIssue("Word 文档内部内容展开后不能超过 %dMB（当前 %s）", limits.uncompressedMB, formatMB(stats.uncompressedBytes))
	}
	if stats.uncompressedBytes > 40*mb {
		report.markHeavy("Word 文档内部内容展开后超过重型阈值 40MB（当前 %s）", formatMB(stats.uncompressedBytes))
	}
	if fileBytes > 0 && float64(stats.uncompressedBytes)/float64(fileBytes) > limits.compressionRate {
		report.addIssue("Word 文档压缩膨胀倍数不能超过 %s 倍（当前 %.2f 倍）", trimFloat(limits.compressionRate), float64(stats.uncompressedBytes)/float64(fileBytes))
	}
	if fileBytes > 0 {
		ratio := float64(stats.uncompressedBytes) / float64(fileBytes)
		report.metric("docx_compression_ratio", ratio)
		if ratio > 15 {
			report.markHeavy("Word 文档压缩膨胀倍数超过重型阈值 15 倍（当前 %.2f 倍）", ratio)
		}
	}
	if stats.wordMediaCount > limits.mediaFiles {
		report.addIssue("Word 文档内图片/媒体文件数量不能超过 %s 个（当前 %s 个）", formatInt(limits.mediaFiles), formatInt(stats.wordMediaCount))
	}
	if stats.wordMediaBytes > limits.mediaMB*mb {
		report.addIssue("Word 文档内图片/媒体总大小不能超过 %dMB（当前 %s）", limits.mediaMB, formatMB(stats.wordMediaBytes))
	}
	if doc == nil {
		report.addIssueText("文件结构异常或已损坏，无法解析")
		return
	}
	report.metric("docx_document_xml_bytes", int64(doc.UncompressedSize64))
	if int64(doc.UncompressedSize64) > limits.documentXMLMB*mb {
		report.addIssue("Word 正文结构过大，正文 XML 不能超过 %dMB（当前 %s）", limits.documentXMLMB, formatMB(int64(doc.UncompressedSize64)))
	}
	if int64(doc.UncompressedSize64) > 15*mb {
		report.markHeavy("Word 正文 XML 超过重型阈值 15MB（当前 %s）", formatMB(int64(doc.UncompressedSize64)))
	}

	if int64(doc.UncompressedSize64) > maxXMLScanBytes {
		report.addIssue("文档内部结构过大，无法安全检查完整结构，单个 XML 最大扫描量不能超过 %dMB（当前 %s）", maxXMLScanBytes/mb, formatMB(int64(doc.UncompressedSize64)))
		return
	}

	counters, err := scanDOCXXML(doc)
	if err != nil {
		report.addIssueText("文件结构异常或已损坏，无法解析")
		return
	}
	report.metric("docx_paragraphs", counters.paragraphs)
	report.metric("docx_text_nodes", counters.textNodes)
	report.metric("docx_tables", counters.tables)
	report.metric("docx_table_rows", counters.tableRows)
	report.metric("docx_table_cells", counters.tableCells)
	report.metric("docx_page_break_hints", counters.pageBreakHints)
	if counters.paragraphs > limits.paragraphs {
		report.addIssue("Word 文档段落数不能超过 %s 个（当前 %s 个）", formatInt(limits.paragraphs), formatInt(counters.paragraphs))
	}
	if counters.paragraphs > 50000 {
		report.markHeavy("Word 文档段落数超过重型阈值 50,000 个（当前 %s 个）", formatInt(counters.paragraphs))
	}
	if counters.textNodes > limits.textNodes {
		report.addIssue("Word 文档文本节点数不能超过 %s 个（当前 %s 个）", formatInt(limits.textNodes), formatInt(counters.textNodes))
	}
	if counters.textNodes > 75000 {
		report.markHeavy("Word 文档文本节点数超过重型阈值 75,000 个（当前 %s 个）", formatInt(counters.textNodes))
	}
	if counters.tables > limits.tables {
		report.addIssue("Word 文档表格数量不能超过 %s 个（当前 %s 个）", formatInt(limits.tables), formatInt(counters.tables))
	}
	if counters.tables > 500 {
		report.markHeavy("Word 文档表格数量超过重型阈值 500 个（当前 %s 个）", formatInt(counters.tables))
	}
	if counters.tableRows > limits.tableRows {
		report.addIssue("Word 文档表格行数不能超过 %s 行（当前 %s 行）", formatInt(limits.tableRows), formatInt(counters.tableRows))
	}
	if counters.tableRows > 10000 {
		report.markHeavy("Word 文档表格行数超过重型阈值 10,000 行（当前 %s 行）", formatInt(counters.tableRows))
	}
	if counters.tableCells > limits.tableCells {
		report.addIssue("Word 文档表格单元格数量不能超过 %s 个（当前 %s 个）", formatInt(limits.tableCells), formatInt(counters.tableCells))
	}
	if counters.tableCells > 40000 {
		report.markHeavy("Word 文档表格单元格数量超过重型阈值 40,000 个（当前 %s 个）", formatInt(counters.tableCells))
	}
	if counters.pageBreakHints > limits.pageBreakHints {
		report.addIssue("Word 文档分页数量不能超过 %s 页左右（当前 %s 个分页提示）", formatInt(limits.pageBreakHints), formatInt(counters.pageBreakHints))
	}
	if counters.pageBreakHints > 400 {
		report.markHeavy("Word 文档分页提示超过重型阈值 400 个（当前 %s 个）", formatInt(counters.pageBreakHints))
	}
}

type docxCounters struct {
	paragraphs     int64
	textNodes      int64
	tables         int64
	tableRows      int64
	tableCells     int64
	pageBreakHints int64
}

func scanDOCXXML(f *zip.File) (docxCounters, error) {
	rc, err := f.Open()
	if err != nil {
		return docxCounters{}, err
	}
	defer rc.Close()

	var c docxCounters
	decoder := xml.NewDecoder(rc)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return c, nil
		}
		if err != nil {
			return c, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "p":
			c.paragraphs++
		case "t":
			c.textNodes++
		case "tbl":
			c.tables++
		case "tr":
			c.tableRows++
		case "tc":
			c.tableCells++
		case "lastRenderedPageBreak":
			c.pageBreakHints++
		case "br":
			if attrValue(start.Attr, "type") == "page" {
				c.pageBreakHints++
			}
		}
	}
}

func validateXLSX(zr *zip.Reader, report *Report) {
	limits := defaultXLSXLimits
	stats := zipStats(zr)
	sheetNames := xlsxSheetNames(zr)
	report.metric("xlsx_zip_entries", len(zr.File))
	report.metric("xlsx_uncompressed_bytes", stats.uncompressedBytes)
	report.metric("xlsx_media_bytes", stats.xlsxMediaBytes)

	if len(zr.File) > limits.entries {
		report.addIssue("Excel 文件内部文件数量不能超过 %s 个（当前 %s 个）", formatInt(int64(limits.entries)), formatInt(int64(len(zr.File))))
	}
	if stats.uncompressedBytes > limits.uncompressedMB*mb {
		report.addIssue("Excel 文件内部内容展开后不能超过 %dMB（当前 %s）", limits.uncompressedMB, formatMB(stats.uncompressedBytes))
	}
	if stats.uncompressedBytes > 40*mb {
		report.markHeavy("Excel 文件内部内容展开后超过重型阈值 40MB（当前 %s）", formatMB(stats.uncompressedBytes))
	}
	var worksheetXMLTotal int64
	var totalRows int64
	var totalCells int64
	var formulaCells int64
	var conditionalFormats int64
	var mergeRanges int64
	var mergedCoveredCells int64
	var maxSheetXML *namedInt64
	var maxSheetRows *namedInt64
	var maxSheetColumns *namedInt64
	var scannedSheet bool

	for _, f := range zr.File {
		if !isWorksheetXML(f.Name) {
			continue
		}
		scannedSheet = true
		sheetName := displaySheetName(f.Name, sheetNames)
		size := int64(f.UncompressedSize64)
		worksheetXMLTotal += size
		if maxSheetXML == nil || size > maxSheetXML.value {
			maxSheetXML = &namedInt64{name: sheetName, value: size}
		}
		if size > maxXMLScanBytes {
			report.addIssue("文档内部结构过大，无法安全检查完整结构，单个 XML 最大扫描量不能超过 %dMB（%s：当前 %s）", maxXMLScanBytes/mb, sheetName, formatMB(size))
			continue
		}
		c, err := scanWorksheetXML(f)
		if err != nil {
			report.addIssueText("文件结构异常或已损坏，无法解析")
			return
		}
		totalRows += c.rows
		totalCells += c.cells
		formulaCells += c.formulas
		conditionalFormats += c.conditionalFormats
		mergeRanges += c.mergeRanges
		mergedCoveredCells += c.mergedCoveredCells
		if maxSheetRows == nil || c.rows > maxSheetRows.value {
			maxSheetRows = &namedInt64{name: sheetName, value: c.rows}
		}
		if maxSheetColumns == nil || c.maxColumn > maxSheetColumns.value {
			maxSheetColumns = &namedInt64{name: sheetName, value: c.maxColumn}
		}
	}
	if !scannedSheet {
		report.addIssueText("文件结构异常或已损坏，无法解析")
		return
	}

	report.metric("xlsx_worksheet_xml_total_bytes", worksheetXMLTotal)
	report.metric("xlsx_total_rows", totalRows)
	report.metric("xlsx_total_cells", totalCells)
	report.metric("xlsx_formula_cells", formulaCells)
	report.metric("xlsx_conditional_formats", conditionalFormats)
	report.metric("xlsx_merge_ranges", mergeRanges)
	report.metric("xlsx_merged_covered_cells", mergedCoveredCells)
	if maxSheetXML != nil {
		report.metric("xlsx_max_sheet_xml_bytes", maxSheetXML.value)
		report.metric("xlsx_max_sheet_xml_name", maxSheetXML.name)
	}
	if maxSheetRows != nil {
		report.metric("xlsx_max_sheet_rows", maxSheetRows.value)
		report.metric("xlsx_max_sheet_rows_name", maxSheetRows.name)
	}
	if maxSheetColumns != nil {
		report.metric("xlsx_max_sheet_columns", maxSheetColumns.value)
		report.metric("xlsx_max_sheet_columns_name", maxSheetColumns.name)
	}

	if maxSheetXML != nil && maxSheetXML.value > limits.worksheetXMLMB*mb {
		report.addIssue("单个工作表内部结构不能超过 %dMB（%s：当前 %s）", limits.worksheetXMLMB, maxSheetXML.name, formatMB(maxSheetXML.value))
	}
	if maxSheetXML != nil && maxSheetXML.value > 5*mb/2 {
		report.markHeavy("单个工作表内部结构超过重型阈值 %s（%s：当前 %s）", formatByteSize(5*mb/2), maxSheetXML.name, formatMB(maxSheetXML.value))
	}
	if worksheetXMLTotal > limits.worksheetXMLTotalMB*mb {
		report.addIssue("所有工作表内部结构总大小不能超过 %dMB（当前 %s）", limits.worksheetXMLTotalMB, formatMB(worksheetXMLTotal))
	}
	if worksheetXMLTotal > 15*mb {
		report.markHeavy("所有工作表内部结构总大小超过重型阈值 15MB（当前 %s）", formatMB(worksheetXMLTotal))
	}

	shared := findZipEntry(zr, "xl/sharedStrings.xml")
	if shared != nil {
		sharedBytes := int64(shared.UncompressedSize64)
		report.metric("xlsx_shared_strings_bytes", sharedBytes)
		if int64(shared.UncompressedSize64) > limits.sharedStringsMB*mb {
			report.addIssue("Excel 共享字符串表不能超过 %dMB（当前 %s）", limits.sharedStringsMB, formatMB(int64(shared.UncompressedSize64)))
		}
		if sharedBytes > 3*mb/2 {
			report.markHeavy("Excel 共享字符串表超过重型阈值 %s（当前 %s）", formatByteSize(3*mb/2), formatMB(sharedBytes))
		}
		if int64(shared.UncompressedSize64) <= maxXMLScanBytes {
			items, err := countStartElements(shared, "si")
			if err != nil {
				report.addIssueText("文件结构异常或已损坏，无法解析")
				return
			}
			report.metric("xlsx_shared_string_items", items)
			if items > limits.sharedStringItems {
				report.addIssue("Excel 唯一文本项数量不能超过 %s 个（当前 %s 个）", formatInt(limits.sharedStringItems), formatInt(items))
			}
			if items > 25000 {
				report.markHeavy("Excel 唯一文本项数量超过重型阈值 25,000 个（当前 %s 个）", formatInt(items))
			}
		}
	}
	if maxSheetRows != nil && maxSheetRows.value > limits.sheetRows {
		report.addIssue("单个工作表行数不能超过 %s 行（%s：当前 %s 行）", formatInt(limits.sheetRows), maxSheetRows.name, formatInt(maxSheetRows.value))
	}
	if maxSheetRows != nil && maxSheetRows.value > 10000 {
		report.markHeavy("单个工作表行数超过重型阈值 10,000 行（%s：当前 %s 行）", maxSheetRows.name, formatInt(maxSheetRows.value))
	}
	if totalRows > limits.totalRows {
		report.addIssue("Excel 总行数不能超过 %s 行（当前 %s 行）", formatInt(limits.totalRows), formatInt(totalRows))
	}
	if totalRows > 15000 {
		report.markHeavy("Excel 总行数超过重型阈值 15,000 行（当前 %s 行）", formatInt(totalRows))
	}
	if maxSheetColumns != nil && maxSheetColumns.value > limits.sheetColumns {
		report.addIssue("单个工作表列数不能超过 %s 列（%s：当前 %s 列）", formatInt(limits.sheetColumns), maxSheetColumns.name, formatInt(maxSheetColumns.value))
	}
	if maxSheetColumns != nil && maxSheetColumns.value > 50 {
		report.markHeavy("单个工作表列数超过重型阈值 50 列（%s：当前 %s 列）", maxSheetColumns.name, formatInt(maxSheetColumns.value))
	}
	if totalCells > limits.totalCells {
		report.addIssue("Excel 总单元格数量不能超过 %s 个（当前 %s 个）", formatInt(limits.totalCells), formatInt(totalCells))
	}
	if totalCells > 50000 {
		report.markHeavy("Excel 总单元格数量超过重型阈值 50,000 个（当前 %s 个）", formatInt(totalCells))
	}
	if formulaCells > limits.formulaCells {
		report.addIssue("Excel 公式单元格数量不能超过 %s 个（当前 %s 个）", formatInt(limits.formulaCells), formatInt(formulaCells))
	}
	if formulaCells > 15000 {
		report.markHeavy("Excel 公式单元格数量超过重型阈值 15,000 个（当前 %s 个）", formatInt(formulaCells))
	}
	if conditionalFormats > limits.conditionalFormats {
		report.addIssue("Excel 条件格式规则数量不能超过 %s 条（当前 %s 条）", formatInt(limits.conditionalFormats), formatInt(conditionalFormats))
	}
	if conditionalFormats > 250 {
		report.markHeavy("Excel 条件格式规则数量超过重型阈值 250 条（当前 %s 条）", formatInt(conditionalFormats))
	}
	if mergeRanges > limits.mergeRanges {
		report.addIssue("Excel 合并单元格区域数量不能超过 %s 个（当前 %s 个）", formatInt(limits.mergeRanges), formatInt(mergeRanges))
	}
	if mergeRanges > 2500 {
		report.markHeavy("Excel 合并单元格区域数量超过重型阈值 2,500 个（当前 %s 个）", formatInt(mergeRanges))
	}
	if mergedCoveredCells > limits.mergedCoveredCells {
		report.addIssue("Excel 合并单元格覆盖范围不能超过 %s 个单元格（当前 %s 个单元格）", formatInt(limits.mergedCoveredCells), formatInt(mergedCoveredCells))
	}
	if mergedCoveredCells > 25000 {
		report.markHeavy("Excel 合并单元格覆盖范围超过重型阈值 25,000 个单元格（当前 %s 个单元格）", formatInt(mergedCoveredCells))
	}

	drawingObjects, drawingErr := countXLSXDrawingObjects(zr)
	if drawingErr != nil {
		report.addIssueText("文件结构异常或已损坏，无法解析")
		return
	}
	report.metric("xlsx_drawing_objects", drawingObjects)
	if drawingObjects > limits.drawingObjects {
		report.addIssue("Excel 图片/绘图对象数量不能超过 %s 个（当前 %s 个）", formatInt(limits.drawingObjects), formatInt(drawingObjects))
	}
	if drawingObjects > 50 {
		report.markHeavy("Excel 图片/绘图对象数量超过重型阈值 50 个（当前 %s 个）", formatInt(drawingObjects))
	}
	if stats.xlsxMediaBytes > limits.mediaMB*mb {
		report.addIssue("Excel 内图片/媒体总大小不能超过 %dMB（当前 %s）", limits.mediaMB, formatMB(stats.xlsxMediaBytes))
	}
	if stats.xlsxMediaBytes > 25*mb {
		report.markHeavy("Excel 内图片/媒体总大小超过重型阈值 25MB（当前 %s）", formatMB(stats.xlsxMediaBytes))
	}

	styles := findZipEntry(zr, "xl/styles.xml")
	if styles != nil && int64(styles.UncompressedSize64) <= maxXMLScanBytes {
		stylesCount, err := countStartElements(styles, "xf")
		if err != nil {
			report.addIssueText("文件结构异常或已损坏，无法解析")
			return
		}
		report.metric("xlsx_cell_styles", stylesCount)
		if stylesCount > limits.cellStyles {
			report.addIssue("Excel 单元格样式数量不能超过 %s 个（当前 %s 个）", formatInt(limits.cellStyles), formatInt(stylesCount))
		}
		if stylesCount > 500 {
			report.markHeavy("Excel 单元格样式数量超过重型阈值 500 个（当前 %s 个）", formatInt(stylesCount))
		}
	}
}

func validatePPTX(zr *zip.Reader, fileBytes int64, report *Report) {
	limits := defaultPPTXLimits
	stats := zipStats(zr)
	report.metric("pptx_zip_entries", len(zr.File))
	report.metric("pptx_uncompressed_bytes", stats.uncompressedBytes)
	report.metric("pptx_media_count", stats.pptMediaCount)
	report.metric("pptx_media_bytes", stats.pptMediaBytes)

	if len(zr.File) > limits.entries {
		report.addIssue("PPT 文件内部文件数量不能超过 %s 个（当前 %s 个）", formatInt(int64(limits.entries)), formatInt(int64(len(zr.File))))
	}
	if stats.uncompressedBytes > limits.uncompressedMB*mb {
		report.addIssue("PPT 文件内部内容展开后不能超过 %dMB（当前 %s）", limits.uncompressedMB, formatMB(stats.uncompressedBytes))
	}
	if stats.uncompressedBytes > 100*mb {
		report.markHeavy("PPT 文件内部内容展开后超过重型阈值 100MB（当前 %s）", formatMB(stats.uncompressedBytes))
	}
	if fileBytes > 0 && float64(stats.uncompressedBytes)/float64(fileBytes) > limits.compressionRate {
		report.addIssue("PPT 文件压缩膨胀倍数不能超过 %s 倍（当前 %.2f 倍）", trimFloat(limits.compressionRate), float64(stats.uncompressedBytes)/float64(fileBytes))
	}
	if fileBytes > 0 {
		ratio := float64(stats.uncompressedBytes) / float64(fileBytes)
		report.metric("pptx_compression_ratio", ratio)
		if ratio > 10 {
			report.markHeavy("PPT 文件压缩膨胀倍数超过重型阈值 10 倍（当前 %.2f 倍）", ratio)
		}
	}
	if stats.pptMediaCount > limits.mediaFiles {
		report.addIssue("PPT 文件内图片/媒体文件数量不能超过 %s 个（当前 %s 个）", formatInt(limits.mediaFiles), formatInt(stats.pptMediaCount))
	}
	if stats.pptMediaBytes > limits.mediaMB*mb {
		report.addIssue("PPT 文件内图片/媒体总大小不能超过 %dMB（当前 %s）", limits.mediaMB, formatMB(stats.pptMediaBytes))
	}
	if stats.pptMediaBytes > 75*mb {
		report.markHeavy("PPT 文件内图片/媒体总大小超过重型阈值 75MB（当前 %s）", formatMB(stats.pptMediaBytes))
	}

	var slideXMLTotal int64
	var slideCount int64
	var shapeCount int64
	var maxSlideXML *namedInt64
	for _, f := range zr.File {
		if !isSlideXML(f.Name) {
			continue
		}
		slideCount++
		slideName := path.Base(normalizeZipName(f.Name))
		size := int64(f.UncompressedSize64)
		slideXMLTotal += size
		if maxSlideXML == nil || size > maxSlideXML.value {
			maxSlideXML = &namedInt64{name: slideName, value: size}
		}
		if size > maxXMLScanBytes {
			report.addIssue("文档内部结构过大，无法安全检查完整结构，单个 XML 最大扫描量不能超过 %dMB（%s：当前 %s）", maxXMLScanBytes/mb, slideName, formatMB(size))
			continue
		}
		shapes, err := countAnyStartElements(f, map[string]struct{}{
			"sp":           {},
			"pic":          {},
			"graphicFrame": {},
		})
		if err != nil {
			report.addIssueText("文件结构异常或已损坏，无法解析")
			return
		}
		shapeCount += shapes
	}

	report.metric("pptx_slides", slideCount)
	report.metric("pptx_slide_xml_total_bytes", slideXMLTotal)
	report.metric("pptx_slide_shapes", shapeCount)
	if maxSlideXML != nil {
		report.metric("pptx_max_slide_xml_bytes", maxSlideXML.value)
		report.metric("pptx_max_slide_xml_name", maxSlideXML.name)
	}
	if slideCount > limits.slides {
		report.addIssue("PPT 幻灯片数量不能超过 %s 页（当前 %s 页）", formatInt(limits.slides), formatInt(slideCount))
	}
	if slideCount > 150 {
		report.markHeavy("PPT 幻灯片数量超过重型阈值 150 页（当前 %s 页）", formatInt(slideCount))
	}
	if maxSlideXML != nil && maxSlideXML.value > limits.slideXMLMB*mb {
		report.addIssue("单页 PPT 内部结构不能超过 %dMB（%s：当前 %s）", limits.slideXMLMB, maxSlideXML.name, formatMB(maxSlideXML.value))
	}
	if maxSlideXML != nil && maxSlideXML.value > 4*mb {
		report.markHeavy("单页 PPT 内部结构超过重型阈值 4MB（%s：当前 %s）", maxSlideXML.name, formatMB(maxSlideXML.value))
	}
	if slideXMLTotal > limits.slideXMLTotalMB*mb {
		report.addIssue("所有 PPT 页面内部结构总大小不能超过 %dMB（当前 %s）", limits.slideXMLTotalMB, formatMB(slideXMLTotal))
	}
	if slideXMLTotal > 40*mb {
		report.markHeavy("所有 PPT 页面内部结构总大小超过重型阈值 40MB（当前 %s）", formatMB(slideXMLTotal))
	}
	if shapeCount > limits.slideShapes {
		report.addIssue("PPT 页面对象数量不能超过 %s 个（当前 %s 个）", formatInt(limits.slideShapes), formatInt(shapeCount))
	}
	if shapeCount > 10000 {
		report.markHeavy("PPT 页面对象数量超过重型阈值 10,000 个（当前 %s 个）", formatInt(shapeCount))
	}
}

func validateCSV(ra readerAtSize, report *Report) {
	limits := defaultCSVLimits
	size := ra.Size()
	if size > limits.scanMB*mb {
		report.addIssue("CSV 文件结构检查最大读取量不能超过 %dMB（当前 %s），请拆分后上传", limits.scanMB, formatMB(size))
		return
	}

	reader := csv.NewReader(io.NewSectionReader(ra, 0, size))
	reader.LazyQuotes = true
	reader.TrimLeadingSpace = true
	reader.FieldsPerRecord = -1

	var rows int64
	var cells int64
	var maxColumns int64
	var maxRowBytes int64
	var maxFieldBytes int64
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			report.addIssueText("CSV 文件结构异常，无法解析，请检查引号、分隔符或换行是否正确")
			return
		}

		rows++
		columns := int64(len(record))
		cells += columns
		if columns > maxColumns {
			maxColumns = columns
		}

		rowBytes := columns - 1
		if rowBytes < 0 {
			rowBytes = 0
		}
		for _, field := range record {
			fieldBytes := int64(len(field))
			rowBytes += fieldBytes
			if fieldBytes > maxFieldBytes {
				maxFieldBytes = fieldBytes
			}
		}
		if rowBytes > maxRowBytes {
			maxRowBytes = rowBytes
		}
	}

	report.metric("csv_rows", rows)
	report.metric("csv_cells", cells)
	report.metric("csv_max_columns", maxColumns)
	report.metric("csv_max_row_bytes", maxRowBytes)
	report.metric("csv_max_field_bytes", maxFieldBytes)

	if rows > limits.rows {
		report.addIssue("CSV 总行数不能超过 %s 行（当前 %s 行）", formatInt(limits.rows), formatInt(rows))
	}
	if rows > 50000 {
		report.markHeavy("CSV 总行数超过重型阈值 50,000 行（当前 %s 行）", formatInt(rows))
	}
	if maxColumns > limits.columns {
		report.addIssue("CSV 单行列数不能超过 %s 列（当前 %s 列）", formatInt(limits.columns), formatInt(maxColumns))
	}
	if maxColumns > 50 {
		report.markHeavy("CSV 单行列数超过重型阈值 50 列（当前 %s 列）", formatInt(maxColumns))
	}
	if cells > limits.cells {
		report.addIssue("CSV 总单元格数量不能超过 %s 个（当前 %s 个）", formatInt(limits.cells), formatInt(cells))
	}
	if cells > 150000 {
		report.markHeavy("CSV 总单元格数量超过重型阈值 150,000 个（当前 %s 个）", formatInt(cells))
	}
	if maxRowBytes > limits.rowBytes {
		report.addIssue("CSV 单行内容不能超过 %s（当前 %s）", formatByteSize(limits.rowBytes), formatByteSize(maxRowBytes))
	}
	if maxRowBytes > 512*1024 {
		report.markHeavy("CSV 单行内容超过重型阈值 512KB（当前 %s）", formatByteSize(maxRowBytes))
	}
	if maxFieldBytes > limits.fieldBytes {
		report.addIssue("CSV 单个单元格内容不能超过 %s（当前 %s）", formatByteSize(limits.fieldBytes), formatByteSize(maxFieldBytes))
	}
	if maxFieldBytes > 256*1024 {
		report.markHeavy("CSV 单个单元格内容超过重型阈值 256KB（当前 %s）", formatByteSize(maxFieldBytes))
	}
}

type zipStatValues struct {
	uncompressedBytes int64
	wordMediaCount    int64
	wordMediaBytes    int64
	xlsxMediaBytes    int64
	pptMediaCount     int64
	pptMediaBytes     int64
}

func zipStats(zr *zip.Reader) zipStatValues {
	var s zipStatValues
	for _, f := range zr.File {
		size := int64(f.UncompressedSize64)
		s.uncompressedBytes += size
		name := normalizeZipName(f.Name)
		if strings.HasPrefix(name, "word/media/") && !strings.HasSuffix(name, "/") {
			s.wordMediaCount++
			s.wordMediaBytes += size
		}
		if strings.HasPrefix(name, "xl/media/") && !strings.HasSuffix(name, "/") {
			s.xlsxMediaBytes += size
		}
		if strings.HasPrefix(name, "ppt/media/") && !strings.HasSuffix(name, "/") {
			s.pptMediaCount++
			s.pptMediaBytes += size
		}
	}
	return s
}

type worksheetCounters struct {
	rows               int64
	cells              int64
	formulas           int64
	conditionalFormats int64
	mergeRanges        int64
	mergedCoveredCells int64
	maxColumn          int64
}

func scanWorksheetXML(f *zip.File) (worksheetCounters, error) {
	rc, err := f.Open()
	if err != nil {
		return worksheetCounters{}, err
	}
	defer rc.Close()

	var c worksheetCounters
	decoder := xml.NewDecoder(rc)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return c, nil
		}
		if err != nil {
			return c, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "row":
			c.rows++
		case "c":
			c.cells++
			if col := cellColumn(attrValue(start.Attr, "r")); col > c.maxColumn {
				c.maxColumn = col
			}
		case "f":
			c.formulas++
		case "conditionalFormatting":
			c.conditionalFormats++
		case "mergeCell":
			c.mergeRanges++
			if area := cellRangeArea(attrValue(start.Attr, "ref")); area > 0 {
				c.mergedCoveredCells += area
			}
		}
	}
}

type namedInt64 struct {
	name  string
	value int64
}

func countXLSXDrawingObjects(zr *zip.Reader) (int64, error) {
	var count int64
	var mediaCount int64
	var sawDrawingXML bool
	for _, f := range zr.File {
		name := normalizeZipName(f.Name)
		if strings.HasPrefix(name, "xl/media/") && !strings.HasSuffix(name, "/") {
			mediaCount++
			continue
		}
		if !strings.HasPrefix(name, "xl/drawings/") || !strings.HasSuffix(name, ".xml") {
			continue
		}
		sawDrawingXML = true
		if int64(f.UncompressedSize64) > maxXMLScanBytes {
			continue
		}
		pics, err := countAnyStartElements(f, map[string]struct{}{
			"pic":          {},
			"graphicFrame": {},
		})
		if err != nil {
			return 0, err
		}
		count += pics
	}
	if !sawDrawingXML {
		return mediaCount, nil
	}
	return count, nil
}

func countStartElements(f *zip.File, local string) (int64, error) {
	return countAnyStartElements(f, map[string]struct{}{local: {}})
}

func countAnyStartElements(f *zip.File, names map[string]struct{}) (int64, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()

	var count int64
	decoder := xml.NewDecoder(rc)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return 0, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if _, ok := names[start.Name.Local]; ok {
			count++
		}
	}
}

func xlsxSheetNames(zr *zip.Reader) map[string]string {
	workbook := findZipEntry(zr, "xl/workbook.xml")
	rels := findZipEntry(zr, "xl/_rels/workbook.xml.rels")
	if workbook == nil || rels == nil {
		return nil
	}
	relTargets := readRelationshipTargets(rels)
	if len(relTargets) == 0 {
		return nil
	}

	rc, err := workbook.Open()
	if err != nil {
		return nil
	}
	defer rc.Close()

	result := map[string]string{}
	decoder := xml.NewDecoder(rc)
	for {
		token, err := decoder.Token()
		if err != nil {
			return result
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "sheet" {
			continue
		}
		name := attrValue(start.Attr, "name")
		rid := attrValue(start.Attr, "id")
		target := normalizeWorkbookRelTarget(relTargets[rid])
		if name != "" && target != "" {
			result[target] = name
		}
	}
}

func readRelationshipTargets(f *zip.File) map[string]string {
	rc, err := f.Open()
	if err != nil {
		return nil
	}
	defer rc.Close()
	result := map[string]string{}
	decoder := xml.NewDecoder(rc)
	for {
		token, err := decoder.Token()
		if err != nil {
			return result
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "Relationship" {
			continue
		}
		id := attrValue(start.Attr, "Id")
		target := attrValue(start.Attr, "Target")
		if id != "" && target != "" {
			result[id] = target
		}
	}
}

func normalizeWorkbookRelTarget(target string) string {
	target = strings.ReplaceAll(target, "\\", "/")
	target = strings.TrimPrefix(target, "/")
	if strings.HasPrefix(target, "xl/") {
		return path.Clean(target)
	}
	return path.Clean("xl/" + target)
}

func displaySheetName(zipPath string, names map[string]string) string {
	zipPath = normalizeZipName(zipPath)
	if names != nil {
		if name := names[zipPath]; name != "" {
			return name
		}
	}
	return path.Base(zipPath)
}

func findZipEntry(zr *zip.Reader, name string) *zip.File {
	name = normalizeZipName(name)
	for _, f := range zr.File {
		if normalizeZipName(f.Name) == name {
			return f
		}
	}
	return nil
}

func normalizeZipName(name string) string {
	return path.Clean(strings.ReplaceAll(name, "\\", "/"))
}

func isWorksheetXML(name string) bool {
	name = normalizeZipName(name)
	return strings.HasPrefix(name, "xl/worksheets/") &&
		strings.HasSuffix(name, ".xml") &&
		!strings.Contains(name, "/_rels/")
}

func isSlideXML(name string) bool {
	name = normalizeZipName(name)
	base := path.Base(name)
	return strings.HasPrefix(name, "ppt/slides/") &&
		strings.HasSuffix(name, ".xml") &&
		!strings.Contains(name, "/_rels/") &&
		strings.HasPrefix(base, "slide")
}

func attrValue(attrs []xml.Attr, local string) string {
	for _, attr := range attrs {
		if attr.Name.Local == local {
			return attr.Value
		}
	}
	return ""
}

func fileTypeFromName(fileName string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(fileName), "."))
	if ext == "" {
		return strings.ToLower(strings.TrimPrefix(fileName, "."))
	}
	return ext
}

func analysisName(fileName, fileType string) string {
	name := strings.TrimSpace(fileName)
	extHint := normalizeExt(fileType)
	if extHint == "" {
		return name
	}
	currentExt := fileTypeFromName(name)
	if name == "" {
		return "download." + extHint
	}
	if currentExt == "" || currentExt != extHint {
		return name + "." + extHint
	}
	return name
}

func normalizeExt(value string) string {
	value = strings.TrimSpace(strings.ToLower(strings.TrimPrefix(value, ".")))
	switch value {
	case "application/pdf":
		return "pdf"
	case "application/json":
		return "json"
	case "text/plain":
		return "txt"
	case "text/csv":
		return "csv"
	case "text/markdown":
		return "md"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return "docx"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return "xlsx"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return "pptx"
	case "application/vnd.ms-powerpoint":
		return "ppt"
	}
	return value
}

func cellColumn(ref string) int64 {
	var col int64
	for _, r := range ref {
		if r >= 'A' && r <= 'Z' {
			col = col*26 + int64(r-'A'+1)
			continue
		}
		if r >= 'a' && r <= 'z' {
			col = col*26 + int64(r-'a'+1)
			continue
		}
		break
	}
	return col
}

func cellRow(ref string) int64 {
	var row int64
	for _, r := range ref {
		if r >= '0' && r <= '9' {
			row = row*10 + int64(r-'0')
		}
	}
	return row
}

func cellRangeArea(ref string) int64 {
	parts := strings.Split(ref, ":")
	if len(parts) != 2 {
		return 1
	}
	c1, r1 := cellColumn(parts[0]), cellRow(parts[0])
	c2, r2 := cellColumn(parts[1]), cellRow(parts[1])
	if c1 == 0 || c2 == 0 || r1 == 0 || r2 == 0 {
		return 0
	}
	if c2 < c1 {
		c1, c2 = c2, c1
	}
	if r2 < r1 {
		r1, r2 = r2, r1
	}
	return (c2 - c1 + 1) * (r2 - r1 + 1)
}

func formatMB(bytes int64) string {
	return fmt.Sprintf("%sMB", trimFloat(float64(bytes)/float64(mb)))
}

func formatByteSize(bytes int64) string {
	if bytes >= mb {
		return formatMB(bytes)
	}
	const kb = 1024
	if bytes >= kb {
		return fmt.Sprintf("%sKB", trimFloat(float64(bytes)/float64(kb)))
	}
	return fmt.Sprintf("%dB", bytes)
}

func trimFloat(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" {
		return "0"
	}
	return s
}

func formatInt(n int64) string {
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return sign + s
	}
	var b strings.Builder
	prefix := len(s) % 3
	if prefix == 0 {
		prefix = 3
	}
	b.WriteString(s[:prefix])
	for i := prefix; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return sign + b.String()
}
