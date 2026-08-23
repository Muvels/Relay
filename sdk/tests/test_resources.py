import pytest

import relay
from relay.exceptions import SpecError
from relay.resources import (
    build_resources,
    format_mib,
    parse_gpu,
    parse_memory,
    parse_timeout,
)


class TestParseMemory:
    @pytest.mark.parametrize(
        ("raw", "mib"),
        [
            ("512MB", 512),
            ("32GB", 32 * 1024),
            ("1.5GB", 1536),
            ("24GiB", 24 * 1024),
            ("1TB", 1024 * 1024),
            (2048, 2048),
        ],
    )
    def test_valid(self, raw, mib):
        assert parse_memory(raw) == mib

    @pytest.mark.parametrize("raw", ["", "GB", "12XB", "-1GB", 0, -5, "1 2GB", True])
    def test_invalid(self, raw):
        with pytest.raises(SpecError):
            parse_memory(raw)

    def test_error_names_the_field(self):
        with pytest.raises(SpecError, match="memory='wat'"):
            parse_memory("wat")


class TestParseTimeout:
    @pytest.mark.parametrize(
        ("raw", "seconds"),
        [("90s", 90), ("30m", 1800), ("8h", 8 * 3600), ("2d", 2 * 86400), (45, 45), ("120", 120)],
    )
    def test_valid(self, raw, seconds):
        assert parse_timeout(raw) == seconds

    @pytest.mark.parametrize("raw", ["", "h", "5w", "-3m", 0])
    def test_invalid(self, raw):
        with pytest.raises(SpecError):
            parse_timeout(raw)


class TestGpuShorthand:
    def test_memory_string(self):
        gpu = parse_gpu("24GB")
        assert gpu.kind == "cuda" and gpu.memory_mib == 24 * 1024 and gpu.count == 1
        assert gpu.exclusive is True

    def test_count_int(self):
        assert parse_gpu(2).count == 2

    def test_any(self):
        gpu = parse_gpu("any")
        assert gpu.kind == "cuda" and gpu.memory_mib == 0
        assert gpu.exclusive is True

    def test_explicit_object_passthrough(self):
        spec = relay.GPU(memory="40GB", count=2, exclusive=False)
        assert parse_gpu(spec) is spec

    def test_explicit_shared_needs_memory_budget(self):
        with pytest.raises(SpecError, match="needs a memory reservation"):
            relay.GPU(exclusive=False)

    def test_explicit_exclusive_can_set_device_minimum(self):
        gpu = relay.GPU(memory="40GB", exclusive=True)
        assert gpu.memory_mib == 40 * 1024
        assert gpu.exclusive is True

    @pytest.mark.parametrize("value", [0, 1, "false"])
    def test_exclusive_must_be_boolean(self, value):
        with pytest.raises(SpecError, match="must be true or false"):
            relay.GPU(memory="8GB", exclusive=value)

    def test_none(self):
        assert parse_gpu(None) is None

    def test_garbage(self):
        with pytest.raises(SpecError, match="gpu='much-gpu'"):
            parse_gpu("much-gpu")


class TestAccelerators:
    def test_any_of(self):
        acc = relay.any_of(relay.CUDA(memory="20GB"), relay.MPS(memory="20GB"))
        kinds = [o.kind for o in acc.options]
        assert kinds == ["cuda", "mps"]
        assert "CUDA" in acc.describe() and "MPS" in acc.describe()

    def test_any_of_rejects_non_accelerators(self):
        with pytest.raises(SpecError):
            relay.any_of("24GB")

    def test_gpu_and_accelerator_mutually_exclusive(self):
        with pytest.raises(SpecError, match="not both"):
            build_resources(gpu="24GB", accelerator=relay.MPS())

    def test_description_names_sharing_mode(self):
        assert relay.GPU(memory="24GB", exclusive=False).describe().endswith("shared")
        assert relay.GPU().describe().endswith("exclusive")


class TestBuildResources:
    def test_full(self):
        r = build_resources(cpu=8, memory="32GB", gpu="24GB", timeout="8h")
        assert r.cpu == 8.0
        assert r.memory_mib == 32 * 1024
        assert r.timeout_s == 8 * 3600
        assert r.accelerators.options[0].memory_mib == 24 * 1024

    def test_empty(self):
        r = build_resources()
        assert r.describe() == "no resource constraints"

    def test_bad_cpu(self):
        with pytest.raises(SpecError):
            build_resources(cpu=-1)


def test_format_mib():
    assert format_mib(2048) == "2GB"
    assert format_mib(1536) == "1536MB"
