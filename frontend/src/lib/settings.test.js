import { describe, expect, test } from 'bun:test';
import { changedValues, formFrom, groupFields } from './settings.js';

const field = (key, extra = {}) => ({
  key,
  label: key,
  kind: 'text',
  group: 'Provider',
  value: '',
  source: '',
  set: false,
  ...extra,
});

describe('formFrom', () => {
  test('starts every field at what the server says it is', () => {
    const form = formFrom([field('FANOUT_MODEL', { value: 'vendor/one' })]);
    expect(form.FANOUT_MODEL).toBe('vendor/one');
  });

  test('starts a secret empty whatever the server says about it', () => {
    const form = formFrom([field('FANOUT_API_KEY', { kind: 'secret', set: true })]);
    expect(form.FANOUT_API_KEY).toBe('');
  });

  test('a bool arrives as a string and becomes a checkbox state', () => {
    const form = formFrom([
      field('FANOUT_SHELL', { kind: 'bool', value: '1' }),
      field('FANOUT_REVIEW', { kind: 'bool', value: '' }),
    ]);
    expect(form.FANOUT_SHELL).toBe(true);
    expect(form.FANOUT_REVIEW).toBe(false);
  });
});

describe('changedValues', () => {
  const fields = [
    field('FANOUT_MODEL', { value: 'vendor/one' }),
    field('FANOUT_SHELL', { kind: 'bool', value: '' }),
    field('FANOUT_API_KEY', { kind: 'secret', set: true }),
  ];

  test('sends nothing when nothing was touched', () => {
    expect(changedValues(fields, formFrom(fields), new Set())).toEqual({});
  });

  test('sends only the fields that actually changed', () => {
    const form = { ...formFrom(fields), FANOUT_MODEL: 'vendor/two' };
    expect(changedValues(fields, form, new Set())).toEqual({ FANOUT_MODEL: 'vendor/two' });
  });

  // Sending every field would turn "unset, using the default" into an explicit
  // empty line in the settings file for every setting on the page.
  test('leaves untouched empty fields out rather than writing them empty', () => {
    const form = formFrom(fields);
    expect('FANOUT_SHELL' in changedValues(fields, form, new Set())).toBe(false);
  });

  test('a checkbox goes as 1 or 0', () => {
    const form = { ...formFrom(fields), FANOUT_SHELL: true };
    expect(changedValues(fields, form, new Set()).FANOUT_SHELL).toBe('1');
  });

  // The server can never send a secret back, so an untouched secret box is
  // empty for a key that is set. Sending it would clear the key.
  test('an untouched secret is left out even though its box is empty', () => {
    const form = formFrom(fields);
    expect('FANOUT_API_KEY' in changedValues(fields, form, new Set())).toBe(false);
  });

  test('a typed secret is sent', () => {
    const form = { ...formFrom(fields), FANOUT_API_KEY: 'sk-new' };
    expect(changedValues(fields, form, new Set(['FANOUT_API_KEY'])).FANOUT_API_KEY).toBe('sk-new');
  });

  // Clearing is the one case where an empty secret has to travel, so it takes
  // an explicit act rather than an empty box.
  test('a cleared secret is sent empty', () => {
    const form = formFrom(fields);
    expect(changedValues(fields, form, new Set(['FANOUT_API_KEY'])).FANOUT_API_KEY).toBe('');
  });
});

describe('groupFields', () => {
  test('keeps the server order and groups by heading', () => {
    const grouped = groupFields([
      field('A', { group: 'Provider' }),
      field('B', { group: 'Shell' }),
      field('C', { group: 'Provider' }),
    ]);
    expect(grouped.map(g => g.name)).toEqual(['Provider', 'Shell']);
    expect(grouped[0].fields.map(f => f.key)).toEqual(['A', 'C']);
  });
});
