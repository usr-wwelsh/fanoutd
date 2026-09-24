export function nearBottom(el, slack = 48) {
  if (!el) return false;
  return el.scrollHeight - el.scrollTop - el.clientHeight <= slack;
}
