package qbit

import (
	"encoding/json"

	"github.com/sirrobot01/decypharr/internal/config"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

type BuildInfo struct {
	Libtorrent string `json:"libtorrent"`
	Bitness    int    `json:"bitness"`
	Boost      string `json:"boost"`
	Openssl    string `json:"openssl"`
	Qt         string `json:"qt"`
	Zlib       string `json:"zlib"`
}

type AppPreferences struct {
	AddTrackers                        string   `json:"add_trackers"`
	AddTrackersEnabled                 bool     `json:"add_trackers_enabled"`
	AltDlLimit                         int      `json:"alt_dl_limit"`
	AltUpLimit                         int      `json:"alt_up_limit"`
	AlternativeWebuiEnabled            bool     `json:"alternative_webui_enabled"`
	AlternativeWebuiPath               string   `json:"alternative_webui_path"`
	AnnounceIP                         string   `json:"announce_ip"`
	AnnounceToAllTiers                 bool     `json:"announce_to_all_tiers"`
	AnnounceToAllTrackers              bool     `json:"announce_to_all_trackers"`
	AnonymousMode                      bool     `json:"anonymous_mode"`
	AsyncIoThreads                     int      `json:"async_io_threads"`
	AutoDeleteMode                     int      `json:"auto_delete_mode"`
	AutoTmmEnabled                     bool     `json:"auto_tmm_enabled"`
	AutorunEnabled                     bool     `json:"autorun_enabled"`
	AutorunProgram                     string   `json:"autorun_program"`
	BannedIPs                          string   `json:"banned_IPs"`
	BittorrentProtocol                 int      `json:"bittorrent_protocol"`
	BypassAuthSubnetWhitelist          string   `json:"bypass_auth_subnet_whitelist"`
	BypassAuthSubnetWhitelistEnabled   bool     `json:"bypass_auth_subnet_whitelist_enabled"`
	BypassLocalAuth                    bool     `json:"bypass_local_auth"`
	CategoryChangedTmmEnabled          bool     `json:"category_changed_tmm_enabled"`
	CheckingMemoryUse                  int      `json:"checking_memory_use"`
	CreateSubfolderEnabled             bool     `json:"create_subfolder_enabled"`
	CurrentInterfaceAddress            string   `json:"current_interface_address"`
	CurrentNetworkInterface            string   `json:"current_network_interface"`
	Dht                                bool     `json:"dht"`
	DiskCache                          int      `json:"disk_cache"`
	DiskCacheTTL                       int      `json:"disk_cache_ttl"`
	DlLimit                            int      `json:"dl_limit"`
	DontCountSlowTorrents              bool     `json:"dont_count_slow_torrents"`
	DyndnsDomain                       string   `json:"dyndns_domain"`
	DyndnsEnabled                      bool     `json:"dyndns_enabled"`
	DyndnsPassword                     string   `json:"dyndns_password"`
	DyndnsService                      int      `json:"dyndns_service"`
	DyndnsUsername                     string   `json:"dyndns_username"`
	EmbeddedTrackerPort                int      `json:"embedded_tracker_port"`
	EnableCoalesceReadWrite            bool     `json:"enable_coalesce_read_write"`
	EnableEmbeddedTracker              bool     `json:"enable_embedded_tracker"`
	EnableMultiConnectionsFromSameIP   bool     `json:"enable_multi_connections_from_same_ip"`
	EnableOsCache                      bool     `json:"enable_os_cache"`
	EnablePieceExtentAffinity          bool     `json:"enable_piece_extent_affinity"`
	EnableSuperSeeding                 bool     `json:"enable_super_seeding"`
	EnableUploadSuggestions            bool     `json:"enable_upload_suggestions"`
	Encryption                         int      `json:"encryption"`
	ExportDir                          string   `json:"export_dir"`
	ExportDirFin                       string   `json:"export_dir_fin"`
	FilePoolSize                       int      `json:"file_pool_size"`
	IncompleteFilesExt                 bool     `json:"incomplete_files_ext"`
	IPFilterEnabled                    bool     `json:"ip_filter_enabled"`
	IPFilterPath                       string   `json:"ip_filter_path"`
	IPFilterTrackers                   bool     `json:"ip_filter_trackers"`
	LimitLanPeers                      bool     `json:"limit_lan_peers"`
	LimitTCPOverhead                   bool     `json:"limit_tcp_overhead"`
	LimitUtpRate                       bool     `json:"limit_utp_rate"`
	ListenPort                         int      `json:"listen_port"`
	Locale                             string   `json:"locale"`
	Lsd                                bool     `json:"lsd"`
	MailNotificationAuthEnabled        bool     `json:"mail_notification_auth_enabled"`
	MailNotificationEmail              string   `json:"mail_notification_email"`
	MailNotificationEnabled            bool     `json:"mail_notification_enabled"`
	MailNotificationPassword           string   `json:"mail_notification_password"`
	MailNotificationSender             string   `json:"mail_notification_sender"`
	MailNotificationSMTP               string   `json:"mail_notification_smtp"`
	MailNotificationSslEnabled         bool     `json:"mail_notification_ssl_enabled"`
	MailNotificationUsername           string   `json:"mail_notification_username"`
	MaxActiveDownloads                 int      `json:"max_active_downloads"`
	MaxActiveTorrents                  int      `json:"max_active_torrents"`
	MaxActiveUploads                   int      `json:"max_active_uploads"`
	MaxConnec                          int      `json:"max_connec"`
	MaxConnecPerTorrent                int      `json:"max_connec_per_torrent"`
	MaxRatio                           int      `json:"max_ratio"`
	MaxRatioAct                        int      `json:"max_ratio_act"`
	MaxRatioEnabled                    bool     `json:"max_ratio_enabled"`
	MaxSeedingTime                     int      `json:"max_seeding_time"`
	MaxSeedingTimeEnabled              bool     `json:"max_seeding_time_enabled"`
	MaxUploads                         int      `json:"max_uploads"`
	MaxUploadsPerTorrent               int      `json:"max_uploads_per_torrent"`
	OutgoingPortsMax                   int      `json:"outgoing_ports_max"`
	OutgoingPortsMin                   int      `json:"outgoing_ports_min"`
	Pex                                bool     `json:"pex"`
	PreallocateAll                     bool     `json:"preallocate_all"`
	ProxyAuthEnabled                   bool     `json:"proxy_auth_enabled"`
	ProxyIP                            string   `json:"proxy_ip"`
	ProxyPassword                      string   `json:"proxy_password"`
	ProxyPeerConnections               bool     `json:"proxy_peer_connections"`
	ProxyPort                          int      `json:"proxy_port"`
	ProxyTorrentsOnly                  bool     `json:"proxy_torrents_only"`
	ProxyType                          int      `json:"proxy_type"`
	ProxyUsername                      string   `json:"proxy_username"`
	QueueingEnabled                    bool     `json:"queueing_enabled"`
	RandomPort                         bool     `json:"random_port"`
	RecheckCompletedTorrents           bool     `json:"recheck_completed_torrents"`
	ResolvePeerCountries               bool     `json:"resolve_peer_countries"`
	RssAutoDownloadingEnabled          bool     `json:"rss_auto_downloading_enabled"`
	RssMaxArticlesPerFeed              int      `json:"rss_max_articles_per_feed"`
	RssProcessingEnabled               bool     `json:"rss_processing_enabled"`
	RssRefreshInterval                 int      `json:"rss_refresh_interval"`
	SavePath                           string   `json:"save_path"`
	SavePathChangedTmmEnabled          bool     `json:"save_path_changed_tmm_enabled"`
	SaveResumeDataInterval             int      `json:"save_resume_data_interval"`
	ScanDirs                           ScanDirs `json:"scan_dirs"`
	ScheduleFromHour                   int      `json:"schedule_from_hour"`
	ScheduleFromMin                    int      `json:"schedule_from_min"`
	ScheduleToHour                     int      `json:"schedule_to_hour"`
	ScheduleToMin                      int      `json:"schedule_to_min"`
	SchedulerDays                      int      `json:"scheduler_days"`
	SchedulerEnabled                   bool     `json:"scheduler_enabled"`
	SendBufferLowWatermark             int      `json:"send_buffer_low_watermark"`
	SendBufferWatermark                int      `json:"send_buffer_watermark"`
	SendBufferWatermarkFactor          int      `json:"send_buffer_watermark_factor"`
	SlowTorrentDlRateThreshold         int      `json:"slow_torrent_dl_rate_threshold"`
	SlowTorrentInactiveTimer           int      `json:"slow_torrent_inactive_timer"`
	SlowTorrentUlRateThreshold         int      `json:"slow_torrent_ul_rate_threshold"`
	SocketBacklogSize                  int      `json:"socket_backlog_size"`
	StartPausedEnabled                 bool     `json:"start_paused_enabled"`
	StopTrackerTimeout                 int      `json:"stop_tracker_timeout"`
	TempPath                           string   `json:"temp_path"`
	TempPathEnabled                    bool     `json:"temp_path_enabled"`
	TorrentChangedTmmEnabled           bool     `json:"torrent_changed_tmm_enabled"`
	UpLimit                            int      `json:"up_limit"`
	UploadChokingAlgorithm             int      `json:"upload_choking_algorithm"`
	UploadSlotsBehavior                int      `json:"upload_slots_behavior"`
	Upnp                               bool     `json:"upnp"`
	UpnpLeaseDuration                  int      `json:"upnp_lease_duration"`
	UseHTTPS                           bool     `json:"use_https"`
	UtpTCPMixedMode                    int      `json:"utp_tcp_mixed_mode"`
	WebUIAddress                       string   `json:"web_ui_address"`
	WebUIBanDuration                   int      `json:"web_ui_ban_duration"`
	WebUIClickjackingProtectionEnabled bool     `json:"web_ui_clickjacking_protection_enabled"`
	WebUICsrfProtectionEnabled         bool     `json:"web_ui_csrf_protection_enabled"`
	WebUIDomainList                    string   `json:"web_ui_domain_list"`
	WebUIHostHeaderValidationEnabled   bool     `json:"web_ui_host_header_validation_enabled"`
	WebUIHTTPSCertPath                 string   `json:"web_ui_https_cert_path"`
	WebUIHTTPSKeyPath                  string   `json:"web_ui_https_key_path"`
	WebUIMaxAuthFailCount              int      `json:"web_ui_max_auth_fail_count"`
	WebUIPort                          int      `json:"web_ui_port"`
	WebUISecureCookieEnabled           bool     `json:"web_ui_secure_cookie_enabled"`
	WebUISessionTimeout                int      `json:"web_ui_session_timeout"`
	WebUIUpnp                          bool     `json:"web_ui_upnp"`
	WebUIUsername                      string   `json:"web_ui_username"`
	WebUIPassword                      string   `json:"web_ui_password"`
	SSLKey                             string   `json:"ssl_key"`
	SSLCert                            string   `json:"ssl_cert"`
	RSSDownloadRepack                  string   `json:"rss_download_repack_proper_episodes"`
	RSSSmartEpisodeFilters             string   `json:"rss_smart_episode_filters"`
	WebUIUseCustomHTTPHeaders          bool     `json:"web_ui_use_custom_http_headers"`
	WebUIUseCustomHTTPHeadersEnabled   bool     `json:"web_ui_use_custom_http_headers_enabled"`
}

type ScanDirs struct{}

type TorrentCategory struct {
	Name     string `json:"name"`
	SavePath string `json:"savePath"`
}

type TorrentProperties struct {
	AdditionDate           int64  `json:"addition_date,omitempty"`
	Comment                string `json:"comment,omitempty"`
	CompletionDate         int64  `json:"completion_date,omitempty"`
	CreatedBy              string `json:"created_by,omitempty"`
	CreationDate           int64  `json:"creation_date,omitempty"`
	DlLimit                int    `json:"dl_limit,omitempty"`
	DlSpeed                int64  `json:"dl_speed,omitempty"`
	DlSpeedAvg             int    `json:"dl_speed_avg,omitempty"`
	Eta                    int    `json:"eta,omitempty"`
	LastSeen               int64  `json:"last_seen,omitempty"`
	NbConnections          int    `json:"nb_connections,omitempty"`
	NbConnectionsLimit     int    `json:"nb_connections_limit,omitempty"`
	Peers                  int    `json:"peers,omitempty"`
	PeersTotal             int    `json:"peers_total,omitempty"`
	PieceSize              int64  `json:"piece_size,omitempty"`
	PiecesHave             int64  `json:"pieces_have,omitempty"`
	PiecesNum              int64  `json:"pieces_num,omitempty"`
	Reannounce             int    `json:"reannounce,omitempty"`
	SavePath               string `json:"save_path,omitempty"`
	SeedingTime            int    `json:"seeding_time,omitempty"`
	Seeds                  int    `json:"seeds,omitempty"`
	SeedsTotal             int    `json:"seeds_total,omitempty"`
	ShareRatio             int    `json:"share_ratio,omitempty"`
	TimeElapsed            int64  `json:"time_elapsed,omitempty"`
	TotalDownloaded        int64  `json:"total_downloaded,omitempty"`
	TotalDownloadedSession int64  `json:"total_downloaded_session,omitempty"`
	TotalSize              int64  `json:"total_size,omitempty"`
	TotalUploaded          int64  `json:"total_uploaded,omitempty"`
	TotalUploadedSession   int64  `json:"total_uploaded_session,omitempty"`
	TotalWasted            int64  `json:"total_wasted,omitempty"`
	UpLimit                int    `json:"up_limit,omitempty"`
	UpSpeed                int64  `json:"up_speed,omitempty"`
	UpSpeedAvg             int    `json:"up_speed_avg,omitempty"`
}

// defaultPreferencesJSON is the static part of what /app/preferences reports:
// plausible qBittorrent defaults that Arr clients read but decypharr ignores.
// Omitted keys decode to zero values.
const defaultPreferencesJSON = `{
	"alt_dl_limit": 10240,
	"alt_up_limit": 10240,
	"announce_to_all_tiers": true,
	"async_io_threads": 4,
	"checking_memory_use": 32,
	"create_subfolder_enabled": true,
	"dht": true,
	"disk_cache": -1,
	"disk_cache_ttl": 60,
	"dyndns_domain": "changeme.dyndns.org",
	"embedded_tracker_port": 9000,
	"enable_coalesce_read_write": true,
	"enable_os_cache": true,
	"file_pool_size": 40,
	"limit_lan_peers": true,
	"limit_utp_rate": true,
	"listen_port": 31193,
	"locale": "en",
	"lsd": true,
	"mail_notification_sender": "qBittorrentNotification@example.com",
	"mail_notification_smtp": "smtp.changeme.com",
	"max_active_uploads": 3,
	"max_connec": 500,
	"max_connec_per_torrent": 100,
	"max_ratio": -1,
	"max_seeding_time": -1,
	"max_uploads": -1,
	"max_uploads_per_torrent": -1,
	"pex": true,
	"proxy_ip": "0.0.0.0",
	"proxy_port": 8080,
	"resolve_peer_countries": true,
	"rss_max_articles_per_feed": 50,
	"rss_refresh_interval": 30,
	"save_resume_data_interval": 60,
	"schedule_from_hour": 8,
	"schedule_to_hour": 20,
	"send_buffer_low_watermark": 10,
	"send_buffer_watermark": 500,
	"send_buffer_watermark_factor": 50,
	"slow_torrent_dl_rate_threshold": 2,
	"slow_torrent_inactive_timer": 60,
	"slow_torrent_ul_rate_threshold": 2,
	"socket_backlog_size": 30,
	"stop_tracker_timeout": 1,
	"torrent_changed_tmm_enabled": true,
	"upload_choking_algorithm": 1,
	"upnp": true,
	"web_ui_address": "*",
	"web_ui_ban_duration": 3600,
	"web_ui_clickjacking_protection_enabled": true,
	"web_ui_csrf_protection_enabled": true,
	"web_ui_domain_list": "*",
	"web_ui_host_header_validation_enabled": true,
	"web_ui_max_auth_fail_count": 5,
	"web_ui_port": 8080,
	"web_ui_secure_cookie_enabled": true,
	"web_ui_session_timeout": 3600
}`

func getAppPreferences() *AppPreferences {
	preferences := &AppPreferences{}
	if err := json.Unmarshal([]byte(defaultPreferencesJSON), preferences); err != nil {
		panic("qbit: invalid defaultPreferencesJSON: " + err.Error()) // constant input; a test covers it
	}
	maxActiveDownloads := config.Get().MaxActiveDownloads
	preferences.MaxActiveDownloads = maxActiveDownloads
	preferences.MaxActiveTorrents = maxActiveDownloads
	return preferences
}

type Torrent struct {
	Hash         string               `json:"hash"`
	Name         string               `json:"name"`
	Size         int64                `json:"size"`
	Progress     float64              `json:"progress"`
	Dlspeed      int64                `json:"dlspeed"`
	Eta          int64                `json:"eta"`
	NumSeeds     int                  `json:"num_seeds"`
	State        storage.TorrentState `json:"state"`
	Category     string               `json:"category"`
	SavePath     string               `json:"save_path"`
	ContentPath  string               `json:"content_path"`
	AddedOn      int64                `json:"added_on"`
	CompletionOn int64                `json:"completion_on"`
	Debrid       string               `json:"debrid"`
	DebridID     string               `json:"debrid_id"`
	AmountLeft   int64                `json:"amount_left"`
	Downloaded   int64                `json:"downloaded"`
	MagnetURI    string               `json:"magnet_uri"`
	Files        []TorrentFile        `json:"files"`

	Ratio      int    `json:"ratio,omitempty"`
	RatioLimit int    `json:"ratio_limit,omitempty"`
	UpLimit    int    `json:"up_limit,omitempty"`
	DlLimit    int    `json:"dl_limit,omitempty"`
	AutoTmm    bool   `json:"auto_tmm,omitempty"`
	Tracker    string `json:"tracker,omitempty"`
}

type TorrentFile struct {
	Index        int     `json:"index"`
	Name         string  `json:"name,omitempty"`
	Size         int64   `json:"size,omitempty"`
	Progress     int     `json:"progress,omitempty"`
	Priority     int     `json:"priority,omitempty"`
	IsSeed       bool    `json:"is_seed,omitempty"`
	PieceRange   []int   `json:"piece_range,omitempty"`
	Availability float64 `json:"availability,omitempty"`
}

const qbitInfiniteETA int64 = 8640000

// ToQBitTorrent converts to QBitTorrent format for API compatibility.
func convertToQBitTorrentTorrent(t *storage.Entry) Torrent {
	name := t.Name
	contentPath := t.ContentPath
	if config.Get().FolderNaming == config.WebDavUseArrSubmittedName {
		name = t.GetFolder()
		contentPath = t.DownloadPath()
	}
	amountLeft := max(int64(float64(t.Size)*(1-t.Progress)), 0)

	qbitTorrent := Torrent{
		Hash:         t.InfoHash,
		Name:         name,
		Size:         t.Size,
		Progress:     t.Progress,
		Dlspeed:      t.Speed,
		Eta:          calculateETA(amountLeft, t.Speed),
		NumSeeds:     t.Seeders,
		State:        t.State,
		Category:     t.Category,
		SavePath:     t.SavePath,
		ContentPath:  contentPath,
		AddedOn:      t.CreatedAt.Unix(),
		CompletionOn: 0,
		Debrid:       t.ActiveProvider,
		DebridID:     "",
		AmountLeft:   amountLeft,
		Downloaded:   int64(float64(t.Size) * t.Progress),
		MagnetURI:    t.Magnet,
		Files:        getTorrentFiles(t),

		UpLimit:    -1,
		DlLimit:    -1,
		AutoTmm:    false,
		Ratio:      1,
		RatioLimit: 1,
		Tracker:    "udp://tracker.opentrackr.org:1337",
	}
	if t.Status == debridTypes.TorrentStatusQueued {
		qbitTorrent.State = storage.TorrentState("queuedDL")
	}

	return qbitTorrent
}

func calculateETA(amountLeft, downloadSpeed int64) int64 {
	if amountLeft <= 0 {
		return 0
	}
	if downloadSpeed <= 0 {
		return qbitInfiniteETA
	}

	eta := amountLeft / downloadSpeed
	if amountLeft%downloadSpeed != 0 {
		eta++
	}
	return min(eta, qbitInfiniteETA)
}

func getTorrentFiles(t *storage.Entry) []TorrentFile {
	index := 0
	files := make([]TorrentFile, 0)
	for _, f := range t.Files {
		files = append(files, TorrentFile{
			Name:  f.Name,
			Size:  f.Size,
			Index: index,
		})
		index++
	}
	return files
}
