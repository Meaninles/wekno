import hashlib
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from urllib.error import URLError

from app.runner import upload_artifact_before_completion
from app.schemas import ChatPayload, SidecarArtifact


class ArtifactUploadTest(unittest.TestCase):
    def test_retry_streams_the_same_bytes_with_an_explicit_length(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder)/'artifact'
            contents = b'abcdefgh' * (256 * 1024)
            path.write_bytes(contents)
            artifact = SidecarArtifact(file_token='same-reservation', filename='data.bin', file_type='bin',
                file_size=len(contents), sha256=hashlib.sha256(contents).hexdigest())
            payload = ChatPayload(run_id='run', session_id='session', assistant_message_id='message',
                query='task', llm={'model_name':'configured'}, tool_callback_url='http://fixture/tools',
                artifact_upload_url='http://fixture/artifacts')
            requests = []
            def upload(request, **_):
                self.assertFalse(isinstance(request.data, bytes))
                self.assertEqual(request.get_header('Content-length'), str(len(contents)))
                received = b''.join(iter(lambda: request.data.read(8192), b''))
                requests.append((request.get_header('X-weknora-artifact-metadata'), received))
                if len(requests) == 1:
                    raise URLError('lost response after commit')
                result = artifact.model_copy(update={'persisted':True, 'artifact_id':'stored'})
                import io
                return io.BytesIO(result.model_dump_json().encode())
            with patch.object(Path, 'read_bytes', side_effect=AssertionError('full-file read')), \
                 patch('app.runner.urlrequest.urlopen', side_effect=upload), patch('app.runner.time.sleep'):
                result = upload_artifact_before_completion(payload, artifact, path)
            self.assertTrue(result.persisted)
            self.assertEqual(requests[0], requests[1])
            self.assertEqual(requests[1][1], contents)
