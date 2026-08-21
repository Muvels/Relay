import pytest

import relay
from relay.exceptions import SpecError


def make_app():
    return relay.App("test-app")


class TestDecoration:
    def test_plain_call_is_plain_python(self):
        app = make_app()

        @app.function(cpu=1)
        def add(a: int, b: int) -> int:
            return a + b

        assert add(2, 3) == 5
        assert add.name == "add"
        assert app.functions["add"] is add

    def test_metadata_preserved(self):
        app = make_app()

        @app.function()
        def documented() -> None:
            """the docstring"""

        assert documented.__doc__ == "the docstring"
        assert documented.__name__ == "documented"

    def test_spec_errors_at_import_time(self):
        app = make_app()
        with pytest.raises(SpecError):

            @app.function(memory="lots")
            def broken() -> None: ...

    def test_duplicate_names_rejected(self):
        app = make_app()

        @app.function()
        def fn() -> None: ...

        with pytest.raises(SpecError, match="already has a function"):
            app.function()(fn.raw)

    def test_default_image_matches_client_python(self):
        app = make_app()

        @app.function()
        def fn() -> None: ...

        assert "python:" in fn.image.dockerfile().splitlines()[0]

    def test_entry_module_and_project_root(self):
        # This test file lives at sdk/tests/; sdk/ carries pyproject.toml,
        # so the discovered root is sdk/ and the module is dotted.
        assert module_level_fn.entry_module == "tests.test_function"
        assert (module_level_fn.project_dir / "pyproject.toml").exists()

    def test_non_function_rejected(self):
        app = make_app()
        with pytest.raises(SpecError):
            app.function()(42)

    def test_async_rejected(self):
        app = make_app()
        with pytest.raises(SpecError, match="async"):

            @app.function()
            async def afn() -> None: ...

    def test_nested_decorates_but_cannot_go_remote(self):
        app = make_app()

        @app.function()
        def inner() -> None: ...

        assert inner() is None  # local call fine
        with pytest.raises(SpecError, match="nested"):
            inner.remote()


_module_app = relay.App("module-level")


@_module_app.function()
def module_level_fn() -> None: ...


class TestAppValidation:
    def test_bad_names(self):
        for bad in ["", "has space", "has/slash"]:
            with pytest.raises(SpecError):
                relay.App(bad)
