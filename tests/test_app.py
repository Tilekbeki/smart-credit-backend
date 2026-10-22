import unittest
from backend.app import APPLICATION_STATUSES


class StatusTests(unittest.TestCase):
    def test_all_documented_statuses_are_available(self):
        self.assertEqual(len(APPLICATION_STATUSES), 9)
        self.assertEqual(len(set(APPLICATION_STATUSES)), 9)
        self.assertIn("manual_review", APPLICATION_STATUSES)


if __name__ == "__main__":
    unittest.main()
