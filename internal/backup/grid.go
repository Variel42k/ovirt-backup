package backup

import "sort"

// The chunk grid is what both backup drivers agree on: oVirt reports extents
// through ovirt-imageio, libvirt reports them through NBD, and the two formats
// have nothing in common — but once a region of interest is expressed as
// (offset, length), the rest of the pipeline is identical.
//
// Keeping the alignment arithmetic here rather than in each driver means a
// mistake in it is a single mistake, findable by a single test.

// ChunkSelector turns arbitrary byte ranges into the set of grid cells that
// cover them.
type ChunkSelector struct {
	chunkSize   int64
	virtualSize int64
	gridChunks  int64
	marked      []bool
	count       int
}

// NewChunkSelector prepares a selector for one disk.
func NewChunkSelector(chunkSize, virtualSize int64) *ChunkSelector {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	grid := int64(0)
	if virtualSize > 0 {
		grid = (virtualSize + chunkSize - 1) / chunkSize
	}
	return &ChunkSelector{
		chunkSize:   chunkSize,
		virtualSize: virtualSize,
		gridChunks:  grid,
		marked:      make([]bool, grid),
	}
}

// Add marks every chunk overlapping [start, start+length).
//
// A one-byte range pulls in the whole chunk containing it. That over-reads a
// little and in exchange restore never has to reconcile partially overlapping
// writes from different runs.
func (s *ChunkSelector) Add(start, length int64) {
	if length <= 0 || s.gridChunks == 0 {
		return
	}
	end := start + length
	if start < 0 {
		start = 0
	}
	if end > s.virtualSize {
		end = s.virtualSize
	}
	if start >= end {
		return
	}

	first := start / s.chunkSize
	last := (end - 1) / s.chunkSize
	for i := first; i <= last && i < s.gridChunks; i++ {
		if !s.marked[i] {
			s.marked[i] = true
			s.count++
		}
	}
}

// Indices returns the selected chunk numbers in ascending order.
func (s *ChunkSelector) Indices() []int64 {
	out := make([]int64, 0, s.count)
	for i, m := range s.marked {
		if m {
			out = append(out, int64(i))
		}
	}
	return out
}

// Count reports how many chunks were selected.
func (s *ChunkSelector) Count() int { return s.count }

// GridChunks reports how many chunks the whole image spans.
func (s *ChunkSelector) GridChunks() int64 { return s.gridChunks }

// ChunkLength returns the logical length of one grid cell, accounting for a
// final short chunk.
func ChunkLength(index, chunkSize, virtualSize int64) int64 {
	offset := index * chunkSize
	if offset >= virtualSize {
		return 0
	}
	if remaining := virtualSize - offset; remaining < chunkSize {
		return remaining
	}
	return chunkSize
}

// ChunkGroup is a run of chunks that can be fetched from the source in one
// request because they are adjacent in the image.
type ChunkGroup struct {
	Offset  int64
	Length  int64
	Indices []int64
	Starts  []int64
	Lengths []int64
}

// GroupChunks merges consecutive chunk indices into batched reads of at most
// maxBatch bytes.
//
// Batching is the difference between one HTTP or NBD round trip per 4 MiB and
// one per 32 MiB; on a high-latency link that is most of the transfer time.
func GroupChunks(indices []int64, chunkSize, virtualSize, maxBatch int64) []ChunkGroup {
	if maxBatch <= 0 {
		maxBatch = 32 << 20
	}
	sorted := append([]int64(nil), indices...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var groups []ChunkGroup
	var cur *ChunkGroup

	for _, index := range sorted {
		offset := index * chunkSize
		length := ChunkLength(index, chunkSize, virtualSize)
		if length <= 0 {
			continue
		}

		contiguous := cur != nil &&
			cur.Offset+cur.Length == offset &&
			cur.Length+length <= maxBatch
		if !contiguous {
			if cur != nil {
				groups = append(groups, *cur)
			}
			cur = &ChunkGroup{Offset: offset}
		}
		cur.Indices = append(cur.Indices, index)
		cur.Starts = append(cur.Starts, offset)
		cur.Lengths = append(cur.Lengths, length)
		cur.Length += length
	}
	if cur != nil {
		groups = append(groups, *cur)
	}
	return groups
}
