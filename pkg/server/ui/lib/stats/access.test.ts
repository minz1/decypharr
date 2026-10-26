import { describe, expect, it } from "vitest";

import { ShapeError } from "#ui/lib/common/json";
import { accessViews, parseAccess } from "#ui/lib/stats/access";

const where = { hostname: "nas.local", protocol: "https:" };

const views = (access: Record<string, unknown>, at = where): ReturnType<typeof accessViews> =>
  accessViews(parseAccess(access), at);

describe("the WebDAV card", () => {
  it("shows the URL on the page's host and protocol", () => {
    expect(
      views({ webdav: { enabled: true, path: "/webdav", port: "8282", auth_required: true } }).webdav,
    ).toStrictEqual({
      status: { text: "Enabled", className: "badge badge-success" },
      rows: [{ label: "URL (browser, rclone, Kodi, Infuse…)", value: "https://nas.local:8282/webdav/" }],
      note: { kind: "text", text: "Requires your Decypharr username and password." },
    });
  });

  it("defaults the path, drops an empty port and falls back to SERVER_IP", () => {
    expect(views({ webdav: { enabled: true, port: "" } }, { hostname: "", protocol: "http:" }).webdav).toStrictEqual({
      status: { text: "Enabled", className: "badge badge-success" },
      rows: [{ label: "URL (browser, rclone, Kodi, Infuse…)", value: "http://SERVER_IP/webdav/" }],
      note: { kind: "text", text: "No authentication required." },
    });
  });

  it("says it is disabled", () => {
    expect(views({}).webdav).toStrictEqual({
      status: { text: "Disabled", className: "badge badge-ghost" },
      rows: [],
      note: { kind: "off", text: "WebDAV is disabled in settings." },
    });
  });
});

describe("the NFS card", () => {
  it("shows Linux and macOS mount commands on the configured port", () => {
    expect(views({ nfs: { enabled: true, port: 2049 } }).nfs).toStrictEqual({
      status: { text: "Enabled", className: "badge badge-success" },
      rows: [
        { label: "Linux", value: "sudo mount -t nfs -o vers=4.0,tcp,port=2049,ro nas.local:/ /mnt/decypharr" },
        {
          label: "macOS",
          value: "sudo mount -t nfs -o vers=4.0,tcp,port=2049,ro,resvport nas.local:/ /Volumes/decypharr",
        },
      ],
      note: { kind: "nfs", port: "2049" },
    });
  });

  it("defaults the port to 20490", () => {
    expect(views({ nfs: { enabled: true, port: 0 } }).nfs.note).toStrictEqual({ kind: "nfs", port: "20490" });
  });

  it("says how to enable it", () => {
    expect(views({ nfs: { enabled: false, port: 2049 } }).nfs).toStrictEqual({
      status: { text: "Disabled", className: "badge badge-ghost" },
      rows: [],
      note: { kind: "off", text: "Enable the NFSv4 server in Settings → Mount Settings." },
    });
  });
});

describe("the SMB card", () => {
  it("shows the Windows, macOS and Linux commands", () => {
    expect(views({ smb: { enabled: true, port: 1446, share_name: "media", username: "kim" } }).smb).toStrictEqual({
      status: { text: "Experimental", className: "badge badge-warning" },
      rows: [
        { label: "Windows (needs host port 445)", value: String.raw`net use Z: \\nas.local\media /user:kim` },
        { label: "macOS", value: "open smb://kim@nas.local:1446/media" },
        {
          label: "Linux",
          value: "sudo mount -t cifs -o vers=3.1.1,port=1446,user=kim,ro //nas.local/media /mnt/decypharr",
        },
      ],
      note: { kind: "smb", port: "1446" },
    });
  });

  it("defaults the share, user and port", () => {
    expect(views({ smb: { enabled: true } }).smb.rows.map((row) => row.value)).toStrictEqual([
      String.raw`net use Z: \\nas.local\decypharr /user:USERNAME`,
      "open smb://USERNAME@nas.local:1445/decypharr",
      "sudo mount -t cifs -o vers=3.1.1,port=1445,user=USERNAME,ro //nas.local/decypharr /mnt/decypharr",
    ]);
  });

  it("says how to enable it", () => {
    expect(views({ smb: { enabled: false } }).smb.note).toStrictEqual({
      kind: "off",
      text: "Enable the SMB server in Settings → Mount Settings.",
    });
  });
});

describe("a malformed access section", () => {
  it("throws a shape error", () => {
    expect(() => parseAccess({ nfs: { enabled: true, port: "2049" } })).toThrow(ShapeError);
  });
});
