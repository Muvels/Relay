"""The smallest Relay app. Run it with:

    relay run examples/hello.py::hi --name relay
"""

import relay

app = relay.App("hello")


@app.function(cpu=1, memory="512MB")
def hi(name: str = "world") -> str:
    print("computing a very important greeting…")
    return f"hello {name}"
