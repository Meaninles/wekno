import base64
import sys
import tempfile
import unittest
from pathlib import Path
from PIL import Image

sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from app.image_inspection import image_transport_args, native_read_modality_error


class ImageInspectionTests(unittest.TestCase):
    def test_local_image_transports_pixels_and_prompt(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);Image.new('RGB',(12,8),(15,45,75)).save(root/'layout.png')
            result=image_transport_args({'file_path':'layout.png','prompt':'Read the visible labels.'},root)
            self.assertTrue(base64.b64decode(result['image_base64']).startswith(b'\x89PNG'))
            self.assertEqual(result['file_name'],'layout.png')
            self.assertEqual(result['prompt'],'Read the visible labels.')

    def test_read_capability_check_is_independent_of_question(self):
        for path in ['draft.png','scan.PDF','/tmp/chart.jpeg']:
            self.assertTrue(native_read_modality_error({'file_path':path},False))
            self.assertFalse(native_read_modality_error({'file_path':path},True))
        self.assertFalse(native_read_modality_error({'file_path':'code.py'},False))

    def test_missing_file_and_animation_do_not_claim_visual_review(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            with self.assertRaises(FileNotFoundError):image_transport_args({'file_path':'missing.png','prompt':'Check'},root)
            Image.new('RGB',(2,2),'red').save(root/'animation.gif',save_all=True,append_images=[Image.new('RGB',(2,2),'blue')])
            with self.assertRaisesRegex(ValueError,'frame'):image_transport_args({'file_path':'animation.gif','prompt':'Check'},root)
