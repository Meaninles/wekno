package vlmguard

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRepetitionDetectorRejectsRepeatedSuffix(t *testing.T) {
	prefix := strings.Repeat("正常识别文本", 400)
	block := strings.Repeat("模型开始重复这一整段内容", 16)
	content := prefix + strings.Repeat(block, runawayRepeats)

	var detector repetitionDetector
	require.True(t, detector.Observe(content))
}

func TestRepetitionDetectorAllowsLongNonRepeatingOCR(t *testing.T) {
	var builder strings.Builder
	for index := 0; index < 500; index++ {
		builder.WriteString("第")
		builder.WriteRune(rune('一' + index%20))
		builder.WriteString("项具有不同字段与数值")
		builder.WriteRune(rune(0x4e00 + index))
		builder.WriteByte('\n')
	}

	var detector repetitionDetector
	require.False(t, detector.Observe(builder.String()))
}
func TestRepetitionDetectorRejectsEmptyMarkdownTableTail(t *testing.T) {
	content := strings.Repeat("有效正文内容", 80) + "\n" + strings.Repeat("| | | |\n", 20)
	var detector repetitionDetector
	require.True(t, detector.Observe(content))
}
