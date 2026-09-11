# Decypharr Fork — Nixfiles Handoff

## Branch: `minz` on `github.com/minz1/decypharr`

---

## What's in this fork vs upstream main

| Change | Status |
|--------|--------|
| Stable FUSE inodes (NFS fix) | ✅ our change |
| Nix flake + NixOS module | ✅ our change |
| DFS cache bypass when over budget | ✅ our change |
| Force-close zero-open items on eviction (all platforms) | ✅ our change |
| Beta branch merged (all upstream beta commits) | ✅ merged |
| Vendored hash updated (Go 1.26, dropped melbahja/got) | ✅ done |

---

## Wiring into nixfiles

### flake.nix input

```nix
inputs.decypharr = {
  url = "github:minz1/decypharr";
  inputs.nixpkgs.follows = "nixpkgs";
};
```

### Host module

```nix
imports = [ inputs.decypharr.nixosModules.default ];

services.decypharr = {
  enable       = true;
  openFirewall = true;
  environmentFiles = [ config.age.secrets.decypharr.path ];

  dfs = {
    cacheDir             = "/var/cache/decypharr";
    diskCacheSize        = "50GB";
    cacheExpiry          = "24h";
    cacheCleanupInterval = "1m";
    uid                  = <your uid>;
    gid                  = <your gid>;
  };

  settings = {
    mount = { type = "dfs"; mount_path = "/mnt/decypharr"; };
    debrids = [{ provider = "realdebrid"; name = "realdebrid"; }];
    arrs = [
      { name = "sonarr"; host = "http://sonarr:8989"; }
      { name = "radarr"; host = "http://radarr:7878"; }
    ];
  };
};
```

### Remove from nixfiles (NFS hack no longer needed)

```nix
# DELETE from modules/services/decypharr.nix:
ExecStartPost = "+${pkgs.systemd}/bin/systemctl restart nfs-server";

# DELETE from hosts/minz-arr-0/configuration.nix:
systemd.services.nfs-server = {
  wants = [ "decypharr.service" ];
  after = [ "decypharr.service" ];
};
```

---

## Runtime concerns to watch

### 1. Inode stability (the whole point)

```bash
ls -lai /mnt/decypharr/__all__/ | head -20 > /tmp/before.txt
systemctl restart decypharr   # do NOT restart nfs-server
sleep 5
ls -lai /mnt/decypharr/__all__/ | head -20 > /tmp/after.txt
diff /tmp/before.txt /tmp/after.txt   # must be empty
```

If inodes still change: check `journalctl -u decypharr` for FUSE mount errors. The fix is in `pkg/mount/dfs/backend/hanwen/dir.go:104`.

### 2. Cache bypass (new behaviour)

When cache crosses the eviction threshold, new file opens stream directly from the debrid network without writing to disk. Watch for:

```bash
journalctl -u decypharr -f | grep "cache over budget"
```

Direct-streamed reads have no prefetch — seeking is slower but nothing stalls. If you see this triggering immediately on start, your `totalSize` from the previous run is stale; restart will recalculate on the first eviction pass.

### 3. Beta buffer system

The beta merge brought a new `internal/buffer` package (LRU block cache layered over the sparse file). If DFS reads are behaving oddly, this is the most likely culprit. There are no new config knobs for it — it activates automatically.

### 4. Usenet: new provider fields

Beta added two new `UsenetProvider` fields:
- `backup: true` — marks a provider as backup-tier; only used when all primary providers are unreachable or exhausted
- `backbone` — shared backbone identifier for failover grouping

These default to zero values (no backup, no backbone) so existing configs are unaffected. If you have multiple usenet providers, consider marking your block accounts as `backup: true` in `settings.usenet.providers`.

### 5. New usenet config (available in module)

```nix
services.decypharr.usenet = {
  socketReadBuffer  = "8MB";   # default 4MB; raise for high-latency/high-bandwidth
  socketWriteBuffer = "2MB";   # default 1MB
  importAvailabilitySamplePercent = 5;  # default 1%; raise if getting bad NZBs
};
```

### 6. Go 1.26 + dropped dependency

`github.com/melbahja/got` was removed upstream. If anything downstream referenced it, it no longer exists. Shouldn't affect runtime but worth knowing.

---

## If things break

| Symptom | Likely cause | Where to look |
|---------|-------------|---------------|
| NFS still stale after restart | inode fix not active (wrong binary?) | `./result/bin/decypharr --help` → check version |
| EIO on reads when cache full | `DirectStreamFile` failing to reach debrid | `journalctl -u decypharr \| grep "DFS-direct"` |
| DFS mount never comes up | FUSE mount race or allow_other missing | `journalctl -u decypharr \| grep "hanwen\|mount"` |
| Usenet articles failing | Backup provider tier logic (new) | Check provider priority/backup fields in config |
| Repair jobs not running | Repair rewrite in beta changed scheduling | Check repair config in the web UI |

Report regressions at `github.com/minz1/decypharr`.
