// What a tab left in the background owes whoever started a run and walked away:
// a title that counts what is still going, and a word when a run comes to rest.

export function tabTitle({ running, held }) {
  const parts = [];
  if (running) parts.push(`${running} running`);
  if (held) parts.push(`${held} review`);
  return parts.length ? `(${parts.join(' · ')}) fanoutd` : 'fanoutd';
}

// A stop is something a person did, so it is not news to them.
export function settled(prev, next) {
  const was = new Map(prev.map(t => [t.id, t.status]));
  return next.filter(t => was.get(t.id) === 'running' && (t.status === 'done' || t.status === 'error'));
}
