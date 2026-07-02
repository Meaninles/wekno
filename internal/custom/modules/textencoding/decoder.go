package textencoding

import (
	"bytes"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	xunicode "golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// DecodeText converts common text uploads to UTF-8 while preserving the old
// byte-as-string fallback for unknown legacy encodings.
func DecodeText(data []byte) string {
	text, _ := DecodeTextWithEncoding(data)
	return text
}

// DecodeTextBytes returns DecodeText as UTF-8 bytes for parsers that require
// valid UTF-8 input, such as encoding/json.
func DecodeTextBytes(data []byte) []byte {
	return []byte(DecodeText(data))
}

// DecodeTextBytesWithEncoding returns DecodeTextWithEncoding as UTF-8 bytes.
// Unsupported legacy bytes are preserved by the same raw fallback.
func DecodeTextBytesWithEncoding(data []byte) ([]byte, string) {
	text, encodingName := DecodeTextWithEncoding(data)
	return []byte(text), encodingName
}

// DecodeTextWithEncoding decodes the supported upload encodings only:
// UTF-8, UTF-8 BOM, GB18030/GBK/GB2312, and UTF-16LE/BE. The encoding label
// is intended for tests/logging.
func DecodeTextWithEncoding(data []byte) (string, string) {
	if len(data) == 0 {
		return "", "empty"
	}
	if hasPrefix(data, 0xEF, 0xBB, 0xBF) {
		return string(data[3:]), "utf-8-bom"
	}
	if hasPrefix(data, 0xFF, 0xFE) || hasPrefix(data, 0xFE, 0xFF) {
		if decoded, ok := decodeUTF16BOM(data); ok {
			return decoded, "utf-16-bom"
		}
	}
	if decoded, encodingName, ok := decodeLikelyUTF16(data); ok {
		return decoded, encodingName
	}
	if utf8.Valid(data) {
		return string(data), "utf-8"
	}
	if decoded, ok := decodeLikelyGB18030(data); ok {
		return decoded, "gb18030"
	}
	return string(data), "legacy-raw"
}

func hasPrefix(data []byte, prefix ...byte) bool {
	return len(data) >= len(prefix) && bytes.Equal(data[:len(prefix)], prefix)
}

func decodeUTF16BOM(data []byte) (string, bool) {
	return decodeUTF16(data, xunicode.BigEndian, xunicode.ExpectBOM)
}

func decodeLikelyUTF16(data []byte) (string, string, bool) {
	if looksLikeUTF16LE(data) {
		if decoded, ok := decodeUTF16(data, xunicode.LittleEndian, xunicode.IgnoreBOM); ok {
			return decoded, "utf-16le", true
		}
	}
	if looksLikeUTF16BE(data) {
		if decoded, ok := decodeUTF16(data, xunicode.BigEndian, xunicode.IgnoreBOM); ok {
			return decoded, "utf-16be", true
		}
	}
	return "", "", false
}

func decodeUTF16(data []byte, endian xunicode.Endianness, bom xunicode.BOMPolicy) (string, bool) {
	if len(data)%2 != 0 {
		return "", false
	}
	decoded, err := io.ReadAll(transform.NewReader(
		bytes.NewReader(data),
		xunicode.UTF16(endian, bom).NewDecoder(),
	))
	if err != nil || !utf8.Valid(decoded) {
		return "", false
	}
	text := string(decoded)
	if !validDecodedUTF16Text(text) {
		return "", false
	}
	return text, true
}

func looksLikeUTF16LE(data []byte) bool {
	pairs, evenZero, oddZero := utf16ZeroPattern(data)
	return pairs >= 8 && oddZero*100/pairs >= 20 && evenZero*100/pairs <= 5
}

func looksLikeUTF16BE(data []byte) bool {
	pairs, evenZero, oddZero := utf16ZeroPattern(data)
	return pairs >= 8 && evenZero*100/pairs >= 20 && oddZero*100/pairs <= 5
}

func utf16ZeroPattern(data []byte) (pairs, evenZero, oddZero int) {
	limit := len(data)
	if limit > 4096 {
		limit = 4096
	}
	if limit%2 == 1 {
		limit--
	}
	for i := 0; i+1 < limit; i += 2 {
		pairs++
		if data[i] == 0 {
			evenZero++
		}
		if data[i+1] == 0 {
			oddZero++
		}
	}
	return pairs, evenZero, oddZero
}

func validDecodedUTF16Text(text string) bool {
	if text == "" || strings.ContainsRune(text, '\x00') {
		return false
	}
	total := 0
	bad := 0
	for _, r := range text {
		total++
		if r == utf8.RuneError {
			bad++
			continue
		}
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			bad++
		}
	}
	return total > 0 && bad*100/total <= 1
}

func decodeLikelyGB18030(data []byte) (string, bool) {
	decoded, err := io.ReadAll(transform.NewReader(
		bytes.NewReader(data),
		simplifiedchinese.GB18030.NewDecoder(),
	))
	if err != nil || !utf8.Valid(decoded) {
		return "", false
	}
	text := string(decoded)
	if !looksLikeRecoveredChinese(data, text) {
		return "", false
	}
	return text, true
}

func looksLikeRecoveredChinese(data []byte, decoded string) bool {
	cjk := countCJK(decoded)
	if cjk < 2 {
		return false
	}
	if countGBKTwoByteSequences(data) < 2 {
		return false
	}
	legacyClean := strings.ToValidUTF8(string(data), "")
	return cjk > countCJK(legacyClean)
}

func countCJK(s string) int {
	n := 0
	for _, r := range s {
		switch {
		case r >= 0x3400 && r <= 0x4DBF:
			n++
		case r >= 0x4E00 && r <= 0x9FFF:
			n++
		case r >= 0xF900 && r <= 0xFAFF:
			n++
		}
	}
	return n
}

func countGBKTwoByteSequences(data []byte) int {
	n := 0
	for i := 0; i+1 < len(data); {
		lead := data[i]
		trail := data[i+1]
		if lead >= 0x81 && lead <= 0xFE &&
			trail >= 0x40 && trail <= 0xFE &&
			trail != 0x7F {
			n++
			i += 2
			continue
		}
		i++
	}
	return n
}
