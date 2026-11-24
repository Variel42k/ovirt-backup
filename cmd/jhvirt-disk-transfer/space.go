package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GNU stat reports available blocks (excluding reserves) and the fundamental
// block size. The index associates each result with its path if a probe fails.
const freeSpaceScript = `index=0
for directory do
    if blocks=$(stat -f -c '%a %S' -- "$directory" 2>/dev/null); then
        printf '%d %s\n' "$index" "$blocks"
    fi
    index=$((index + 1))
done`

func localFreeSpace(ctx context.Context, paths []string) map[string]uint64 {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "sh", append([]string{"-c", freeSpaceScript, "jhvirt-free-space"}, paths...)...)
	command.WaitDelay = 500 * time.Millisecond
	output, _ := command.Output()
	return parseFreeSpace(paths, string(output))
}

func (destination sshDestination) freeSpace(ctx context.Context, paths []string) map[string]uint64 {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	quoted := make([]string, len(paths))
	for index, directory := range paths {
		quoted[index] = "'" + strings.ReplaceAll(directory, "'", "'\\''") + "'"
	}
	output, _ := destination.remoteOutput(ctx, io.Discard, "set -- "+strings.Join(quoted, " ")+"\n"+freeSpaceScript)
	return parseFreeSpace(paths, string(output))
}

func parseFreeSpace(paths []string, output string) map[string]uint64 {
	available := make(map[string]uint64)
	for {
		line, rest, found := strings.Cut(output, "\n")
		if !found {
			return available
		}
		output = rest
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		index, indexErr := strconv.Atoi(fields[0])
		blocks, blocksErr := strconv.ParseUint(fields[1], 10, 64)
		blockSize, sizeErr := strconv.ParseUint(fields[2], 10, 64)
		if indexErr != nil || index < 0 || index >= len(paths) ||
			blocksErr != nil || sizeErr != nil || blockSize == 0 || blocks > math.MaxUint64/blockSize {
			continue
		}
		available[paths[index]] = blocks * blockSize
	}
}

func freeSpaceLabel(available map[string]uint64, directory string) string {
	bytes, known := available[directory]
	if !known {
		return "свободное место не удалось определить"
	}
	if bytes < 1024 {
		return fmt.Sprintf("свободно %d Б", bytes)
	}
	size := float64(bytes) / 1024
	units := [...]string{"КиБ", "МиБ", "ГиБ", "ТиБ", "ПиБ", "ЭиБ"}
	unit := 0
	for size >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}
	return fmt.Sprintf("свободно %.2f %s", size, units[unit])
}
