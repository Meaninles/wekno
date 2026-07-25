'use strict'

const crypto = require('crypto')
const { spawn } = require('child_process')

async function main() {
  const baseURL = (process.env.WEKNORA_E2E_HOST || 'http://localhost:8080').replace(/\/+$/, '')
  const username = process.env.WEKNORA_E2E_USERNAME || ''
  const password = process.env.WEKNORA_E2E_PASSWORD || ''
  const [command, ...args] = process.argv.slice(2)
  if (!username || !password) {
    throw new Error('WEKNORA_E2E_USERNAME and WEKNORA_E2E_PASSWORD are required')
  }
  if (!command) {
    throw new Error('a child command is required')
  }

  const challengeResponse = await fetch(
    `${baseURL}/api/v1/custom/auth-security/challenge`,
  )
  const challengePayload = await challengeResponse.json()
  const challenge = challengePayload && challengePayload.data
  if (!challengeResponse.ok || !challenge) {
    throw new Error('could not obtain an authentication challenge')
  }
  const encodedSVG = String(challenge.captcha_image || '').split(',')[1] || ''
  const svg = Buffer.from(encodedSVG, 'base64').toString('utf8')
  const match = svg.match(/<text[^>]*>([^<]+)<\/text>/)
  if (!match) {
    throw new Error('authentication challenge contains no CAPTCHA text')
  }
  const encryptedPassword = crypto.publicEncrypt(
    {
      key: challenge.public_key,
      oaepHash: 'sha256',
      padding: crypto.constants.RSA_PKCS1_OAEP_PADDING,
    },
    Buffer.from(password),
  ).toString('base64')
  const loginResponse = await fetch(`${baseURL}/api/v1/auth/login`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({
      username,
      encrypted_password: encryptedPassword,
      challenge_id: challenge.challenge_id,
      captcha_answer: match[1],
    }),
  })
  const loginPayload = await loginResponse.json()
  if (!loginResponse.ok || !loginPayload.success || !loginPayload.token) {
    throw new Error('authenticated E2E login failed')
  }

  const {
    WEKNORA_E2E_USERNAME: _discardUsername,
    WEKNORA_E2E_PASSWORD: _discardPassword,
    ...safeEnvironment
  } = process.env
  const child = spawn(command, args, {
    cwd: process.cwd(),
    env: {
      ...safeEnvironment,
      WEKNORA_E2E_ADMIN_TOKEN: loginPayload.token,
    },
    stdio: 'inherit',
  })
  child.on('error', error => {
    throw error
  })
  child.on('exit', code => {
    process.exit(code === null ? 1 : code)
  })
}

main().catch(error => {
  console.error(error.message)
  process.exit(1)
})
