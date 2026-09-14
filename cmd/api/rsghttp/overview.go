package rsghttp

import (
	_ "embed"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/rsg"
)

// The Project Overview page (T0212): the HTML representation of the
// project's research summary, served on GET
// /api/v1/projects/{projectId}/overview when the caller asks for
// text/html (a browser navigation). Non-HTML clients get the JSON
// summary contract on the same route — the same content negotiation the
// research outline route uses.
//
// The page is the project's landing (docs/05: the Overview tab comes
// first): the first screen explains what the project is doing — its
// purpose, key questions, key findings, the active research paths, what
// needs attention and where the accepted state stands (current main).
// Never a files/type listing (docs/06 §4: 绝不把文件树作为首页首屏).
//
// The page is exactly as visible as its project: the service runs the
// same requireRead entry gate as every RSG read, so a denied reader gets
// the same existence-hidden neutral not-found page. Read-only by
// construction: no form, no input, no mutation affordance.

//go:embed overview.html
var overviewTemplateSrc string

var overviewTemplate = template.Must(template.New("overview").Parse(overviewTemplateSrc))

// overviewPayload is the JSON contract for non-HTML clients.
type overviewPayload struct {
	ProjectID      string                     `json:"project_id"`
	ProjectSlug    string                     `json:"project_slug"`
	ProjectName    string                     `json:"project_name"`
	Purpose        string                     `json:"purpose"`
	Visibility     string                     `json:"visibility"`
	MainFrozen     bool                       `json:"main_frozen"`
	ActivityStatus string                     `json:"activity_status"`
	Counts         researchCountsPayload      `json:"counts"`
	KeyQuestions   []researchQuestionPayload  `json:"key_questions"`
	TotalQuestions int                        `json:"total_questions"`
	KeyFindings    []researchFindingPayload   `json:"key_findings"`
	TotalFindings  int                        `json:"total_findings"`
	Branches       overviewBranchesPayload    `json:"branches"`
	NeedsAttention []overviewAttentionPayload `json:"needs_attention"`
	AttentionTotal int                        `json:"attention_total"`
	CurrentMain    overviewMainPayload        `json:"current_main"`
	Empty          bool                       `json:"empty"`
}

type overviewBranchesPayload struct {
	Active  []overviewBranchPayload `json:"active"`
	Merged  int                     `json:"merged"`
	Aborted int                     `json:"aborted"`
}

type overviewBranchPayload struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Purpose     string `json:"purpose,omitempty"`
	HeadStateID string `json:"head_state_id,omitempty"`
	CreatedAt   string `json:"created_at"`
}

type overviewAttentionPayload struct {
	Kind     string `json:"kind"`
	ObjectID string `json:"object_id"`
	Title    string `json:"title"`
	Signal   string `json:"signal"`
}

type overviewMainPayload struct {
	BranchID      string                 `json:"branch_id,omitempty"`
	BranchName    string                 `json:"branch_name,omitempty"`
	HeadStateID   string                 `json:"head_state_id,omitempty"`
	StateHash     string                 `json:"state_hash,omitempty"`
	GitCommitSHA  string                 `json:"git_commit_sha,omitempty"`
	HeadCreatedAt string                 `json:"head_created_at,omitempty"`
	LatestCommit  *overviewCommitPayload `json:"latest_commit,omitempty"`
}

type overviewCommitPayload struct {
	Message   string `json:"message"`
	Via       string `json:"via"`
	Actor     string `json:"actor"`
	CreatedAt string `json:"created_at"`
}

// handleOverview: GET /api/v1/projects/{projectId}/overview — the
// research summary as JSON, or the page for browser navigations.
func (h *handlers) handleOverview(w http.ResponseWriter, r *http.Request) {
	if wantsHTML(r) {
		h.handleOverviewPage(w, r)
		return
	}
	// Both representations share one URL — the shared caches on Accept
	// rule the research route follows. Set here rather than at the route
	// top: the HTML error path renders through the shared error page,
	// which adds its own Vary.
	w.Header().Add("Vary", "Accept")
	overview, err := h.svc.ProjectOverview(r.Context(), reader(r), r.PathValue("projectId"))
	if err != nil {
		rsgError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, overviewPayloadFrom(overview))
}

// overviewPayloadFrom renders the summary as the wire contract.
func overviewPayloadFrom(ov rsg.ProjectOverview) overviewPayload {
	p := overviewPayload{
		ProjectID:      ov.ProjectID,
		ProjectSlug:    ov.ProjectSlug,
		ProjectName:    ov.ProjectName,
		Purpose:        ov.Purpose,
		Visibility:     ov.Visibility,
		MainFrozen:     ov.MainFrozen,
		ActivityStatus: ov.ActivityStatus,
		Counts:         researchCountsPayload(ov.Counts),
		KeyQuestions:   make([]researchQuestionPayload, 0, len(ov.KeyQuestions)),
		TotalQuestions: ov.TotalQuestions,
		KeyFindings:    make([]researchFindingPayload, 0, len(ov.KeyFindings)),
		TotalFindings:  ov.TotalFindings,
		NeedsAttention: make([]overviewAttentionPayload, 0, len(ov.NeedsAttention)),
		AttentionTotal: ov.AttentionTotal,
		Empty:          ov.Empty,
		Branches: overviewBranchesPayload{
			Active:  make([]overviewBranchPayload, 0, len(ov.Branches.Active)),
			Merged:  ov.Branches.Merged,
			Aborted: ov.Branches.Aborted,
		},
	}
	for _, q := range ov.KeyQuestions {
		p.KeyQuestions = append(p.KeyQuestions, researchQuestionPayloadFrom(q))
	}
	for _, f := range ov.KeyFindings {
		p.KeyFindings = append(p.KeyFindings, researchFindingPayloadFrom(f))
	}
	for _, b := range ov.Branches.Active {
		p.Branches.Active = append(p.Branches.Active, overviewBranchPayload{
			ID:          b.ID,
			Name:        b.Name,
			Purpose:     b.Purpose,
			HeadStateID: b.HeadStateID,
			CreatedAt:   b.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	for _, a := range ov.NeedsAttention {
		p.NeedsAttention = append(p.NeedsAttention, overviewAttentionPayload{
			Kind: a.Kind, ObjectID: a.ObjectID, Title: a.Title, Signal: a.Signal,
		})
	}
	if ov.CurrentMain.BranchID != "" {
		p.CurrentMain = overviewMainPayload{
			BranchID:      ov.CurrentMain.BranchID,
			BranchName:    ov.CurrentMain.BranchName,
			HeadStateID:   ov.CurrentMain.HeadStateID,
			StateHash:     ov.CurrentMain.StateHash,
			GitCommitSHA:  ov.CurrentMain.GitCommitSHA,
			HeadCreatedAt: formatOverviewTime(ov.CurrentMain.HeadCreatedAt),
		}
		if ov.CurrentMain.LatestCommit.Present {
			p.CurrentMain.LatestCommit = &overviewCommitPayload{
				Message:   ov.CurrentMain.LatestCommit.Message,
				Via:       ov.CurrentMain.LatestCommit.Via,
				Actor:     ov.CurrentMain.LatestCommit.ActorID,
				CreatedAt: formatOverviewTime(ov.CurrentMain.LatestCommit.CreatedAt),
			}
		}
	}
	return p
}

// formatOverviewTime renders a zero time as "" — the page and the payload
// both treat an absent time as "no activity yet".
func formatOverviewTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// handleOverviewPage renders the overview page for browser requests.
func (h *handlers) handleOverviewPage(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	overview, err := h.svc.ProjectOverview(r.Context(), reader(r), projectID)
	if err != nil {
		status, code, message := rsgErrorOutcome(err)
		renderObjectPageError(w, r, status, code, message)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Add("Vary", "Accept")
	if err := overviewTemplate.Execute(w, overviewPageModelFrom(r, overview)); err != nil {
		// The template parses at init and the model is plain data — an
		// execute failure is a programming error, and the response is
		// already partially written.
		slog.Error("rsghttp: overview page render failed", "error", err)
	}
}

// overviewPageModel is the template's data shape.
type overviewPageModel struct {
	ProjectSlug       string
	ProjectName       string
	Purpose           string
	Visibility        string
	MainFrozen        bool
	ActivityStatus    string
	CountsLine        string
	BranchSummary     string
	Empty             bool
	HasMain           bool
	MainBranchID      string
	OverviewHref      string
	ResearchHref      string
	KeyQuestions      []researchPageQuestion
	TotalQuestions    int
	KeyFindings       []researchPageFinding
	TotalFindings     int
	QuestionsHref     string
	FindingsHref      string
	ActiveBranches    []overviewPageBranch
	MergedBranches    int
	AbortedBranches   int
	NeedsAttention    []overviewPageAttention
	AttentionTotal    int
	CurrentMain       overviewPageMain
	AgentBranchJSON   string
	AgentQuestionJSON string
	AgentRelationJSON string
}

type overviewPageBranch struct {
	Name        string
	Purpose     string
	HeadStateID string
	CreatedAt   string
}

type overviewPageAttention struct {
	Kind        string
	Title       string
	Signal      string
	SignalClass string
	Href        string
}

type overviewPageMain struct {
	Present      bool
	Name         string
	HeadStateID  string
	StateHash    string
	GitCommitSHA string
	HeadAt       string
	Commit       overviewPageCommit
}

type overviewPageCommit struct {
	Present bool
	Message string
	Meta    string
}

// overviewPageModelFrom builds the template model. Every href is built
// from the request path so the page never hard-codes the mount prefix,
// and every dynamic value is plain data — html/template escapes it all.
func overviewPageModelFrom(r *http.Request, ov rsg.ProjectOverview) overviewPageModel {
	projectID := ov.ProjectID
	// The research page lives on the sibling route; derive its canonical
	// URL from the request path (never hard-code the mount prefix).
	researchBase := strings.TrimSuffix(r.URL.Path, "/overview") + "/research"

	questionsQuery := url.Values{}
	questionsQuery.Set("view", "questions")
	findingsQuery := url.Values{}
	findingsQuery.Set("view", "findings")

	model := overviewPageModel{
		ProjectSlug:     ov.ProjectSlug,
		ProjectName:     ov.ProjectName,
		Purpose:         ov.Purpose,
		Visibility:      ov.Visibility,
		MainFrozen:      ov.MainFrozen,
		ActivityStatus:  ov.ActivityStatus,
		CountsLine:      researchCountsLine(ov.Counts),
		Empty:           ov.Empty,
		HasMain:         ov.CurrentMain.BranchID != "",
		MainBranchID:    ov.CurrentMain.BranchID,
		OverviewHref:    r.URL.Path,
		ResearchHref:    researchBase,
		KeyQuestions:    make([]researchPageQuestion, 0, len(ov.KeyQuestions)),
		TotalQuestions:  ov.TotalQuestions,
		KeyFindings:     make([]researchPageFinding, 0, len(ov.KeyFindings)),
		TotalFindings:   ov.TotalFindings,
		ActiveBranches:  make([]overviewPageBranch, 0, len(ov.Branches.Active)),
		MergedBranches:  ov.Branches.Merged,
		AbortedBranches: ov.Branches.Aborted,
		NeedsAttention:  make([]overviewPageAttention, 0, len(ov.NeedsAttention)),
		AttentionTotal:  ov.AttentionTotal,
		QuestionsHref:   researchBase + "?" + questionsQuery.Encode(),
		FindingsHref:    researchBase + "?" + findingsQuery.Encode(),
		BranchSummary:   overviewBranchSummary(ov),
	}

	for _, q := range ov.KeyQuestions {
		model.KeyQuestions = append(model.KeyQuestions, researchPageQuestionFrom(projectID, q))
	}
	for _, f := range ov.KeyFindings {
		model.KeyFindings = append(model.KeyFindings, researchPageFindingFrom(projectID, f))
	}
	for _, b := range ov.Branches.Active {
		model.ActiveBranches = append(model.ActiveBranches, overviewPageBranch{
			Name:        b.Name,
			Purpose:     b.Purpose,
			HeadStateID: b.HeadStateID,
			CreatedAt:   formatOverviewTime(b.CreatedAt),
		})
	}
	for _, a := range ov.NeedsAttention {
		model.NeedsAttention = append(model.NeedsAttention, overviewPageAttention{
			Kind:        a.Kind,
			Title:       a.Title,
			Signal:      a.Signal,
			SignalClass: a.Signal,
			Href:        objectHref(projectID, a.BranchID, a.ObjectID),
		})
	}

	if ov.CurrentMain.BranchID != "" {
		model.CurrentMain = overviewPageMain{
			Present:      true,
			Name:         ov.CurrentMain.BranchName,
			HeadStateID:  ov.CurrentMain.HeadStateID,
			StateHash:    ov.CurrentMain.StateHash,
			GitCommitSHA: ov.CurrentMain.GitCommitSHA,
			HeadAt:       formatOverviewTime(ov.CurrentMain.HeadCreatedAt),
		}
		if ov.CurrentMain.LatestCommit.Present {
			meta := []string{"via " + ov.CurrentMain.LatestCommit.Via}
			if ov.CurrentMain.LatestCommit.ActorID != "" {
				meta = append(meta, ov.CurrentMain.LatestCommit.ActorID)
			}
			meta = append(meta, formatOverviewTime(ov.CurrentMain.LatestCommit.CreatedAt))
			model.CurrentMain.Commit = overviewPageCommit{
				Present: true,
				Message: ov.CurrentMain.LatestCommit.Message,
				Meta:    strings.Join(meta, " · "),
			}
		}
	}

	// The empty-state agent tool references, pre-filled with this
	// project's coordinates — the same "Work with Agent" shape the object
	// detail page uses. The object/relation refs name main's real branch
	// id once main exists (a placeholder before).
	agentBranchID := "<branch_id>"
	if ov.CurrentMain.BranchID != "" {
		agentBranchID = ov.CurrentMain.BranchID
	}
	model.AgentBranchJSON = mustIndentJSON(map[string]any{
		"name": "branch.create",
		"args": map[string]any{
			"project_id": projectID,
			"name":       "main",
		},
	})
	model.AgentQuestionJSON = mustIndentJSON(map[string]any{
		"name": "object.create",
		"args": map[string]any{
			"project_id":  projectID,
			"branch_id":   agentBranchID,
			"object_type": "research_question",
			"payload": map[string]any{
				"statement":      "<the question this project asks>",
				"question_state": "open",
			},
		},
	})
	model.AgentRelationJSON = mustIndentJSON(map[string]any{
		"name": "relation.create",
		"args": map[string]any{
			"project_id":         projectID,
			"branch_id":          agentBranchID,
			"type":               "addresses_question",
			"source_version_ref": "<hypothesis_or_finding_version_id>",
			"target_version_ref": "<question_version_id>",
		},
	})
	return model
}

// overviewBranchSummary renders the branch-surface annotation ("2 active
// paths · 1 merged · 1 aborted"; "" for a project with only main).
func overviewBranchSummary(ov rsg.ProjectOverview) string {
	parts := make([]string, 0, 3)
	if len(ov.Branches.Active) > 0 {
		parts = append(parts, pluralCount(len(ov.Branches.Active), "active path"))
	}
	if ov.Branches.Merged > 0 {
		parts = append(parts, pluralCount(ov.Branches.Merged, "merged path"))
	}
	if ov.Branches.Aborted > 0 {
		parts = append(parts, pluralCount(ov.Branches.Aborted, "aborted path"))
	}
	return joinCounts(parts)
}
