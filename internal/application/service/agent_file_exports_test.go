package service

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/collaboration"
	"github.com/Tencent/WeKnora/internal/application/attachment"
	"github.com/Tencent/WeKnora/internal/application/repository"
	fileService "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
)

type exportSave struct {
	tenant uint64
	name   string
	data   []byte
}

type exportTestFiles struct {
	interfaces.FileService
	saves     []exportSave
	ref       string
	saveErr   error
	url       string
	urlRef    string
	urlTenant uint64
	urlErr    error
}

func (f *exportTestFiles) SaveBytes(
	_ context.Context, data []byte, tenant uint64, name string, _ bool,
) (string, error) {
	f.saves = append(f.saves, exportSave{tenant: tenant, name: name, data: append([]byte(nil), data...)})
	if f.saveErr != nil {
		return "", f.saveErr
	}
	if f.ref != "" {
		return f.ref, nil
	}
	return "resource://abcdefghijklmnopqrstuv", nil
}

func (f *exportTestFiles) GetFileURL(ctx context.Context, ref string) (string, error) {
	f.urlRef = ref
	if tenant, ok := ctx.Value(types.TenantIDContextKey).(uint64); ok {
		f.urlTenant = tenant
	}
	if f.urlErr != nil {
		return "", f.urlErr
	}
	if f.url != "" {
		return f.url, nil
	}
	return "https://files.example/download/" + url.PathEscape(ref), nil
}

type recordingResourceCatalog struct {
	interfaces.ResourceCatalog
	binds []string
}

func (c *recordingResourceCatalog) Bind(
	_ context.Context, reference, ownerType, ownerID, relation string,
) error {
	c.binds = append(c.binds, strings.Join([]string{reference, ownerType, ownerID, relation}, "|"))
	return nil
}

func TestExportedFileURLRequiresAnHTTPDownload(t *testing.T) {
	for _, raw := range []string{"https://files.example/a", "http://127.0.0.1:8080/b"} {
		file, err := exportedFileURL(" "+raw+" ", "report.pdf", 7)
		require.NoError(t, err, raw)
		require.Equal(t, raw, file.URL)
		require.Equal(t, "report.pdf", file.FileName)
		require.Equal(t, int64(7), file.Size)
	}

	for _, raw := range []string{"", "local://7/exports/a.pdf", "ftp://files.example/a", "not a url"} {
		_, err := exportedFileURL(raw, "report.pdf", 7)
		require.Error(t, err, raw)
		require.Contains(t, err.Error(), "APP_EXTERNAL_URL")
	}
}

func TestAttachmentExportSignsTheStoredOriginalUpload(t *testing.T) {
	db := stagingTempDocDB(t)
	require.NoError(t, db.AutoMigrate(&types.Session{}, &types.Message{}))
	document := sessionDocument("doc-1", ".pdf", types.TemporaryDocumentStatusReady, 10, "parsed body")
	require.NoError(t, db.Create(document).Error)
	missing := sessionDocument("doc-2", ".pdf", types.TemporaryDocumentStatusReady, 10, "")
	missing.ResourceRef = ""
	require.NoError(t, db.Create(missing).Error)
	require.NoError(t, db.Create(&types.Message{
		SessionID: document.SessionID, Role: "user",
		Attachments: types.MessageAttachments{
			{ID: document.ID, FileName: document.FileName, FileType: document.FileType,
				FileSize: document.FileSize, URL: document.ResourceRef},
			{ID: missing.ID, FileName: missing.FileName, FileType: missing.FileType,
				FileSize: missing.FileSize},
			{ID: "doc-3", FileName: "gone.pdf", FileType: ".pdf"},
		},
	}).Error)
	reader := attachment.NewReader(&agentSessionAttachments{
		messages:  repository.NewMessageRepository(db),
		documents: repository.NewTemporaryDocumentRepository(db),
		tenantID:  document.TenantID,
		sessionID: document.SessionID,
	}, 0, nil)

	files := &exportTestFiles{}
	exporter := &agentAttachmentExports{reader: reader, files: files, tenantID: document.TenantID}
	// The shared agent runs under another tenant; signing must use the owner's.
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(99))

	for _, ref := range []string{types.AttachmentPath(document.ID), types.AttachmentHandle(document.ID)} {
		files.urlRef, files.urlTenant = "", 0
		file, err := exporter.ExportURL(ctx, ref)
		require.NoError(t, err, ref)
		require.Equal(t, document.FileName, file.FileName)
		require.Equal(t, document.FileSize, file.Size)
		require.Equal(t, document.ResourceRef, files.urlRef)
		require.Equal(t, document.TenantID, files.urlTenant)
		require.Equal(t, "https://files.example/download/"+url.PathEscape(document.ResourceRef), file.URL)
	}

	for ref, want := range map[string]string{
		"attachment://att_unknown":                      "unknown attachment",
		types.AttachmentPath(document.ID) + "/images/1": "not one of its images",
		types.AttachmentPath(missing.ID):                "has no stored file to export",
		types.AttachmentPath("doc-3"):                   "has no stored file to export",
	} {
		_, err := exporter.ExportURL(ctx, ref)
		require.Error(t, err, ref)
		require.Contains(t, err.Error(), want, ref)
	}
}

func TestAttachmentExportReportsStorageFailures(t *testing.T) {
	db := stagingTempDocDB(t)
	require.NoError(t, db.AutoMigrate(&types.Session{}, &types.Message{}))
	document := sessionDocument("doc-1", ".pdf", types.TemporaryDocumentStatusReady, 10, "")
	require.NoError(t, db.Create(document).Error)
	require.NoError(t, db.Create(&types.Message{
		SessionID: document.SessionID, Role: "user",
		Attachments: types.MessageAttachments{{ID: document.ID, FileName: document.FileName}},
	}).Error)
	reader := attachment.NewReader(&agentSessionAttachments{
		messages:  repository.NewMessageRepository(db),
		documents: repository.NewTemporaryDocumentRepository(db),
		tenantID:  document.TenantID,
		sessionID: document.SessionID,
	}, 0, nil)

	_, err := (&agentAttachmentExports{}).ExportURL(context.Background(), types.AttachmentPath(document.ID))
	require.Error(t, err)
	require.Contains(t, err.Error(), "attachment export is unavailable")

	_, err = (&agentAttachmentExports{reader: reader}).ExportURL(
		context.Background(), types.AttachmentPath(document.ID))
	require.Error(t, err)
	require.Contains(t, err.Error(), "attachment export is unavailable")

	_, err = (&agentAttachmentExports{
		reader: reader, files: &exportTestFiles{urlErr: errors.New("signing failed")}, tenantID: 7,
	}).ExportURL(context.Background(), types.AttachmentPath(document.ID))
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to create a download URL")

	_, err = (&agentAttachmentExports{
		reader: reader, files: &exportTestFiles{url: "local://7/exports/report.pdf"}, tenantID: 7,
	}).ExportURL(context.Background(), types.AttachmentPath(document.ID))
	require.Error(t, err)
	require.Contains(t, err.Error(), "APP_EXTERNAL_URL")
}

func TestWorkspaceExportSinkStoresABindableCopy(t *testing.T) {
	files := &exportTestFiles{}
	catalog := &recordingResourceCatalog{}
	sink := &agentWorkspaceExportSink{
		files: files, tenantID: 7, sessionID: "session-1",
		messageID: "message-1", catalog: catalog,
	}
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(99))

	file, err := sink.PersistExportedFile(ctx, "../report.pdf", []byte("exported bytes"))

	require.NoError(t, err)
	require.Len(t, files.saves, 1)
	save := files.saves[0]
	require.Equal(t, uint64(7), save.tenant, "the session owner stores the copy")
	require.True(t, strings.HasPrefix(save.name, "export_"))
	require.True(t, strings.HasSuffix(save.name, "_.._report.pdf"), save.name)
	require.Equal(t, []byte("exported bytes"), save.data)
	require.Equal(t, "../report.pdf", file.FileName)
	require.Equal(t, int64(len("exported bytes")), file.Size)
	require.Equal(t, "https://files.example/download/"+
		url.PathEscape("resource://abcdefghijklmnopqrstuv"), file.URL)
	require.Equal(t, uint64(7), files.urlTenant)
	require.Equal(t, []string{
		"resource://abcdefghijklmnopqrstuv|" + types.ResourceOwnerMessage + "|message-1|" +
			types.ResourceRelationArtifact,
	}, catalog.binds)
}

func TestWorkspaceExportSinkRefusesUnusableStorage(t *testing.T) {
	_, err := (&agentWorkspaceExportSink{}).PersistExportedFile(context.Background(), "a.txt", []byte("x"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "file storage is unavailable")

	_, err = (&agentWorkspaceExportSink{files: &exportTestFiles{}}).
		PersistExportedFile(context.Background(), "a.txt", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "refusing to export an empty file")

	_, err = (&agentWorkspaceExportSink{files: &exportTestFiles{saveErr: errors.New("disk full")}}).
		PersistExportedFile(context.Background(), "a.txt", []byte("x"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to store exported file")

	_, err = (&agentWorkspaceExportSink{files: &exportTestFiles{
		ref: "local://7/exports/a.txt", url: "local://7/exports/a.txt",
	}}).PersistExportedFile(context.Background(), "a.txt", []byte("x"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "APP_EXTERNAL_URL")
}

func TestWorkspaceExportSinkSkipsBindingWithoutAMessageOwner(t *testing.T) {
	files := &exportTestFiles{}
	catalog := &recordingResourceCatalog{}
	sink := &agentWorkspaceExportSink{files: files, tenantID: 7, catalog: catalog}

	_, err := sink.PersistExportedFile(context.Background(), "a.txt", []byte("x"))

	require.NoError(t, err)
	require.Empty(t, catalog.binds)
}

func TestArtifactExportSignsADeliveredFile(t *testing.T) {
	t.Setenv("APP_EXTERNAL_URL", "https://files.example.com")
	catalog, db := newResourceCatalogForTest(t)
	require.NoError(t, db.AutoMigrate(&collaboration.Record{}, &types.MessageArtifactRecord{}))
	require.NoError(t, db.Exec(`CREATE TABLE sessions (
 id TEXT PRIMARY KEY, tenant_id INTEGER, user_id TEXT, deleted_at DATETIME)`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE messages (
 id TEXT PRIMARY KEY, session_id TEXT, role TEXT, deleted_at DATETIME)`).Error)
	scope := collaboration.Scope{ID: "parent-answer", SessionID: "parent", TenantID: 7, UserID: "alice"}
	r := collaboration.New(collaboration.NewStore(db), nil)
	b, err := r.AcquireParent(t.Context(), scope)
	require.NoError(t, err)
	for _, name := range []string{"parent", "child"} {
		require.NoError(t, db.Exec(
			"INSERT INTO sessions(id,tenant_id,user_id) VALUES(?,7,'alice')", name).Error)
		require.NoError(t, db.Exec(
			"INSERT INTO messages(id,session_id,role) VALUES(?,?,'assistant')", name+"-answer", name).Error)
	}
	memory := &webPageMemoryFiles{objects: map[string]string{}}
	fs := fileService.NewResourceCatalogFileService(memory, catalog)
	body := "# Chapter\nchild text\n"
	ref, err := fs.SaveBytes(t.Context(), []byte(body), 7, "report.md", false)
	require.NoError(t, err)
	records := types.NewMessageArtifactRecords("child", "child-answer", types.MessageArtifacts{{
		URL: ref, FileName: "report.md", FileType: ".md", FileSize: int64(len(body)),
	}})
	require.NoError(t, db.Create(&records).Error)
	require.NoError(t, r.Store.Change(b.Context(t.Context()), scope, func(g *collaboration.Group) error {
		g.Members = append(g.Members, &collaboration.Member{
			ID: "child", Name: "child", SessionID: "child",
			Runs: []*collaboration.Run{{
				ID: "child-run", State: "succeeded",
				Result: collaboration.Result{MessageID: "child-answer"},
			}},
		})
		return nil
	}))
	source := &agentArtifactFiles{
		db: db, catalog: catalog, binding: b, sessionID: "parent",
		files: func(context.Context, string) (interfaces.FileService, error) { return fs, nil },
	}

	file, err := source.ExportURL(t.Context(), ref)

	require.NoError(t, err)
	require.Equal(t, "report.md", file.FileName)
	require.Equal(t, int64(len(body)), file.Size)
	require.True(t, strings.HasPrefix(file.URL, "https://files.example.com/r/"), file.URL)

	_, err = source.ExportURL(t.Context(), "resource://abcdefghijklmnopqrstuv")
	require.Error(t, err, "undelivered references must not be exportable")
}
