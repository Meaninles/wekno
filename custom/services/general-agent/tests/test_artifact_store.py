import hashlib
import tempfile
import unittest
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from unittest.mock import patch

from app.artifact_store import ArtifactStore
from app.schemas import ChatPayload, LLMConfig


class ArtifactAdmissionTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.store = ArtifactStore(self.root, ChatPayload(run_id='r', session_id='s',
            assistant_message_id='a', query='prepare files', enable_artifacts=True,
            llm=LLMConfig(model_name='test', api_key='unused'), tool_callback_url='http://local/tools'))

    def store_bytes(self, filename, contents):
        source = self.root / ("source-" + filename)
        source.write_bytes(contents)
        return self.store.store_file(filename, source)

    def test_limit_rejects_at_registration_and_replacement_counts_current_version(self):
        with patch('app.artifact_store.ARTIFACT_RETURN_LIMIT_BYTES', 8):
            self.store_bytes('a.txt', b'1234')
            self.store_bytes('b.txt', b'5678')
            with self.assertRaisesRegex(ValueError, 'not registered'):
                self.store_bytes('c.txt', b'9')
            self.store_bytes('a.txt', b'xy')
            self.store_bytes('c.txt', b'90')
        delivered = self.store.finalize_for_result()
        self.assertEqual({x.filename: (self.store.out_dir/x.file_token).read_bytes() for x in delivered},
            {'a.txt': b'xy', 'b.txt': b'5678', 'c.txt': b'90'})
        self.assertEqual(self.store.dropped_count, 0)

    def test_concurrent_admission_cannot_exceed_budget(self):
        def store(n):
            try:
                return self.store_bytes(f'{n}.txt', b'123456')
            except ValueError:
                return None
        with patch('app.artifact_store.ARTIFACT_RETURN_LIMIT_BYTES', 8), ThreadPoolExecutor(2) as pool:
            results = list(pool.map(store, range(2)))
        self.assertEqual(sum(r is not None for r in results), 1)
        self.assertEqual(len(self.store.finalize_for_result()), 1)

    def test_streamed_copy_preserves_bytes_and_rejects_changed_inspection(self):
        source = self.root/'original.bin'
        contents = b'abcdefgh' * (256 * 1024)
        source.write_bytes(contents)
        sha = hashlib.sha256(contents).hexdigest()
        self.store.reviewed_fingerprints.add(f'{source.resolve()}::{sha}')
        with patch.object(Path, 'read_bytes', side_effect=AssertionError('full-file read')):
            item = self.store.store_file('copy.bin', source, require_review=True)
        self.assertEqual(item['sha256'], sha)
        self.assertEqual((self.store.out_dir/item['file_token']).read_bytes(), contents)
        files_before = set(self.store.out_dir.iterdir())
        source.write_bytes(b'changed')
        with self.assertRaisesRegex(ValueError, 'no complete page inspection'):
            self.store.store_file('copy.bin', source, require_review=True)
        self.assertEqual(set(self.store.out_dir.iterdir()), files_before)
        self.assertEqual(self.store.finalize_for_result()[0].sha256, sha)

    def test_uncertain_delivery_retries_same_identity_before_registration(self):
        attempts = []
        def publish(payload, artifact, path):
            attempts.append((artifact.file_token, path.read_bytes()))
            if len(attempts) == 1:
                raise RuntimeError('upload response lost after commit')
            return artifact.model_copy(update={'persisted': True, 'artifact_id': 'stored', 'download_url': '/download'})
        self.store.publisher = publish
        with self.assertRaisesRegex(RuntimeError, 'response lost'):
            self.store_bytes('notes.txt', b'complete bytes')
        self.assertEqual(self.store.finalize_for_result(), [])
        self.assertEqual(list(self.store.out_dir.iterdir()), [])
        result = self.store_bytes('notes.txt', b'complete bytes')
        self.assertTrue(result['persisted'])
        self.assertEqual(result['artifact_id'], 'stored')
        self.assertEqual(attempts[0], attempts[1])
        self.assertEqual(self.store_bytes('notes.txt', b'complete bytes'), result)
        self.assertEqual(len(attempts), 2)
        self.assertEqual(len(self.store.finalize_for_result()), 1)

    def test_failed_replacement_keeps_previously_delivered_version(self):
        first = self.store_bytes('notes.txt', b'original')
        def unavailable(*_):
            raise RuntimeError('object storage unavailable')
        self.store.publisher = unavailable
        with self.assertRaisesRegex(RuntimeError, 'storage unavailable'):
            self.store_bytes('notes.txt', b'replacement')
        retained = self.store.finalize_for_result()
        self.assertEqual(len(retained), 1)
        self.assertEqual(retained[0].file_token, first['file_token'])
        self.assertEqual((self.store.out_dir / retained[0].file_token).read_bytes(), b'original')
