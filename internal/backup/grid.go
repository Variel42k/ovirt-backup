package backup

import "sort"

// oVirt получает диапазоны через ovirt-imageio, а libvirt через NBD. Оба драйвера
// преобразуют их в пары «смещение и длина». После этого данные обрабатываются по общей
// сетке чанков.
//
// Соблюдение арифметики выравнивания здесь, а не у каждого водителя означает, что ошибка в
// ней является единственной ошибкой, которую можно обнаружить с помощью одного теста.

// ChunkSelector преобразует произвольные диапазоны байтов в покрывающие их чанки.
type ChunkSelector struct {
	chunkSize   int64
	virtualSize int64
	gridChunks  int64
	marked      []bool
	count       int
}

// NewChunkSelector готовит селектор для одного диска.
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

// Добавить пометки каждого чанка, перекрывающего [start, start+length].
//
// Диапазон в один байт заставляет прочитать весь содержащий его чанк. Дополнительное чтение
// позволяет восстанавливать данные без слияния перекрывающихся диапазонов разных запусков.
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

// Indices возвращает выбранные номера блоков в порядке возрастания.
func (s *ChunkSelector) Indices() []int64 {
	out := make([]int64, 0, s.count)
	for i, m := range s.marked {
		if m {
			out = append(out, int64(i))
		}
	}
	return out
}

// Count сообщает, сколько фрагментов было выбрано.
func (s *ChunkSelector) Count() int { return s.count }

// GridChunks сообщает, сколько фрагментов охватывает все образ.
func (s *ChunkSelector) GridChunks() int64 { return s.gridChunks }

// ChunkLength возвращает логическую длину чанка с учётом укороченного последнего чанка.
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

// ChunkGroup объединяет соседние чанки образа, которые можно загрузить одним запросом.
type ChunkGroup struct {
	Offset  int64
	Length  int64
	Indices []int64
	Starts  []int64
	Lengths []int64
}

// GroupChunks объединяет последовательные чанки в чтения объёмом не более maxBatch байт.
//
// Пакетирование — разница между одним HTTP или NBD циклом запроса и ответа на 4 МБ и одним
// на 32 МБ; на линии связи с высокой задержкой, которая составляет большую часть времени
// передачи.
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
