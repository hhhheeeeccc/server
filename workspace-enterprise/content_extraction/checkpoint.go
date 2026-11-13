package content_extraction

import (
	"encoding/json"
	"errors"
	"strconv"

	"github.com/mattermost/mattermost/server/public/model"
)

const checkpointKey = "workspace_extraction_checkpoint_v1"

// A checkpoint describes a completed file outcome, including a failed or
// deliberately ignored file. In-progress converter work is never confirmed.
// Keep the frozen range and mode with it so a mismatched resume fails closed.
type fileCheckpoint struct {
	JobID     string `json:"job_id"`
	At        int64  `json:"at"`
	ID        string `json:"id"`
	From      int64  `json:"from"`
	To        int64  `json:"to"`
	Catchup   bool   `json:"catchup"`
	Processed int64  `json:"processed"`
	Errors    int64  `json:"errors"`
}

func loadCheckpoint(job *model.Job, from, to int64, catchup bool) (fileCheckpoint, error) {
	data := job.Data
	initial := fileCheckpoint{JobID: job.Id, At: from, From: from, To: to, Catchup: catchup}
	saved, exists := data[checkpointKey]
	if !exists {
		// Pre-checkpoint claims cannot prove which files their counters cover.
		// Preserve their existing full-range recovery rather than skip work.
		return initial, nil
	}
	var checkpoint fileCheckpoint
	if json.Unmarshal([]byte(saved), &checkpoint) != nil ||
		checkpoint.JobID != job.Id || !model.IsValidId(checkpoint.ID) || checkpoint.At < from || checkpoint.At > to ||
		checkpoint.From != from || checkpoint.To != to || checkpoint.Catchup != catchup ||
		checkpoint.Processed < 0 || checkpoint.Errors < 0 || checkpoint.Errors > checkpoint.Processed ||
		data["processed"] != strconv.FormatInt(checkpoint.Processed, 10) ||
		data["errors"] != strconv.FormatInt(checkpoint.Errors, 10) {
		return initial, errors.New("invalid saved content extraction checkpoint")
	}
	return checkpoint, nil
}

func (c fileCheckpoint) counters(data model.StringMap) {
	data["processed"], data["errors"] = strconv.FormatInt(c.Processed, 10), strconv.FormatInt(c.Errors, 10)
}
