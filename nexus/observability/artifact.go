package observability

import (
	"encoding/json"
	"fmt"
)

// ParseArtifact inspects a tool name + raw input JSON and, if the
// tool is one of the file-mutating built-ins, returns a structured
// Artifact. Unknown tool names return (nil, nil) — not an error,
// just "no artifact for this tool". Malformed JSON for a known
// tool returns a wrapped error so callers can choose to leave
// Artifact nil and fall back to raw-input rendering.
func ParseArtifact(name string, args json.RawMessage) (*Artifact, error) {
	switch name {
	case "Edit":
		var in struct {
			FilePath  string `json:"file_path"`
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("parse Edit args: %w", err)
		}
		return &Artifact{
			Kind:     ArtifactFileEdit,
			FilePath: in.FilePath,
			OldText:  in.OldString,
			NewText:  in.NewString,
		}, nil

	case "Write":
		var in struct {
			FilePath string `json:"file_path"`
			Content  string `json:"content"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("parse Write args: %w", err)
		}
		return &Artifact{
			Kind:     ArtifactFileWrite,
			FilePath: in.FilePath,
			NewText:  in.Content,
		}, nil

	case "MultiEdit":
		var in struct {
			FilePath string `json:"file_path"`
			Edits    []struct {
				OldString string `json:"old_string"`
				NewString string `json:"new_string"`
			} `json:"edits"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("parse MultiEdit args: %w", err)
		}
		pairs := make([]EditPair, len(in.Edits))
		for i, e := range in.Edits {
			pairs[i] = EditPair{OldText: e.OldString, NewText: e.NewString}
		}
		return &Artifact{
			Kind:     ArtifactMultiEdit,
			FilePath: in.FilePath,
			Edits:    pairs,
		}, nil

	case "NotebookEdit":
		// Notebook tools use notebook_path for the file slot and
		// new_source for the cell body; mirror those into the
		// generic FilePath/NewText so renderers can use one shape.
		var in struct {
			NotebookPath string `json:"notebook_path"`
			NewSource    string `json:"new_source"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, fmt.Errorf("parse NotebookEdit args: %w", err)
		}
		return &Artifact{
			Kind:     ArtifactNotebookEdit,
			FilePath: in.NotebookPath,
			NewText:  in.NewSource,
		}, nil
	}
	return nil, nil
}
