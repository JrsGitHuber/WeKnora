package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/Tencent/WeKnora/internal/sandbox"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

type recordingExporter struct {
	file *ExportedFile
	err  error
	refs []string
}

func (r *recordingExporter) ExportURL(_ context.Context, ref string) (*ExportedFile, error) {
	r.refs = append(r.refs, ref)
	return r.file, r.err
}

type recordingExportSink struct {
	name   string
	data   []byte
	result *ExportedFile
	err    error
}

func (s *recordingExportSink) PersistExportedFile(
	_ context.Context, fileName string, data []byte,
) (*ExportedFile, error) {
	s.name, s.data = fileName, data
	return s.result, s.err
}

func mustExportFile(t *testing.T, tool *ExportFileTool, path string) *types.ToolResult {
	t.Helper()
	result, err := tool.Execute(t.Context(), json.RawMessage(fmt.Sprintf(`{"path":%q}`, path)))
	require.NoError(t, err)
	return result
}

func TestExportFileRoutesEachReferenceToItsSource(t *testing.T) {
	attachments := &recordingExporter{file: &ExportedFile{
		URL: "https://files.example/a", FileName: "report.pdf", Size: 7,
	}}
	artifacts := &recordingExporter{file: &ExportedFile{
		URL: "https://files.example/b", FileName: "chart.png", Size: 9,
	}}
	workspace := &recordingExporter{file: &ExportedFile{
		URL: "https://files.example/c", FileName: "run.py", Size: 11,
	}}
	tool := NewExportFileTool().WithAttachments(attachments).WithArtifacts(artifacts).WithWorkspace(workspace)
	require.True(t, tool.HasSources())

	for _, tc := range []struct {
		path string
		want *recordingExporter
		url  string
	}{
		{types.AttachmentPath("doc-1"), attachments, "https://files.example/a"},
		{"resource://abcdefghijklmnopqrstuv", artifacts, "https://files.example/b"},
		{"sandbox:chart.png", artifacts, "https://files.example/b"},
		{"/workspace/output/run.py", workspace, "https://files.example/c"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			attachments.refs, artifacts.refs, workspace.refs = nil, nil, nil

			result := mustExportFile(t, tool, tc.path)

			require.True(t, result.Success, result.Error)
			require.Equal(t, []string{tc.path}, tc.want.refs)
			require.Equal(t, tc.url, result.Data["url"])
			require.Contains(t, result.Output, "url: "+tc.url)
			require.Contains(t, result.Output, "temporary download access")
		})
	}
}

func TestExportFileWithoutSourcesExplainsWhatIsUnavailable(t *testing.T) {
	tool := NewExportFileTool()
	require.False(t, tool.HasSources())

	for _, tc := range []struct{ path, want string }{
		{types.AttachmentPath("doc-1"), "no exportable attachments"},
		{"resource://abcdefghijklmnopqrstuv", "generated files are unavailable"},
		{"sandbox:chart.png", "generated files are unavailable"},
		{"/workspace/output/run.py", "sandbox file export is unavailable"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			result := mustExportFile(t, tool, tc.path)
			require.False(t, result.Success)
			require.Contains(t, result.Error, tc.want)
		})
	}
}

func TestExportFileRejectsReferencesItCannotAddress(t *testing.T) {
	attachments := &recordingExporter{file: &ExportedFile{URL: "https://files.example/a"}}
	artifacts := &recordingExporter{file: &ExportedFile{URL: "https://files.example/b"}}
	workspace := &recordingExporter{file: &ExportedFile{URL: "https://files.example/c"}}
	tool := NewExportFileTool().WithAttachments(attachments).WithArtifacts(artifacts).WithWorkspace(workspace)

	for _, path := range []string{"skill://pdf/SKILL.md", "web://saved-page"} {
		result := mustExportFile(t, tool, path)
		require.False(t, result.Success, path)
		require.Contains(t, result.Error, "export_file supports")
	}
	require.Empty(t, attachments.refs)
	require.Empty(t, artifacts.refs)
	require.Empty(t, workspace.refs)
}

func TestExportFileReportsExporterFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		file *ExportedFile
		err  error
		want string
	}{
		{"error", nil, errors.New("attachment is unavailable"), "attachment is unavailable"},
		{"nil file", nil, nil, "no downloadable URL"},
		{"empty url", &ExportedFile{FileName: "report.pdf"}, nil, "no downloadable URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := NewExportFileTool().WithAttachments(&recordingExporter{file: tc.file, err: tc.err})

			result := mustExportFile(t, tool, types.AttachmentPath("doc-1"))

			require.False(t, result.Success)
			require.Contains(t, result.Error, tc.want)
		})
	}
}

func TestExportFileRequiresAPath(t *testing.T) {
	tool := NewExportFileTool().WithAttachments(&recordingExporter{file: &ExportedFile{URL: "https://x"}})

	for _, args := range []string{`{"path":"   "}`, `{"path":""}`, `{}`} {
		result, err := tool.Execute(t.Context(), json.RawMessage(args))
		require.NoError(t, err)
		require.False(t, result.Success, args)
		require.Contains(t, result.Error, "path is required")
	}

	result, err := tool.Execute(t.Context(), json.RawMessage(`not json`))
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.Error, "invalid export_file arguments")
}

func TestWorkspaceExporterStoresACopyOfTheSandboxFile(t *testing.T) {
	source := &fakeSandboxFileSource{
		stat: &sandbox.RemoteStatEntry{Type: sandbox.RemoteEntryFile, Size: 12},
		data: []byte("print('hi')\n"),
	}
	sink := &recordingExportSink{result: &ExportedFile{
		URL: "https://files.example/run.py", FileName: "run.py", Size: 12,
	}}
	exporter := NewWorkspaceFileExporter(source, sink)
	require.NotNil(t, exporter)

	file, err := exporter.ExportURL(sandboxFileTestContext(), "/workspace/output/run.py")

	require.NoError(t, err)
	require.Equal(t, "https://files.example/run.py", file.URL)
	require.Equal(t, "run.py", sink.name)
	require.Equal(t, []byte("print('hi')\n"), sink.data)
	require.Equal(t, 1, source.readCalls)
	require.Equal(t, "session-1", source.readSession)
	require.Equal(t, "/workspace/output/run.py", source.readPath)
}

func TestWorkspaceExporterRefusesPathsItCannotPublish(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source *fakeSandboxFileSource
		want   string
	}{
		{
			name:   "directory",
			source: &fakeSandboxFileSource{stat: &sandbox.RemoteStatEntry{Type: sandbox.RemoteEntryDir}},
			want:   "path is a directory",
		},
		{
			name:   "symlink",
			source: &fakeSandboxFileSource{stat: &sandbox.RemoteStatEntry{Type: sandbox.RemoteEntryOther}},
			want:   "not a regular file",
		},
		{
			name: "oversize",
			source: &fakeSandboxFileSource{stat: &sandbox.RemoteStatEntry{
				Type: sandbox.RemoteEntryFile, Size: maxSandboxFileBytes + 1,
			}},
			want: "export_file is limited",
		},
		{
			name:   "missing",
			source: &fakeSandboxFileSource{},
			want:   "file not found",
		},
		{
			name:   "stat error",
			source: &fakeSandboxFileSource{statErr: errors.New("backend down")},
			want:   "failed to inspect",
		},
		{
			name: "read error",
			source: &fakeSandboxFileSource{
				stat:    &sandbox.RemoteStatEntry{Type: sandbox.RemoteEntryFile, Size: 1},
				readErr: errors.New("backend down"),
			},
			want: "failed to read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordingExportSink{result: &ExportedFile{URL: "https://files.example/x"}}
			exporter := NewWorkspaceFileExporter(tc.source, sink)

			_, err := exporter.ExportURL(sandboxFileTestContext(), "/workspace/output/x.txt")

			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
			require.Empty(t, sink.name, "a refused file must not reach storage")
		})
	}
}

func TestWorkspaceExporterRequiresASession(t *testing.T) {
	source := &fakeSandboxFileSource{
		stat: &sandbox.RemoteStatEntry{Type: sandbox.RemoteEntryFile, Size: 1},
		data: []byte("x"),
	}
	exporter := NewWorkspaceFileExporter(source, &recordingExportSink{})
	require.NotNil(t, exporter)

	_, err := exporter.ExportURL(context.Background(), "/workspace/output/x.txt")

	require.Error(t, err)
	require.Contains(t, err.Error(), "no session ID")
	require.Zero(t, source.statCalls)
}

func TestWorkspaceExporterKeepsHostPathsInsideTheProject(t *testing.T) {
	source := &layoutFileSource{
		fakeSandboxFileSource: fakeSandboxFileSource{
			stat: &sandbox.RemoteStatEntry{Type: sandbox.RemoteEntryFile, Size: 1},
			data: []byte("x"),
		},
		layout: hostLayout(),
	}
	exporter := NewWorkspaceFileExporter(source, &recordingExportSink{})

	_, err := exporter.ExportURL(sandboxFileTestContext(), "/Users/dev/.ssh/config")

	require.Error(t, err)
	require.Contains(t, err.Error(), "outside that scope")
	require.Zero(t, source.statCalls)
}

func TestNewWorkspaceFileExporterNeedsBothHalves(t *testing.T) {
	require.Nil(t, NewWorkspaceFileExporter(nil, &recordingExportSink{}))
	require.Nil(t, NewWorkspaceFileExporter(&fakeSandboxFileSource{}, nil))
}

func TestExportFileDescriptionOnlyAdvertisesAvailableScopes(t *testing.T) {
	tool := NewExportFileTool().WithAttachments(&recordingExporter{})
	description := tool.Description()
	require.Contains(t, description, "attachment://")
	require.NotContains(t, description, "resource://")
	require.NotContains(t, description, "current session workspace")

	tool.WithWorkspace(&recordingExporter{})
	description = tool.Description()
	require.Contains(t, description, "current session workspace")
	require.Contains(t, description, "temporary access to anyone who holds it")
}

func TestExportedFileSizeIsOptionalInOutput(t *testing.T) {
	tool := NewExportFileTool().WithAttachments(&recordingExporter{file: &ExportedFile{
		URL: "https://files.example/empty", FileName: "empty.txt",
	}})

	result := mustExportFile(t, tool, types.AttachmentPath("doc-1"))

	require.True(t, result.Success)
	require.NotContains(t, result.Output, "size:")
	require.Equal(t, int64(0), result.Data["size"])
}

func TestExportFileTrimsTheRequestedPath(t *testing.T) {
	attachments := &recordingExporter{file: &ExportedFile{URL: "https://files.example/a"}}
	tool := NewExportFileTool().WithAttachments(attachments)

	result, err := tool.Execute(t.Context(), json.RawMessage(
		`{"path":"  `+types.AttachmentPath("doc-1")+`  "}`))

	require.NoError(t, err)
	require.True(t, result.Success, result.Error)
	require.Equal(t, []string{types.AttachmentPath("doc-1")}, attachments.refs)
}
