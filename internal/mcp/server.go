// Package mcp exposes Nexora to external AI clients (ChatGPT, Claude, ...)
// as an MCP server over streamable HTTP, using the Code Mode pattern: a
// small fixed tool surface where the model reads a typed API and submits
// scripts instead of calling every extension tool directly.
//
// Long runs and proxy timeouts: many deployments sit behind a proxy that
// drops requests after ~100 s (e.g. Cloudflare's 524). run_code therefore
// waits for the result only up to InlineWait. If the script finishes in
// time, the result comes back in the same call. Otherwise the call returns
// a job ID, and the model polls get_job, which long-polls for up to
// InlineWait per call. Every run goes through the shared job queue.
//
// The client adapter (external MCP servers as extensions, runtime "mcp")
// will live here too.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"nexora/internal/codemode"
	"nexora/internal/extension"
	"nexora/internal/jobs"
	"nexora/internal/runner"
)

type Options struct {
	// InlineWait is the longest a single tool call blocks. Keep it safely
	// below the shortest proxy timeout between the AI and Nexora.
	InlineWait time.Duration
	// ProgressInterval is how often progress notifications are sent while
	// waiting, for clients that pass a progress token.
	ProgressInterval time.Duration
}

func (o *Options) defaults() {
	if o.InlineWait <= 0 {
		o.InlineWait = 80 * time.Second
	}
	if o.ProgressInterval <= 0 {
		o.ProgressInterval = 10 * time.Second
	}
}

const instructions = `Nexora gives you access to the user's local extensions (spreadsheets, documents, ...) through Code Mode.

1. Call get_api to see the TypeScript declaration of the global "tools" object.
2. Call run_code with a JavaScript async function body that uses it, e.g.
     const rows = await tools.excel.readSheet({ path: "/home/me/q3.xlsx" })
     return rows.length
   Use Promise.all for independent calls; they run in parallel.
3. If run_code returns status "queued" or "running", the script is still executing.
   Call get_job with the returned jobId until the status is "succeeded", "failed" or "cancelled".
   Do not resubmit the same script while it is still running.
4. tools.store (when listed by get_api) is a cache that keeps JSON between runs, e.g. to remember
   what a previous run already processed. Entries expire and may be evicted, so handle a miss.`

// RunCodeInput is the input of run_code.
type RunCodeInput struct {
	Code string `json:"code" jsonschema:"JavaScript async function body. Use the global 'tools' object from get_api and 'return' a JSON-serialisable result."`
}

// GetJobInput is the input of get_job.
type GetJobInput struct {
	JobID       string `json:"jobId" jsonschema:"Job ID returned by run_code"`
	WaitSeconds int    `json:"waitSeconds,omitempty" jsonschema:"How long to wait for completion before returning the current status (default and maximum: the server's inline wait)"`
}

// CancelJobInput is the input of cancel_job.
type CancelJobInput struct {
	JobID string `json:"jobId" jsonschema:"Job ID returned by run_code"`
}

type GetAPIInput struct{}

type GetAPIOutput struct {
	TypeScript string   `json:"typescript" jsonschema:"Declaration of the global 'tools' object available to run_code scripts"`
	Extensions []string `json:"extensions" jsonschema:"Running extensions (namespaces under 'tools')"`
}

// JobOutput is returned by run_code, get_job and cancel_job.
type JobOutput struct {
	JobID         string   `json:"jobId"`
	Status        string   `json:"status" jsonschema:"queued, running, succeeded, failed or cancelled"`
	QueuePosition int      `json:"queuePosition,omitempty" jsonschema:"Place in the queue while queued (1 = next)"`
	Result        any      `json:"result,omitempty" jsonschema:"Value returned by the script (when succeeded)"`
	Error         string   `json:"error,omitempty"`
	Logs          []string `json:"logs,omitempty" jsonschema:"console output from the script"`
	ToolCalls     int      `json:"toolCalls,omitempty"`
	ElapsedMs     int64    `json:"elapsedMs,omitempty"`
	// WaitingForApproval lists tool calls paused until the user approves
	// them in the Nexora app.
	WaitingForApproval []string `json:"waitingForApproval,omitempty"`
	Next               string   `json:"next,omitempty" jsonschema:"What to do next"`
}

// NewServer builds the MCP server. Mount it with NewHandler.
func NewServer(r *runner.Runner, opts Options) *mcp.Server {
	opts.defaults()
	s := &server{runner: r, opts: opts}

	srv := mcp.NewServer(&mcp.Implementation{Name: "nexora", Title: "Nexora", Version: extension.HostVersion},
		&mcp.ServerOptions{Instructions: instructions})

	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_api",
		Description: "Get the TypeScript API of the user's extensions, available to run_code scripts as the global 'tools' object.",
		Annotations: readOnly,
	}, s.getAPI)
	mcp.AddTool(srv, &mcp.Tool{
		Name: "run_code",
		Description: fmt.Sprintf("Run a JavaScript script against the user's extensions. Returns the result directly if it finishes within %d seconds; "+
			"otherwise returns a jobId to poll with get_job.", int(opts.InlineWait.Seconds())),
	}, s.runCode)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_job",
		Description: "Get the status and result of a run_code job, waiting up to waitSeconds for it to finish.",
		Annotations: readOnly,
	}, s.getJob)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "cancel_job",
		Description: "Cancel a queued or running run_code job.",
	}, s.cancelJob)
	return srv
}

type server struct {
	runner *runner.Runner
	opts   Options
}

func (s *server) getAPI(_ context.Context, _ *mcp.CallToolRequest, _ GetAPIInput) (*mcp.CallToolResult, GetAPIOutput, error) {
	out := GetAPIOutput{TypeScript: s.runner.API(), Extensions: s.runner.Extensions()}
	return nil, out, nil
}

func (s *server) runCode(ctx context.Context, req *mcp.CallToolRequest, in RunCodeInput) (*mcp.CallToolResult, JobOutput, error) {
	if in.Code == "" {
		return nil, JobOutput{}, errors.New("code is required")
	}
	job, err := s.runner.Submit(in.Code, runner.SourceMCP)
	if err != nil {
		return nil, JobOutput{}, err
	}
	return s.wait(ctx, req, job.ID, s.opts.InlineWait)
}

func (s *server) getJob(ctx context.Context, req *mcp.CallToolRequest, in GetJobInput) (*mcp.CallToolResult, JobOutput, error) {
	wait := s.opts.InlineWait
	if in.WaitSeconds > 0 {
		wait = min(time.Duration(in.WaitSeconds)*time.Second, wait)
	}
	return s.wait(ctx, req, in.JobID, wait)
}

func (s *server) cancelJob(_ context.Context, _ *mcp.CallToolRequest, in CancelJobInput) (*mcp.CallToolResult, JobOutput, error) {
	job, err := s.runner.Queue().Cancel(in.JobID)
	if err != nil {
		return nil, JobOutput{}, err
	}
	return s.toOutput(job)
}

// wait blocks for up to maxWait, sending progress notifications if the
// client asked for them, and reports the job's state at that point.
func (s *server) wait(ctx context.Context, req *mcp.CallToolRequest, id string, maxWait time.Duration) (*mcp.CallToolResult, JobOutput, error) {
	stop := s.reportProgress(ctx, req, id)
	defer stop()
	job, err := s.runner.Queue().Wait(ctx, id, maxWait)
	if err != nil {
		return nil, JobOutput{}, err
	}
	return s.toOutput(job)
}

// reportProgress periodically notifies the client while a job is pending.
// Only clients that send a progress token receive these notifications.
func (s *server) reportProgress(ctx context.Context, req *mcp.CallToolRequest, id string) (stop func()) {
	token := req.Params.GetProgressToken()
	if token == nil || req.Session == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(s.opts.ProgressInterval)
		defer t.Stop()
		for n := 1; ; n++ {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				job, err := s.runner.Queue().Get(id)
				if err != nil || job.State.Finished() {
					return
				}
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
					ProgressToken: token,
					Progress:      float64(n),
					Message:       s.progressMessage(job),
				})
			}
		}
	}()
	return cancel
}

func (s *server) progressMessage(job jobs.Job) string {
	if job.State == jobs.StateQueued {
		return fmt.Sprintf("queued (position %d)", job.Position)
	}
	if waiting := s.runner.WaitingFor(job.ID); len(waiting) > 0 {
		return "waiting for the user to approve " + strings.Join(waiting, ", ")
	}
	return "running"
}

func (s *server) toOutput(job jobs.Job) (*mcp.CallToolResult, JobOutput, error) {
	out := JobOutput{
		JobID:         job.ID,
		Status:        string(job.State),
		QueuePosition: job.Position,
		Error:         job.Error,
	}
	if res, ok := job.Result.(*codemode.Result); ok && res != nil {
		out.Result = res.Value
		out.Logs = res.Logs
		out.ToolCalls = res.ToolCalls
	}
	if job.StartedAt != nil {
		end := time.Now()
		if job.FinishedAt != nil {
			end = *job.FinishedAt
		}
		out.ElapsedMs = end.Sub(*job.StartedAt).Milliseconds()
	}

	switch job.State {
	case jobs.StateQueued, jobs.StateRunning:
		out.WaitingForApproval = s.runner.WaitingFor(job.ID)
		if len(out.WaitingForApproval) > 0 {
			out.Next = fmt.Sprintf("Paused until the user approves %s in the Nexora app; tell them. Then call get_job with jobId %q.",
				strings.Join(out.WaitingForApproval, ", "), job.ID)
		} else {
			out.Next = fmt.Sprintf("Still %s. Call get_job with jobId %q to wait for the result.", job.State, job.ID)
		}
		return nil, out, nil
	case jobs.StateSucceeded:
		return nil, out, nil
	default: // failed, cancelled
		return &mcp.CallToolResult{IsError: true}, out, nil
	}
}
