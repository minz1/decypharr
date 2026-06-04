{ config, lib, pkgs, ... }:

let
  cfg = config.services.decypharr;
  format = pkgs.formats.json { };
  configFile = format.generate "decypharr-config.json" cfg.settings;
in
{
  options.services.decypharr = {
    enable = lib.mkEnableOption "Decypharr debrid mock qBittorrent service";

    package = lib.mkOption {
      type = lib.types.package;
      description = "The decypharr package to use. Set automatically when importing via the flake's nixosModules.default.";
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "decypharr";
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "decypharr";
    };

    extraGroups = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Extra groups for the decypharr user (e.g. 'media' for mount access).";
      example = [ "media" ];
    };

    mediaGroup = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = ''
        If set, the download folder is created as group-writable (0775) owned by
        this group rather than 0750 decypharr:decypharr. Use when the download
        directory is shared with other services (e.g. sonarr, radarr) via a
        common media group.
      '';
      example = "media";
    };

    configDir = lib.mkOption {
      type = lib.types.str;
      default = "/var/lib/decypharr";
      description = "Directory passed to --config. config.json, auth.json, and the data store live here.";
    };

    openFirewall = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Open the firewall for the decypharr port.";
    };

    environmentFiles = lib.mkOption {
      type = lib.types.listOf lib.types.path;
      default = [ ];
      description = ''
        Files containing secret environment variables loaded into the service.
        Use agenix or sops-nix to manage these. Variables follow the
        DECYPHARR_ prefix convention, e.g.:
          DECYPHARR_DEBRIDS__0__API_KEY=abc123
          DECYPHARR_ARRS__0__TOKEN=xyz
      '';
      example = [ "/run/secrets/decypharr.env" ];
    };

    # -------------------------------------------------------------------------
    # Server
    # -------------------------------------------------------------------------

    port = lib.mkOption {
      type = lib.types.port;
      default = 8282;
      description = "Port decypharr listens on. Maps to DECYPHARR_PORT.";
    };

    bindAddress = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "Bind address (empty = all interfaces). Maps to DECYPHARR_BIND_ADDRESS.";
    };

    urlBase = lib.mkOption {
      type = lib.types.str;
      default = "/";
      description = "URL base path prefix for the web UI. Maps to DECYPHARR_URL_BASE.";
    };

    logLevel = lib.mkOption {
      type = lib.types.enum [ "debug" "info" "warn" "error" ];
      default = "info";
      description = "Log verbosity. Maps to DECYPHARR_LOG_LEVEL.";
    };

    useAuth = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Require authentication for the web UI. Maps to DECYPHARR_USE_AUTH.";
    };

    enableWebdavAuth = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Require authentication for WebDAV access. Maps to DECYPHARR_ENABLE_WEBDAV_AUTH.";
    };

    # -------------------------------------------------------------------------
    # Manager / downloads
    # -------------------------------------------------------------------------

    downloadFolder = lib.mkOption {
      type = lib.types.str;
      default = "/var/lib/decypharr/downloads";
      description = "Directory where decypharr writes category symlinks. Maps to DECYPHARR_DOWNLOAD_FOLDER.";
    };

    refreshInterval = lib.mkOption {
      type = lib.types.str;
      default = "30s";
      description = "How often decypharr polls debrid for status changes. Maps to DECYPHARR_REFRESH_INTERVAL.";
    };

    maxDownloads = lib.mkOption {
      type = lib.types.int;
      default = 0;
      description = "Concurrent download limit (0 = unlimited). Maps to DECYPHARR_MAX_DOWNLOADS.";
    };

    skipPreCache = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Skip pre-caching files after adding. Maps to DECYPHARR_SKIP_PRE_CACHE.";
    };

    skipAutoMove = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Skip automatically moving completed downloads. Maps to DECYPHARR_SKIP_AUTO_MOVE.";
    };

    alwaysRmTrackerUrls = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Always strip tracker URLs from torrents before sending to debrid. Maps to DECYPHARR_ALWAYS_RM_TRACKER_URLS.";
    };

    retries = lib.mkOption {
      type = lib.types.int;
      default = 3;
      description = "Retry attempts before marking an entry bad. Maps to DECYPHARR_RETRIES.";
    };

    minFileSize = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "Ignore files smaller than this (e.g. '10MB'). Maps to DECYPHARR_MIN_FILE_SIZE.";
    };

    maxFileSize = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "Ignore files larger than this (e.g. '50GB'). Maps to DECYPHARR_MAX_FILE_SIZE.";
    };

    removeStalledAfter = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "Remove stalled downloads after this duration. Maps to DECYPHARR_REMOVE_STALLED_AFTER.";
    };

    nzbUserAgent = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "User-agent string for NZB download requests. Maps to DECYPHARR_NZB_USER_AGENT.";
    };

    fixNzbSizes = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "One-shot flag to recompute NZB file sizes in the store. Maps to DECYPHARR_FIX_NZB_SIZES=1.";
    };

    # -------------------------------------------------------------------------
    # Process / runtime
    # -------------------------------------------------------------------------

    umask = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = "Process umask (e.g. '0002'). Maps to the UMASK env var (no DECYPHARR_ prefix).";
    };

    fuseBackend = lib.mkOption {
      type = lib.types.enum [ "hanwen" "cgofuse" ];
      default = "hanwen";
      description = "FUSE backend to use. Maps to DFS_FUSE_BACKEND (no DECYPHARR_ prefix).";
    };

    enablePprof = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Enable the pprof debug HTTP server. Maps to ENABLE_PPROF (no DECYPHARR_ prefix).";
    };

    yencPureGo = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = "Use pure-Go yenc decoder instead of the optimised C one. Maps to YENC_PURE_GO=true.";
    };

    # -------------------------------------------------------------------------
    # DFS (FUSE) cache
    # -------------------------------------------------------------------------

    dfs = lib.mkOption {
      default = { };
      description = "DFS FUSE backend settings. All fields map to DECYPHARR_MOUNT__DFS__* env vars.";
      type = lib.types.submodule {
        options = {
          cacheDir = lib.mkOption {
            type = lib.types.str;
            default = "/var/cache/decypharr";
            description = "Sparse-file cache directory. Maps to DECYPHARR_MOUNT__DFS__CACHE_DIR.";
          };
          diskCacheSize = lib.mkOption {
            type = lib.types.str;
            default = "500MB";
            description = "Max disk space for the cache (e.g. '10GB'). Maps to DECYPHARR_MOUNT__DFS__DISK_CACHE_SIZE.";
          };
          cacheExpiry = lib.mkOption {
            type = lib.types.str;
            default = "24h";
            description = "Evict cached files unused for this long. Maps to DECYPHARR_MOUNT__DFS__CACHE_EXPIRY.";
          };
          cacheCleanupInterval = lib.mkOption {
            type = lib.types.str;
            default = "5m";
            description = "How often the eviction loop runs. Maps to DECYPHARR_MOUNT__DFS__CACHE_CLEANUP_INTERVAL.";
          };
          chunkSize = lib.mkOption {
            type = lib.types.str;
            default = "8MB";
            description = "Download chunk size per HTTP request. Maps to DECYPHARR_MOUNT__DFS__CHUNK_SIZE.";
          };
          readAheadSize = lib.mkOption {
            type = lib.types.str;
            default = "128MB";
            description = "Prefetch window size ahead of read position. Maps to DECYPHARR_MOUNT__DFS__READ_AHEAD_SIZE.";
          };
          daemonTimeout = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "FUSE daemon idle timeout before exit. Maps to DECYPHARR_MOUNT__DFS__DAEMON_TIMEOUT.";
          };
          uid = lib.mkOption {
            type = lib.types.int;
            default = 0;
            description = "UID for files in the FUSE mount (0 = inherit from service user). Maps to DECYPHARR_MOUNT__DFS__UID.";
          };
          gid = lib.mkOption {
            type = lib.types.int;
            default = 0;
            description = "GID for files in the FUSE mount (0 = inherit from service group). Maps to DECYPHARR_MOUNT__DFS__GID.";
          };
          umask = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Umask for files in the FUSE mount (e.g. '0022'). Maps to DECYPHARR_MOUNT__DFS__UMASK.";
          };
        };
      };
    };

    # -------------------------------------------------------------------------
    # Usenet
    # -------------------------------------------------------------------------

    usenet = lib.mkOption {
      default = { };
      description = "Usenet global settings. All fields map to DECYPHARR_USENET__* env vars. Providers go in settings or environmentFiles.";
      type = lib.types.submodule {
        options = {
          maxConnections = lib.mkOption {
            type = lib.types.int;
            default = 0;
            description = "Max concurrent connections per file (0 = use default of 15). Maps to DECYPHARR_USENET__MAX_CONNECTIONS.";
          };
          readAhead = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Prefetch buffer size (e.g. '32MB'). Maps to DECYPHARR_USENET__READ_AHEAD.";
          };
          processingTimeout = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "Timeout before marking an NZB bad (e.g. '15m'). Maps to DECYPHARR_USENET__PROCESSING_TIMEOUT.";
          };
          availabilitySamplePercent = lib.mkOption {
            type = lib.types.int;
            default = 0;
            description = "Percentage of segments to check for availability (0 = use default of 10). Maps to DECYPHARR_USENET__AVAILABILITY_SAMPLE_PERCENT.";
          };
          maxConcurrentNZB = lib.mkOption {
            type = lib.types.int;
            default = 0;
            description = "NZBs processed in parallel (0 = use default of 2). Maps to DECYPHARR_USENET__MAX_CONCURRENT_NZB.";
          };
          skipRepair = lib.mkOption {
            type = lib.types.bool;
            default = false;
            description = "Skip par2 repair for usenet files. Maps to DECYPHARR_USENET__SKIP_REPAIR.";
          };
          socketReadBuffer = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "TCP receive buffer per NNTP connection (e.g. '8MB'). Default 4MB. Maps to DECYPHARR_USENET__SOCKET_READ_BUFFER.";
          };
          socketWriteBuffer = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = "TCP send buffer per NNTP connection (e.g. '2MB'). Default 1MB. Maps to DECYPHARR_USENET__SOCKET_WRITE_BUFFER.";
          };
          importAvailabilitySamplePercent = lib.mkOption {
            type = lib.types.int;
            default = 0;
            description = "Segment check % when adding an NZB (0 = default 1%). Maps to DECYPHARR_USENET__IMPORT_AVAILABILITY_SAMPLE_PERCENT.";
          };
        };
      };
    };

    # -------------------------------------------------------------------------
    # Full structured config (written to config.json)
    # Use this for debrids, arrs, usenet, mount type/path, and anything
    # not covered by the scalar options above.
    # Secret values (API keys, tokens) belong in environmentFiles.
    # -------------------------------------------------------------------------

    settings = lib.mkOption {
      type = format.type;
      default = { };
      description = ''
        Full decypharr config, serialised to config.json at service start.
        Scalar options above take precedence via env vars (they override config.json).

        Put structured config here: mount type, debrids list, arrs list, usenet providers.
        Do NOT put secrets here — they end up in the Nix store.
        Use environmentFiles for API keys and tokens.
      '';
      example = lib.literalExpression ''
        {
          mount = {
            type = "dfs";
            mount_path = "/mnt/decypharr";
          };
          debrids = [
            {
              provider = "realdebrid";
              name = "realdebrid";
              # api_key via environmentFiles: DECYPHARR_DEBRIDS__0__API_KEY=...
            }
          ];
          arrs = [
            {
              name = "sonarr";
              host = "http://sonarr:8989";
              # token via environmentFiles: DECYPHARR_ARRS__0__TOKEN=...
            }
            {
              name = "radarr";
              host = "http://radarr:7878";
            }
          ];
        }
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    boot.kernelModules = [ "fuse" ];
    programs.fuse.userAllowOther = true;

    users.users.${cfg.user} = {
      isSystemUser = true;
      group = cfg.group;
      extraGroups = cfg.extraGroups
        ++ lib.optional (cfg.mediaGroup != "") cfg.mediaGroup;
    };
    users.groups.${cfg.group} = { };

    systemd.tmpfiles.rules = [
      "d ${cfg.dfs.cacheDir} 0750 ${cfg.user} ${cfg.group} -"
      (if cfg.mediaGroup != ""
       then "d ${cfg.downloadFolder} 0775 ${cfg.user} ${cfg.mediaGroup} -"
       else "d ${cfg.downloadFolder} 0750 ${cfg.user} ${cfg.group} -")
    ];

    systemd.services.decypharr = {
      description = "Decypharr — debrid mock qBittorrent";
      after = [ "network.target" ];
      wantedBy = [ "multi-user.target" ];
      # Write settings to config.json before start (runs as root to reach StateDirectory).
      serviceConfig.ExecStartPre = [
        "+${pkgs.coreutils}/bin/install -m 600 -o ${cfg.user} -g ${cfg.group} ${configFile} ${cfg.configDir}/config.json"
      ];

      # Scalar options override anything in settings via env vars.
      # filterAttrs drops empty strings so unset options don't clobber config.json defaults.
      environment = lib.filterAttrs (_: v: v != "") ({
        DECYPHARR_PORT                               = toString cfg.port;
        DECYPHARR_BIND_ADDRESS                       = cfg.bindAddress;
        DECYPHARR_URL_BASE                           = cfg.urlBase;
        DECYPHARR_LOG_LEVEL                          = cfg.logLevel;
        DECYPHARR_USE_AUTH                           = if cfg.useAuth then "true" else "false";
        DECYPHARR_ENABLE_WEBDAV_AUTH                 = if cfg.enableWebdavAuth then "true" else "false";
        DECYPHARR_DOWNLOAD_FOLDER                    = cfg.downloadFolder;
        DECYPHARR_REFRESH_INTERVAL                   = cfg.refreshInterval;
        DECYPHARR_MAX_DOWNLOADS                      = toString cfg.maxDownloads;
        DECYPHARR_SKIP_PRE_CACHE                     = if cfg.skipPreCache then "true" else "false";
        DECYPHARR_SKIP_AUTO_MOVE                     = if cfg.skipAutoMove then "true" else "false";
        DECYPHARR_ALWAYS_RM_TRACKER_URLS             = if cfg.alwaysRmTrackerUrls then "true" else "false";
        DECYPHARR_RETRIES                            = toString cfg.retries;
        DECYPHARR_MIN_FILE_SIZE                      = cfg.minFileSize;
        DECYPHARR_MAX_FILE_SIZE                      = cfg.maxFileSize;
        DECYPHARR_REMOVE_STALLED_AFTER               = cfg.removeStalledAfter;
        DECYPHARR_NZB_USER_AGENT                     = cfg.nzbUserAgent;
        DECYPHARR_MOUNT__DFS__CACHE_DIR              = cfg.dfs.cacheDir;
        DECYPHARR_MOUNT__DFS__DISK_CACHE_SIZE        = cfg.dfs.diskCacheSize;
        DECYPHARR_MOUNT__DFS__CACHE_EXPIRY           = cfg.dfs.cacheExpiry;
        DECYPHARR_MOUNT__DFS__CACHE_CLEANUP_INTERVAL = cfg.dfs.cacheCleanupInterval;
        DECYPHARR_MOUNT__DFS__CHUNK_SIZE             = cfg.dfs.chunkSize;
        DECYPHARR_MOUNT__DFS__READ_AHEAD_SIZE        = cfg.dfs.readAheadSize;
        DECYPHARR_MOUNT__DFS__DAEMON_TIMEOUT         = cfg.dfs.daemonTimeout;
        DECYPHARR_MOUNT__DFS__UMASK                  = cfg.dfs.umask;
      } // lib.optionalAttrs (cfg.dfs.uid != 0) {
        DECYPHARR_MOUNT__DFS__UID                    = toString cfg.dfs.uid;
      } // lib.optionalAttrs (cfg.dfs.gid != 0) {
        DECYPHARR_MOUNT__DFS__GID                    = toString cfg.dfs.gid;
      } // lib.optionalAttrs (cfg.usenet.maxConnections != 0) {
        DECYPHARR_USENET__MAX_CONNECTIONS            = toString cfg.usenet.maxConnections;
      } // lib.optionalAttrs (cfg.usenet.maxConcurrentNZB != 0) {
        DECYPHARR_USENET__MAX_CONCURRENT_NZB         = toString cfg.usenet.maxConcurrentNZB;
      } // lib.optionalAttrs (cfg.usenet.availabilitySamplePercent != 0) {
        DECYPHARR_USENET__AVAILABILITY_SAMPLE_PERCENT = toString cfg.usenet.availabilitySamplePercent;
      } // lib.optionalAttrs (cfg.usenet.skipRepair) {
        DECYPHARR_USENET__SKIP_REPAIR                = "true";
      } // lib.optionalAttrs (cfg.usenet.importAvailabilitySamplePercent != 0) {
        DECYPHARR_USENET__IMPORT_AVAILABILITY_SAMPLE_PERCENT = toString cfg.usenet.importAvailabilitySamplePercent;
      } // lib.filterAttrs (_: v: v != "") {
        DECYPHARR_USENET__READ_AHEAD                 = cfg.usenet.readAhead;
        DECYPHARR_USENET__PROCESSING_TIMEOUT         = cfg.usenet.processingTimeout;
        DECYPHARR_USENET__SOCKET_READ_BUFFER         = cfg.usenet.socketReadBuffer;
        DECYPHARR_USENET__SOCKET_WRITE_BUFFER        = cfg.usenet.socketWriteBuffer;
        # No DECYPHARR_ prefix — these are read directly via os.Getenv in the binary.
        UMASK                                        = cfg.umask;
        DFS_FUSE_BACKEND                             = cfg.fuseBackend;
      } // lib.optionalAttrs cfg.enablePprof {
        ENABLE_PPROF                                 = "1";
      } // lib.optionalAttrs cfg.yencPureGo {
        YENC_PURE_GO                                 = "true";
      } // lib.optionalAttrs cfg.fixNzbSizes {
        DECYPHARR_FIX_NZB_SIZES                      = "1";
        # DECYPHARR_SECRET_KEY should be set via environmentFiles — it is a session secret.
      });

      serviceConfig = {
        ExecStart = "${cfg.package}/bin/decypharr --config ${cfg.configDir}";
        User = cfg.user;
        Group = cfg.group;
        WorkingDirectory = cfg.configDir;
        Restart = "on-failure";
        RestartSec = 5;
        StateDirectory = "decypharr";
        EnvironmentFile = cfg.environmentFiles;

        # FUSE needs /dev/fuse and the mount/umount2 syscalls.
        DeviceAllow = [ "/dev/fuse rw" ];
        PrivateDevices = false;
        UMask = "0002";
        SystemCallFilter = [
          "@system-service"
          "mount"
          "umount2"
        ];
        RestrictAddressFamilies = "AF_INET AF_INET6 AF_UNIX";
        RestrictNamespaces = true;
        LockPersonality = true;
      };
    };

    networking.firewall.allowedTCPPorts = lib.mkIf cfg.openFirewall [ cfg.port ];
  };
}
