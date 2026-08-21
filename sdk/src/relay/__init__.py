"""Relay: Modal for hardware you already own.

    import relay

    app = relay.App("trainer")

    @app.function(gpu="24GB", cpu=8, memory="32GB")
    def train(steps: int = 1000) -> dict:
        ...

    train(10)              # plain local call
    train.remote(10)       # run on Relay, block, return the result
    run = train.spawn(10)  # fire and forget → Run handle
"""

from .app import App
from .exceptions import (
    ExecutorError,
    RelayError,
    RemoteFunctionError,
    SpecError,
)
from .image import Image
from .resources import CUDA, GPU, MPS, Resources, any_of
from .secret import Secret
from .volume import Volume

__version__ = "0.1.0"

__all__ = [
    "App",
    "Image",
    "Volume",
    "Secret",
    "GPU",
    "CUDA",
    "MPS",
    "any_of",
    "Resources",
    "RelayError",
    "SpecError",
    "ExecutorError",
    "RemoteFunctionError",
    "__version__",
]
