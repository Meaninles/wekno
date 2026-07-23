import assert from 'node:assert/strict'
import test from 'node:test'

import {
  hasPasswordTypeMix,
  passwordComplexityError,
} from './passwordPolicy.ts'

test('password policy matches the backend two-category rule', () => {
  for (const password of ['abcdef12', 'abcdef!!', '123456!!', '中文测试12ab']) {
    assert.equal(hasPasswordTypeMix(password), true)
    assert.equal(passwordComplexityError(password), '')
  }

  assert.match(passwordComplexityError('abcdefgh'), /至少包含/)
  assert.match(passwordComplexityError('abc 1234'), /空白/)
  assert.match(passwordComplexityError('Ab1!'), /8-32/)
})
