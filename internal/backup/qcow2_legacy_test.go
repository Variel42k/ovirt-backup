package backup

import "testing"

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
