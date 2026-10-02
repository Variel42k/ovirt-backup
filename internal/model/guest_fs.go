package model

// GuestFilesystem — файловая система, которую гость смонтировал, по данным
// гостевого агента. Размеры равны нулю, если агент их не сообщил.
type GuestFilesystem struct {
	Mountpoint string `json:"mountpoint"`
	Type       string `json:"type,omitempty"`
	Device     string `json:"device,omitempty"`
	TotalBytes int64  `json:"total_bytes,omitempty"`
	UsedBytes  int64  `json:"used_bytes,omitempty"`
}
