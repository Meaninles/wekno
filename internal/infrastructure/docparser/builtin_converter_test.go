package docparser

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

func TestSimpleFormatReaderCSV_UTF8StillWorks(t *testing.T) {
	reader := &SimpleFormatReader{}
	input := "time,active_count,cash_ratio\n2020-01-02 16:00:00,0,1.29\n"

	result, err := reader.Read(context.Background(), &types.ReadRequest{
		FileName:    "result.csv",
		FileType:    "csv",
		FileContent: []byte(input),
	})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if result == nil || !strings.Contains(result.MarkdownContent, "| time | active_count | cash_ratio |") {
		if result == nil {
			t.Fatalf("Read() result is nil")
		}
		t.Fatalf("Read() markdown = %q, want UTF-8 CSV header preserved", result.MarkdownContent)
	}
}

func TestSimpleFormatReaderCSV_GB18030Header(t *testing.T) {
	reader := &SimpleFormatReader{}
	input := "时间,基准收益,策略收益,当日盈利,当日亏损,当日买入,当日卖出,超额收益(%),active_count,cash_ratio\n2020-01-02 16:00:00,1.29,0,0,0,0,0,-1.27,0,1\n"
	data, _, err := transform.Bytes(
		simplifiedchinese.GB18030.NewEncoder(),
		[]byte(input),
	)
	if err != nil {
		t.Fatalf("encode GB18030 fixture: %v", err)
	}

	result, err := reader.Read(context.Background(), &types.ReadRequest{
		FileName:    "result.csv",
		FileType:    "csv",
		FileContent: data,
	})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if result == nil {
		t.Fatalf("Read() result is nil")
	}
	for _, want := range []string{"| 时间 | 基准收益 | 策略收益 | 当日盈利 |", "超额收益(%)", "active_count", "cash_ratio"} {
		if !strings.Contains(result.MarkdownContent, want) {
			t.Fatalf("Read() markdown missing %q:\n%s", want, result.MarkdownContent)
		}
	}
	if strings.Contains(result.MarkdownContent, "ʱ") || strings.Contains(result.MarkdownContent, "׼") {
		t.Fatalf("Read() markdown still contains mojibake:\n%s", result.MarkdownContent)
	}
}
