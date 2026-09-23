import { test } from 'node:test'
import assert from 'node:assert/strict'
import { normalizeLocalCode, isLocalCode, safeStorage, secureBrowserID } from './local-code.mjs'
test('long and short codes survive clipboard formatting', () => {
 for (const code of ['MAPLE-'+'A'.repeat(48),'MAPLE-PLUS-'+'B'.repeat(48),'PULS-'+'C'.repeat(15),'GO-'+'D'.repeat(15),'PRO-'+'E'.repeat(15),'PRO5X-'+'G'.repeat(15),'F'.repeat(15)]) {
  assert.ok(isLocalCode(code));assert.equal(normalizeLocalCode(' \u200b'+code.toLowerCase().replaceAll('-','—')+'\n'),code)
 }
 assert.equal(isLocalCode('SXC-1234'),false)
})
test('blocked storage does not break verification setup', () => {
 globalThis.window={get localStorage(){throw Error('blocked')}}
 const storage=safeStorage('localStorage');assert.equal(storage.getItem('x'),null);storage.setItem('x','y');storage.removeItem('x')
 assert.equal(secureBrowserID().length,48)
})
