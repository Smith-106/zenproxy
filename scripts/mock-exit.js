// Mock egress exit: a minimal HTTP forward proxy. It relays absolute-form
// requests to their target, which is exactly what zenproxy's pool expects from
// an `http://` exit.
const http = require('http');
const net = require('net');

let hits = 0;

const srv = http.createServer((req, res) => {
  hits += 1;
  if (!/^https?:\/\//i.test(req.url)) {
    res.writeHead(400, { 'content-type': 'text/plain' });
    res.end('mock-exit expects an absolute-form request URI');
    return;
  }
  const target = new URL(req.url);
  const headers = Object.assign({}, req.headers, { host: target.host });
  delete headers['proxy-authorization'];

  const upstream = http.request(
    {
      hostname: target.hostname,
      port: target.port || 80,
      path: target.pathname + target.search,
      method: req.method,
      headers,
    },
    (upstreamRes) => {
      res.writeHead(upstreamRes.statusCode, upstreamRes.headers);
      upstreamRes.pipe(res);
    }
  );
  upstream.on('error', (err) => {
    res.writeHead(502, { 'content-type': 'text/plain' });
    res.end('mock-exit: ' + err.message);
  });
  req.pipe(upstream);
});

srv.on('request', () => {
  if (process.env.MOCK_EXIT_VERBOSE) console.log('mock-exit hit #' + hits);
});

// CONNECT support. zenproxy's health probe dials the upstream with a TCP
// tunnel (m.DialContext), which for an http:// exit means CONNECT. Node's
// http.Server destroys the socket when nothing handles 'connect', so without
// this the mock would look dead to the prober while serving real traffic
// perfectly — and once pruning exists, that false signal retires good exits.
srv.on('connect', (req, clientSocket, head) => {
  const [host, port] = req.url.split(':');
  const upstream = net.connect(Number(port) || 443, host, () => {
    clientSocket.write('HTTP/1.1 200 Connection Established\r\n\r\n');
    if (head && head.length) upstream.write(head);
    upstream.pipe(clientSocket);
    clientSocket.pipe(upstream);
  });
  upstream.on('error', () => clientSocket.destroy());
  clientSocket.on('error', () => upstream.destroy());
});

srv.listen(28082, '127.0.0.1', () => console.log('mock exit listening on 127.0.0.1:28082'));
