export function normalizeLocalCode(value) {
  return String(value || '').normalize('NFKC').replace(/[\s\u200B-\u200D\u2060\uFEFF]/g, '').replace(/[‐‑‒–—−]/g, '-').toUpperCase()
}
export function isLocalCode(value) {
  const code = normalizeLocalCode(value)
  return /^MAPLE-/.test(code) || /^(?:(?:PULS|GO|PRO|PRO5X)-)?[A-Z]{15}$/.test(code)
}
export function safeStorage(name) {
  return {
    getItem(key) { try { return window[name].getItem(key) } catch { return null } },
    setItem(key, value) { try { window[name].setItem(key, value) } catch {} },
    removeItem(key) { try { window[name].removeItem(key) } catch {} },
  }
}
export function secureBrowserID() {
  const bytes = new Uint8Array(24)
  crypto.getRandomValues(bytes)
  return Array.from(bytes, v => v.toString(16).padStart(2, '0')).join('')
}
