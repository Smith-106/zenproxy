// Mock upstream: echoes back what it received so the proxy's header rewrite
// and path collapsing can be checked from the outside.
const http = require('http');

http
  .createServer((req, res) => {
    // The health prober asks this endpoint for the exit's public IP. Answering
    // with a real-shaped address keeps the smoke harness faithful end to end:
    // config.smoke.yaml points probe_url here, and zenproxy only accepts a
    // response that actually parses as an IP. TEST-NET-3, never routed.
    if (req.url.includes('probe')) {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ ip: '203.0.113.7' }));
      return;
    }
    let body = '';
    req.on('data', (c) => (body += c));
    req.on('end', () => {
      res.writeHead(200, { 'content-type': 'application/json' });
      res.end(
        JSON.stringify({
          path: req.url,
          auth: req.headers.authorization || null,
          apiKeyHeader: req.headers['x-api-key'] || null,
          proxyAuthLeaked: req.headers['proxy-authorization'] || null,
          bodyBytes: body.length,
        })
      );
    });
  })
  .listen(28081, '127.0.0.1', () => console.log('mock upstream listening on 127.0.0.1:28081'));
