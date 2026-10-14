class MusicOpsError(Exception):
    error_class = 'STATE_ERROR'

    def __init__(self, message, *, details=None):
        super().__init__(message)
        self.details = details or {}


class Conflict(MusicOpsError):
    error_class = 'CONFLICT'


class PolicyBlocked(MusicOpsError):
    error_class = 'POLICY_BLOCKED'


class ValidationFailed(MusicOpsError):
    error_class = 'VALIDATION_FAILED'


class PathBlocked(MusicOpsError):
    error_class = 'PATH_BLOCKED'


class RecoveryRequired(MusicOpsError):
    error_class = 'RECOVERY_REQUIRED'
