import unittest

from restart_measurements import mount_omission_phase


class MountOmissionTests(unittest.TestCase):
    def test_stop_interval_shapes(self):
        for name, begin, end, expected in [
            ('before', 1, 9, 'planned'),
            ('contained', 12, 18, 'precondition'),
            ('enters', 9, 12, 'boundary'),
            ('leaves', 18, 21, 'boundary'),
            ('spans', 9, 21, 'boundary'),
            ('after', 21, 29, 'planned'),
            ('exact', 10, 20, 'precondition'),
            ('touches-start', 9, 10, 'boundary'),
            ('touches-end', 20, 21, 'boundary'),
            ('next-stop', 31, 39, 'precondition'),
            ('enters-next', 29, 31, 'boundary'),
        ]:
            with self.subTest(name=name):
                self.assertEqual(mount_omission_phase(begin, end, [(10, 20), (30, 40)]), expected)
        self.assertEqual(mount_omission_phase(1, 2, []), 'planned')


if __name__ == '__main__':
    unittest.main()
