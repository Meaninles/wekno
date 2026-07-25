package questiondedup

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrepareNormalizesDuplicatePresentation(t *testing.T) {
	first, ok := Prepare("batch:chunk-a:0", "1. 采购审批的完成时限是什么？")
	require.True(t, ok)
	second, ok := Prepare("batch:chunk-b:1", "采购审批的完成时限是什么?")
	require.True(t, ok)
	require.Equal(t, first.QuestionHash, second.QuestionHash)
	require.Equal(t, "采购审批的完成时限是什么？", first.Question)
}

func TestPrepareRejectsGenerationMetadata(t *testing.T) {
	for _, question := range []string{
		"根据原文件第 18 页，采购审批时限是什么？",
		"根据《采购管理办法》，供应商准入条件是什么？",
		"依据主数据管理规定中的要求，谁负责监督考核？",
		"第3组内容说明了哪些监督要求？",
		"文档doc-cluster-001.md的第2节验证了哪些核心功能？",
		"doc-cluster-001.md主要说明了哪些处理能力？",
		"第二章介绍了哪些审批要求？",
		"第二十条规定的内容是什么？",
		"上述文档中规定了哪些责任？",
		"制度原文中列出了什么？",
	} {
		_, ok := Prepare("claim", question)
		require.False(t, ok, question)
	}
}

func TestSuperficialParaphraseRejectsTinyStemEdits(t *testing.T) {
	first := Normalize("采购管理制度的第二节规定了哪些审批要求？")
	second := Normalize("采购管理制度的第八节规定了哪些审批要求？")
	require.True(t, IsSuperficialParaphrase(first, second))

	require.False(t, IsSuperficialParaphrase(
		Normalize("采购审批必须在多长时间内完成？"),
		Normalize("发生紧急事件时可以适用哪些例外程序？"),
	))
	require.False(t, IsSuperficialParaphrase(Normalize("时限几天？"), Normalize("金额多少？")))
}

func TestPrepareKeepsNaturalPolicyQuestions(t *testing.T) {
	for _, question := range []string{
		"采购审批必须在多长时间内完成？",
		"哪些情形禁止未经授权导出客户数据？",
		"发生紧急事件时可以适用哪些例外审批程序？",
		"项目负责人和监督部门分别承担哪些职责？",
	} {
		candidate, ok := Prepare("claim", question)
		require.True(t, ok, question)
		require.NotEmpty(t, candidate.QuestionHash)
	}
}
