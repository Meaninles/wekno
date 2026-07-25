import unittest
from unittest import mock

from weknora_document_splitter import image_normalizer


class ImageNormalizerTests(unittest.TestCase):
    def test_raster_image_is_passed_through(self):
        payload = b"\x89PNG\r\n\x1a\nnot-a-full-test-image"
        name, mime, normalized, converted = (
            image_normalizer.normalize_image_for_transport("sample.png", payload)
        )
        self.assertEqual(name, "sample.png")
        self.assertEqual(mime, "image/png")
        self.assertEqual(normalized, payload)
        self.assertFalse(converted)

    def test_x_emf_is_detected_and_renamed_after_conversion(self):
        payload = bytearray(64)
        payload[40:44] = b" EMF"
        with mock.patch.object(
            image_normalizer, "_rasterize_vector", return_value=b"\x89PNG\r\n\x1a\n"
        ) as rasterize:
            name, mime, normalized, converted = (
                image_normalizer.normalize_image_for_transport(
                    "diagram.x-emf", bytes(payload)
                )
            )
        rasterize.assert_called_once_with(bytes(payload), ".emf")
        self.assertEqual(name, "diagram.png")
        self.assertEqual(mime, "image/png")
        self.assertEqual(normalized, b"\x89PNG\r\n\x1a\n")
        self.assertTrue(converted)

    def test_conversion_failure_is_not_silently_forwarded(self):
        payload = bytearray(64)
        payload[40:44] = b" EMF"
        with mock.patch.object(
            image_normalizer,
            "_rasterize_vector",
            side_effect=image_normalizer.ImageNormalizationError("boom"),
        ):
            with self.assertRaisesRegex(
                image_normalizer.ImageNormalizationError, "boom"
            ):
                image_normalizer.normalize_image_for_transport(
                    "diagram.emf", bytes(payload)
                )

    def test_office_vectors_share_one_bounded_batch_and_keep_order(self):
        first = bytearray(64)
        first[40:44] = b" EMF"
        second = bytearray(64)
        second[40:44] = b" EMF"
        raster = b"\x89PNG\r\n\x1a\nraster"
        with mock.patch.object(
            image_normalizer,
            "_rasterize_office_vector_batch",
            return_value=[b"png-one", b"png-two"],
        ) as rasterize:
            normalized = image_normalizer.normalize_images_for_transport(
                [
                    ("first.x-emf", bytes(first)),
                    ("already.png", raster),
                    ("second.emf", bytes(second)),
                ]
            )
        rasterize.assert_called_once_with(
            [(bytes(first), ".emf"), (bytes(second), ".emf")]
        )
        self.assertEqual(
            normalized,
            [
                ("first.png", "image/png", b"png-one", True),
                ("already.png", "image/png", raster, False),
                ("second.png", "image/png", b"png-two", True),
            ],
        )


if __name__ == "__main__":
    unittest.main()
