// The form state behind the settings page, kept out of the component so the one
// piece with a rule worth stating can be tested on its own: what gets sent.
//
// Only changes are sent. Sending the whole form would write every setting into
// the file, turning "unset, so the default applies" into an explicit empty line
// for two dozen settings nobody touched — and, for a secret, would clear a key
// the server was never able to show us in the first place.

// formFrom builds the editable state from what the server served. A secret
// starts empty however set it is: the value is not ours to have.
export function formFrom(fields) {
  const form = {};
  for (const f of fields) {
    if (f.kind === 'secret') form[f.key] = '';
    else if (f.kind === 'bool') form[f.key] = f.value === '1';
    else form[f.key] = f.value ?? '';
  }
  return form;
}

// changedValues is the payload for a save. `touched` names the secrets the
// operator typed into or explicitly cleared — the only way an empty secret box
// can mean "clear this" rather than "I did not touch it".
export function changedValues(fields, form, touched) {
  const values = {};
  for (const f of fields) {
    if (f.kind === 'secret') {
      if (touched.has(f.key)) values[f.key] = form[f.key] ?? '';
      continue;
    }
    const next = f.kind === 'bool' ? (form[f.key] ? '1' : '0') : String(form[f.key] ?? '').trim();
    const now = f.kind === 'bool' ? (f.value === '1' ? '1' : '0') : (f.value ?? '');
    if (next !== now) values[f.key] = next;
  }
  return values;
}

// groupFields turns the flat list into the headings the form is laid out under,
// in the order the server listed them — the server decides what belongs
// together, so the page cannot disagree with the file it writes.
export function groupFields(fields) {
  const groups = [];
  for (const f of fields) {
    let group = groups.find(g => g.name === f.group);
    if (!group) {
      group = { name: f.group, fields: [] };
      groups.push(group);
    }
    group.fields.push(f);
  }
  return groups;
}
