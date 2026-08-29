package sourcerefs

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func repairTestRefs() []*types.SearchResult {
	return []*types.SearchResult{
		{
			ID: "chunk-36", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "第三十六条 竞价采购，是指在买方市场条件下，征集3家以上供应商，采购人对供应商多次竞争报价进行比较，最后确定价格最优的供应商的一种采购方式。",
			Metadata:        map[string]string{MetadataCitationID: "S9", MetadataChunkID: "chunk-36", "source_type": SourceTypeKnowledge},
		},
		{
			ID: "chunk-37", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "第三十七条 竞争谈判适用于采购需求只能提出功能性指标的项目。",
			Metadata:        map[string]string{MetadataCitationID: "S10", MetadataChunkID: "chunk-37", "source_type": SourceTypeKnowledge},
		},
	}
}

func TestRepairAnswerCitationsNormalizesOnlyKnownPlainAliasesOutsideCode(t *testing.T) {
	answer := "竞价定义。（S9） 竞价条件<S9> 竞争谈判<S10/> <src id=\"S9\"> <SRC id='S10' /> 未知项（S99） 代码 `（S10）`"
	got := RepairAnswerCitations(answer, repairTestRefs())
	if strings.Count(got, `<src id="S9" />`) != 3 || strings.Count(got, `<src id="S10" />`) != 2 || !strings.Contains(got, "（S99）") ||
		!strings.Contains(got, "`（S10）`") {
		t.Fatalf("alias normalization was not registry-safe: %q", got)
	}
}

func TestRepairAnswerCitationsAttachesExactUnambiguousEvidence(t *testing.T) {
	answer := "## 竞价采购\n\n竞价采购，是指在买方市场条件下，征集3家以上供应商，采购人对供应商多次竞争报价进行比较，最后确定价格最优的供应商的一种采购方式。"
	got := RepairAnswerCitations(answer, repairTestRefs())
	if strings.Count(got, `<src id="S9" />`) != 1 || strings.Contains(got, `<src id="S10" />`) {
		t.Fatalf("exact evidence was not repaired minimally: %q", got)
	}
}

func TestRepairAnswerCitationsDoesNotGuessAmbiguousOrUnrelatedEvidence(t *testing.T) {
	answer := "采购方式需要综合考虑项目情况后确定。"
	if got := RepairAnswerCitations(answer, repairTestRefs()); got != answer {
		t.Fatalf("unrelated answer received a guessed citation: %q", got)
	}
	ambiguous := []*types.SearchResult{
		{ID: "a", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText), EvidenceContent: "候选供应商公示期不少于3日。", Metadata: map[string]string{MetadataCitationID: "S1", MetadataChunkID: "a", "source_type": SourceTypeKnowledge}},
		{ID: "b", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText), EvidenceContent: "候选供应商公示期不少于3日。", Metadata: map[string]string{MetadataCitationID: "S2", MetadataChunkID: "b", "source_type": SourceTypeKnowledge}},
	}
	claim := "候选供应商公示期不少于3日。"
	if got := RepairAnswerCitations(claim, ambiguous); got != claim {
		t.Fatalf("ambiguous evidence received a guessed citation: %q", got)
	}
}

func TestRepairAnswerCitationsAddsMissingDefinitionBindingBesideExistingConditionCitation(t *testing.T) {
	refs := append(repairTestRefs(), &types.SearchResult{
		ID: "chunk-37-definition", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1", ChunkType: string(types.ChunkTypeText),
		EvidenceContent: "第三十七条 竞争谈判是指采购人与二家以上符合资格条件的供应商洽谈确定供应商的采购方式。",
		Metadata:        map[string]string{MetadataCitationID: "S11", MetadataChunkID: "chunk-37-definition", "source_type": SourceTypeKnowledge},
	})
	answer := "**竞争谈判**\n竞争谈判是指采购人与二家以上符合资格条件的供应商洽谈确定供应商的采购方式。其适用重点是采购需求只能提出功能性指标。<src id=\"S10\" />"

	got := RepairAnswerCitations(answer, refs)
	if !strings.Contains(got, `采购方式。<src id="S11" />其适用重点`) {
		t.Fatalf("missing definition binding was not repaired adjacent to its sentence: %q", got)
	}
	if strings.Count(got, `<src id="S10" />`) != 1 {
		t.Fatalf("existing condition citation was changed: %q", got)
	}
}

func TestRepairAnswerCitationsDoesNotDuplicateParagraphSourceAlreadyCitedAtEnd(t *testing.T) {
	answer := "竞价采购，是指在买方市场条件下，征集3家以上供应商，采购人对供应商多次竞争报价进行比较。最后确定价格最优的供应商。<src id=\"S9\" />"
	got := RepairAnswerCitations(answer, repairTestRefs())
	if strings.Count(got, `<src id="S9" />`) != 1 {
		t.Fatalf("paragraph source was duplicated: %q", got)
	}
}

func TestRepairAnswerCitationsMovesSourceTitleHandleBesideSupportedClaim(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID: "goods-threshold", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "第三十四条 依法必须招标的重要设备、材料等货物的采购，单项合同估算价在200万元（含）以上的，必须公开招标。",
			Metadata:        map[string]string{MetadataCitationID: "S1", MetadataChunkID: "goods-threshold", "source_type": SourceTypeKnowledge},
		},
	}
	claim := "根据《采购管理办法》第三十四条，依法必须招标的重要设备、材料等货物，单项合同估算价在200万元（含）以上的，必须公开招标。"
	answer := claim + "\n\n📄《采购管理办法》第三十四条第（一）款第1项第（2）目。<src id=\"S1\" />"

	got := RepairAnswerCitations(answer, refs)
	if !strings.Contains(got, claim+`<src id="S1" />`) {
		t.Fatalf("source handle was not moved beside the supported claim: %q", got)
	}
	parts := strings.Split(got, "\n\n")
	if len(parts) != 2 || strings.Contains(parts[1], "<src") {
		t.Fatalf("source-title paragraph retained the non-adjacent handle: %q", got)
	}
}

func TestRepairAnswerCitationsMovesQuestionHandleToSupportedAnswer(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID: "publication-period", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "中标候选人公示期应不少于3日。",
			Metadata:        map[string]string{MetadataCitationID: "S3", MetadataChunkID: "publication-period", "source_type": SourceTypeKnowledge},
		},
	}
	question := `**1. 中标候选人公示至少多少日？**<src id="S3" />`
	claim := "中标候选人公示期应不少于 **3日（日历日）**。"
	answer := question + "\n\n" + claim + "📄《采购管理办法》成交供应商公示条款。"

	got := RepairAnswerCitations(answer, refs)
	parts := strings.Split(got, "\n\n")
	if len(parts) != 2 || strings.Contains(parts[0], "<src") {
		t.Fatalf("question retained a non-claim citation: %q", got)
	}
	if !strings.Contains(parts[1], claim+`<src id="S3" />`) {
		t.Fatalf("question citation was not moved beside the supported answer: %q", got)
	}
	if twice := RepairAnswerCitations(got, refs); twice != got {
		t.Fatalf("question citation repair was not idempotent: %q", twice)
	}
}

func TestRepairAnswerCitationsRepairsEvidenceSentencesInsideMixedParagraphs(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID: "inquiry", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "第三十五条 询比采购是在采购需求确定的条件下一次性报出不可更改价格，经评审确定供应商。适用于行业规范、收费标准统一的服务事项。",
			Metadata:        map[string]string{MetadataCitationID: "S1", MetadataChunkID: "inquiry", "source_type": SourceTypeKnowledge},
		},
		{
			ID: "auction", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "第三十六条 竞价采购适用于采购需求明确、规格型号同一，或者服务标准要求完整的项目。",
			Metadata:        map[string]string{MetadataCitationID: "S2", MetadataChunkID: "auction", "source_type": SourceTypeKnowledge},
		},
		{
			ID: "negotiation", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "竞争谈判适用于采购需求只能提出功能性指标、采购目标明确但可以有不同路径和方案实现的项目。",
			Metadata:        map[string]string{MetadataCitationID: "S3", MetadataChunkID: "negotiation", "source_type": SourceTypeKnowledge},
		},
	}
	answer := "已确认：系统升级服务预算220万元。\n\n" +
		"**询比采购**：在采购需求确定的条件下一次性报出不可更改价格。项目需求尚未确认完整，因此存在适配风险。\n\n" +
		"**竞价采购**：适用于采购需求明确、规格型号同一或服务标准要求完整的项目。项目是否标准化仍未知。\n\n" +
		"**竞争谈判**：适用于只能提出功能性指标、目标明确但可以有不同路径和方案实现的项目。是否采用仍待确认。"

	got := RepairAnswerCitations(answer, refs)
	for _, id := range []string{"S1", "S2", "S3"} {
		if strings.Count(got, canonicalCitationTag(id)) != 1 {
			t.Fatalf("expected exactly one repaired %s citation: %q", id, got)
		}
	}
	if strings.Contains(strings.Split(got, "\n\n")[0], "<src") {
		t.Fatalf("conversation-only fact received a document citation: %q", got)
	}
}

func TestRepairNamedTopicCitationBindingsReplacesOnlyUniqueStrongMismatch(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID: "public", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "公开采购的方式包括公开询比采购、公开竞价采购、公开谈判采购。选择公开采购应满足采购信息可以公开、采购时间允许。",
			Metadata:        map[string]string{MetadataCitationID: "S1", MetadataChunkID: "public", "source_type": SourceTypeKnowledge},
		},
		{
			ID: "auction", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "第三十六条 竞价采购适用于采购需求明确、规格型号同一、货源充足竞争充分、价格稳定或价格形成机制明确，采购标的物以价格竞争为主；或者服务标准要求完整。",
			Metadata:        map[string]string{MetadataCitationID: "S7", MetadataChunkID: "auction", "source_type": SourceTypeKnowledge},
		},
		{
			ID: "negotiation-tail", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "采购目标总体明确但可以有不同路径和方案实现，采购人需要和供应商通过对话确定采购路径。",
			Metadata:        map[string]string{MetadataCitationID: "S9", MetadataChunkID: "negotiation-tail", "source_type": SourceTypeKnowledge},
		},
	}
	answer := "竞价：制度条件为采购需求明确、规格型号同一、货源充足竞争充分、价格稳定或价格形成机制明确，以价格竞争为主；或者服务标准要求完整。<src id=\"S9\" />"
	got := RepairNamedTopicCitationBindings(answer, []string{"公开采购", "询比", "竞价", "竞争谈判"}, refs)
	if !strings.Contains(got, `<src id="S7" />`) || strings.Contains(got, `<src id="S9" />`) {
		t.Fatalf("wrong named-topic citation was not conservatively rebound: %q", got)
	}

	if unchanged := RepairNamedTopicCitationBindings(got, []string{"公开采购", "询比", "竞价", "竞争谈判"}, refs); unchanged != got {
		t.Fatalf("correct named-topic citation was not idempotent: %q", unchanged)
	}
}

func TestRepairNamedTopicCitationBindingsMovesSharedDefinitionCitation(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID: "definitions", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "第三十三条 采购方式按照是否邀请特定供应商参与采购项目分为公开采购和邀请采购两类。公开采购是指采购人以采购公告方式邀请不特定的潜在供应商参与采购项目的方式；邀请采购是指采购人发出采购邀请书邀请特定的供应商或3家以上的潜在供应商参与采购项目的方式。",
			Metadata: map[string]string{
				MetadataCitationID: "S1", MetadataChunkID: "definitions", "source_type": SourceTypeKnowledge,
			},
		},
	}
	answer := "根据《采购管理办法》第三十三条：<src id=\"S1\" />\n\n" +
		"**公开采购**——采购人以**采购公告**方式邀请**不特定**的潜在供应商参与采购项目。📄《采购管理办法》第六章第三十三条。<src id=\"S1\" />\n\n" +
		"**邀请采购**——采购人发出**采购邀请书**邀请**特定的供应商或3家以上的潜在供应商**参与采购项目。📄《采购管理办法》第六章第三十三条。"
	evidence := repairEvidence(refs)
	if !namedDefinitionEvidence(evidence[0].content, "邀请采购") {
		t.Fatal("shared source was not recognized as direct invitation-procurement definition evidence")
	}
	if id := uniqueNamedDefinitionEvidenceForParagraph("邀请采购", strings.Split(answer, "\n\n")[2], evidence); id != "S1" {
		t.Fatalf("shared definition evidence was not uniquely matched, got %q", id)
	}
	if donor := strings.Split(answer, "\n\n")[0]; !isSourceAttributionParagraph(donor) ||
		len(repairTopicsForParagraph(donor, []string{"公开采购", "邀请采购"})) != 0 {
		t.Fatalf("source-only donor paragraph was not recognized: %q", donor)
	}
	paragraphs := strings.Split(answer, "\n\n")
	if gotTopics := repairTopicsForParagraph(paragraphs[2], []string{"公开采购", "邀请采购"}); len(gotTopics) != 1 || gotTopics[0] != "邀请采购" || canonicalSourceTagRE.MatchString(paragraphs[2]) {
		t.Fatalf("uncited invitation paragraph was not recognized: topics=%v paragraph=%q", gotTopics, paragraphs[2])
	}
	if _, present := citationIDsInText(paragraphs[0])["S1"]; !present {
		t.Fatalf("source-only donor did not expose S1: %q", paragraphs[0])
	}
	if isSourceAttributionParagraph(paragraphs[1]) || isSourceAttributionParagraph(paragraphs[2]) {
		t.Fatal("substantive definition with an inline source title was misclassified as source-only")
	}
	if relocated := relocateUnscopedNamedTopicCitation(answer, []string{"公开采购", "邀请采购"}, evidence); relocated == answer {
		t.Fatalf("direct shared-definition relocation did not change the answer")
	}

	got := RepairNamedTopicCitationBindings(answer, []string{"公开采购", "邀请采购"}, refs)
	if !strings.Contains(strings.Split(got, "\n\n")[2], `<src id="S1" />`) {
		t.Fatalf("named-topic pass lost the relocated definition citation: %s", got)
	}
	got = RepairAnswerCitations(got, refs)
	paragraphs = strings.Split(got, "\n\n")
	if !strings.Contains(paragraphs[2], `<src id="S1" />`) {
		t.Fatalf("shared evidence was not attached to the second definition: %s", got)
	}
	if strings.Contains(paragraphs[0], `<src id="S1" />`) {
		t.Fatalf("unscoped source-preface citation was not moved: %s", got)
	}
	if strings.Count(got, `<src id="S1" />`) != 2 {
		t.Fatalf("citation relocation changed the bounded citation count: %s", got)
	}
	if twice := RepairNamedTopicCitationBindings(got, []string{"公开采购", "邀请采购"}, refs); twice != got {
		t.Fatalf("shared definition citation relocation is not idempotent: %s", twice)
	}
}

func TestRepairNamedTopicCitationBindingsLeavesAmbiguousEvidenceUntouched(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID: "auction-a", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "竞价采购适用于采购需求明确、规格型号同一的项目。",
			Metadata:        map[string]string{MetadataCitationID: "S1", MetadataChunkID: "auction-a", "source_type": SourceTypeKnowledge},
		},
		{
			ID: "auction-b", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "竞价采购适用于采购需求明确、规格型号同一的项目。",
			Metadata:        map[string]string{MetadataCitationID: "S2", MetadataChunkID: "auction-b", "source_type": SourceTypeKnowledge},
		},
		{
			ID: "wrong", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "竞争谈判适用于技术复杂的项目。",
			Metadata:        map[string]string{MetadataCitationID: "S3", MetadataChunkID: "wrong", "source_type": SourceTypeKnowledge},
		},
	}
	answer := `竞价：适用于采购需求明确、规格型号同一的项目。<src id="S3" />`
	if got := RepairNamedTopicCitationBindings(answer, []string{"竞价", "竞争谈判"}, refs); got != answer {
		t.Fatalf("ambiguous evidence was guessed: %q", got)
	}
}

func TestRepairNamedTopicCitationBindingsUsesDirectConditionPassage(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID: "procedure", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "竞争谈判采购递交文件的供应商有2家及以上即可启动谈判程序。",
			Metadata:        map[string]string{MetadataCitationID: "S1", MetadataChunkID: "procedure", "source_type": SourceTypeKnowledge},
		},
		{
			ID: "conditions", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "适宜采用竞争谈判的采购方式，且符合下列特定条件之一：1.技术复杂，只能提出功能性指标；2.采购目标明确但可以有不同路径和方案实现。",
			Metadata:        map[string]string{MetadataCitationID: "S2", MetadataChunkID: "conditions", "source_type": SourceTypeKnowledge},
		},
	}
	answer := "竞争谈判：制度条件为2家以上即可启动谈判程序。<src id=\"S1\" />；该直接条件在本项目中是否成立待确认。"
	got := RepairNamedTopicCitationBindings(answer, []string{"公开采购", "竞争谈判"}, refs)
	if !strings.Contains(got, `<src id="S2" />`) || strings.Contains(got, `<src id="S1" />`) ||
		!strings.Contains(got, "功能性指标") || !strings.Contains(got, "不同路径和方案") {
		t.Fatalf("procedure evidence was not replaced with the direct condition passage: %s", got)
	}
}

func TestRepairNamedTopicCitationBindingsPrefersFocusedSameDocumentConditionChunk(t *testing.T) {
	direct := "选择公开采购方式应同时满足下列条件：1.采购信息可以公开；2.采购标的具有竞争条件；3.采购时间允许；4.采购成本合理。"
	refs := []*types.SearchResult{
		{
			ID: "focused", KnowledgeID: "procurement-policy", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: direct,
			Metadata:        map[string]string{MetadataCitationID: "S1", MetadataChunkID: "focused", "source_type": SourceTypeKnowledge},
		},
		{
			ID: "aggregate", KnowledgeID: "procurement-policy", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: strings.Repeat("采购管理通则与流程说明。", 40) + direct + strings.Repeat("其他采购方式和审批流程。", 40),
			Metadata:        map[string]string{MetadataCitationID: "S2", MetadataChunkID: "aggregate", "source_type": SourceTypeKnowledge},
		},
		{
			ID: "wrong", KnowledgeID: "procurement-policy", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "竞价采购适用于采购需求明确、规格型号同一且以价格竞争为主的项目。",
			Metadata:        map[string]string{MetadataCitationID: "S8", MetadataChunkID: "wrong", "source_type": SourceTypeKnowledge},
		},
	}
	answer := "公开采购：制度条件为采购信息可以公开、标的具有竞争条件、采购时间允许且采购成本合理。<src id=\"S8\" />；该直接条件在本项目中是否成立待确认。"
	got := RepairNamedTopicCitationBindings(answer, []string{"公开采购", "询比", "竞价", "竞争谈判"}, refs)
	if !strings.Contains(got, `<src id="S1" />`) || strings.Contains(got, `<src id="S8" />`) ||
		!strings.Contains(got, "采购标的具有竞争条件") {
		t.Fatalf("focused condition chunk was not selected over its aggregate copy: %s", got)
	}
}

func TestRepairNamedTopicCitationBindingsFillsExplicitMissingEvidenceOnly(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID: "auction", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "竞价采购符合下列特定条件之一：1.采购需求明确、规格型号同一；2.服务标准要求完整。",
			Metadata:        map[string]string{MetadataCitationID: "S4", MetadataChunkID: "auction", "source_type": SourceTypeKnowledge},
		},
	}
	answer := "竞价采购：制度条件为未在检索信息中找到直接说明；该直接条件在本项目中是否成立待确认。"
	got := RepairNamedTopicCitationBindings(answer, []string{"询比采购", "竞价采购"}, refs)
	if !strings.Contains(got, `<src id="S4" />`) || strings.Contains(got, "未在检索信息中找到") ||
		!strings.Contains(got, "采购需求明确") {
		t.Fatalf("explicit missing-evidence fallback was not grounded: %s", got)
	}
}

func TestNamedTopicConditionEvidenceRejectsNeighboringMethodConditions(t *testing.T) {
	neighbor := "竞争谈判是指采购人与二家以上供应商洽谈确定供应商的采购方式。" +
		"采购项目满足邀请的采购条件，且符合下列特定条件的，适宜采用合作谈判的采购方式。"
	if namedTopicConditionEvidence(neighbor, "竞争谈判") {
		t.Fatal("a definition followed by a neighboring method's conditions was treated as direct evidence")
	}
	direct := "采购项目满足公开（邀请）的采购条件，且符合下列特定条件之一的，适宜采用公开（邀请）竞争谈判的采购方式：技术复杂只能提出功能性指标。"
	if !namedTopicConditionEvidence(direct, "竞争谈判") {
		t.Fatal("the target method's own condition sentence was rejected")
	}
}

func TestRepairNamedTopicCitationBindingsRebuildsMixedConditionCitations(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID: "definition", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "竞争谈判是指采购人与二家以上供应商洽谈确定供应商的采购方式。适宜采用合作谈判的采购方式。",
			Metadata:        map[string]string{MetadataCitationID: "S1", MetadataChunkID: "definition", "source_type": SourceTypeKnowledge},
		},
		{
			ID: "conditions", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "采购项目满足公开（邀请）的采购条件，且符合下列特定条件之一的，适宜采用公开（邀请）竞争谈判的采购方式：1.技术复杂，只能提出功能性指标；2.采购目标明确但可以有不同路径和方案实现。",
			Metadata:        map[string]string{MetadataCitationID: "S2", MetadataChunkID: "conditions", "source_type": SourceTypeKnowledge},
		},
	}
	answer := "竞争谈判：制度条件为技术复杂、功能性指标或不同路径方案。<src id=\"S2\" />" +
		"适用于谈判方式的其他采购。<src id=\"S1\" />；该直接条件在本项目中是否成立待确认。"
	got := RepairNamedTopicCitationBindings(answer, []string{"竞价", "竞争谈判"}, refs)
	if strings.Contains(got, `<src id="S1" />`) || strings.Contains(got, "适用于谈判方式的其他采购") ||
		strings.Count(got, `<src id="S2" />`) != 1 || !strings.Contains(got, "功能性指标") {
		t.Fatalf("mixed condition citations were not rebuilt from the unique direct passage: %s", got)
	}
}

func TestRepairNamedTopicCitationBindingsUsesLeadingTopicWithTrailingComparison(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID: "wrong", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "公开采购是指面向不特定供应商的采购方式。",
			Metadata:        map[string]string{MetadataCitationID: "S1", MetadataChunkID: "wrong", "source_type": SourceTypeKnowledge},
		},
		{
			ID: "auction", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "竞价采购符合下列特定条件之一：采购需求明确、规格型号同一；或者服务标准要求完整。",
			Metadata:        map[string]string{MetadataCitationID: "S2", MetadataChunkID: "auction", "source_type": SourceTypeKnowledge},
		},
	}
	answer := "竞价：制度条件为需求明确、规格统一；与询比相比允许多次报价。<src id=\"S1\" />"
	got := RepairNamedTopicCitationBindings(answer, []string{"询比", "竞价", "竞争谈判"}, refs)
	if !strings.Contains(got, `<src id="S2" />`) || strings.Contains(got, `<src id="S1" />`) {
		t.Fatalf("leading-topic paragraph was not repaired: %s", got)
	}
}

func TestEnsureNamedTopicDefinitionsCopiesOnlyCurrentEvidence(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID: "definition", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "竞争谈判是指采购人与二家以上符合资格条件的供应商洽谈确定供应商的采购方式。",
			Metadata:        map[string]string{MetadataCitationID: "S4", MetadataChunkID: "definition", "source_type": SourceTypeKnowledge},
		},
	}
	answer := "竞争谈判：适用条件包括技术复杂或存在不同实现路径。<src id=\"S5\" />"
	got := EnsureNamedTopicDefinitions(answer, []string{"询比", "竞争谈判"}, refs)
	if !strings.Contains(got, "竞争谈判是指采购人与二家以上") || !strings.Contains(got, `<src id="S4" />`) {
		t.Fatalf("missing named definition was not restored from evidence: %s", got)
	}
	if twice := EnsureNamedTopicDefinitions(got, []string{"询比", "竞争谈判"}, refs); twice != got {
		t.Fatalf("definition coverage was not idempotent: %s", twice)
	}
}

func TestEnsureNamedTopicDefinitionsAppendsEntirelyMissingOption(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID: "inquiry", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "第三十五条 询比采购，是指在采购需求确定的条件下依照既定规则一次性报出不可更改价格，经评审确定供应商的采购方式。",
			Metadata:        map[string]string{MetadataCitationID: "S2", MetadataChunkID: "inquiry", "source_type": SourceTypeKnowledge},
		},
		{
			ID: "negotiation", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "第三十七条 竞争谈判是指采购人与二家以上符合资格条件的供应商洽谈确定供应商的采购方式。",
			Metadata:        map[string]string{MetadataCitationID: "S6", MetadataChunkID: "negotiation", "source_type": SourceTypeKnowledge},
		},
	}
	answer := "询比采购是指在采购需求确定后一次性报价的采购方式。<src id=\"S2\" />"
	got := EnsureNamedTopicDefinitions(answer, []string{"询比采购", "竞争谈判"}, refs)
	if !strings.Contains(got, "竞争谈判是指采购人与二家以上") || !strings.Contains(got, `<src id="S6" />`) {
		t.Fatalf("entirely missing option was not restored from current evidence: %s", got)
	}
	if twice := EnsureNamedTopicDefinitions(got, []string{"询比采购", "竞争谈判"}, refs); twice != got {
		t.Fatalf("missing-option restoration was not idempotent: %s", twice)
	}
}

func TestRecoverOffTopicNarrowEvidenceAnswerUsesEveryCurrentTopic(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID: "notice", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "中标候选人公示期不得少于3日。",
			Metadata:        map[string]string{MetadataCitationID: "S1", MetadataChunkID: "notice", "source_type": SourceTypeKnowledge},
		},
		{
			ID: "review", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "质疑投诉事项涉及评审结果实质性内容并影响中标候选人排名的，由分管立项和采购部门的公司领导共同批准复核。",
			Metadata:        map[string]string{MetadataCitationID: "S2", MetadataChunkID: "review", "source_type": SourceTypeKnowledge},
		},
	}
	topics := []string{"中标候选人公示", "异议涉及实质内容并影响候选人排名"}
	stale := "询比和竞价的定义如下。"
	got := RecoverOffTopicNarrowEvidenceAnswer(stale, topics, refs)
	for _, expected := range []string{"不得少于3日", "分管立项", "采购部门", "公司领导", "S1", "S2"} {
		if !strings.Contains(got, expected) {
			t.Fatalf("narrow evidence recovery lost %q: %s", expected, got)
		}
	}
	if strings.Contains(got, "询比") || strings.Contains(got, "竞价") {
		t.Fatalf("stale answer survived narrow recovery: %s", got)
	}
	aligned := "中标候选人公示期不少于3日。<src id=\"S1\" />"
	if unchanged := RecoverOffTopicNarrowEvidenceAnswer(aligned, topics, refs); unchanged != aligned {
		t.Fatalf("partially aligned answer was unexpectedly replaced: %s", unchanged)
	}
}

func TestRepairAnswerCitationsCitesIndependentUncitedParagraphFromSameEvidence(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID: "public-invited", KnowledgeID: "doc", KnowledgeBaseID: "kb", ChunkType: string(types.ChunkTypeText),
			EvidenceContent: "公开采购是采购人以采购公告邀请不特定的潜在供应商参与采购项目。邀请采购是采购人以采购邀请书邀请特定的供应商或三家以上潜在供应商参与采购项目。",
			Metadata: map[string]string{
				MetadataCitationID: "S1", MetadataChunkID: "public-invited", "source_type": SourceTypeKnowledge,
			},
		},
	}
	answer := "公开采购以采购公告邀请不特定的潜在供应商参与。<src id=\"S1\" />\n\n" +
		"邀请采购以采购邀请书邀请特定的供应商或三家以上潜在供应商参与。"

	got := RepairAnswerCitations(answer, refs)
	if strings.Count(got, `<src id="S1" />`) != 2 {
		t.Fatalf("independent claim paragraph did not receive its adjacent citation: %q", got)
	}
	if !strings.Contains(got, `三家以上潜在供应商参与。<src id="S1" />`) {
		t.Fatalf("citation was not placed beside the independent invited claim: %q", got)
	}
}
