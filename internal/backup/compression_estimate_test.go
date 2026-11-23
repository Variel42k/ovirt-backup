package backup

import (
	"bytes"
	"context"
	"errors"
	"math"
	"math/rand"
	"testing"
)

func TestCompressionSamplePlanBoundedAndSpansEveryDisk(t *testing.T) {
	disks := []CompressionSampleDisk{{ID: "a", Size: 2 << 40}, {ID: "b", Size: 127}, {ID: "c", Size: math.MaxInt64}}
	points, err := CompressionSamplePlan(disks, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	seen := map[[2]int64]bool{}
	visited := map[int]bool{}
	for _, p := range points {
		if p.Offset < 0 || p.Length <= 0 || p.Length > 4<<20 || p.Offset > disks[p.Disk].Size-p.Length {
			t.Fatalf("invalid point %+v", p)
		}
		key := [2]int64{int64(p.Disk), p.Offset}
		if seen[key] {
			t.Fatal("duplicate point")
		}
		seen[key] = true
		visited[p.Disk] = true
		total += p.Length
	}
	if total > CompressionSampleBudget || len(points) > CompressionSamplePoints || len(visited) != len(disks) {
		t.Fatalf("unbounded plan: %d %v", total, points)
	}
	for _, size := range []int64{0, 8 << 20} {
		if _, err := CompressionSamplePlan(disks, size); err == nil {
			t.Fatalf("accepted chunk size %d", size)
		}
	}
	if _, err := CompressionSamplePlan(make([]CompressionSampleDisk, 17), 4<<20); err == nil {
		t.Fatal("accepted too many disks")
	}
}

func TestCompressionEstimateSeparatesZerosAndUsesWriterRepresentation(t *testing.T) {
	data := bytes.Repeat([]byte("application records abcdefghijklmnopqrstuvwxyz\n"), 4096)
	noise := make([]byte, 64<<10)
	_, _ = rand.New(rand.NewSource(7)).Read(noise)
	samples := [][]byte{make([]byte, len(data)), data, noise}
	result, err := CompareCompression(context.Background(), samples, 3)
	if err != nil {
		t.Fatal(err)
	}
	if result.ZeroBytes != int64(len(data)) || result.ComparedBytes != int64(len(data)+len(noise)) || len(result.Results) != len(Compressions) {
		t.Fatalf("bad coverage %+v", result)
	}
	for _, row := range result.Results {
		c, err := newCodec(row.Algorithm, 3, nil)
		if err != nil {
			t.Fatal(err)
		}
		var want int64
		for _, plain := range [][]byte{data, noise} {
			encoded, _, err := c.encode(plain)
			if err != nil {
				t.Fatal(err)
			}
			want += int64(len(encoded))
		}
		c.close()
		if row.StoredBytes != want || row.SavingPercent < 0 || row.SavingPercent > 100 {
			t.Fatalf("wrong estimate %+v want %d", row, want)
		}
		if row.Algorithm == CompressionNone && row.SavingPercent != 0 {
			t.Fatal("none claims savings")
		}
	}
}

func TestCompressionEstimateEmptyAndCanceled(t *testing.T) {
	r, err := CompareCompression(context.Background(), [][]byte{make([]byte, 4096)}, 3)
	if err != nil || len(r.Results) != 0 || len(r.Warnings) == 0 {
		t.Fatalf("zeros presented as real compression: %+v %v", r, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = CompareCompression(ctx, [][]byte{bytes.Repeat([]byte("x"), 4096)}, 3); !errors.Is(err, context.Canceled) {
		t.Fatalf("ignored cancellation: %v", err)
	}
}

func TestCompressionRangePointsAvoidThinHolesWithoutExpandingWholeDisk(t *testing.T) {
	points, err := CompressionRangePoints(2<<40, 4<<20, 16, []CompressionDataRange{{Offset: 0, Length: 64 << 20}, {Offset: 1 << 40, Length: 64 << 20}})
	if err != nil || len(points) != 16 {
		t.Fatalf("sample %v %v", points, err)
	}
	for _, p := range points {
		if p.Offset >= 64<<20 && p.Offset < 1<<40 {
			t.Fatalf("sampled a hole: %+v", p)
		}
	}
	if _, err = CompressionRangePoints(100, 4, 16, []CompressionDataRange{{Offset: 90, Length: 11}}); err == nil {
		t.Fatal("accepted malformed extent")
	}
	if empty, err := CompressionRangePoints(100, 4, 16, nil); err != nil || len(empty) != 0 {
		t.Fatal("empty allocated map needs no data reads")
	}
}
