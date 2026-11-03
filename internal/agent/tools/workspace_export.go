package tools

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/Tencent/WeKnora/internal/sandbox"
)

// WorkspaceExportSink persists exported bytes and returns a URL for them. The
// service layer owns storage and signing; tools only resolve and read the
// authorized sandbox path.
type WorkspaceExportSink interface {
	PersistExportedFile(ctx context.Context, fileName string, data []byte) (*ExportedFile, error)
}

// workspaceFileExporter resolves sandbox paths with the same scope rules as
// read_file, then persists a copy the external tool can download.
type workspaceFileExporter struct {
	source SandboxFileSource
	sink   WorkspaceExportSink
}

// NewWorkspaceFileExporter builds an exporter for files inside the session
// sandbox. The source must be the same session-scoped store read_file uses.
func NewWorkspaceFileExporter(source SandboxFileSource, sink WorkspaceExportSink) FileExporter {
	if source == nil || sink == nil {
		return nil
	}
	return &workspaceFileExporter{source: source, sink: sink}
}

func (w *workspaceFileExporter) ExportURL(ctx context.Context, ref string) (*ExportedFile, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return nil, fmt.Errorf("path is required")
	}
	sessionID := resolveSessionID(ctx)
	if sessionID == "" {
		return nil, fmt.Errorf("no session ID in context; sandbox files can only be exported during an agent turn")
	}
	layout, layoutErr := executeWorkspaceLayout(ctx, sessionID, w.source)
	if layoutErr != nil {
		return nil, fmt.Errorf("%s", layoutErr.Error)
	}
	clean := resolveIn(layout, trimmed)
	if _, ok := inspectRoot(layout, clean); !ok {
		return nil, fmt.Errorf("this tool only exports files under %s. path %q is outside that scope",
			layoutScopeName(layout), trimmed)
	}

	ctx = sandbox.WithSessionFileOperation(ctx)
	stat, err := w.source.StatSessionFile(ctx, sessionID, clean)
	if err != nil {
		return nil, fmt.Errorf("failed to inspect %s before exporting: %w", clean, err)
	}
	if stat == nil {
		return nil, fmt.Errorf("file not found: %s", clean)
	}
	if stat.Type == sandbox.RemoteEntryDir {
		return nil, fmt.Errorf("path is a directory, not a file: %s", clean)
	}
	if stat.Type != sandbox.RemoteEntryFile {
		return nil, fmt.Errorf("path is not a regular file: %s", clean)
	}
	if stat.Size > int64(maxSandboxFileBytes) {
		return nil, fmt.Errorf("%s is %d bytes; export_file is limited to %d bytes",
			clean, stat.Size, maxSandboxFileBytes)
	}

	data, err := w.source.ReadSessionFile(ctx, sessionID, clean)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", clean, err)
	}
	if len(data) > maxSandboxFileBytes {
		return nil, fmt.Errorf("%s grew past the %d byte export limit while being read",
			clean, maxSandboxFileBytes)
	}
	return w.sink.PersistExportedFile(ctx, path.Base(clean), data)
}
