import type { ModelConfig } from "@/api/model";

export interface QuickKnowledgeBasePayload {
  name: string;
  description: string;
  type: "document";
  chunking_config: {
    chunk_size: number;
    chunk_overlap: number;
    separators: string[];
    enable_parent_child: boolean;
    parent_chunk_size: number;
    child_chunk_size: number;
    strategy: string;
    token_limit: number;
    languages: string[];
  };
  embedding_model_id: string;
  summary_model_id: string;
  derivative_model_id: string;
  vlm_config: { enabled: boolean; model_id: string };
  asr_config: { enabled: boolean; model_id: string; language: string };
  indexing_strategy: {
    vector_enabled: boolean;
    keyword_enabled: boolean;
    wiki_enabled: boolean;
    graph_enabled: boolean;
  };
}

export interface QuickKnowledgeBaseBuildResult {
  payload?: QuickKnowledgeBasePayload;
  missing: Array<"KnowledgeQA" | "Embedding">;
}

function pickDefaultModel(models: readonly ModelConfig[], type: ModelConfig["type"]) {
  const candidates = models.filter(
    (model) => model.type === type && (type !== "KnowledgeQA" || model.workload_scope !== "derivative_only"),
  );
  return candidates.find((model) => model.is_default) || candidates[0];
}

export function buildQuickKnowledgeBasePayload(
  models: readonly ModelConfig[],
  form: { name: string; description?: string },
): QuickKnowledgeBaseBuildResult {
  const chat = pickDefaultModel(models, "KnowledgeQA");
  const embedding = pickDefaultModel(models, "Embedding");
  const vlm = pickDefaultModel(models, "VLLM");
  const missing: QuickKnowledgeBaseBuildResult["missing"] = [];
  if (!chat?.id) missing.push("KnowledgeQA");
  if (!embedding?.id) missing.push("Embedding");
  if (missing.length) return { missing };

  return {
    missing,
    payload: {
      name: form.name.trim(),
      description: String(form.description || "").trim(),
      type: "document",
      chunking_config: {
        chunk_size: 512,
        chunk_overlap: 80,
        separators: ["\n\n", "\n", "。", "！", "？", ";", "；"],
        enable_parent_child: true,
        parent_chunk_size: 4096,
        child_chunk_size: 384,
        strategy: "auto",
        token_limit: 0,
        languages: [],
      },
      embedding_model_id: String(embedding!.id),
      summary_model_id: String(chat!.id),
      derivative_model_id: "",
      vlm_config: {
        enabled: Boolean(vlm?.id),
        model_id: vlm?.id || "",
      },
      asr_config: {
        enabled: false,
        model_id: "",
        language: "",
      },
      indexing_strategy: {
        vector_enabled: true,
        keyword_enabled: true,
        wiki_enabled: false,
        graph_enabled: false,
      },
    },
  };
}
