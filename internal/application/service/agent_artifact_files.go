package service

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/collaboration"
	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

const maxArtifactReadBytes = 8 << 20

// agentArtifactFiles reads only persisted outputs, never another session's
// workspace. The model supplies a reference, not an owner or child session ID.
type agentArtifactFiles struct {
	db        *gorm.DB
	catalog   interfaces.ResourceCatalog
	files     func(context.Context, string) (interfaces.FileService, error)
	binding   *collaboration.Binding
	sessionID string
}

func (s *agentArtifactFiles) Read(ctx context.Context, ref string) ([]byte, error) {
	artifact, fs, err := s.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	if artifact.FileSize > maxArtifactReadBytes {
		return nil, fmt.Errorf("generated file exceeds the 8 MiB read limit")
	}
	reader, err := fs.GetFile(ctx, artifact.URL)
	if err != nil {
		return nil, fmt.Errorf("generated file is unavailable in this conversation")
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(reader, maxArtifactReadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("generated file is unavailable in this conversation")
	}
	if len(data) > maxArtifactReadBytes {
		return nil, fmt.Errorf("generated file exceeds the 8 MiB read limit")
	}
	return data, nil
}

// ExportURL authorizes the same references Read accepts, but returns a signed
// download URL instead of loading the bytes into the model context.
func (s *agentArtifactFiles) ExportURL(ctx context.Context, ref string) (*tools.ExportedFile, error) {
	artifact, fs, err := s.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	raw, err := fs.GetFileURL(ctx, artifact.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to create a download URL for generated file: %w", err)
	}
	return exportedFileURL(raw, artifact.FileName, artifact.FileSize)
}

// resolve authorizes ref and returns the artifact row together with the file
// service that owns its storage backend.
func (s *agentArtifactFiles) resolve(
	ctx context.Context, ref string,
) (types.MessageArtifactRecord, interfaces.FileService, error) {
	var empty types.MessageArtifactRecord
	unavailable := fmt.Errorf("generated file is unavailable in this conversation")
	b := s.binding
	if b == nil {
		return empty, nil, unavailable
	}
	if b.Child == nil {
		if err := b.Runtime.CheckParent(ctx, b.Scope, b.ParentOwner); err != nil {
			return empty, nil, unavailable
		}
	} else if err := b.Runtime.CheckOwner(ctx, *b.Child); err != nil {
		return empty, nil, unavailable
	}
	var parentCount int64
	if err := s.db.WithContext(ctx).Table("sessions").
		Where("id = ? AND tenant_id = ? AND user_id = ? AND deleted_at IS NULL",
			b.Scope.SessionID, b.Scope.TenantID, b.Scope.UserID).
		Count(&parentCount).Error; err != nil || parentCount != 1 {
		return empty, nil, unavailable
	}
	var name string
	if strings.HasPrefix(ref, "sandbox:") {
		var err error
		name, err = url.PathUnescape(strings.TrimPrefix(strings.TrimPrefix(ref, "sandbox:"), "//"))
		if err != nil || name == "" || strings.ContainsAny(name, "/\\") {
			return empty, nil, unavailable
		}
	} else if _, ok := types.ParseResourcePath(ref); !ok {
		return empty, nil, unavailable
	}
	// Own-session files remain usable in later turns. Child access is restricted
	// to completed run messages in this runtime-owned group, not a tenant-wide
	// catalog lookup or an arbitrary session ID from model arguments.
	allowed := s.db.Where("ma.session_id = ?", s.sessionID)
	if b.Child == nil {
		g, err := b.Runtime.Store.Read(ctx, b.Scope)
		if err != nil {
			return empty, nil, unavailable
		}
		for _, child := range g.Members {
			for _, run := range child.Runs {
				if run.Result.MessageID != "" && !run.Result.Suspended {
					allowed = allowed.Or("ma.session_id = ? AND ma.message_id = ?",
						child.SessionID, run.Result.MessageID)
				}
			}
		}
	}
	query := s.db.WithContext(ctx).Table("message_artifacts AS ma").Select("ma.*").
		Joins("JOIN messages m ON m.id = ma.message_id AND m.session_id = ma.session_id").
		Joins("JOIN sessions s ON s.id = ma.session_id").
		Where("s.tenant_id = ? AND s.user_id = ?", b.Scope.TenantID, b.Scope.UserID).
		Where("s.deleted_at IS NULL AND m.deleted_at IS NULL AND ma.deleted_at IS NULL").
		Where("m.role = ?", "assistant").Where(allowed)
	if name != "" {
		query = query.Where("ma.file_name = ?", name)
	} else {
		query = query.Where("ma.url = ?", ref)
	}
	var rows []types.MessageArtifactRecord
	if err := query.Find(&rows).Error; err != nil || len(rows) == 0 {
		return empty, nil, unavailable
	}
	artifact := rows[0]
	for _, row := range rows[1:] {
		if row.URL != artifact.URL {
			return empty, nil, fmt.Errorf("multiple generated files have this name; " +
				"use the exact resource:// reference from the child result")
		}
	}
	backend := ""
	if _, ok := types.ParseResourcePath(artifact.URL); ok {
		resource, err := s.catalog.Resolve(ctx, artifact.URL)
		if err != nil || resource == nil || resource.TenantID != b.Scope.TenantID {
			return empty, nil, unavailable
		}
		backend = resource.StorageBackendID
	}
	fs, err := s.files(ctx, backend)
	if err != nil || fs == nil {
		return empty, nil, unavailable
	}
	return artifact, fs, nil
}

func (s *agentService) collaborationArtifactFiles(
	_ context.Context, b *collaboration.Binding, sessionID string,
) tools.ArtifactFileSource {
	if s.db == nil {
		return nil
	}
	return &agentArtifactFiles{
		db: s.db, binding: b, sessionID: sessionID,
		catalog: NewResourceCatalog(repository.NewResourceRepository(s.db)),
		files: func(ctx context.Context, backend string) (interfaces.FileService, error) {
			if s.storageResolver != nil {
				fs, _, err := s.storageResolver.ResolveFileService(
					ctx, &types.Tenant{ID: b.Scope.TenantID}, backend, "", "",
				)
				return fs, err
			}
			return s.fileService, nil
		},
	}
}
