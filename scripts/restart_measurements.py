"""Separate deliberate stop attempts from the planned-restart observation."""


def mount_omission_phase(begin, end, stop_windows):
    if any(begin >= left and end <= right for left, right in stop_windows):
        return 'precondition'
    if any(begin <= right and end >= left for left, right in stop_windows):
        return 'boundary'
    return 'planned'
