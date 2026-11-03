package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

// ExportedFile is a file made reachable to a tool outside WeKnora, such as a
// custom MCP server. The URL is a capability: anyone who holds it can download
// the file until it expires.
type ExportedFile struct {
	URL      string
	FileName string
	Size     int64
}

// FileExporter turns an already-authorized conversation reference into a
// download URL. Implementations must authorize the reference against the
// current session; they must never accept arbitrary storage or host paths.
type FileExporter interface {
	ExportURL(ctx context.Context, ref string) (*ExportedFile, error)
}

// ExportFileInput defines the input parameters for export_file.
type ExportFileInput struct {
	Path string `json:"path" jsonschema:"attachment://<handle>, resource:// reference from this conversation, sandbox:<filename>, or a file path inside the current session workspace"` //nolint:lll // one-line struct tag
}

// ExportFileTool publishes a file by URL so an external tool can fetch it
// itself. It only exposes sources the session already authorized; unlike
// read_file it returns no bytes, so a large file does not enter the model
// context.
type ExportFileTool struct {
	BaseTool
	attachments FileExporter
	artifacts   FileExporter
	workspace   FileExporter
}

// NewExportFileTool creates an export_file tool with no sources. Sources are
// added as the run's capabilities are resolved.
func NewExportFileTool() *ExportFileTool {
	t := &ExportFileTool{BaseTool: BaseTool{name: ToolExportFile, schema: utils.GenerateSchema[ExportFileInput]()}}
	t.updateDescription()
	return t
}

// WithAttachments adds attachment://<handle> export for this conversation.
func (t *ExportFileTool) WithAttachments(source FileExporter) *ExportFileTool {
	if t == nil {
		return nil
	}
	t.attachments = source
	t.updateDescription()
	return t
}

// WithArtifacts adds resource:// and sandbox:<filename> export for files whose
// ownership this conversation can prove.
func (t *ExportFileTool) WithArtifacts(source FileExporter) *ExportFileTool {
	if t == nil {
		return nil
	}
	t.artifacts = source
	t.updateDescription()
	return t
}

// WithWorkspace adds on-demand export of files inside the session sandbox.
func (t *ExportFileTool) WithWorkspace(source FileExporter) *ExportFileTool {
	if t == nil {
		return nil
	}
	t.workspace = source
	t.updateDescription()
	return t
}

// HasSources reports whether any file kind can actually be exported, so a run
// without files does not advertise a tool that can only fail.
func (t *ExportFileTool) HasSources() bool {
	return t != nil && (t.attachments != nil || t.artifacts != nil || t.workspace != nil)
}

func (t *ExportFileTool) updateDescription() {
	if t == nil {
		return
	}
	var scopes []string
	if t.attachments != nil {
		scopes = append(scopes, "Attachments: attachment://<handle> exports the original uploaded file.")
	}
	if t.artifacts != nil {
		scopes = append(scopes, "Generated files: use a resource:// reference from this conversation, "+
			"or sandbox:<filename> for a uniquely delivered file.")
	}
	if t.workspace != nil {
		scopes = append(scopes, "Sandbox files: use a path inside the current session workspace "+
			"(relative paths resolve from the workspace root).")
	}
	t.description = "Export one file from this conversation as a download URL. " +
		"Use this when another tool, such as an MCP server, must fetch the file itself: pass the returned url " +
		"as that tool's file argument. " + strings.Join(scopes, " ") +
		" The URL grants temporary access to anyone who holds it; share it only with the intended tool. " +
		"Use read_file to inspect content yourself; export_file does not return file bytes."
}

// sourceFor picks the exporter for a reference. The second return is the
// error to report when that kind of file has no exporter.
func (t *ExportFileTool) sourceFor(ref string) (FileExporter, string) {
	switch {
	case strings.HasPrefix(ref, types.AttachmentPathScheme):
		if t.attachments == nil {
			return nil, "this conversation has no exportable attachments"
		}
		return t.attachments, ""
	case strings.HasPrefix(ref, types.ResourceScheme), strings.HasPrefix(ref, "sandbox:"):
		if t.artifacts == nil {
			return nil, "generated files are unavailable in this conversation"
		}
		return t.artifacts, ""
	case strings.HasPrefix(ref, "skill://"), strings.HasPrefix(ref, "web://"):
		return nil, "export_file supports attachment://, resource://, sandbox:<filename>, " +
			"or a file path inside the current session workspace"
	default:
		if t.workspace == nil {
			return nil, "sandbox file export is unavailable; " +
				"export an attachment:// or resource:// file instead"
		}
		return t.workspace, ""
	}
}

// Execute exports the requested file and returns its URL.
func (t *ExportFileTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var input ExportFileInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{Success: false, Error: fmt.Sprintf("invalid export_file arguments: %v", err)}, nil
	}
	ref := strings.TrimSpace(input.Path)
	if ref == "" {
		return &types.ToolResult{Success: false, Error: "path is required"}, nil
	}
	source, unavailable := t.sourceFor(ref)
	if source == nil {
		return &types.ToolResult{Success: false, Error: unavailable}, nil
	}
	file, err := source.ExportURL(ctx, ref)
	if err != nil {
		return &types.ToolResult{Success: false, Error: err.Error()}, nil
	}
	if file == nil || strings.TrimSpace(file.URL) == "" {
		return &types.ToolResult{Success: false, Error: "the file has no downloadable URL"}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "=== Exported file: %s ===\n\n", file.FileName)
	fmt.Fprintf(&b, "url: %s\n", file.URL)
	if file.Size > 0 {
		fmt.Fprintf(&b, "size: %d bytes\n", file.Size)
	}
	b.WriteString("\nThis URL grants temporary download access to anyone who holds it. " +
		"Pass it only to the intended tool; do not paste it into public places.\n")

	return &types.ToolResult{
		Success: true,
		Output:  b.String(),
		Data: map[string]interface{}{
			"url":       file.URL,
			"file_name": file.FileName,
			"size":      file.Size,
		},
	}, nil
}
