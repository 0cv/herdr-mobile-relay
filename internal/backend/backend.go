package backend

import (
	"context"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

type TopologyStream = herdr.TopologyStream

type Backend interface {
	GetInventory(ctx context.Context) (*herdr.Inventory, error)
	PaneList(ctx context.Context) ([]herdr.Pane, error)
	WorkspaceList(ctx context.Context) ([]herdr.Workspace, error)
	WorkspaceCreate(ctx context.Context, cwd, label string) (*herdr.CreateResult, error)
	WorkspaceRename(ctx context.Context, workspaceID, label string) error
	WorkspaceMove(ctx context.Context, workspaceID string, insertIndex int) error
	WorkspaceMoveBlock(
		ctx context.Context,
		workspaceIDs []string,
		beforeWorkspaceID string,
	) error
	WorkspaceClose(ctx context.Context, workspaceID string, closeGroup bool) error
	WorktreeList(ctx context.Context, workspaceID string) (*herdr.WorktreeListResult, error)
	WorktreeCreate(
		ctx context.Context,
		workspaceID, branch, base, label string,
	) (*herdr.WorktreeMutationResult, error)
	WorktreeOpen(
		ctx context.Context,
		workspaceID, path, branch, label string,
	) (*herdr.WorktreeMutationResult, error)
	WorktreeRemove(
		ctx context.Context,
		workspaceID string,
		force bool,
	) (*herdr.WorktreeRemoveResult, error)
	TabCreate(ctx context.Context, workspaceID, cwd, label string) (*herdr.CreateResult, error)
	TabList(ctx context.Context) ([]herdr.Tab, error)
	TabRename(ctx context.Context, tabID, label string) error
	TabMove(ctx context.Context, tabID string, insertIndex int) error
	PaneRun(ctx context.Context, paneID string, argv []string) error
	AgentGet(ctx context.Context, paneID string) (*herdr.AgentInfo, error)
	PaneGet(ctx context.Context, paneID string) (*herdr.Pane, error)
	PaneProcessInfoFresh(ctx context.Context, paneID string) (*herdr.PaneProcessInfo, error)
	ReadPane(ctx context.Context, paneID string, lines int, format string) (herdr.PaneRead, error)
	ReadPaneRecent(ctx context.Context, paneID string, lines int, format string) (herdr.PaneRead, error)
	ReadPaneVisible(ctx context.Context, paneID string, lines int, format string) (herdr.PaneRead, error)
	ProbePaneVisible(ctx context.Context, paneID string, lines int, format string) ([]byte, error)
	Close() error
	SendKeys(ctx context.Context, paneID string, keys []string) error
	SendText(ctx context.Context, paneID, text string) error
	SendSecret(ctx context.Context, paneID, text string) error
	Prompt(ctx context.Context, paneID, text string) error
	StopPane(ctx context.Context, paneID string) error
	RenameAgent(ctx context.Context, paneID, name string) error
	StartAgent(ctx context.Context, name, kind, paneID string, timeoutMs int, agentArgs ...string) (string, error)
	IntegrationStatus(ctx context.Context) ([]byte, error)
	CapabilityStatus() herdr.ServerStatus
	SetCapabilityChangeCallback(callback func(herdr.ServerStatus))
	RunCapabilityRefresh(ctx context.Context, interval time.Duration)
	SendInput(ctx context.Context, paneID string, input herdr.PaneInput) error
	SendLiteralKey(ctx context.Context, paneID, character string) error
	Events(ctx context.Context) (herdr.TopologySnapshot, herdr.TopologyStream, error)
}
