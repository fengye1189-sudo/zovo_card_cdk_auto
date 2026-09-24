export function effectiveCurrentPlan(currentPlan, subscriptionHasActive) {
  if (subscriptionHasActive === false) return 'free'
  return String(currentPlan || '').trim() || 'free'
}

export function resultQueryDeadline(payload) {
  const value = payload?.query_expires_at
    || payload?.data?.query_expires_at
    || payload?.order?.query_expires_at
  return String(value || '').trim()
}

export function resultCanResubmit(payload) {
  const values = [
    payload?.order?.can_resubmit,
    payload?.data?.order?.can_resubmit,
    payload?.data?.can_resubmit,
    payload?.can_resubmit,
  ]
  return values.find(value => typeof value === 'boolean') === true
}
