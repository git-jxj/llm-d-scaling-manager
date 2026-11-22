#!/usr/bin/env python3
"""A Prometheus query API for hack/check-record-export.sh.

Answers /api/v1/query the way Prometheus answers the manual recording's
instant query for a range, NAME{namespace='NS'}[RANGEs] at TIME: a matrix of
one series of samples every 15 s for each name in --present, in the
namespace --namespace (--lmcache-namespace for lmcache_mp_ names), and an
empty matrix for any other name, as a real Prometheus answers a series it
does not hold. A name in --fail answers with a Prometheus error and HTTP 422.
It writes the port it listens on to --port-file, then serves until killed.
"""

import argparse
import json
import re
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

SELECTOR = re.compile(r"^(?P<name>[A-Za-z_:][A-Za-z0-9_:]*)(\{namespace='(?P<ns>[^']*)'\})?\[(?P<range>\d+)s\]$")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--present", default="")
    ap.add_argument("--fail", default="")
    ap.add_argument("--namespace", required=True)
    ap.add_argument("--lmcache-namespace", required=True)
    ap.add_argument("--port-file", required=True)
    args = ap.parse_args()
    present = set(args.present.split())
    fail = set(args.fail.split())

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def reply(self, code, body):
            data = json.dumps(body).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            u = urlparse(self.path)
            q = parse_qs(u.query)
            if u.path != "/api/v1/query":
                self.reply(404, {"status": "error", "errorType": "not_found", "error": u.path})
                return
            m = SELECTOR.match(q.get("query", [""])[0])
            if not m:
                self.reply(400, {"status": "error", "errorType": "bad_data", "error": "unexpected query"})
                return
            name, ns = m["name"], m["ns"]
            if name in fail:
                self.reply(422, {"status": "error", "errorType": "execution",
                                 "error": "query processing would load too many samples into memory"})
                return
            want_ns = args.lmcache_namespace if name.startswith("lmcache_mp_") else args.namespace
            result = []
            if name in present and (ns is None or ns == want_ns):
                end = float(q["time"][0])
                start = end - int(m["range"])
                values = []
                t = start + 15
                while t <= end:
                    values.append([t, str(int(t - start))])
                    t += 15
                labels = {"__name__": name, "pod": "m-dec-0"}
                if ns is not None:
                    labels["namespace"] = ns
                result.append({"metric": labels, "values": values})
            self.reply(200, {"status": "success", "data": {"resultType": "matrix", "result": result}})

    srv = HTTPServer(("127.0.0.1", 0), Handler)
    with open(args.port_file, "w") as f:
        f.write(str(srv.server_address[1]))
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
