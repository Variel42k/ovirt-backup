package backup

import (
	"os"
	"strings"
	"testing"
)

func TestBackingMatchesExactOVirtImageID(t *testing.T) {
	const imageID = "b27e0406-94d2-4d1c-9014-819503f4b030"
	for _, path := range []string{
		"/rhev/data-center/mnt/domain/images/group/" + imageID,
		`C:\images\` + imageID + `.raw`,
	} {
		if !backingMatchesImage(path, imageID) {
			t.Fatalf("image_id не найден в %q", path)
		}
	}
	if backingMatchesImage("/images/prefix-"+imageID+"-suffix", imageID) {
		t.Fatal("частичное совпадение backing path принято за нужный image_id")
	}
}

func TestFindQemuImgHonoursExplicitPathOutsidePATH(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got, err := FindQemuImg("  " + executable + "  ")
	if err != nil {
		t.Fatalf("явный исполняемый путь не найден: %v", err)
	}
	if got == "" {
		t.Fatal("получен пустой путь")
	}

	_, err = FindQemuImg(executable + ".missing")
	if err == nil || !strings.Contains(err.Error(), "по указанному пути") {
		t.Fatalf("нет точной ошибки явного пути: %v", err)
	}
}
