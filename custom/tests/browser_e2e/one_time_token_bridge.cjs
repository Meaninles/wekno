'use strict'

// A localhost-only, single-use bridge for authenticated browser E2E.
// The test runner obtains an existing short-lived token without printing it,
// passes it through the environment, and the browser consumes it once. This
// avoids putting real credentials or bearer tokens in Playwright commands,
// screenshots, logs, or checked-in fixtures.
const http = require('http')

const token = String(process.env.WEKNORA_E2E_ADMIN_TOKEN || '').trim()
const nonce = String(process.env.WEKNORA_E2E_TOKEN_BRIDGE_NONCE || '').trim()
const port = Number(process.env.WEKNORA_E2E_TOKEN_BRIDGE_PORT || '19091')
const allowedOrigin = String(
  process.env.WEKNORA_E2E_TOKEN_BRIDGE_ORIGIN || 'http://localhost:5177',
).replace(/\/+$/, '')

if (!token || !/^[a-f0-9]{32}$/i.test(nonce) || !Number.isInteger(port) || port < 1024 || port > 65535) {
  process.exit(2)
}

let consumed = false
const server = http.createServer((request, response) => {
  const origin = String(request.headers.origin || '')
  if (
    consumed
    || request.method !== 'GET'
    || request.url !== `/token/${nonce}`
    || origin !== allowedOrigin
  ) {
    response.writeHead(404, {
      'Cache-Control': 'no-store',
      'Content-Type': 'text/plain; charset=utf-8',
    })
    response.end('not found')
    return
  }

  consumed = true
  response.writeHead(200, {
    'Access-Control-Allow-Origin': allowedOrigin,
    'Cache-Control': 'no-store',
    'Content-Type': 'text/plain; charset=utf-8',
    'Referrer-Policy': 'no-referrer',
  })
  response.end(token, () => server.close())
})

server.listen(port, '127.0.0.1')

const expiry = setTimeout(() => server.close(), 60_000)
expiry.unref()
