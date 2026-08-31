from __future__ import annotations

import json
import os
import tempfile
import unittest
from collections import Counter, defaultdict
from pathlib import Path
from unittest.mock import patch

from curation.build_multiturn_ready_v1 import build_cases
from curation.build_multiturn_ready_v2 import build_cases as build_v2_cases
from curation.build_multiturn_ready_v3 import build_cases as build_v3_cases
from curation.build_multiturn_optimization_dev_v1 import (
    build_cases as build_optimization_cases,
)
from weknora_eval.calibration import load_calibration, run_judge_calibration
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset
from weknora_eval.langfuse_store import publish_dataset
from weknora_eval.models import Capability, SUTFingerprint, Split, TurnContract
from weknora_eval.readiness import evaluate_readiness, file_sha256


ROOT = Path(__file__).resolve().parents[1]


def eval_sut() -> SUTFingerprint:
    return SUTFingerprint(
        mode="eval",
        release="test",
        commit="test",
        environment="agent-eval",
        capabilities=[
            Capability.RAG_RETRIEVAL.value,
            Capability.DOCUMENT_PROCESSING.value,
            Capability.LONG_CONTEXT_DIALOGUE.value,
            Capability.TOOL_USE.value,
            Capability.CITATION.value,
        ],
        raw={
            "recorder_enabled": True,
            "capture_policy": "full",
            "worktree_dirty": "false",
            "runtime_image_id": "sha256:runtime",
            "general_agent_image_id": "sha256:general",
        },
    )


class ReadinessTests(unittest.TestCase):
    def test_text_dependency_hash_is_cross_platform_line_ending_invariant(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            lf = root / "lf.py"
            crlf = root / "crlf.py"
            lf.write_bytes(b"first\nsecond\n")
            crlf.write_bytes(b"first\r\nsecond\r\n")
            self.assertEqual(file_sha256(lf), file_sha256(crlf))

    def test_pre_agent_change_lock_matches_committed_dependencies(self) -> None:
        lock = json.loads(
            (ROOT / "baselines" / "pre-agent-change.v1.json").read_text(
                encoding="utf-8"
            )
        )
        self.assertEqual(lock["status"], "FROZEN_PRE_AGENT_CHANGE")
        self.assertFalse(lock["sealed_holdout"]["content_accessed"])

        for section in ("dev", "gate"):
            dataset = lock[section]["dataset"]
            dataset_path = ROOT / dataset["path"]
            self.assertEqual(
                dataset_sha256(load_jsonl(dataset_path)),
                dataset["dataset_sha256"],
            )
            self.assertEqual(file_sha256(dataset_path), dataset["file_sha256"])

        for baseline_name in ("experiment_baseline", "promotion_baseline"):
            baseline = lock["dev"][baseline_name]
            self.assertEqual(
                file_sha256(ROOT / baseline["policy"]), baseline["policy_sha256"]
            )
            self.assertEqual(
                file_sha256(ROOT / baseline["manifest"]), baseline["manifest_sha256"]
            )

        release = lock["gate"]["release_baseline"]
        self.assertEqual(
            file_sha256(ROOT / release["policy"]), release["policy_sha256"]
        )
        self.assertEqual(
            file_sha256(ROOT / release["manifest"]), release["manifest_sha256"]
        )
        self.assertEqual(
            file_sha256(ROOT / lock["frozen_dependencies"]["profile"]["path"]),
            lock["frozen_dependencies"]["profile"]["sha256"],
        )
        self.assertEqual(
            file_sha256(ROOT / lock["sealed_holdout"]["manifest"]),
            lock["sealed_holdout"]["manifest_sha256"],
        )

        local_artifacts = [
            (
                lock["judge_calibration"]["artifact"],
                lock["judge_calibration"]["sha256"],
            ),
            (
                lock["dev"]["raw_observation"]["path"],
                lock["dev"]["raw_observation"]["sha256"],
            ),
            (
                lock["dev"]["experiment_baseline"]["path"],
                lock["dev"]["experiment_baseline"]["sha256"],
            ),
            (
                lock["dev"]["promotion_baseline"]["path"],
                lock["dev"]["promotion_baseline"]["sha256"],
            ),
            (
                lock["dev"]["self_control"]["gate"],
                lock["dev"]["self_control"]["gate_sha256"],
            ),
            (
                lock["dev"]["self_control"]["report"],
                lock["dev"]["self_control"]["report_sha256"],
            ),
            (
                lock["dev"]["promotion_self_control"]["gate"],
                lock["dev"]["promotion_self_control"]["gate_sha256"],
            ),
            (
                lock["dev"]["promotion_self_control"]["report"],
                lock["dev"]["promotion_self_control"]["report_sha256"],
            ),
            (
                lock["dev"]["preflight"]["experiment"],
                lock["dev"]["preflight"]["experiment_sha256"],
            ),
            (
                lock["dev"]["preflight"]["promotion"],
                lock["dev"]["preflight"]["promotion_sha256"],
            ),
            (
                lock["gate"]["raw_observation"]["path"],
                lock["gate"]["raw_observation"]["sha256"],
            ),
            (
                lock["gate"]["release_baseline"]["path"],
                lock["gate"]["release_baseline"]["sha256"],
            ),
            (
                lock["gate"]["self_control"]["gate"],
                lock["gate"]["self_control"]["gate_sha256"],
            ),
            (
                lock["gate"]["self_control"]["report"],
                lock["gate"]["self_control"]["report_sha256"],
            ),
            (
                lock["gate"]["preflight"]["path"],
                lock["gate"]["preflight"]["sha256"],
            ),
        ]
        present = [ROOT / path for path, _ in local_artifacts if (ROOT / path).exists()]
        if present:
            self.assertEqual(len(present), len(local_artifacts))
            for path, expected in local_artifacts:
                self.assertEqual(file_sha256(ROOT / path), expected)

    def test_runner_compose_forwards_the_frozen_evaluator_identity(self) -> None:
        compose = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")
        self.assertIn(
            "AGENT_EVAL_FRAMEWORK_SCOPE: ${AGENT_EVAL_FRAMEWORK_SCOPE:-unknown}",
            compose,
        )
        self.assertIn(
            "AGENT_EVAL_GATE_POLICY_SHA256: ${AGENT_EVAL_GATE_POLICY_SHA256:-unknown}",
            compose,
        )

    def env(self) -> dict[str, str]:
        return {
            "AGENT_EVAL_SUMMARY_MODEL_ID": "prod-deepseek-v4-flash-int8-chat",
            "AGENT_EVAL_PROCUREMENT_KNOWLEDGE_ID": "knowledge-id",
            "AGENT_EVAL_CORPUS_VERSION": "sha256:corpus",
            "AGENT_EVAL_JUDGE_BASE_URL": "https://judge.example/v1",
            "AGENT_EVAL_JUDGE_API_KEY": "test-only",
            "AGENT_EVAL_JUDGE_MODEL": "judge-model",
        }

    def test_committed_ready_dataset_matches_compiler(self) -> None:
        committed = load_jsonl(ROOT / "datasets" / "multiturn-ready.v1.jsonl")
        compiled = build_cases()
        self.assertEqual(dataset_sha256(committed), dataset_sha256(compiled))
        self.assertEqual(validate_dataset(committed), [])

    def test_v2_contract_revision_is_frozen_and_observation_compatible(self) -> None:
        committed = load_jsonl(ROOT / "datasets" / "multiturn-ready.v2.jsonl")
        compiled = build_v2_cases()
        v1_by_id = {case.case_id: case for case in build_cases()}
        self.assertEqual(dataset_sha256(committed), dataset_sha256(compiled))
        self.assertEqual(validate_dataset(committed), [])
        self.assertEqual(
            {case.case_id for case in committed},
            set(v1_by_id),
            "contract-only revisions keep stable scenario IDs for immutable observation rescoring",
        )
        for case in committed:
            v1 = v1_by_id[case.case_id]
            self.assertEqual(
                [turn.query for turn in case.turns],
                [turn.query for turn in v1.turns],
            )
            self.assertEqual(case.repetitions, v1.repetitions)
            self.assertEqual(case.setup, v1.setup)

        active = next(
            case
            for case in committed
            if case.family_id == "active-retired-state-window-plus-two"
        )
        turn7 = active.turns[6].contract.conversation_state
        self.assertIn("尚未核验", turn7.unknown_facts[0].any_of)
        self.assertIn("待核实", turn7.unknown_facts[0].any_of)
        turn8 = active.turns[7].contract.conversation_state
        self.assertIn("不成立", turn8.retired_facts[0].any_of)
        turn8_active = {
            rule.rule_id: rule for rule in turn8.active_facts
        }
        self.assertEqual(turn8_active["not-exclusive"].all_of, ["A", "B", "C"])
        self.assertIn(
            "不可替代主张：已核验为否",
            turn8_active["not-exclusive"].any_of,
        )
        turn1_boundaries = {
            rule.rule_id: rule
            for rule in active.turns[0].contract.conversation_state.action_boundaries
        }
        self.assertEqual(turn1_boundaries["no-procurement-action"].all_of, ["采购"])
        self.assertIn("不予执行", turn1_boundaries["no-procurement-action"].any_of)
        self.assertEqual(turn1_boundaries["chat-only"].all_of, ["对话"])
        turn10_legal = {
            rule.rule_id: rule
            for rule in active.turns[9].contract.conversation_state.active_facts
        }
        self.assertIn("涉密：否", turn10_legal["legal-confirmation-confidentiality"].any_of)
        self.assertIn("不属应急", turn10_legal["legal-confirmation-urgency"].any_of)
        self.assertEqual(
            turn10_legal["legal-confirmation"].all_of,
            ["不可替代专利"],
        )
        self.assertIn("不存在", turn10_legal["legal-confirmation"].any_of)
        self.assertIn("不可替代专利：否", turn10_legal["legal-confirmation"].any_of)
        turn12_unknowns = {
            rule.rule_id: rule
            for rule in active.turns[11].contract.conversation_state.unknown_facts
        }
        self.assertEqual(turn12_unknowns["user"].all_of, ["用户", "身份"])
        self.assertIn("没有提供", turn12_unknowns["user"].any_of)
        turn12_active = {
            rule.rule_id: rule
            for rule in active.turns[11].contract.conversation_state.active_facts
        }
        self.assertIn("supplier-route-result-relationship", turn12_active)
        self.assertEqual(
            turn12_active["supplier-route-result-relationship"].all_of,
            ["技术路线"],
        )
        turn12_forbidden = {
            rule.rule_id: rule
            for rule in active.turns[
                11
            ].contract.conversation_state.forbidden_unknown_facts
        }
        self.assertIn("resolved-a-not-unknown", turn12_forbidden)
        turn12_inferences = {
            rule.rule_id: rule
            for rule in active.turns[11].contract.conversation_state.forbidden_inferences
        }
        self.assertIn("wrong-current-date-source", turn12_inferences)
        self.assertIn(
            "业务团队确认",
            turn12_inferences["wrong-current-date-source"].any_of,
        )
        self.assertIn(
            "业务调整",
            turn12_inferences["wrong-current-date-source"].any_of,
        )
        self.assertIn("historical-response-scope-persisted", turn12_inferences)
        self.assertIn(
            "不推断设备、软件或施工范围",
            turn12_inferences["historical-response-scope-persisted"].any_of,
        )
        self.assertIn(
            "不得选择采购方式",
            turn12_inferences["historical-response-scope-persisted"].any_of,
        )

        alignment = next(
            case
            for case in committed
            if case.family_id == "current-turn-alignment-topic-detour"
        )
        for turn_index in (1, 4):
            unresolved = {
                rule.rule_id: rule
                for rule in alignment.turns[
                    turn_index
                ].contract.conversation_state.unknown_facts
            }
            self.assertEqual(set(unresolved), {"public", "complete", "schedule"})
            for rule in unresolved.values():
                self.assertIn("尚未确认", rule.any_of)
                self.assertNotIn("待确认", rule.all_of)
        self.assertTrue(
            alignment.turns[1].contract.conversation_state.require_scoped_sections
        )
        self.assertIn(
            "不定首选",
            alignment.turns[2].contract.decision.required_defer_claims[0].any_of,
        )
        turn3_state = alignment.turns[2].contract.conversation_state
        self.assertEqual(
            [rule.rule_id for rule in turn3_state.active_facts],
            ["known-project-type", "known-budget", "known-suppliers"],
        )
        self.assertEqual(
            [rule.rule_id for rule in turn3_state.unknown_facts],
            ["public", "complete", "schedule"],
        )
        self.assertIn(
            "unsupported-goods-classification",
            {rule.rule_id for rule in turn3_state.forbidden_inferences},
        )
        self.assertIn(
            "no-priority-inquiry",
            {
                rule.rule_id
                for rule in alignment.turns[2].contract.decision.forbidden_recommendations
            },
        )
        self.assertIn(
            "no-ranking-competitive-negotiation",
            {
                rule.rule_id
                for rule in alignment.turns[2].contract.decision.forbidden_recommendations
            },
        )
        review_rule = next(
            rule
            for rule in alignment.turns[3].contract.required_claims
            if rule.rule_id == "review-approval"
        )
        self.assertEqual(
            review_rule.all_of,
            ["分管立项", "采购部门", "公司领导"],
        )
        review_anchor = next(
            anchor
            for anchor in alignment.turns[3].contract.evidence_anchors
            if anchor.anchor_id == "review-approval"
        )
        self.assertEqual(
            review_anchor.all_of,
            ["分管立项", "采购部门", "公司领导", "复核"],
        )
        self.assertEqual(review_anchor.any_of, ["审批", "批准"])
        publication_rule = next(
            rule
            for rule in alignment.turns[3].contract.required_claims
            if rule.rule_id == "publication-days"
        )
        self.assertIn("至少3日", publication_rule.any_of)

        decision = next(
            case
            for case in committed
            if case.family_id == "decision-under-unknowns-procurement-path"
        )
        known_ids = {
            rule.rule_id for rule in decision.turns[0].contract.required_claims
        }
        self.assertTrue(
            {
                "known-budget",
                "known-legal-scope",
                "known-suppliers",
                "known-nonurgent",
            }
            <= known_ids
        )
        legal_rule = next(
            rule
            for rule in decision.turns[0].contract.required_claims
            if rule.rule_id == "known-legal-scope"
        )
        self.assertIn("依法可不招标", legal_rule.any_of)
        claims = {
            claim.rule_id: claim.claim
            for claim in decision.turns[0].contract.evidence_claims
        }
        self.assertEqual(claims["publicity-condition-binding"].all_of, ["信息", "公开"])
        self.assertIn("允许", claims["public-time-condition-binding"].any_of)
        decision_rankings = {
            rule.rule_id: rule
            for rule in decision.turns[0].contract.decision.forbidden_recommendations
        }
        self.assertIn("较适配", decision_rankings["no-ranking-competitive-negotiation"].any_of)
        self.assertIn("风险最低", decision_rankings["no-ranking-competitive-negotiation"].any_of)
        self.assertIn("适用性反而较高", decision_rankings["no-ranking-competitive-negotiation"].any_of)
        self.assertIn("适用性反而更高", decision_rankings["no-ranking-competitive-negotiation"].any_of)
        self.assertIn(
            "I have the retrieval results",
            next(
                rule
                for rule in decision.turns[0].contract.forbidden_claims
                if rule.rule_id == "no-internal-planning"
            ).any_of,
        )
        decision_state = decision.turns[0].contract.conversation_state
        self.assertTrue(decision_state.require_scoped_sections)
        self.assertEqual(
            {rule.rule_id for rule in decision_state.active_facts},
            {"known-budget", "known-legal-scope", "known-suppliers", "known-nonurgent"},
        )
        state_unknowns = {rule.rule_id: rule for rule in decision_state.unknown_facts}
        decision_unknowns = {
            rule.rule_id: rule
            for rule in decision.turns[0].contract.decision.required_unknowns
        }
        self.assertEqual(set(state_unknowns), {"public", "complete", "schedule"})
        self.assertIn("尚未确认", state_unknowns["complete"].any_of)
        self.assertEqual(state_unknowns, decision_unknowns)
        decision_inferences = {
            rule.rule_id
            for rule in decision_state.forbidden_inferences
        }
        self.assertIn("unsupported-standardization", decision_inferences)
        self.assertIn("unsupported-service-price-competition", decision_inferences)
        self.assertIn("unsupported-project-heuristic", decision_inferences)
        self.assertIn("unsupported-goods-classification-generic", decision_inferences)
        self.assertIn("unsupported-public-demand-condition", decision_inferences)
        self.assertIn("unknown-demand-implies-negotiation", decision_inferences)
        self.assertIn("unsupported-forced-invitation-path", decision_inferences)
        self.assertIn("unsupported-demand-public-competition-bridge", decision_inferences)
        self.assertIn("unsupported-demand-fee-standard-bridge", decision_inferences)
        self.assertIn("unsupported-demand-detailed-spec-bridge", decision_inferences)
        self.assertIn("unknown-demand-parenthetical-redefinition", decision_inferences)
        self.assertIn("unknown-schedule-parenthetical-redefinition", decision_inferences)
        self.assertIn("no-unsupported-causal-bridge-language", decision_inferences)

    def test_v3_evaluator_revision_is_frozen_and_observation_compatible(self) -> None:
        committed = load_jsonl(ROOT / "datasets" / "multiturn-ready.v3.jsonl")
        compiled = build_v3_cases()
        v2_by_id = {case.case_id: case for case in build_v2_cases()}
        self.assertEqual(dataset_sha256(committed), dataset_sha256(compiled))
        self.assertEqual(validate_dataset(committed), [])
        for case in committed:
            self.assertEqual(
                [turn.query for turn in case.turns],
                [turn.query for turn in v2_by_id[case.case_id].turns],
            )
        warehouse = next(
            case
            for case in committed
            if case.family_id == "gate-warehouse-state-supersession-and-detour"
            and case.agent_profile_id == "general-agent"
        )
        final_forbidden = {
            rule.rule_id
            for rule in warehouse.turns[-1].contract.conversation_state.forbidden_unknown_facts
        }
        self.assertIn("resolved-d-not-unknown-final", final_forbidden)

    def test_optimization_dev_is_complete_repeated_and_gate_independent(self) -> None:
        committed = load_jsonl(
            ROOT / "datasets" / "multiturn-optimization-dev.v1.jsonl"
        )
        compiled = build_optimization_cases()
        source_dev = [case for case in build_v3_cases() if case.split == Split.DEV]
        self.assertEqual(dataset_sha256(committed), dataset_sha256(compiled))
        self.assertEqual(validate_dataset(committed), [])
        self.assertEqual({case.case_id for case in committed}, {case.case_id for case in source_dev})
        self.assertTrue(all(case.split == Split.DEV for case in committed))
        self.assertTrue(all(case.repetitions == 3 for case in committed))
        self.assertTrue(
            all(case.provenance.metadata["gate_prompt_derived"] is False for case in committed)
        )
        self.assertEqual(
            Counter(case.agent_profile_id for case in committed),
            Counter({"quick-answer": 4, "rag-reasoning": 4, "general-agent": 4}),
        )

    def test_gate_matrix_is_new_repeated_and_overflows_each_history_window(self) -> None:
        cases = [case for case in build_cases() if case.split == Split.GATE]
        self.assertEqual(len(cases), 6)
        self.assertTrue(all(case.repetitions == 3 for case in cases))
        self.assertEqual(
            Counter(case.agent_profile_id for case in cases),
            Counter({"quick-answer": 2, "rag-reasoning": 2, "general-agent": 2}),
        )
        families: dict[str, set[str | None]] = defaultdict(set)
        for case in cases:
            families[case.family_id].add(case.agent_profile_id)
        self.assertEqual(len(families), 2)
        self.assertTrue(
            all(value == {"quick-answer", "rag-reasoning", "general-agent"} for value in families.values())
        )
        long_cases = {
            case.agent_profile_id: case
            for case in cases
            if Capability.LONG_CONTEXT_DIALOGUE in case.capabilities
        }
        self.assertEqual(len(long_cases["quick-answer"].turns), 12)
        self.assertEqual(len(long_cases["rag-reasoning"].turns), 12)
        self.assertEqual(len(long_cases["general-agent"].turns), 12)

    def test_preflight_is_read_only_and_ready_for_gate(self) -> None:
        with patch.dict(os.environ, self.env(), clear=False):
            report = evaluate_readiness(
                dataset_path=ROOT / "datasets" / "multiturn-ready.v1.jsonl",
                manifest_path=ROOT / "manifests" / "multiturn-ready.v1.manifest.json",
                profile_path=ROOT / "profiles" / "multiturn-agents.v1.json",
                policy_path=ROOT / "policies" / "multiturn-release-gate.v1.json",
                calibration_path=ROOT / "calibration" / "judge-multiturn.v1.json",
                split=Split.GATE,
                sut=eval_sut(),
            )
        self.assertEqual(report["status"], "READY")
        self.assertFalse(report["formal_eval_executed"])
        self.assertEqual(report["case_count"], 6)
        self.assertEqual(report["planned_session_count"], 18)
        self.assertTrue(all(check["passed"] for check in report["checks"]))

    def test_historical_v10_manifest_fails_closed_after_dual_track_upgrade(self) -> None:
        with patch.dict(os.environ, self.env(), clear=False):
            report = evaluate_readiness(
                dataset_path=ROOT / "datasets" / "multiturn-ready.v3.jsonl",
                manifest_path=ROOT
                / "manifests"
                / "multiturn-ready.v3-evaluator-v10.manifest.json",
                profile_path=ROOT / "profiles" / "multiturn-agents.v1.json",
                policy_path=ROOT / "policies" / "multiturn-release-gate.v2.json",
                calibration_path=ROOT / "calibration" / "judge-multiturn.v1.json",
                split=Split.GATE,
                sut=eval_sut(),
            )
        self.assertEqual(report["status"], "NOT_READY")
        frozen = next(
            check for check in report["checks"] if check["name"] == "frozen_dependencies"
        )
        self.assertFalse(frozen["passed"])
        self.assertNotEqual(
            frozen["data"]["expected"]["scorer"],
            frozen["data"]["manifest"]["scorer"],
        )

    def test_v3_evaluator_v8_manifest_fails_closed_after_action_semantics_upgrade(self) -> None:
        with patch.dict(os.environ, self.env(), clear=False):
            report = evaluate_readiness(
                dataset_path=ROOT / "datasets" / "multiturn-ready.v3.jsonl",
                manifest_path=ROOT
                / "manifests"
                / "multiturn-ready.v3-evaluator-v8.manifest.json",
                profile_path=ROOT / "profiles" / "multiturn-agents.v1.json",
                policy_path=ROOT / "policies" / "multiturn-release-gate.v2.json",
                calibration_path=ROOT / "calibration" / "judge-multiturn.v1.json",
                split=Split.GATE,
                sut=eval_sut(),
            )
        self.assertEqual(report["status"], "NOT_READY")
        frozen = next(
            check for check in report["checks"] if check["name"] == "frozen_dependencies"
        )
        self.assertFalse(frozen["passed"])
        self.assertNotEqual(
            frozen["data"]["expected"]["scorer"],
            frozen["data"]["manifest"]["scorer"],
        )

    def test_v3_evaluator_v7_manifest_fails_closed_after_planning_detection_upgrade(self) -> None:
        with patch.dict(os.environ, self.env(), clear=False):
            report = evaluate_readiness(
                dataset_path=ROOT / "datasets" / "multiturn-ready.v3.jsonl",
                manifest_path=ROOT
                / "manifests"
                / "multiturn-ready.v3-evaluator-v7.manifest.json",
                profile_path=ROOT / "profiles" / "multiturn-agents.v1.json",
                policy_path=ROOT / "policies" / "multiturn-release-gate.v2.json",
                calibration_path=ROOT / "calibration" / "judge-multiturn.v1.json",
                split=Split.GATE,
                sut=eval_sut(),
            )
        self.assertEqual(report["status"], "NOT_READY")
        frozen = next(
            check for check in report["checks"] if check["name"] == "frozen_dependencies"
        )
        self.assertFalse(frozen["passed"])
        self.assertNotEqual(
            frozen["data"]["expected"]["scorer"],
            frozen["data"]["manifest"]["scorer"],
        )

    def test_v3_evaluator_v2_manifest_fails_closed_after_protocol_upgrade(self) -> None:
        with patch.dict(os.environ, self.env(), clear=False):
            report = evaluate_readiness(
                dataset_path=ROOT / "datasets" / "multiturn-ready.v3.jsonl",
                manifest_path=ROOT
                / "manifests"
                / "multiturn-ready.v3-evaluator-v2.manifest.json",
                profile_path=ROOT / "profiles" / "multiturn-agents.v1.json",
                policy_path=ROOT / "policies" / "multiturn-release-gate.v2.json",
                calibration_path=ROOT / "calibration" / "judge-multiturn.v1.json",
                split=Split.GATE,
                sut=eval_sut(),
            )
        self.assertEqual(report["status"], "NOT_READY")
        frozen = next(
            check for check in report["checks"] if check["name"] == "frozen_dependencies"
        )
        self.assertFalse(frozen["passed"])

    def test_historical_optimization_manifest_fails_closed_after_upgrade(self) -> None:
        with patch.dict(os.environ, self.env(), clear=False):
            report = evaluate_readiness(
                dataset_path=ROOT / "datasets" / "multiturn-optimization-dev.v1.jsonl",
                manifest_path=ROOT
                / "manifests"
                / "multiturn-optimization-dev.v1-evaluator-v10.manifest.json",
                profile_path=ROOT / "profiles" / "multiturn-agents.v1.json",
                policy_path=ROOT / "policies" / "multiturn-optimization-gate.v1.json",
                calibration_path=ROOT / "calibration" / "judge-multiturn.v1.json",
                split=Split.DEV,
                sut=eval_sut(),
            )
        self.assertEqual(report["status"], "NOT_READY")
        self.assertEqual(report["case_count"], 12)
        self.assertEqual(report["planned_session_count"], 36)
        frozen = next(
            check for check in report["checks"] if check["name"] == "frozen_dependencies"
        )
        self.assertFalse(frozen["passed"])

    def test_historical_experiment_manifest_fails_closed_after_upgrade(self) -> None:
        with patch.dict(os.environ, self.env(), clear=False):
            report = evaluate_readiness(
                dataset_path=ROOT / "datasets" / "multiturn-optimization-dev.v1.jsonl",
                manifest_path=ROOT
                / "manifests"
                / "multiturn-optimization-dev-experiment.v1-evaluator-v10.manifest.json",
                profile_path=ROOT / "profiles" / "multiturn-agents.v1.json",
                policy_path=ROOT / "policies" / "multiturn-experiment-gate.v1.json",
                calibration_path=ROOT / "calibration" / "judge-multiturn.v1.json",
                split=Split.DEV,
                sut=eval_sut(),
            )
        self.assertEqual(report["status"], "NOT_READY")
        self.assertEqual(report["case_count"], 12)
        self.assertEqual(report["planned_session_count"], 36)
        frozen = next(
            check for check in report["checks"] if check["name"] == "frozen_dependencies"
        )
        self.assertFalse(frozen["passed"])

    def test_v3_preflight_fails_before_execution_when_judge_is_unconfigured(self) -> None:
        env = self.env()
        env.update(
            {
                "AGENT_EVAL_JUDGE_BASE_URL": "",
                "AGENT_EVAL_JUDGE_API_KEY": "",
                "AGENT_EVAL_JUDGE_MODEL": "",
            }
        )
        with patch.dict(os.environ, env, clear=False):
            report = evaluate_readiness(
                dataset_path=ROOT / "datasets" / "multiturn-ready.v3.jsonl",
                manifest_path=ROOT
                / "manifests"
                / "multiturn-ready.v3-evaluator-v10.manifest.json",
                profile_path=ROOT / "profiles" / "multiturn-agents.v1.json",
                policy_path=ROOT / "policies" / "multiturn-release-gate.v2.json",
                calibration_path=ROOT / "calibration" / "judge-multiturn.v1.json",
                split=Split.GATE,
                sut=eval_sut(),
            )
        self.assertEqual(report["status"], "NOT_READY")
        failed = {check["name"] for check in report["checks"] if not check["passed"]}
        self.assertIn("judge_binding", failed)

    def test_preflight_fails_closed_on_model_drift(self) -> None:
        env = self.env()
        env["AGENT_EVAL_SUMMARY_MODEL_ID"] = "wrong-model"
        with patch.dict(os.environ, env, clear=False):
            report = evaluate_readiness(
                dataset_path=ROOT / "datasets" / "multiturn-ready.v1.jsonl",
                manifest_path=ROOT / "manifests" / "multiturn-ready.v1.manifest.json",
                profile_path=ROOT / "profiles" / "multiturn-agents.v1.json",
                policy_path=ROOT / "policies" / "multiturn-release-gate.v1.json",
                calibration_path=ROOT / "calibration" / "judge-multiturn.v1.json",
                split=Split.GATE,
                sut=eval_sut(),
            )
        self.assertEqual(report["status"], "NOT_READY")
        failed = {check["name"] for check in report["checks"] if not check["passed"]}
        self.assertIn("model_binding", failed)

    def test_judge_calibration_has_all_verdict_and_boundary_classes(self) -> None:
        suite = load_calibration(ROOT / "calibration" / "judge-multiturn.v1.json")
        self.assertEqual(len(suite.items), 10)
        self.assertEqual({item.expected_label for item in suite.items}, {"pass", "fail", "invalid"})
        self.assertEqual({item.boundary_kind for item in suite.items}, {"positive", "negative", "boundary"})
        self.assertEqual(
            {item.calibration_id for item in suite.items if item.critical},
            {
                "state-fail-required-unknown-omitted",
                "state-pass-owner-and-user-distinct",
            },
        )
        self.assertTrue(all(isinstance(item.contract, TurnContract) for item in suite.items))
        for item in suite.items:
            self.assertTrue(item.query.strip())
            if not item.critical:
                continue
            state = item.contract.conversation_state
            self.assertTrue(state.active_facts)
            self.assertTrue(state.unknown_facts)

    def test_judge_calibration_uses_the_formal_turn_protocol(self) -> None:
        suite = load_calibration(ROOT / "calibration" / "judge-multiturn.v1.json")
        expected = {item.calibration_id: item.expected_label for item in suite.items}

        def fake_single_turn(**payload: object) -> dict:
            contract = payload["contract"]
            candidate = payload["candidate"]
            self.assertIsInstance(contract, dict)
            self.assertIsInstance(candidate, dict)
            turn_id = contract["turn_id"]
            self.assertEqual(candidate["turn_id"], turn_id)
            self.assertIn("completed", candidate)
            item = next(item for item in suite.items if item.calibration_id == turn_id)
            self.assertEqual(contract["query"], item.query)
            self.assertIn("conversation_state", contract["contract"])
            self.assertIn("required_claims", contract["contract"])
            return {
                "turn_id": turn_id,
                "label": expected[turn_id],
                "confidence": 0.99,
                "reason": "fixture",
            }

        with patch(
            "weknora_eval.calibration.judge_single_turn",
            side_effect=fake_single_turn,
        ):
            result = run_judge_calibration(suite)
        self.assertTrue(result["passed"])
        self.assertEqual(result["accuracy"], 1.0)

        def low_confidence_single_turn(**payload: object) -> dict:
            turn_id = payload["contract"]["turn_id"]
            return {
                "turn_id": turn_id,
                "label": expected[turn_id],
                "confidence": 0.5,
                "reason": "uncertain fixture",
            }

        with patch(
            "weknora_eval.calibration.judge_single_turn",
            side_effect=low_confidence_single_turn,
        ):
            uncertain = run_judge_calibration(suite)
        self.assertEqual(uncertain["accuracy"], 1.0)
        self.assertFalse(uncertain["passed"])

        critical_id = next(item.calibration_id for item in suite.items if item.critical)

        def critical_mismatch_single_turn(**payload: object) -> dict:
            turn_id = payload["contract"]["turn_id"]
            label = expected[turn_id]
            if turn_id == critical_id:
                label = "pass" if label != "pass" else "fail"
            return {
                "turn_id": turn_id,
                "label": label,
                "confidence": 0.99,
                "reason": "critical fixture",
            }

        with patch(
            "weknora_eval.calibration.judge_single_turn",
            side_effect=critical_mismatch_single_turn,
        ):
            critical_failure = run_judge_calibration(suite)
        self.assertFalse(critical_failure["passed"])
        self.assertEqual(critical_failure["critical_mismatches"], [critical_id])

    def test_langfuse_publishes_each_repetition_as_an_independent_item(self) -> None:
        class FakeLangfuse:
            def __init__(self) -> None:
                self.items: list[dict] = []

            def create_dataset(self, **_: object) -> None:
                return None

            def create_dataset_item(self, **kwargs: object) -> None:
                self.items.append(kwargs)

            def flush(self) -> None:
                return None

        case = next(case for case in build_cases() if case.split == Split.GATE)
        fake = FakeLangfuse()
        with patch("weknora_eval.langfuse_store._client", return_value=fake):
            publish_dataset([case], "test/repeated")
        self.assertEqual(len(fake.items), 3)
        self.assertEqual(
            [item["input"]["attempt_index"] for item in fake.items],
            [1, 2, 3],
        )


if __name__ == "__main__":
    unittest.main()
