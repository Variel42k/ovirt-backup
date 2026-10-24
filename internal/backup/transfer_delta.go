package backup

import (
	"context"
	"fmt"
	"sort"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// ChangedSince refuses a replaced/truncated baseline: silently streaming a full
// disk here would invalidate the promised downtime and could mix generations.
func (r *ChainReader) ChangedSince(runID string) ([]int64, error) {
	found := false
	changed := map[int64]bool{}
	for _, m := range r.manifests {
		if m.RunID == runID {
			found = true
			continue
		}
		if !found {
			continue
		}
		if m.Type != model.BackupIncremental {
			return nil, fmt.Errorf("после опорной точки %s обнаружен полный диск; предварительную миграцию нужно подготовить заново", runID)
		}
		for _, ch := range m.Chunks {
			changed[ch.Index] = true
		}
	}
	if !found {
		return nil, fmt.Errorf("опорная точка %s отсутствует в цепочке диска", runID)
	}
	out := make([]int64, 0, len(changed))
	for i := range changed {
		out = append(out, i)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (r *ChainReader) streamChanged(ctx context.Context, after string, sink ImageSink, observe StreamObserver) error {
	indices, err := r.ChangedSince(after)
	if err != nil {
		return err
	}
	for _, i := range indices {
		if err = ctx.Err(); err != nil {
			return err
		}
		offset, length := i*r.chunkSize, r.ChunkLength(i)
		if observe != nil {
			observe(StreamReadingBackup, offset, length)
		}
		data, err := r.ReadChunk(ctx, i)
		if err != nil {
			return err
		}
		if observe != nil {
			observe(StreamWritingTarget, offset, length)
		}
		if data == nil {
			err = sink(ctx, offset, nil, length)
		} else {
			err = sink(ctx, offset, data, 0)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
