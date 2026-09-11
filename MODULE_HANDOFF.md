# Decypharr Module Fix — nixfiles integration issue

## Repo

Fork: `github.com/minz1/decypharr`, branch `minz`  
Module: `nix/module.nix`

## The problem

The module creates a tmpfiles rule for `downloadFolder`:

```nix
systemd.tmpfiles.rules = [
  "d ${cfg.dfs.cacheDir} 0750 ${cfg.user} ${cfg.group} -"
  "d ${cfg.downloadFolder} 0750 ${cfg.user} ${cfg.group} -"  # ← this one
];
```

In nixfiles, `downloadFolder = "/data/downloads"`. The arr stack (Sonarr, Radarr, Bazarr) runs as separate users (sonarr, radarr, bazarr) that are all members of a shared `media` group. The download subdirectories need to be accessible to those users:

```
/data/downloads         0775  root     media
/data/downloads/sonarr  2775  sonarr   media
/data/downloads/radarr  2775  radarr   media
```

The module's `0750 decypharr:decypharr` on `/data/downloads` means `ls /data/downloads/` returns `Permission denied` for the sonarr/radarr users, even though their own subdirectories have correct permissions.

## The fix

Add a `mediaGroup` option (mirrors the old nixfiles `services.decypharr.mediaGroup`):

```nix
mediaGroup = lib.mkOption {
  type = lib.types.str;
  default = "";
  description = ''
    If set, the download folder is created as group-writable (0775) with this
    group rather than 0750 decypharr:decypharr. Use when the download directory
    is shared with other services (e.g. sonarr, radarr) via a common media group.
  '';
  example = "media";
};
```

Then in the tmpfiles rule:

```nix
systemd.tmpfiles.rules = [
  "d ${cfg.dfs.cacheDir} 0750 ${cfg.user} ${cfg.group} -"
  (if cfg.mediaGroup != ""
   then "d ${cfg.downloadFolder} 0775 ${cfg.user} ${cfg.mediaGroup} -"
   else "d ${cfg.downloadFolder} 0750 ${cfg.user} ${cfg.group} -")
];
```

And add the user to the group when set:

```nix
users.users.${cfg.user} = {
  isSystemUser = true;
  group = cfg.group;
  extraGroups = cfg.extraGroups
    ++ lib.optional (cfg.mediaGroup != "") cfg.mediaGroup;
};
```

## nixfiles usage after fix

```nix
services.decypharr = {
  mediaGroup = "media";
  # ...
};
```

## Also needed: indexer failures

Sonarr/Radarr/Prowlarr are reporting all indexers unavailable (Torrentio, Nyaa, AnimeTosho, EZTV, 1337x, NZBgeek failing for 6+ hours). This may be a separate Prowlarr/Flaresolverr issue unrelated to the module — investigate after the permissions fix.
