package service

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/application/attachment"
	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
)

// exportedFileURL rejects non-HTTP storage results. A local:// path is useless
// to an external tool and would otherwise look like a successful export.
func exportedFileURL(raw, fileName string, size int64) (*tools.ExportedFile, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("file storage cannot produce a downloadable HTTP URL in this deployment; " +
			"set APP_EXTERNAL_URL for local storage or configure a public storage backend")
	}
	return &tools.ExportedFile{URL: raw, FileName: fileName, Size: size}, nil
}

// agentAttachmentExports turns an attachment:// handle into the stored
// original upload's download URL. Resolution goes through the same inventory
// read_file uses, so only this conversation's attachments are reachable.
type agentAttachmentExports struct {
	reader   *attachment.Reader
	files    interfaces.FileService
	tenantID uint64
}

func (a *agentAttachmentExports) ExportURL(ctx context.Context, ref string) (*tools.ExportedFile, error) {
	if a == nil || a.reader == nil || a.files == nil {
		return nil, fmt.Errorf("attachment export is unavailable")
	}
	handle := strings.Trim(strings.TrimPrefix(strings.TrimSpace(ref), types.AttachmentPathScheme), "/")
	handle, sub, _ := strings.Cut(handle, "/")
	if handle == "" {
		return nil, fmt.Errorf("attachment handle is required; use attachment://<handle>")
	}
	if sub != "" {
		return nil, fmt.Errorf("export_file expects the original attachment address " +
			"attachment://<handle>, not one of its images")
	}
	document, err := a.reader.Resolve(ctx, handle, false)
	if err != nil {
		return nil, err
	}
	if document == nil || document.Document == nil || strings.TrimSpace(document.Document.ResourceRef) == "" {
		return nil, fmt.Errorf("attachment has no stored file to export")
	}
	// Attachments belong to the session owner, which can differ from a shared
	// agent's workspace tenant.
	ownerCtx := context.WithValue(ctx, types.TenantIDContextKey, a.tenantID)
	raw, err := a.files.GetFileURL(ownerCtx, document.Document.ResourceRef)
	if err != nil {
		return nil, fmt.Errorf("failed to create a download URL for %s: %w", document.Document.FileName, err)
	}
	return exportedFileURL(raw, document.Document.FileName, document.Document.FileSize)
}

// agentWorkspaceExportSink persists a copy of an exported sandbox file under the
// session owner's tenant, then signs a URL for it.
type agentWorkspaceExportSink struct {
	files     interfaces.FileService
	tenantID  uint64
	sessionID string
	messageID string
	catalog   interfaces.ResourceCatalog
}

func (s *agentWorkspaceExportSink) PersistExportedFile(
	ctx context.Context, fileName string, data []byte,
) (*tools.ExportedFile, error) {
	if s == nil || s.files == nil {
		return nil, fmt.Errorf("file storage is unavailable")
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("refusing to export an empty file")
	}
	ctx = context.WithValue(ctx, types.TenantIDContextKey, s.tenantID)
	storageName := "export_" + uuid.NewString() + "_" + safeFileName(fileName)
	ref, err := s.files.SaveBytes(ctx, data, s.tenantID, storageName, false)
	if err != nil {
		return nil, fmt.Errorf("failed to store exported file: %w", err)
	}
	// Binding the copy to the assistant message gives resource cleanup the same
	// owner the collected artifacts use. Best-effort: the URL still works if
	// the registry is unavailable.
	if s.catalog != nil && s.messageID != "" {
		if _, ok := types.ParseResourcePath(ref); ok {
			if bindErr := s.catalog.Bind(
				ctx, ref, types.ResourceOwnerMessage, s.messageID, types.ResourceRelationArtifact,
			); bindErr != nil {
				logger.Warnf(ctx, "[FileExport] bind exported resource failed: session=%s ref=%s err=%v",
					s.sessionID, ref, bindErr)
			}
		}
	}
	raw, err := s.files.GetFileURL(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("failed to create a download URL for exported file: %w", err)
	}
	return exportedFileURL(raw, fileName, int64(len(data)))
}

// registerFileExportTool exposes export_file when the run can export at least
// one kind of file. Artifacts are attached later by AgentCollaboration, once a
// run binding exists.
func (s *agentService) registerFileExportTool(
	ctx context.Context,
	registry *tools.ToolRegistry,
	config *types.AgentConfig,
	sessionID, messageID string,
	sandboxStore sandbox.SessionFileStore,
	attachments *attachment.Reader,
) {
	if config != nil && config.SkillInstallMode() {
		return
	}
	if sandboxStore == nil && attachments == nil {
		return
	}
	tenantID, ok := types.SandboxTenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return
	}
	exporter := tools.NewExportFileTool()
	if attachments != nil && s.fileService != nil {
		exporter.WithAttachments(&agentAttachmentExports{
			reader: attachments, files: s.fileService, tenantID: tenantID,
		})
	}
	if sandboxStore != nil && s.fileService != nil {
		var catalog interfaces.ResourceCatalog
		if s.db != nil {
			catalog = NewResourceCatalog(repository.NewResourceRepository(s.db))
		}
		exporter.WithWorkspace(tools.NewWorkspaceFileExporter(sandboxStore, &agentWorkspaceExportSink{
			files: s.fileService, tenantID: tenantID, sessionID: sessionID,
			messageID: messageID, catalog: catalog,
		}))
	}
	if exporter.HasSources() {
		registry.RegisterTool(exporter)
	}
}
