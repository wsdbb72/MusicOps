from .index import index_library
from musicops.domain.identity import match_track
from musicops.domain.quality import propose_duplicates

__all__ = ['index_library', 'match_track', 'propose_duplicates', 'TransactionEngine']


def __getattr__(name):
    if name == 'TransactionEngine':
        from .transactions import TransactionEngine
        return TransactionEngine
    raise AttributeError(name)
