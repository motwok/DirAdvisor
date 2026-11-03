# SPDX-License-Identifier: AGPL-3.0-or-later
# Registry that answers every manifest request with 429.
import json
from http.server import BaseHTTPRequestHandler, HTTPServer


class Handler(BaseHTTPRequestHandler):
    def answer(self):
        if self.path == '/v2/':
            code, body = 200, {}
        else:
            code, body = 429, {'errors': [{'code': 'TOOMANYREQUESTS', 'message': 'rate limit'}]}
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Docker-Distribution-API-Version', 'registry/2.0')
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        if self.command != 'HEAD':
            self.wfile.write(data)

    do_GET = answer
    do_HEAD = answer


HTTPServer(('', 5000), Handler).serve_forever()
