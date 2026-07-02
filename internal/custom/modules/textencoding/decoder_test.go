package textencoding

import (
	"testing"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	xunicode "golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

func TestDecodeTextWithEncoding_UTF8Unchanged(t *testing.T) {
	input := "time,return\n2020-01-02,1.23"

	got, enc := DecodeTextWithEncoding([]byte(input))
	if got != input {
		t.Fatalf("DecodeTextWithEncoding() text = %q, want %q", got, input)
	}
	if enc != "utf-8" {
		t.Fatalf("DecodeTextWithEncoding() encoding = %q, want utf-8", enc)
	}
}

func TestDecodeTextWithEncoding_UTF8BOM(t *testing.T) {
	data := append([]byte{0xEF, 0xBB, 0xBF}, []byte("name,value")...)

	got, enc := DecodeTextWithEncoding(data)
	if got != "name,value" {
		t.Fatalf("DecodeTextWithEncoding() text = %q, want BOM stripped text", got)
	}
	if enc != "utf-8-bom" {
		t.Fatalf("DecodeTextWithEncoding() encoding = %q, want utf-8-bom", enc)
	}
}

func TestDecodeTextWithEncoding_UTF16LEWithBOM(t *testing.T) {
	input := "时间,基准收益,active_count,cash_ratio\n2020-01-02 16:00:00,1.29,0,1\n"
	data := encodeFixture(t, xunicode.UTF16(xunicode.LittleEndian, xunicode.UseBOM).NewEncoder(), input)

	got, enc := DecodeTextWithEncoding(data)
	if got != input {
		t.Fatalf("DecodeTextWithEncoding() text = %q, want %q", got, input)
	}
	if enc != "utf-16-bom" {
		t.Fatalf("DecodeTextWithEncoding() encoding = %q, want utf-16-bom", enc)
	}
}

func TestDecodeTextWithEncoding_UTF16BEWithBOM(t *testing.T) {
	input := "时间,基准收益,active_count,cash_ratio\n2020-01-02 16:00:00,1.29,0,1\n"
	data := encodeFixture(t, xunicode.UTF16(xunicode.BigEndian, xunicode.UseBOM).NewEncoder(), input)

	got, enc := DecodeTextWithEncoding(data)
	if got != input {
		t.Fatalf("DecodeTextWithEncoding() text = %q, want %q", got, input)
	}
	if enc != "utf-16-bom" {
		t.Fatalf("DecodeTextWithEncoding() encoding = %q, want utf-16-bom", enc)
	}
}

func TestDecodeTextWithEncoding_UTF16LENoBOM(t *testing.T) {
	input := "时间,基准收益,active_count,cash_ratio\n2020-01-02 16:00:00,1.29,0,1\n"
	data := encodeFixture(t, xunicode.UTF16(xunicode.LittleEndian, xunicode.IgnoreBOM).NewEncoder(), input)

	got, enc := DecodeTextWithEncoding(data)
	if got != input {
		t.Fatalf("DecodeTextWithEncoding() text = %q, want %q", got, input)
	}
	if enc != "utf-16le" {
		t.Fatalf("DecodeTextWithEncoding() encoding = %q, want utf-16le", enc)
	}
}

func TestDecodeTextWithEncoding_UTF16BENoBOM(t *testing.T) {
	input := "时间,基准收益,active_count,cash_ratio\n2020-01-02 16:00:00,1.29,0,1\n"
	data := encodeFixture(t, xunicode.UTF16(xunicode.BigEndian, xunicode.IgnoreBOM).NewEncoder(), input)

	got, enc := DecodeTextWithEncoding(data)
	if got != input {
		t.Fatalf("DecodeTextWithEncoding() text = %q, want %q", got, input)
	}
	if enc != "utf-16be" {
		t.Fatalf("DecodeTextWithEncoding() encoding = %q, want utf-16be", enc)
	}
}

func TestDecodeTextWithEncoding_GB18030(t *testing.T) {
	input := "时间,基准收益,策略收益,超额收益(%)"
	data := encodeFixture(t, simplifiedchinese.GB18030.NewEncoder(), input)

	got, enc := DecodeTextWithEncoding(data)
	if got != input {
		t.Fatalf("DecodeTextWithEncoding() text = %q, want %q", got, input)
	}
	if enc != "gb18030" {
		t.Fatalf("DecodeTextWithEncoding() encoding = %q, want gb18030", enc)
	}
}

func TestDecodeTextWithEncoding_UnknownLegacyKeepsRawFallback(t *testing.T) {
	data := []byte{'c', 'a', 'f', 0xE9}

	got, enc := DecodeTextWithEncoding(data)
	if got != string(data) {
		t.Fatalf("DecodeTextWithEncoding() text = %q, want raw fallback", got)
	}
	if enc != "legacy-raw" {
		t.Fatalf("DecodeTextWithEncoding() encoding = %q, want legacy-raw", enc)
	}
	if utf8.ValidString(got) {
		t.Fatalf("raw fallback should preserve previous invalid UTF-8 behavior")
	}
}

func encodeFixture(t *testing.T, transformer transform.Transformer, input string) []byte {
	t.Helper()
	data, _, err := transform.Bytes(transformer, []byte(input))
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return data
}
