package fileguard

import (
	"archive/zip"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestValidateBytesDOCXReportsMultipleStructuralLimits(t *testing.T) {
	var doc strings.Builder
	doc.WriteString(`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`)
	for i := 0; i < 20001; i++ {
		doc.WriteString(`<w:tr/>`)
	}
	for i := 0; i < 80001; i++ {
		doc.WriteString(`<w:tc/>`)
	}
	doc.WriteString(`</w:body></w:document>`)

	data := zipBytes(t, map[string]string{
		"word/document.xml": doc.String(),
	})

	err := ValidateBytes("heavy.docx", "docx", data)
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	for _, want := range []string{
		"Word 文档压缩膨胀倍数不能超过 30 倍",
		"Word 文档表格行数不能超过 20,000 行",
		"Word 文档表格单元格数量不能超过 80,000 个",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q:\n%s", want, msg)
		}
	}
	if strings.HasSuffix(msg, "。") {
		t.Fatalf("message should not end with punctuation: %s", msg)
	}
	if !strings.Contains(msg, "，") {
		t.Fatalf("multiple issues should be joined by Chinese comma: %s", msg)
	}
}

func TestValidateBytesXLSXReportsMultipleStructuralLimits(t *testing.T) {
	var sheet strings.Builder
	sheet.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for row := 1; row <= 30001; row++ {
		sheet.WriteString(fmt.Sprintf(`<row r="%d">`, row))
		for col := 1; col <= 11; col++ {
			sheet.WriteString(fmt.Sprintf(`<c r="%s%d"><v>%d</v></c>`, columnName(col), row, row))
		}
		sheet.WriteString(`</row>`)
	}
	sheet.WriteString(`</sheetData>`)
	for i := 0; i < 501; i++ {
		sheet.WriteString(`<conditionalFormatting sqref="A1"/>`)
	}
	sheet.WriteString(`</worksheet>`)

	data := zipBytes(t, map[string]string{
		"xl/workbook.xml":            `<workbook xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="数据项" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml":   sheet.String(),
	})

	err := ValidateBytes("heavy.xlsx", "xlsx", data)
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	for _, want := range []string{
		"单个工作表行数不能超过 30,000 行（数据项：当前 30,001 行）",
		"Excel 总单元格数量不能超过 300,000 个",
		"Excel 条件格式规则数量不能超过 500 条",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q:\n%s", want, msg)
		}
	}
	if strings.HasSuffix(msg, "。") {
		t.Fatalf("message should not end with punctuation: %s", msg)
	}
}

func TestAnalyzeBytesXLSXAllowsBusinessInventoryShapeAsHeavy(t *testing.T) {
	var sheet strings.Builder
	sheet.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for row := 1; row <= 10000; row++ {
		sheet.WriteString(fmt.Sprintf(`<row r="%d">`, row))
		for col := 1; col <= 11; col++ {
			sheet.WriteString(fmt.Sprintf(`<c r="%s%d"><v>%d</v></c>`, columnName(col), row, row))
		}
		sheet.WriteString(`</row>`)
	}
	sheet.WriteString(`</sheetData><mergeCells>`)
	for row := 1; row <= 1200; row++ {
		sheet.WriteString(fmt.Sprintf(`<mergeCell ref="A%d:Z%d"/>`, row, row+1))
	}
	sheet.WriteString(`</mergeCells><extLst><ext uri="{fileguard-test}">`)
	sheet.WriteString(strings.Repeat("x", 6*mb))
	sheet.WriteString(`</ext></extLst></worksheet>`)

	report := AnalyzeBytes("business-inventory.xlsx", "xlsx", zipBytes(t, map[string]string{
		"xl/worksheets/sheet1.xml": sheet.String(),
	}))
	if err := report.ValidationError(); err != nil {
		t.Fatalf("expected business inventory shaped workbook to pass hard limits: %s", err.Error())
	}
	if !report.IsHeavy() {
		t.Fatal("expected business inventory shaped workbook to be routed as heavy")
	}
}

func TestValidateBytesXLSXDoesNotDoubleCountPictures(t *testing.T) {
	files := map[string]string{
		"xl/worksheets/sheet1.xml": `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1"><v>1</v></c></row></sheetData></worksheet>`,
	}
	var drawing strings.Builder
	drawing.WriteString(`<xdr:wsDr xmlns:xdr="http://schemas.openxmlformats.org/drawingml/2006/spreadsheetDrawing">`)
	for i := 0; i < 60; i++ {
		drawing.WriteString(`<xdr:pic/>`)
		files[fmt.Sprintf("xl/media/image%d.png", i+1)] = "x"
	}
	drawing.WriteString(`</xdr:wsDr>`)
	files["xl/drawings/drawing1.xml"] = drawing.String()

	err := ValidateBytes("pictures.xlsx", "xlsx", zipBytes(t, files))
	if err != nil {
		t.Fatalf("expected pictures below limit to pass without double counting: %s", err.Error())
	}
}

func TestValidateBytesSizeOnlyLimit(t *testing.T) {
	err := ValidateBytes("large.json", "json", make([]byte, mb+1))
	if err == nil {
		t.Fatal("expected validation error")
	}
	want := "JSON 文件大小不能超过 1MB（当前 1MB）"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("message missing %q: %s", want, err.Error())
	}
}

func TestValidateBytesDocumentSizeLimitsDoubled(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  string
		mb   int
		want string
	}{
		{name: "report.pdf", typ: "pdf", mb: 20, want: "PDF 文件大小不能超过 20MB"},
		{name: "deck.pptx", typ: "pptx", mb: 20, want: "PPT 文件大小不能超过 20MB"},
		{name: "deck.ppt", typ: "ppt", mb: 16, want: "PPT 旧格式文件大小不能超过 16MB"},
	} {
		if err := ValidateBytes(tc.name, tc.typ, make([]byte, int64(tc.mb)*mb)); err != nil {
			t.Fatalf("expected %s at limit to pass: %s", tc.name, err.Error())
		}
		err := ValidateBytes(tc.name, tc.typ, make([]byte, int64(tc.mb)*mb+1))
		if err == nil {
			t.Fatalf("expected %s just above limit to fail", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("message missing %q: %s", tc.want, err.Error())
		}
	}
}

func TestValidateBytesCSVReportsMultipleStructuralLimits(t *testing.T) {
	var data strings.Builder
	for i := 0; i < 101; i++ {
		if i > 0 {
			data.WriteByte(',')
		}
		data.WriteString("h")
	}
	data.WriteByte('\n')
	for i := 0; i < 100000; i++ {
		data.WriteString("a,b,c\n")
	}

	err := ValidateBytes("large.csv", "csv", []byte(data.String()))
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	for _, want := range []string{
		"CSV 总行数不能超过 100,000 行（当前 100,001 行）",
		"CSV 单行列数不能超过 100 列（当前 101 列）",
		"CSV 总单元格数量不能超过 300,000 个（当前 300,101 个）",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "，") {
		t.Fatalf("multiple issues should be joined by Chinese comma: %s", msg)
	}
	if strings.HasSuffix(msg, "。") {
		t.Fatalf("message should not end with punctuation: %s", msg)
	}
}

func TestValidateBytesCSVReportsLargeFieldAndRow(t *testing.T) {
	err := ValidateBytes("wide.csv", "csv", []byte(strings.Repeat("x", mb+1)))
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	for _, want := range []string{
		"CSV 单行内容不能超过 1MB",
		"CSV 单个单元格内容不能超过 512KB",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestValidateBytesUsesFileTypeWhenNameHasNoExtension(t *testing.T) {
	err := ValidateBytes("download", "json", make([]byte, mb+1))
	if err == nil {
		t.Fatal("expected validation error")
	}
	want := "JSON 文件大小不能超过 1MB"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("message missing %q: %s", want, err.Error())
	}
}

func TestValidateBytesAudioLimitsCoverOneHourRecording(t *testing.T) {
	if err := ValidateBytes("meeting.mp3", "mp3", make([]byte, 100*mb)); err != nil {
		t.Fatalf("expected 100MB mp3 to pass: %s", err.Error())
	}
	err := ValidateBytes("meeting.mp3", "mp3", make([]byte, 100*mb+1))
	if err == nil {
		t.Fatal("expected mp3 just above 100MB to fail")
	}
	want := "音频文件大小不能超过 100MB"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("message missing %q: %s", want, err.Error())
	}
}

func TestValidateBytesFLACLimit(t *testing.T) {
	err := ValidateBytes("meeting.flac", "flac", make([]byte, 100*mb+1))
	if err == nil {
		t.Fatal("expected flac above 100MB to fail")
	}
	want := "音频文件大小不能超过 100MB"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("message missing %q: %s", want, err.Error())
	}
}

func TestValidateBytesRejectsOfficeTempFile(t *testing.T) {
	err := ValidateBytes("~$draft.docx", "docx", zipBytes(t, map[string]string{
		"word/document.xml": `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"/>`,
	}))
	if err == nil {
		t.Fatal("expected validation error")
	}
	want := "该文件是 Office 临时文件，不能上传，请关闭文档后选择正式文件上传"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("message missing %q: %s", want, err.Error())
	}
}

func TestAnalyzeSizeMarksHeavyBeforeHardLimit(t *testing.T) {
	report := AnalyzeSize("report.pdf", "pdf", 10*mb+1)
	if report.ValidationError() != nil {
		t.Fatalf("expected pdf below hard limit to pass: %s", report.ValidationError())
	}
	if !report.IsHeavy() {
		t.Fatalf("expected pdf above heavy threshold to be marked heavy")
	}

	report = AnalyzeSize("report.pdf", "pdf", 20*mb+1)
	if report.ValidationError() == nil {
		t.Fatal("expected pdf above hard limit to fail")
	}
	if !report.IsHeavy() {
		t.Fatalf("expected pdf above hard limit to also be marked heavy")
	}
}

func TestAnalyzeBytesCSVMarksHeavyWithoutHardFailure(t *testing.T) {
	var data strings.Builder
	for i := 0; i < 50001; i++ {
		data.WriteString("a\n")
	}

	report := AnalyzeBytes("rows.csv", "csv", []byte(data.String()))
	if err := report.ValidationError(); err != nil {
		t.Fatalf("expected csv below hard limits to pass: %s", err.Error())
	}
	if !report.IsHeavy() {
		t.Fatal("expected csv row count above heavy threshold to be marked heavy")
	}
}

func TestAnalyzeBytesXLSXMarksHeavyWithoutHardFailure(t *testing.T) {
	var sheet strings.Builder
	sheet.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for row := 1; row <= 15001; row++ {
		sheet.WriteString(fmt.Sprintf(`<row r="%d">`, row))
		for col := 1; col <= 3; col++ {
			sheet.WriteString(fmt.Sprintf(`<c r="%s%d"><v>%d</v></c>`, columnName(col), row, row))
		}
		sheet.WriteString(`</row>`)
	}
	sheet.WriteString(`</sheetData></worksheet>`)

	report := AnalyzeBytes("rows.xlsx", "xlsx", zipBytes(t, map[string]string{
		"xl/worksheets/sheet1.xml": sheet.String(),
	}))
	if err := report.ValidationError(); err != nil {
		t.Fatalf("expected xlsx below hard limits to pass: %s", err.Error())
	}
	if !report.IsHeavy() {
		t.Fatal("expected xlsx row count above heavy threshold to be marked heavy")
	}
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func columnName(col int) string {
	name := ""
	for col > 0 {
		col--
		name = string(rune('A'+col%26)) + name
		col /= 26
	}
	return name
}
