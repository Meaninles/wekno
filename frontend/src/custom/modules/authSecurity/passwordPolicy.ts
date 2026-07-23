export const PASSWORD_MIN_LENGTH = 8
export const PASSWORD_MAX_LENGTH = 32

export function hasPasswordTypeMix(value: unknown): boolean {
  const password = String(value || '')
  if (!password) return true

  let hasLetter = false
  let hasNumber = false
  let hasSymbol = false
  for (const char of Array.from(password)) {
    if (/\p{L}/u.test(char)) {
      hasLetter = true
    } else if (/\p{N}/u.test(char)) {
      hasNumber = true
    } else if (!/\s/u.test(char)) {
      hasSymbol = true
    }
  }
  return [hasLetter, hasNumber, hasSymbol].filter(Boolean).length >= 2
}

export function passwordComplexityError(value: unknown): string {
  const password = String(value || '')
  if (password.length < PASSWORD_MIN_LENGTH || password.length > PASSWORD_MAX_LENGTH) {
    return `密码长度须为 ${PASSWORD_MIN_LENGTH}-${PASSWORD_MAX_LENGTH} 个字符`
  }
  if (/\s/u.test(password)) {
    return '密码不能包含空白字符'
  }
  if (!hasPasswordTypeMix(password)) {
    return '密码须至少包含字母、数字、符号中的两种'
  }
  return ''
}
