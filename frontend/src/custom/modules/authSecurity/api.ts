import { get } from '@/utils/request'

export interface AuthChallenge {
  challenge_id: string
  public_key: string
  captcha_image: string
  expires_in_seconds: number
}

export type LoginChallenge = AuthChallenge

export async function getAuthChallenge(): Promise<{ success: boolean; data?: AuthChallenge; message?: string }> {
  try {
    const response = await get('/api/v1/custom/auth-security/challenge')
    return response as { success: boolean; data?: AuthChallenge; message?: string }
  } catch (error: any) {
    return {
      success: false,
      message: error?.message || '获取验证码失败',
    }
  }
}

export const getLoginChallenge = getAuthChallenge

export async function encryptAuthPassword(password: string, publicKeyPem: string): Promise<string> {
  const key = await window.crypto.subtle.importKey(
    'spki',
    pemToArrayBuffer(publicKeyPem),
    {
      name: 'RSA-OAEP',
      hash: 'SHA-256',
    },
    false,
    ['encrypt'],
  )
  const encrypted = await window.crypto.subtle.encrypt(
    { name: 'RSA-OAEP' },
    key,
    new TextEncoder().encode(password),
  )
  return arrayBufferToBase64(encrypted)
}

export const encryptLoginPassword = encryptAuthPassword

function pemToArrayBuffer(pem: string): ArrayBuffer {
  const base64 = pem
    .replace(/-----BEGIN PUBLIC KEY-----/g, '')
    .replace(/-----END PUBLIC KEY-----/g, '')
    .replace(/\s+/g, '')
  const binary = window.atob(base64)
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i)
  }
  return bytes.buffer
}

function arrayBufferToBase64(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer)
  let binary = ''
  for (let i = 0; i < bytes.length; i += 1) {
    binary += String.fromCharCode(bytes[i])
  }
  return window.btoa(binary)
}
