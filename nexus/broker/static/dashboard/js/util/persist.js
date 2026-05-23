// persist.js — namespaced localStorage wrappers with safe JSON +
// quota / disabled-storage guards. Used by Feed (and intended for
// any view that wants the same shape) for filter / focus / cursor
// state that should survive a page reload.
//
// Keys are namespaced as `nexus.<scope>.<name>.v1`. The `v1` suffix
// lets us bump the schema later by writing to `.v2` without
// silently consuming stale data of a different shape — old keys
// just orphan in localStorage until a future cleanup pass.

const NS = 'nexus';
const VER = 'v1';

function fullKey(scope, name) {
  return `${NS}.${scope}.${name}.${VER}`;
}

// persistGet returns the parsed value or `fallback` if anything goes
// wrong: storage missing (SSR / safari private mode), key absent,
// or JSON corruption (manual edits, version skew on the .v1 → .v2
// transition before the cleanup pass).
export function persistGet(scope, name, fallback) {
  try {
    if (typeof localStorage === 'undefined') return fallback;
    const raw = localStorage.getItem(fullKey(scope, name));
    if (raw == null) return fallback;
    return JSON.parse(raw);
  } catch {
    return fallback;
  }
}

// persistSet writes the value (JSON-serialised). On quota exceeded
// or storage-disabled errors, logs a warn but doesn't throw — the
// caller's flow continues with in-memory state only.
export function persistSet(scope, name, value) {
  try {
    if (typeof localStorage === 'undefined') return;
    localStorage.setItem(fullKey(scope, name), JSON.stringify(value));
  } catch (e) {
    // eslint-disable-next-line no-console
    console.warn(`[persist] set failed for ${scope}.${name}:`, e);
  }
}

// persistRemove clears a single key. Symmetric to persistSet; same
// no-throw failure mode.
export function persistRemove(scope, name) {
  try {
    if (typeof localStorage === 'undefined') return;
    localStorage.removeItem(fullKey(scope, name));
  } catch (e) {
    // eslint-disable-next-line no-console
    console.warn(`[persist] remove failed for ${scope}.${name}:`, e);
  }
}
