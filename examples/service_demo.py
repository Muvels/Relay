"""A tiny service for testing exposure modes end to end.

    relay deploy examples/service_demo.py

Flip `expose=` between "private" and "funnel" and redeploy to test each.
Call it with the key `relay deploy` prints:

    curl -H "Authorization: Bearer <service-key>" http://<endpoint>/
"""

import relay

app = relay.App("demo")


@app.function(cpu=1, memory="256MB")
def hello(name: str = "fleet") -> str:
    return f"hello {name}"


@app.service(port=8900, cpu=1, memory="256MB", expose="private")
def whoami():
    import json
    import platform
    from datetime import datetime, timezone
    from http.server import BaseHTTPRequestHandler, HTTPServer

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            body = json.dumps({
                "service": "demo.whoami",
                "machine": platform.node(),
                "system": f"{platform.system()} {platform.machine()}",
                "time": datetime.now(timezone.utc).isoformat(),
            }, indent=2).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass

    print("whoami listening on 8900", flush=True)
    HTTPServer(("0.0.0.0", 8900), Handler).serve_forever()
