import asyncio
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import patch

from app.artifact_store import ArtifactStore
from app.runtime_tools import RuntimeTools
from app.schemas import ChatPayload


class OriginalSourceTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.data = b'original-source\n' * 10000
        self.requests = 0
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                owner.requests += 1
                self.send_response(200)
                self.end_headers()
                self.wfile.write(owner.data)
            def log_message(self, *args):
                pass

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    async def asyncTearDown(self):
        await asyncio.to_thread(self.server.shutdown)
        self.server.server_close()
        self.thread.join()
        self.temp.cleanup()

    def tools(self, size=None):
        payload = ChatPayload.model_validate(dict(run_id='lazy-test', session_id='session', assistant_message_id='answer',
            query='Use the supplied material.', llm={'model_name':'configured'}, tool_callback_url='http://unused',
            original_input_files=[dict(id='source-one',file_name='source.csv',file_type='csv',
                file_size=len(self.data) if size is None else size, sha256=hashlib.sha256(self.data).hexdigest(),
                download_url=f'http://127.0.0.1:{self.server.server_port}/secret-url?signature=private')]))
        return RuntimeTools(payload, ArtifactStore(self.root, payload))

    async def test_no_eager_download_exact_authorization_and_parallel_reuse(self):
        tools = self.tools()
        self.assertEqual(self.requests, 0)
        prompt = tools.originals.prompt()
        self.assertNotIn('secret-url', prompt)
        self.assertIn('source-one', prompt)
        self.assertIn('not been downloaded or inspected', prompt)
        denied = await tools.execute('materialize_file', {'file_id':'another-session'}, 'foreign')
        self.assertFalse(denied['success'])
        self.assertEqual(self.requests, 0)
        results = await asyncio.gather(*(tools.execute('materialize_file', {'file_id':'source-one'}, str(i)) for i in range(2)))
        self.assertTrue(all(r['success'] for r in results))
        self.assertEqual(self.requests, 1)
        self.assertEqual(Path(results[0]['path']).read_bytes(), self.data)
        with patch.object(Path, 'read_bytes', side_effect=AssertionError('whole-file read forbidden')):
            metadata = tools.workspace.metadata(results[0]['path'])
        self.assertEqual(metadata['sha256'], hashlib.sha256(self.data).hexdigest())

    async def test_declared_size_overrun_removes_partial_file_and_does_not_cache_success(self):
        tools = self.tools(size=1)
        result = await tools.execute('materialize_file', {'file_id':'source-one'}, 'oversized')
        self.assertFalse(result['success'])
        self.assertIn('exceeds its declared size', result['error'])
        self.assertFalse(list(self.root.rglob('*.download')))
        self.assertFalse(tools.originals.prepared)
