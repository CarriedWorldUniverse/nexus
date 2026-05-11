package observability

import (
	"encoding/json"
	"testing"
)

func TestParseArtifact(t *testing.T) {
	tests := []struct {
		name    string
		tool    string
		args    string
		want    *Artifact
		wantErr bool
	}{
		{
			name: "Edit",
			tool: "Edit",
			args: `{"file_path":"/a/b.go","old_string":"foo","new_string":"bar"}`,
			want: &Artifact{Kind: ArtifactFileEdit, FilePath: "/a/b.go", OldText: "foo", NewText: "bar"},
		},
		{
			name: "Write",
			tool: "Write",
			args: `{"file_path":"/a/b.txt","content":"hello"}`,
			want: &Artifact{Kind: ArtifactFileWrite, FilePath: "/a/b.txt", NewText: "hello"},
		},
		{
			name: "MultiEdit",
			tool: "MultiEdit",
			args: `{"file_path":"/a/b.go","edits":[{"old_string":"x","new_string":"y"},{"old_string":"p","new_string":"q"}]}`,
			want: &Artifact{
				Kind:     ArtifactMultiEdit,
				FilePath: "/a/b.go",
				Edits:    []EditPair{{OldText: "x", NewText: "y"}, {OldText: "p", NewText: "q"}},
			},
		},
		{
			name: "NotebookEdit",
			tool: "NotebookEdit",
			args: `{"notebook_path":"/n.ipynb","new_source":"print(1)","cell_id":"abc"}`,
			want: &Artifact{Kind: ArtifactNotebookEdit, FilePath: "/n.ipynb", NewText: "print(1)"},
		},
		{
			name:    "Edit bad JSON",
			tool:    "Edit",
			args:    `{not json`,
			wantErr: true,
		},
		{
			name:    "Write bad JSON",
			tool:    "Write",
			args:    `[]`, // type mismatch
			wantErr: true,
		},
		{
			name: "Unknown tool returns nil,nil",
			tool: "Bash",
			args: `{"command":"ls"}`,
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseArtifact(tc.tool, json.RawMessage(tc.args))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil; got=%+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("want nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("got nil, want %+v", tc.want)
			}
			if got.Kind != tc.want.Kind || got.FilePath != tc.want.FilePath ||
				got.OldText != tc.want.OldText || got.NewText != tc.want.NewText {
				t.Errorf("scalar mismatch: got=%+v want=%+v", got, tc.want)
			}
			if len(got.Edits) != len(tc.want.Edits) {
				t.Fatalf("edits len: got=%d want=%d", len(got.Edits), len(tc.want.Edits))
			}
			for i := range got.Edits {
				if got.Edits[i] != tc.want.Edits[i] {
					t.Errorf("edits[%d]: got=%+v want=%+v", i, got.Edits[i], tc.want.Edits[i])
				}
			}
		})
	}
}
