package types

// TorrentStatus represents the current state of a managed torrent.
type TorrentStatus string

const (
	TorrentStatusRemoteQueued TorrentStatus = "queued"
	TorrentStatusDownloading  TorrentStatus = "downloading"
	TorrentStatusDownloaded   TorrentStatus = "downloaded"
	TorrentStatusError        TorrentStatus = "error"
)
