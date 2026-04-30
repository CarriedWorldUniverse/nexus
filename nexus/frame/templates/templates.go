// Package templates renders the personality bundle (aspect.json + SOUL.md
// + CLAUDE.md + PRIMER.md) for a new Frame. Substitution is the simplest
// possible: {{name}}-style placeholders, regex-replaced from a vars map.
// No conditional logic, no loops. If a template grows past that, revisit.
//
// Templates ship as embed.FS-backed files under embed/<name>/. v1 ships
// the "default" template — neutral professional voice, sensible defaults.
// Operators can override voice/values via the bootstrap wizard (P4) or
// edit the rendered files manually after first-boot.
//
// Spec drift note: frame-role spec §5.4 names a filesystem path
// `<nexus_root>/templates/frame/` with `.template` suffix. We embed
// instead — files ship in the binary at embed/<name>/<filename> with
// the filename matching the rendered output. Spec to be reconciled in
// a follow-up patch; behavior matches what P4/P5 will consume.
//
// Frame aspect.json conventions:
//   - port: 0 means "no peer port." The Frame embeds in the Nexus
//     process — there's no harness subprocess to listen, and admin
//     surfaces are served via in-process REST handlers (P7). The
//     port field exists in the rendered output only for AspectConfig
//     schema compatibility; nothing reads it for Frame aspects.
//   - context_mode: global is mandatory. The Frame sees the whole
//     network's chat surface (per the routing rules) and needs a
//     single coherent context across turns.
//
// Substitution semantics:
//   - Placeholder names are case-sensitive: {{name}} and {{NAME}} are
//     distinct keys. Templates and the wizard schema must agree on case.
//   - Substitution is single-pass: a value containing "{{x}}" is NOT
//     re-scanned for further substitution. This means the wizard's
//     `voice`/`values` strings can include literal {{ braces without
//     causing recursion or `ErrMissingVar`.
//   - Repeated placeholders count as one used-var (the strict-vars
//     check sees them once, not N times).
package templates

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
)

//go:embed embed
var embedFS embed.FS

// Bundle is the rendered output of a template. Keys are filenames
// relative to the new Frame's home folder (e.g. "aspect.json", "SOUL.md");
// values are the rendered file contents.
type Bundle map[string][]byte

// ErrUnknownTemplate is returned when Render is called with a template
// name that has no matching directory under embed/.
var ErrUnknownTemplate = errors.New("templates: unknown template")

// ErrMissingVar is returned when a template references {{x}} and x is
// not in the supplied vars map. Failing loud is deliberate per the §6.5
// build plan §5 lean: a typo in a template should surface at render time
// rather than ship a half-substituted Frame home.
var ErrMissingVar = errors.New("templates: missing variable")

// ErrUnknownVar is returned when vars contains a key the template never
// references. Strict by design — keeps wizard payload schema in lockstep
// with templates so a stale field name doesn't silently get dropped.
var ErrUnknownVar = errors.New("templates: unknown variable")

// placeholderRE matches {{name}} where name is alphanumeric + underscore.
// Whitespace inside the braces is allowed: {{ name }} works too.
var placeholderRE = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// List returns the available template names (sorted). Useful for the
// wizard to populate a "choose template" dropdown when more land.
func List() ([]string, error) {
	entries, err := fs.ReadDir(embedFS, "embed")
	if err != nil {
		return nil, fmt.Errorf("templates: list embed dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Render produces a Bundle from the named template, substituting
// {{placeholder}} occurrences with values from vars. All placeholders
// the template references must be in vars (ErrMissingVar otherwise).
// All vars keys must be referenced somewhere in the template (ErrUnknownVar
// otherwise — keeps the wizard payload schema honest).
//
// File contents are returned verbatim except for placeholder
// substitution. No file-name templating in v1.
func Render(name string, vars map[string]string) (Bundle, error) {
	dir := path.Join("embed", name)
	if _, err := fs.Stat(embedFS, dir); err != nil {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTemplate, name)
	}

	files, err := fs.ReadDir(embedFS, dir)
	if err != nil {
		return nil, fmt.Errorf("templates: read %s: %w", dir, err)
	}

	bundle := make(Bundle, len(files))
	usedVars := make(map[string]struct{}, len(vars))
	var missing []string

	for _, fent := range files {
		if fent.IsDir() {
			continue
		}
		filePath := path.Join(dir, fent.Name())
		raw, rerr := fs.ReadFile(embedFS, filePath)
		if rerr != nil {
			return nil, fmt.Errorf("templates: read %s: %w", filePath, rerr)
		}
		// Output filename = input filename (no extension stripping; the
		// embed files already use their final names like "aspect.json"
		// and "SOUL.md").
		outName := fent.Name()

		rendered, perFileMissing := substitute(raw, vars, usedVars)
		missing = append(missing, perFileMissing...)
		bundle[outName] = rendered
	}

	// Error ordering: ErrMissingVar wins over ErrUnknownVar. A template
	// that references a missing var implies the wizard's payload is
	// incomplete — that's the actionable error. Reporting "unused vars"
	// in the same case would be noise (the supplied vars are fine; the
	// template just needs more). Fail-fast on missing keeps usedVars'
	// partial state from misleading the unused-var check below.
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrMissingVar, strings.Join(dedupSort(missing), ", "))
	}

	// Strict-vars check: every supplied var must have been referenced
	// somewhere across the template files. Catches stale wizard payloads.
	var unused []string
	for k := range vars {
		if _, ok := usedVars[k]; !ok {
			unused = append(unused, k)
		}
	}
	if len(unused) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrUnknownVar, strings.Join(dedupSort(unused), ", "))
	}

	return bundle, nil
}

// substitute walks the template text replacing {{x}} with vars[x].
// Placeholders not in vars accumulate into missing[]. As a side effect,
// every successful resolution marks the var as used in usedVars so the
// caller can detect supplied-but-unreferenced vars.
func substitute(in []byte, vars map[string]string, usedVars map[string]struct{}) (out []byte, missing []string) {
	out = placeholderRE.ReplaceAllFunc(in, func(match []byte) []byte {
		sub := placeholderRE.FindSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		key := string(sub[1])
		val, ok := vars[key]
		if !ok {
			missing = append(missing, key)
			return match
		}
		usedVars[key] = struct{}{}
		return []byte(val)
	})
	return out, missing
}

// dedupSort returns ss with duplicates removed, sorted.
func dedupSort(ss []string) []string {
	seen := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		seen[s] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
