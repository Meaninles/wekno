package vlmguard

import (
	"strings"
	"unicode"

	"github.com/Tencent/WeKnora/internal/custom/modules/ocrstructure"
)

const (
	runawayMinimumRunes = 1600
	runawayCheckStep    = 256
	runawayMinimumBlock = 128
	runawayMaximumBlock = 1024
	runawayRepeats      = 4
)

type repetitionDetector struct {
	lastCheckedRunes int
}

func (detector *repetitionDetector) Observe(content string) bool {
	runes := []rune(content)
	if len(runes) >= 512 && ocrstructure.HasRunawayEmptyTableTail(content) {
		return true
	}
	if len(runes) < runawayMinimumRunes ||
		len(runes)-detector.lastCheckedRunes < runawayCheckStep {
		return false
	}
	detector.lastCheckedRunes = len(runes)
	return hasRepeatedSuffix(runes)
}

func hasRepeatedSuffix(content []rune) bool {
	maximumBlock := len(content) / runawayRepeats
	if maximumBlock > runawayMaximumBlock {
		maximumBlock = runawayMaximumBlock
	}
	for blockSize := runawayMinimumBlock; blockSize <= maximumBlock; blockSize++ {
		start := len(content) - blockSize*runawayRepeats
		block := content[start : start+blockSize]
		if !hasMeaningfulText(block) {
			continue
		}
		repeated := true
		for repeat := 1; repeat < runawayRepeats; repeat++ {
			candidateStart := start + repeat*blockSize
			if !equalRunes(block, content[candidateStart:candidateStart+blockSize]) {
				repeated = false
				break
			}
		}
		if repeated {
			return true
		}
	}
	return false
}

func hasMeaningfulText(content []rune) bool {
	var meaningful int
	for _, char := range content {
		if unicode.IsLetter(char) || unicode.IsNumber(char) {
			meaningful++
		}
	}
	return meaningful >= len(content)/4 &&
		strings.TrimSpace(string(content)) != ""
}

func equalRunes(left, right []rune) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
