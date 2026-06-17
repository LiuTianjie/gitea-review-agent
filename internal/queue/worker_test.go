package queue

import (
	"context"
	"strings"
	"testing"

	"github.com/turning4th/codex-gitea/internal/model"
)

type finishSkipStore struct {
	model.Store
	logs []string
}

func (s *finishSkipStore) AppendJobLog(_ context.Context, _ int64, stage, message string) error {
	s.logs = append(s.logs, stage+": "+message)
	return nil
}

func (s *finishSkipStore) FinishRunningJobDetailed(context.Context, int64, model.JobFinish) (bool, error) {
	return false, nil
}

type nilProcessor struct{}

func (nilProcessor) Process(context.Context, *model.Job) error { return nil }

func TestRunSkipsFinishWhenJobNoLongerRunning(t *testing.T) {
	st := &finishSkipStore{}
	q := New(st, nilProcessor{}, 1, nil)
	q.run(context.Background(), &model.Job{
		ID: 1,
		PR: model.PRRef{Owner: "acme", Repo: "widgets", Number: 7},
	})

	for _, line := range st.logs {
		if strings.Contains(line, "finish skipped because job is no longer running") {
			return
		}
	}
	t.Fatalf("finish skip log not found in %v", st.logs)
}
