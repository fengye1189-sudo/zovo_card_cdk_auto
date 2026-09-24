import test from 'node:test'
import assert from 'node:assert/strict'
import {
  effectiveCurrentPlan,
  resultCanResubmit,
  resultQueryDeadline,
} from './redemption-display.mjs'

test('inactive subscription overrides a stale upstream Plus label', () => {
  assert.equal(effectiveCurrentPlan('plus', false), 'free')
  assert.equal(effectiveCurrentPlan('Plus', false), 'free')
})

test('active and unknown subscriptions keep the upstream plan label', () => {
  assert.equal(effectiveCurrentPlan('plus', true), 'plus')
  assert.equal(effectiveCurrentPlan('pro_5x', null), 'pro_5x')
})

test('failed-result recovery reads retry permission and query deadline from safe envelopes', () => {
  assert.equal(resultCanResubmit({ data: { order: { can_resubmit: true } } }), true)
  assert.equal(resultCanResubmit({ can_resubmit: false }), false)
  assert.equal(resultQueryDeadline({ query_expires_at: '2026-10-01T00:00:00Z' }), '2026-10-01T00:00:00Z')
})
