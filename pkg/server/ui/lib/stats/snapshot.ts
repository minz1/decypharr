import { asRecord } from "#ui/lib/common/json";
import { accessViews, parseAccess, renderAccess } from "#ui/lib/stats/access";
import { parseDebrids, renderDebrids } from "#ui/lib/stats/debrid";
import type { PageLocation, StatsElements } from "#ui/lib/stats/elements";
import { parseMount, renderMount } from "#ui/lib/stats/mount";
import { parseOverview, renderOverview } from "#ui/lib/stats/overview";
import { optRecord, recordOrEmpty } from "#ui/lib/stats/read";
import { parseStreams, renderStreams } from "#ui/lib/stats/streams";
import { parseUsenet, renderUsenet } from "#ui/lib/stats/usenet";

interface Snapshot {
  overview: ReturnType<typeof parseOverview>;
  mount: ReturnType<typeof parseMount>;
  debrids: ReturnType<typeof parseDebrids>;
  usenet: ReturnType<typeof parseUsenet>;
  streams: ReturnType<typeof parseStreams>;
  access: ReturnType<typeof parseAccess>;
}

const parseSnapshot = (value: unknown): Snapshot => {
  const stats = asRecord(value, "stats");
  return {
    overview: parseOverview(stats),
    mount: parseMount(optRecord(stats, "mount")),
    debrids: parseDebrids(stats["debrids"]),
    usenet: parseUsenet(optRecord(stats, "usenet")),
    streams: parseStreams(optRecord(stats, "active_streams")),
    access: parseAccess(recordOrEmpty(stats, "access")),
  };
};

const renderSnapshot = (elements: StatsElements, snapshot: Snapshot, where: PageLocation): void => {
  renderOverview(elements.overview, snapshot.overview);
  renderMount(elements.mount, snapshot.mount);
  renderDebrids(elements.debrids, snapshot.debrids);
  renderUsenet(elements.usenet, snapshot.usenet);
  renderStreams(elements.streams, snapshot.streams, Date.now());
  renderAccess(elements.access, accessViews(snapshot.access, where));
};

export { parseSnapshot, renderSnapshot };
