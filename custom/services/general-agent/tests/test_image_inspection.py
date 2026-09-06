import base64
import io
import sys
import tempfile
import unittest
from pathlib import Path
from PIL import Image

sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from app.image_inspection import prepare_image, native_read_modality_error


class ImageInspectionTests(unittest.TestCase):
    def test_local_image_transports_pixels_and_prompt(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);Image.new('RGB',(12,8),(15,45,75)).save(root/'layout.png')
            result, view=prepare_image({'file_path':'layout.png','prompt':'Read the visible labels.'},root)
            self.assertTrue(base64.b64decode(result['image_base64']).startswith(b'\x89PNG'))
            self.assertEqual(result['file_name'],'layout.png')
            self.assertEqual(result['prompt'],'Read the visible labels.')
            self.assertEqual(view['region'],[0,0,12,8])
            self.assertFalse(view['resized'])

    def test_read_capability_check_is_independent_of_question(self):
        for path in ['draft.png','scan.PDF','/tmp/chart.jpeg']:
            self.assertTrue(native_read_modality_error({'file_path':path},False))
            self.assertFalse(native_read_modality_error({'file_path':path},True))
        self.assertFalse(native_read_modality_error({'file_path':'code.py'},False))

    def test_missing_file_and_animation_do_not_claim_visual_review(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            with self.assertRaises(FileNotFoundError):prepare_image({'file_path':'missing.png','prompt':'Check'},root)
            Image.new('RGB',(2,2),'red').save(root/'animation.gif',save_all=True,append_images=[Image.new('RGB',(2,2),'blue')])
            with self.assertRaisesRegex(ValueError,'frame'):prepare_image({'file_path':'animation.gif','prompt':'Check'},root)
            result, view=prepare_image({'file_path':'animation.gif','prompt':'Check','frame':1},root)
            self.assertEqual(view['frame_count'],2)
            with Image.open(io.BytesIO(base64.b64decode(result['image_base64']))) as image:
                self.assertEqual(image.convert('RGB').getpixel((0,0)),(0,0,255))

    def test_large_original_overview_and_exact_detail_share_source_coordinates(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            # A BMP just under the upload limit, with distinct source pixels.
            source=Image.new('RGB',(6688,6688),(15,45,75))
            source.putpixel((6000,6001),(200,100,50));source.save(root/'large.bmp');source.close()
            self.assertGreater((root/'large.bmp').stat().st_size,127*1024*1024)
            result,view=prepare_image({'file_path':'large.bmp','prompt':'Inspect'},root)
            self.assertTrue(view['resized'])
            self.assertEqual(view['source_width'],6688)
            self.assertLessEqual(view['view_width']*view['view_height'],4_000_000)
            self.assertLess(len(base64.b64decode(result['image_base64'])),20*1024*1024)
            result,view=prepare_image({'file_path':'large.bmp','prompt':'Inspect','region':[5999,6000,6002,6003]},root)
            self.assertFalse(view['resized'])
            with Image.open(io.BytesIO(base64.b64decode(result['image_base64']))) as image:
                self.assertEqual(image.getpixel((1,1)),(200,100,50))
            with self.assertRaisesRegex(ValueError,'positive area'):
                prepare_image({'file_path':'large.bmp','prompt':'Inspect','region':[6000,0,5000,2]},root)
