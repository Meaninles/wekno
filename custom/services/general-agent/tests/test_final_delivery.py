"""The terminal boundary is typed protocol state, never task-word heuristics."""
from dataclasses import dataclass
import unittest
from app.final_delivery import ClaudeSDKTerminalCollector

@dataclass
class ResultMessage:
    result: str | None
    subtype: str = 'success'
    is_error: bool = False
    stop_reason: str | None = 'end_turn'

@dataclass
class AssistantMessage:
    content: list

class TerminalTests(unittest.TestCase):
    def test_partial_and_tool_text_are_not_a_result(self):
        collector=ClaudeSDKTerminalCollector()
        collector.observe(AssistantMessage([{'type':'text','text':'partial'}, {'type':'tool_use','id':'c'}]))
        self.assertEqual(collector.answer(),'')
        self.assertFalse(collector.terminal_seen)

    def test_stop_errors_and_empty_text_never_complete(self):
        for event in (ResultMessage('partial',stop_reason='max_tokens'),ResultMessage('partial',stop_reason=None),
                      ResultMessage('error',is_error=True),ResultMessage(' ',stop_reason='stop'),
                      ResultMessage('partial',subtype='error_max_turns')):
            with self.subTest(event=event):
                collector=ClaudeSDKTerminalCollector();collector.observe(event)
                self.assertEqual(collector.answer(),'')

    def test_exact_body_has_no_private_marker_or_semantic_projection(self):
        for body in ('  literal\n','<business_final_answer>Payload</business_final_answer>',
                     '<historical_assistant_output>quoted example</historical_assistant_output>',
                     '的。'*80,'Now I will prepare the answer.'):
            # Completion does not assert that the answer satisfied the user.
            collector=ClaudeSDKTerminalCollector();collector.observe(ResultMessage(body))
            self.assertEqual(collector.answer(),body)
            self.assertEqual(collector.answer_source,'typed_result')

    def test_collectors_keep_concurrent_runs_isolated(self):
        first,second=ClaudeSDKTerminalCollector(),ClaudeSDKTerminalCollector()
        first.observe(ResultMessage('one'));second.observe(ResultMessage('two'))
        self.assertEqual(first.answer(),'one');self.assertEqual(second.answer(),'two')
