import sys

import pytest

from relay.exceptions import SpecError
from relay.image import Image, client_python_minor, default_image


class TestDualDispatch:
    def test_debian_then_python_keeps_both(self):
        df = Image.debian("12").python("3.12").dockerfile()
        assert "FROM python:3.12-slim-bookworm" in df

    def test_chain_state_survives_python_modifier(self):
        df = Image.debian("12").apt("git").python("3.12").dockerfile()
        assert "FROM python:3.12-slim-bookworm" in df
        assert "git" in df

    def test_registry_plus_python_rejected(self):
        with pytest.raises(SpecError, match="contradictory"):
            Image.from_registry("nvidia/cuda:13.0.0-base").python("3.12").dockerfile()


class TestInjectionGuards:
    def test_env_name_with_newline_rejected(self):
        with pytest.raises(SpecError):
            Image.python("3.12").env(**{"SAFE\nRUN touch /pwned": "x"})

    def test_env_value_with_newline_rejected(self):
        with pytest.raises(SpecError):
            Image.python("3.12").env(HOME="a\nRUN evil")

    def test_workdir_injection_rejected(self):
        with pytest.raises(SpecError):
            Image.python("3.12").workdir("/app\nRUN evil")

    def test_registry_ref_with_newline_rejected(self):
        with pytest.raises(SpecError):
            Image.from_registry("img\nRUN evil")

    def test_package_with_tab_rejected(self):
        with pytest.raises(SpecError):
            Image.python("3.12").pip("torch\tRUN evil")


class TestConstruction:
    def test_python_default_matches_client(self):
        img = Image.python()
        assert f"python:{sys.version_info.major}.{sys.version_info.minor}-slim" in img.dockerfile()

    def test_python_explicit(self):
        assert "FROM python:3.12-slim" in Image.python("3.12").dockerfile()

    def test_python_invalid(self):
        with pytest.raises(SpecError):
            Image.python("twelve")

    def test_debian_bootstraps_python(self):
        df = Image.debian("12").dockerfile()
        assert "FROM debian:12-slim" in df
        assert "python3-pip" in df

    def test_from_registry(self):
        df = Image.from_registry("nvidia/cuda:13.0.0-runtime-ubuntu24.04").dockerfile()
        assert df.startswith("FROM nvidia/cuda:13.0.0-runtime-ubuntu24.04")


class TestChaining:
    def test_immutable(self):
        base = Image.python("3.12")
        derived = base.pip("torch")
        assert base.content_hash() != derived.content_hash()
        assert "torch" not in base.dockerfile()

    def test_ops_render_in_order(self):
        df = (
            Image.python("3.12")
            .apt("git", "ffmpeg")
            .pip("torch==2.8", "transformers")
            .env(HF_HOME="/cache/hf")
            .run("echo hello")
            .dockerfile()
        )
        apt_pos = df.index("apt-get install -y --no-install-recommends git ffmpeg")
        pip_pos = df.index("torch==2.8 transformers")
        env_pos = df.index("ENV HF_HOME=/cache/hf")
        run_pos = df.index("RUN echo hello")
        assert apt_pos < pip_pos < env_pos < run_pos

    def test_runtime_contract_always_present(self):
        import cloudpickle

        df = Image.python("3.12").dockerfile()
        assert f"cloudpickle=={cloudpickle.__version__}" in df
        assert "WORKDIR /workspace" in df

    def test_modal_aliases(self):
        img = Image.python("3.12").pip_install("torch").apt_install("git")
        assert "torch" in img.dockerfile() and "git" in img.dockerfile()

    def test_empty_ops_rejected(self):
        with pytest.raises(SpecError):
            Image.python().pip()
        with pytest.raises(SpecError):
            Image.python().env()


class TestHashing:
    def test_deterministic(self):
        a = Image.python("3.12").pip("torch")
        b = Image.python("3.12").pip("torch")
        assert a.content_hash() == b.content_hash()
        assert a.tag() == b.tag()

    def test_sensitive_to_content(self):
        assert (
            Image.python("3.12").pip("torch").content_hash()
            != Image.python("3.12").pip("torch==2.8").content_hash()
        )

    def test_tag_shape(self):
        assert default_image().tag().startswith("relay-img:")


def test_client_python_minor_shape():
    major, minor = client_python_minor().split(".")
    assert int(major) >= 3 and int(minor) >= 0
