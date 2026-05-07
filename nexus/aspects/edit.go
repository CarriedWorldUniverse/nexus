// Personality editing helpers.
//
// Per agent-network/docs/2026-05-08-nexus-resident-personality-spec.md §8.
// The CLI (`nexus personality edit`) and the admin REST handler share
// the same write path so the resulting state is identical regardless
// of entry point. Both end up calling EditPersonality below.
//
// On a successful edit, EditPersonality returns a *PersonalityChange
// that downstream code (Part 7c) uses to fan out a personality.refresh
// frame on the live WS connection. Today only the in-process refresh
// path (Embedded Frame) is wired; the WS push protocol is a TODO at
// the broker side.

package aspects

import (
	"context"
	"errors"
	"fmt"
)

// PersonalityChange is the post-edit summary the editor returns. Used
// by callers (CLI, REST handler) to log/render the result and by the
// refresh-broadcast layer to know who/what changed.
type PersonalityChange struct {
	AspectName string
	OldVersion int64
	NewVersion int64
}

// EditPersonality applies a 3-section edit to an aspect's personality
// row. Atomically: load → upsert via PersonalitySet → re-load to
// capture the bumped version. Returns ErrNotFound when the aspect
// itself doesn't exist (personality requires a parent aspect row per
// the FK in schema.sql).
//
// Empty sections are allowed — operator may not have anything to put
// in PRIMER.md yet, etc. The renderer (future) decides what to do
// with empties; we don't validate at the storage layer.
//
// Concurrency: PersonalitySet uses an UPSERT and increments version
// atomically. Two concurrent editors will both succeed; the version
// reflects the order SQLite serialized them.
func EditPersonality(ctx context.Context, store Store, name, nexusMD, soulMD, primerMD string) (*PersonalityChange, error) {
	if store == nil {
		return nil, errors.New("aspects.EditPersonality: store nil")
	}
	if name == "" {
		return nil, errors.New("aspects.EditPersonality: aspect name required")
	}

	// FK guard: PersonalitySet would fail with a constraint error if
	// the aspect doesn't exist; surface it as ErrNotFound so callers
	// render a clearer message.
	if _, err := store.Get(ctx, name); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("aspects.EditPersonality: aspect %q does not exist (mint it first): %w", name, err)
		}
		return nil, fmt.Errorf("aspects.EditPersonality: lookup %q: %w", name, err)
	}

	// Capture current version so we can report the bump. Missing row
	// is fine — first edit; OldVersion=0.
	var oldVersion int64
	if existing, err := store.PersonalityGet(ctx, name); err == nil {
		oldVersion = existing.Version
	} else if !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("aspects.EditPersonality: read prior personality: %w", err)
	}

	if err := store.PersonalitySet(ctx, Personality{
		AspectName: name,
		NexusMD:    nexusMD,
		SoulMD:     soulMD,
		PrimerMD:   primerMD,
	}); err != nil {
		return nil, fmt.Errorf("aspects.EditPersonality: write: %w", err)
	}

	updated, err := store.PersonalityGet(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("aspects.EditPersonality: re-read post-write: %w", err)
	}

	return &PersonalityChange{
		AspectName: name,
		OldVersion: oldVersion,
		NewVersion: updated.Version,
	}, nil
}

// EditBlob is the shared serialization format for the CLI's $EDITOR
// flow. Three labeled sections separated by canonical headers; on
// parse, leading/trailing whitespace inside each section is trimmed.
const (
	editHeaderNexus  = "# === NEXUS.md ==="
	editHeaderSoul   = "# === SOUL.md ==="
	editHeaderPrimer = "# === PRIMER.md ==="
)

// MarshalEditBlob renders the three sections in the canonical CLI
// editing format. Used to seed the editor with current content.
func MarshalEditBlob(p *Personality) string {
	if p == nil {
		p = &Personality{}
	}
	return editHeaderNexus + "\n" + p.NexusMD + "\n\n" +
		editHeaderSoul + "\n" + p.SoulMD + "\n\n" +
		editHeaderPrimer + "\n" + p.PrimerMD + "\n"
}

// UnmarshalEditBlob parses a CLI-edited blob back into the three
// sections. Section order is fixed (NEXUS → SOUL → PRIMER); a missing
// header is an error rather than a silent default. Empty section
// content is allowed.
func UnmarshalEditBlob(blob string) (nexusMD, soulMD, primerMD string, err error) {
	// Find the three headers in order. The blob may have leading
	// content (operator-added comments outside any section); we drop
	// it. Content within a section is everything between its header
	// and the next header (or EOF for the final section).
	idxNexus, idxSoul, idxPrimer, ferr := findEditHeaders(blob)
	if ferr != nil {
		return "", "", "", ferr
	}
	nexusMD = trimSection(blob[idxNexus+len(editHeaderNexus) : idxSoul])
	soulMD = trimSection(blob[idxSoul+len(editHeaderSoul) : idxPrimer])
	primerMD = trimSection(blob[idxPrimer+len(editHeaderPrimer):])
	return
}

// findEditHeaders returns the byte offsets of the three section
// headers in order, or an error describing the first missing one.
func findEditHeaders(blob string) (int, int, int, error) {
	idxNexus := indexOf(blob, editHeaderNexus, 0)
	if idxNexus < 0 {
		return 0, 0, 0, fmt.Errorf("aspects.UnmarshalEditBlob: missing %q header", editHeaderNexus)
	}
	idxSoul := indexOf(blob, editHeaderSoul, idxNexus+len(editHeaderNexus))
	if idxSoul < 0 {
		return 0, 0, 0, fmt.Errorf("aspects.UnmarshalEditBlob: missing %q header (must come after NEXUS)", editHeaderSoul)
	}
	idxPrimer := indexOf(blob, editHeaderPrimer, idxSoul+len(editHeaderSoul))
	if idxPrimer < 0 {
		return 0, 0, 0, fmt.Errorf("aspects.UnmarshalEditBlob: missing %q header (must come after SOUL)", editHeaderPrimer)
	}
	return idxNexus, idxSoul, idxPrimer, nil
}

// indexOf is strings.Index but with a starting offset. Returns the
// absolute index in s, or -1 if not found.
func indexOf(s, sub string, start int) int {
	if start >= len(s) {
		return -1
	}
	rel := -1
	for i := start; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			rel = i
			break
		}
	}
	return rel
}

// trimSection strips a single leading newline (the one immediately
// after the header) and trailing whitespace. Leaves the operator's
// indentation/structure inside the body untouched.
func trimSection(s string) string {
	if len(s) > 0 && s[0] == '\n' {
		s = s[1:]
	}
	// Trim trailing whitespace including newlines so the next section
	// header doesn't get a leading blank line attached.
	end := len(s)
	for end > 0 {
		c := s[end-1]
		if c != '\n' && c != '\r' && c != ' ' && c != '\t' {
			break
		}
		end--
	}
	return s[:end]
}
